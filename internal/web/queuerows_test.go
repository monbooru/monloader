package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/internal/sitestate"
)

func leadJob(id int64, status queue.JobStatus, url string) *queue.Job {
	j := &queue.Job{}
	j.ID = id
	j.Status = status
	j.URL = url
	return j
}

func row(g jobGroup) jobGroup {
	g.Worst = g.Lead
	g.Count = len(g.Items)
	for _, it := range g.Items {
		if it.Outcome == outcome.SkippedArchive {
			g.Summary.Archived++
		}
	}
	return g
}

type itemsProc struct{}

func (itemsProc) Process(_ context.Context, job *queue.Job) error {
	job.SetSite("danbooru")
	job.SetItems([]queue.Item{
		{PostID: "a", Num: 1, URL: "https://example.com/posts/100"},
		{PostID: "b", Num: 2},
		{PostID: "c", Num: 3},
		{URL: "https://danbooru.donmai.us/posts/7"},
	})
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = outcome.Created
		it.MonbooruID = 5
		it.SHA256 = "abc123"
	})
	job.UpdateItem(1, func(it *queue.Item) {
		it.Status = queue.ItemFailed
		it.Outcome = outcome.Failed
		it.ErrorCode = outcome.ErrCodeMonbooruRejected
	})
	job.UpdateItem(2, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(2, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(2, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = outcome.Enriched
		it.MonbooruID = 9
	})
	job.UpdateItem(3, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = outcome.Enriched
		it.MonbooruID = 9
	})
	return nil
}

func serverWith(t *testing.T, proc queue.Processor) *Server {
	t.Helper()
	mb := monbooruStub()
	t.Cleanup(mb.Close)
	cfg := scratchConfig(t)
	cfg.Monbooru.APIURL = mb.URL
	cfg.Monbooru.APIToken = "tok"
	provider := config.NewProvider(cfg)
	q := queue.New(proc, 1, 100)
	q.Start()
	t.Cleanup(q.Close)
	mapper, err := mapping.New(provider, filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "monloader.toml")
	provider.Persist(configPath, "")
	srv, err := NewServer(provider, configPath, t.TempDir(), t.TempDir(), q, monbooru.New(provider), fakeRunner{}, mapper, gdl.NewCatalog("1.32.1", "1.32.1", false, []gdl.Extractor{}, nil), sitestate.New(), ptr.NewEngine(cfg.PTR), similarity.New(provider, mapper.CanonicalPostURL, mapper.PostURLFor), Desktop{})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func serveProc(t *testing.T, proc queue.Processor) (*Server, *httptest.Server) {
	t.Helper()
	srv := serverWith(t, proc)
	return srv, newTestServer(t, srv)
}

func renderRows(srv *Server, groups ...jobGroup) string {
	rec := httptest.NewRecorder()
	srv.render(rec, "queue_rows", map[string]any{"Groups": groups, "MonbooruURL": "", "CSRFToken": "t", "Page": 1, "TotalPages": 1})
	return rec.Body.String()
}

func TestAnEmptySearchNamesWhatASiteCanLeaveOut(t *testing.T) {
	srv := stubbedServer(t, "")
	lead := &queue.Job{}
	lead.ID, lead.Status = 1, queue.JobSucceeded
	body := renderRows(srv, jobGroup{Root: 1, Lead: lead, Worst: lead})
	if !strings.Contains(body, "need a login") || !strings.Contains(body, "no posts matched") {
		t.Errorf("rows = %s", body)
	}
}

func finishItem(job *queue.Job, i int, o outcome.Outcome) {
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = o
	})
}

type cappedProc struct{}

func (cappedProc) Process(_ context.Context, job *queue.Job) error {
	job.SetItems([]queue.Item{{PostID: "1"}})
	finishItem(job, 0, outcome.Created)
	job.SetCapped(8)
	return nil
}

type skipProc struct{}

func (skipProc) Process(_ context.Context, job *queue.Job) error {
	job.SetItems([]queue.Item{{PostID: "1"}, {PostID: "2"}})
	for i := 0; i < 2; i++ {
		job.UpdateItem(i, func(it *queue.Item) {
			it.Status = queue.ItemSkipped
			it.Outcome = outcome.SkippedArchive
		})
	}
	return nil
}

type replaceProc struct{}

func (replaceProc) Process(_ context.Context, job *queue.Job) error {
	job.SetItems([]queue.Item{{PostID: "1"}})
	finishItem(job, 0, outcome.Replaced)
	return nil
}

func TestQueueRowsShowsReplaced(t *testing.T) {
	srv, ts := serveProc(t, replaceProc{})
	id := srv.queue.EnqueueReplace(7, "", "http://danbooru/posts/1")
	waitJob(t, srv, id)
	_, body := get(t, ts, "/internal/queue-rows")
	if !strings.Contains(body, `<span class="o-replaced">1 replaced</span>`) {
		t.Errorf("a replace job's row should summarise the outcome, got %q", body)
	}
}

type matchProc struct{}

func (matchProc) Process(_ context.Context, job *queue.Job) error {
	job.SetItems([]queue.Item{{PostID: "1"}})
	finishItem(job, 0, outcome.Matched)
	return nil
}

func TestQueueRowsShowMatchedInItsOwnClass(t *testing.T) {
	srv, ts := serveProc(t, matchProc{})
	finishJob(t, srv, "http://danbooru/posts/1")
	_, body := get(t, ts, "/internal/queue-rows")
	if !strings.Contains(body, `<span class="o-matched">1 matched</span>`) {
		t.Errorf("a matched count should carry o-matched, got %q", body)
	}
}

type cappedOnceProc struct{}

func (cappedOnceProc) Process(_ context.Context, job *queue.Job) error {
	s := job.Snapshot()
	job.SetItems([]queue.Item{{PostID: "1"}})
	finishItem(job, 0, outcome.Created)
	if s.Offset == 0 {
		job.SetCapped(8)
	}
	return nil
}

func TestQueueRowsShowsCap(t *testing.T) {
	srv, ts := serveProc(t, cappedProc{})
	finishJob(t, srv, "http://danbooru/posts?tags=x")
	_, body := get(t, ts, "/internal/queue-rows")
	for _, want := range []string{"more available", "Get next 8"} {
		if !strings.Contains(body, want) {
			t.Errorf("a capped job should show %q, got %q", want, body)
		}
	}
}

func TestQueueGroupsContinuations(t *testing.T) {
	srv, ts := serveProc(t, cappedProc{})
	id := finishJob(t, srv, "http://danbooru/posts?tags=x")
	nid, err := srv.queue.Continue(id)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	waitJob(t, srv, nid)

	_, body := get(t, ts, "/internal/queue-rows")
	if n := strings.Count(body, "queue-url"); n != 1 {
		t.Errorf("the series should collapse into one row, got %d url cells", n)
	}
	if n := strings.Count(body, `/continue"`); n != 1 {
		t.Errorf("only the newest window should offer continue, got %d", n)
	}
	if !strings.Contains(body, "/queue/"+itoa(nid)+"/continue") {
		t.Errorf("continue should target the newest window %d, not the original", nid)
	}
	if got := srv.queueCounts().Finished; got != 1 {
		t.Errorf("finished = %d, want the series counted as the one row it renders", got)
	}
}

func TestQueueSeriesRowReportsTheWorstWindow(t *testing.T) {
	srv, ts := serveProc(t, cappedProc{})
	id := finishJob(t, srv, "http://danbooru/posts?tags=x")

	srv.queue.Pause()
	canceled, err := srv.queue.Continue(id)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if err := srv.queue.CancelLive(canceled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	last, err := srv.queue.Continue(id)
	if err != nil {
		t.Fatalf("continue again: %v", err)
	}
	srv.queue.Resume()
	waitJob(t, srv, last)

	_, body := get(t, ts, "/internal/queue-rows")
	if !strings.Contains(body, `class="st-canceled">canceled`) {
		t.Errorf("row should report the canceled window, got %q", body)
	}
	if !strings.Contains(body, `/queue/`+itoa(canceled)+`/retry"`) {
		t.Errorf("retry should be offered on the canceled window %d, got %q", canceled, body)
	}
}

func TestQueueSeriesKeepsContinueAfterACancel(t *testing.T) {
	srv, ts := serveProc(t, cappedProc{})
	id := finishJob(t, srv, "http://danbooru/posts?tags=x")
	srv.queue.Pause()
	next, err := srv.queue.Continue(id)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if err := srv.queue.CancelLive(next); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	_, body := get(t, ts, "/internal/queue-rows")
	if !strings.Contains(body, `/queue/`+itoa(id)+`/continue"`) {
		t.Errorf("continue should fall back to the capped window %d, got %q", id, body)
	}
	if !strings.Contains(body, "more available") {
		t.Error("the row should still say more is available")
	}
	if !strings.Contains(body, "Get next 8") {
		t.Errorf("the continue label should carry the window size, got %q", body)
	}
}

func TestContinueButtons(t *testing.T) {
	for _, action := range []string{"continue", "continue-all"} {
		t.Run(action, func(t *testing.T) {
			srv, ts := serveProc(t, cappedOnceProc{})
			id := finishJob(t, srv, "http://danbooru/posts?tags=x")
			if _, body := get(t, ts, "/internal/queue-rows"); !strings.Contains(body, "/queue/"+itoa(id)+"/"+action) {
				t.Errorf("a capped job row should offer a %s button", action)
			}
			before, _ := srv.queue.List(queue.ListOptions{})
			resp := postClosed(t, ts, srv, "/queue/"+itoa(id)+"/"+action, map[string][]string{})
			if resp.StatusCode != 200 {
				t.Fatalf("%s status = %d", action, resp.StatusCode)
			}
			if after, _ := srv.queue.List(queue.ListOptions{}); len(after) != len(before)+1 {
				t.Errorf("%s should queue one follow-up job: before=%d after=%d", action, len(before), len(after))
			}
		})
	}
}

func finishJob(t *testing.T, srv *Server, url string) int64 {
	t.Helper()
	id := srv.queue.Enqueue(url, queue.Options{})
	waitJob(t, srv, id)
	return id
}

func TestBulkQueueActionGating(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})

	hasClear := func(s string) bool { return strings.Contains(s, `"/queue/clear"`) }
	hasCancel := func(s string) bool { return strings.Contains(s, "/queue/cancel-pending") }

	_, empty := get(t, ts, "/internal/queue-actions")
	if hasClear(empty) || hasCancel(empty) {
		t.Errorf("an empty queue should offer neither bulk button, got %q", empty)
	}

	srv.queue.Pause()
	srv.queue.Enqueue("http://x/pending", queue.Options{})
	_, pending := get(t, ts, "/internal/queue-actions")
	if !hasCancel(pending) {
		t.Errorf("a job still in the FIFO should offer cancel pending, got %q", pending)
	}
	if hasClear(pending) {
		t.Errorf("nothing has finished, so clear should stay hidden, got %q", pending)
	}

	srv.queue.Resume()
	finishJob(t, srv, "http://x/done")
	_, done := get(t, ts, "/internal/queue-actions")
	if !hasClear(done) {
		t.Errorf("finished history should offer clear, got %q", done)
	}
	if hasCancel(done) {
		t.Errorf("an empty FIFO should hide cancel pending, got %q", done)
	}

	if _, page := get(t, ts, "/queue"); !strings.Contains(page, `id="queue-actions"`) || !hasClear(page) {
		t.Errorf("queue screen should carry the polled bulk buttons, got %q", page)
	}
}

func TestClearSucceededGating(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proc  queue.Processor
		offer bool
	}{
		{"clean", noopProc{}, true},
		{"capped", cappedProc{}, false},
		{"partial", itemsProc{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := serveProc(t, tc.proc)
			finishJob(t, srv, "http://x/done")

			_, frag := get(t, ts, "/internal/queue-actions")
			if got := strings.Contains(frag, "/queue/clear-succeeded"); got != tc.offer {
				t.Errorf("clear succeeded offered = %v, want %v, got %q", got, tc.offer, frag)
			}
			if !strings.Contains(frag, `"/queue/clear"`) {
				t.Errorf("finished history should still offer clear, got %q", frag)
			}
		})
	}
}

func TestClearConfirmPopin(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	finishJob(t, srv, "http://x/clear")
	_, body := get(t, ts, "/queue")
	for _, want := range []string{
		`id="confirm-dialog"`, `id="confirm-dialog-ok"`, `id="confirm-dialog-cancel"`,
		`hx-confirm="Clear all finished jobs?"`, `data-confirm-ok="Clear finished"`, "data-confirm-danger",
		"it may read as already fetched",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue page missing %q", want)
		}
	}
}

func TestQueueRowsRendersItems(t *testing.T) {
	srv, ts := serveProc(t, itemsProc{})

	id := finishJob(t, srv, "http://danbooru/pools/1")
	_, plain := get(t, ts, "/internal/queue-rows/"+itoa(id)+"/items")
	if strings.Contains(plain, "/i/abc123") || strings.Contains(plain, "/images/9") {
		t.Error("without web_url an item must not link monbooru through api_url")
	}
	srv.cfg.Current().Monbooru.WebURL = "http://mb.example"
	_, rows := get(t, ts, "/internal/queue-rows")
	for _, want := range []string{"items-row", "data-job=", "hx-preserve", "/internal/queue-rows/" + itoa(id) + "/items", "created</span>", "total</span>"} {
		if !strings.Contains(rows, want) {
			t.Errorf("queue rows missing %q", want)
		}
	}
	if strings.Contains(rows, "/i/abc123") {
		t.Error("a finished job's items should load lazily, not inline in the poll")
	}

	_, items := get(t, ts, "/internal/queue-rows/"+itoa(id)+"/items")
	for _, want := range []string{"/i/abc123", "/images/9", ">monbooru</a>", `<a href="https://example.com/posts/100"`, ">danbooru</a>", "o-created", "o-enriched", "o-failed", "refused by monbooru"} {
		if !strings.Contains(items, want) {
			t.Errorf("items fragment missing %q", want)
		}
	}
}

func TestQueueRowsShowWhyAJobFailed(t *testing.T) {
	srv := serverWith(t, noopProc{})
	failed := leadJob(1, queue.JobFailed, "u1")
	failed.ErrorCode, failed.Error = outcome.ErrCodeDownloadFailed, "NotFoundError: Requested post could not be found"
	running := leadJob(2, queue.JobRunning, "u2")
	coded := leadJob(3, queue.JobFailed, "u3")
	coded.ErrorCode, coded.Error = outcome.ErrCodeRateLimited, outcome.ErrCodeRateLimited+": HttpError: 429 Too Many Requests"
	groups := []jobGroup{
		row(jobGroup{Root: 1, Lead: failed}),
		row(jobGroup{Root: 2, Lead: running, Items: []queue.Item{{PostID: "a", TagWarnings: []string{"dropped an over-long tag", "second"}, TagsSent: 24, Held: []string{"lowres (held by your rules)", "watermark (rejected by the gallery)"}}}}),
		row(jobGroup{Root: 3, Lead: coded}),
	}
	body := renderRows(srv, groups...)
	for _, want := range []string{
		`<span class="job-reason" title="NotFoundError: Requested post could not be found">NotFoundError: Requested post could not be found</span>`,
		`<span class="job-reason" title="dropped an over-long tag">dropped an over-long tag</span>`,
		`<span class="o-sent" title="lowres (held by your rules), watermark (rejected by the gallery)">24 tags sent, 2 held</span>`,
		`<span class="job-reason" title="HttpError: 429 Too Many Requests">HttpError: 429 Too Many Requests</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue rows missing %s", want)
		}
	}
}

func TestQueueRowDefaultOpenState(t *testing.T) {
	srv := serverWith(t, noopProc{})
	groups := []jobGroup{
		row(jobGroup{Root: 1, Lead: leadJob(1, queue.JobRunning, "u1"), Items: []queue.Item{{PostID: "a"}}}),
		row(jobGroup{Root: 2, Lead: leadJob(2, queue.JobSucceeded, "u2"), Items: []queue.Item{{PostID: "b"}}}),
	}
	body := renderRows(srv, groups...)
	if !strings.Contains(body, `data-job="1" open`) {
		t.Error("the active (running) job should render expanded")
	}
	if !strings.Contains(body, `data-job="2" open`) {
		t.Error("a finished job should also render expanded by default")
	}
	if n := strings.Count(body, "item-row"); n != 1 {
		t.Errorf("only the active job's items should be inline, got %d item rows", n)
	}
	if !strings.Contains(body, `hx-get="/internal/queue-rows/2/items"`) {
		t.Error("the finished job should carry a lazy items loader")
	}
}

func TestReasonKeepsWhatFitsTheColumn(t *testing.T) {
	for _, tc := range []struct{ code, stored, want string }{
		{"download_failed", "download_failed: HttpError '404 Not Found' for 'https://x/posts/9.json'", "404 Not Found"},
		{"auth_required", "auth_required: [danbooru][error] HttpError '401 Unauthorized' for 'https://x/1'", "401 Unauthorized"},
		{"unsupported_url", "unsupported_url: [gallery-dl][error] Unsupported URL 'https://x/1'", ""},
		{"unsupported_url", "unsupported_url: the url names more than one post", "the url names more than one post"},
		{"download_failed", "download_failed: [gallery-dl][error] no such file", "no such file"},
	} {
		g := jobGroup{Lead: &queue.Job{ErrorCode: tc.code, Error: tc.stored}}
		if got := g.Reason(); got != tc.want {
			t.Errorf("Reason(%q) = %q, want %q", tc.stored, got, tc.want)
		}
	}
}

func TestRowPrintsATotalOnlyWhenOutcomesAddUp(t *testing.T) {
	for _, tc := range []struct {
		s    queue.Summary
		want bool
	}{
		{queue.Summary{Created: 1, Total: 1}, false},
		{queue.Summary{Skipped: 3, Blacklisted: 3, Total: 3}, false},
		{queue.Summary{Created: 3, Duplicate: 2, Total: 5}, true},
		{queue.Summary{Created: 3, Total: 5}, true},
	} {
		if got := (jobGroup{Summary: tc.s}).ShowTotal(); got != tc.want {
			t.Errorf("%+v: ShowTotal = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestQueueRowPhase(t *testing.T) {
	srv := serverWith(t, noopProc{})
	groups := []jobGroup{
		row(jobGroup{Root: 1, Lead: leadJob(1, queue.JobRunning, "u1"), Items: []queue.Item{{PostID: "a", Status: queue.ItemPending}}}),
		row(jobGroup{Root: 2, Lead: leadJob(2, queue.JobRunning, "u2"), Items: []queue.Item{{PostID: "b", Status: queue.ItemDownloaded}}}),
		row(jobGroup{Root: 3, Lead: leadJob(3, queue.JobSucceeded, "u3"), Items: []queue.Item{{PostID: "c", Status: queue.ItemDone}}}),
	}
	body := renderRows(srv, groups...)
	for _, want := range []string{"downloading...", "sending..."} {
		if !strings.Contains(body, want) {
			t.Errorf("queue row phase %q missing from render", want)
		}
	}
	if !strings.Contains(body, "job-phase-downloading") || strings.Contains(body, "job-phase-finished") {
		t.Error("only a running job's phase span should render")
	}
}

func TestInterruptedRowSaysContentsUnknown(t *testing.T) {
	srv := serverWith(t, noopProc{})
	body := renderRows(srv, row(jobGroup{Root: 1, Lead: leadJob(1, queue.JobInterrupted, "u")}))
	if !strings.Contains(body, "contents unknown") || strings.Contains(body, "0 total") {
		t.Errorf("interrupted row should not report empty counts, got %q", body)
	}
}

func TestReplaceRowReadsAsMonboorusUpgrade(t *testing.T) {
	srv := serverWith(t, noopProc{})
	lead := leadJob(1, queue.JobSucceeded, "u")
	lead.Kind = queue.KindReplace
	if body := renderRows(srv, row(jobGroup{Root: 1, Lead: lead})); !strings.Contains(body, `job-kind">upgrade</span>`) {
		t.Errorf("a replace job should read upgrade in its badge and tooltip, got %q", body)
	}
}

func TestMetadataRowReadsAsMonboorusRefresh(t *testing.T) {
	srv := serverWith(t, noopProc{})
	lead := leadJob(1, queue.JobSucceeded, "u")
	lead.Kind = queue.KindMetadata
	if body := renderRows(srv, row(jobGroup{Root: 1, Lead: lead})); !strings.Contains(body, `job-kind">refresh</span>`) {
		t.Errorf("a metadata job should read refresh in its badge and tooltip, got %q", body)
	}
}

func TestQueueRowActionGating(t *testing.T) {
	srv := serverWith(t, noopProc{})
	hasRetry := func(s string) bool { return strings.Contains(s, `/retry"`) }
	hasForce := func(s string) bool { return strings.Contains(s, "force=1") }
	hasRemove := func(s string) bool { return strings.Contains(s, `hx-delete="/queue/`) }

	ok := renderRows(srv, row(jobGroup{Root: 1, Lead: leadJob(1, queue.JobSucceeded, "u"), Summary: queue.Summary{Created: 1, Total: 1}}))
	if hasRetry(ok) || hasForce(ok) {
		t.Errorf("a clean success should offer neither retry nor force download, got %q", ok)
	}
	if !hasRemove(ok) {
		t.Error("remove should always be offered")
	}

	skip := renderRows(srv, row(jobGroup{Root: 2, Lead: leadJob(2, queue.JobSucceeded, "u"), Summary: queue.Summary{Skipped: 2, Total: 2},
		Items: []queue.Item{{Outcome: outcome.SkippedArchive}, {Outcome: outcome.SkippedArchive}}}))
	if !hasForce(skip) {
		t.Errorf("a job with archive-skipped items should offer force download, got %q", skip)
	}
	if hasRetry(skip) {
		t.Errorf("a succeeded job should not offer retry, got %q", skip)
	}

	unsupported := renderRows(srv, row(jobGroup{Root: 5, Lead: leadJob(5, queue.JobSucceeded, "u"), Summary: queue.Summary{Skipped: 1, Created: 1, Total: 2},
		Items: []queue.Item{{Outcome: outcome.Created}, {Outcome: outcome.SkippedUnsupported}}}))
	if hasForce(unsupported) {
		t.Errorf("an unsupported-type skip should not offer force download, got %q", unsupported)
	}

	partial := renderRows(srv, row(jobGroup{Root: 3, Lead: leadJob(3, queue.JobPartial, "u"), Summary: queue.Summary{Created: 1, Failed: 1, Total: 2}}))
	if !hasRetry(partial) {
		t.Errorf("a partial job should offer retry, got %q", partial)
	}
	if hasForce(partial) {
		t.Errorf("a job with no skips should not offer force download, got %q", partial)
	}

	canceled := renderRows(srv, row(jobGroup{Root: 4, Lead: leadJob(4, queue.JobCanceled, "u"), Summary: queue.Summary{Canceled: 1, Total: 1}}))
	if !hasRetry(canceled) {
		t.Errorf("a canceled job should offer retry, got %q", canceled)
	}
}

func TestARowSaysWhatItsArchiveSkipsAndRemovalMean(t *testing.T) {
	srv := serverWith(t, noopProc{})
	const fetched = "Already fetched by an earlier run"
	const reAdd = "it may read as already fetched"

	skip := renderRows(srv, row(jobGroup{Root: 1, Lead: leadJob(1, queue.JobSucceeded, "u"), Summary: queue.Summary{Skipped: 2, Total: 2},
		Items: []queue.Item{{Outcome: outcome.SkippedArchive}, {Outcome: outcome.SkippedArchive}}}))
	if !strings.Contains(skip, fetched) {
		t.Errorf("an archive skip should say the earlier run may not have reached monbooru, got %q", skip)
	}
	if strings.Contains(skip, reAdd) {
		t.Errorf("removing a clean row loses nothing, got %q", skip)
	}

	unsupported := renderRows(srv, row(jobGroup{Root: 2, Lead: leadJob(2, queue.JobSucceeded, "u"), Summary: queue.Summary{Skipped: 1, Total: 1},
		Items: []queue.Item{{Outcome: outcome.SkippedUnsupported}}}))
	if strings.Contains(unsupported, fetched) {
		t.Errorf("an unsupported-type skip was never fetched, got %q", unsupported)
	}

	failed := renderRows(srv, row(jobGroup{Root: 3, Lead: leadJob(3, queue.JobFailed, "u"), Summary: queue.Summary{Failed: 1, Total: 1}}))
	if !strings.Contains(failed, reAdd) {
		t.Errorf("removing a failed row should warn about a re-add, got %q", failed)
	}
}

func TestQueueItemsCap(t *testing.T) {
	srv := serverWith(t, noopProc{})
	mk := func(n int) []queue.Item {
		items := make([]queue.Item, n)
		for i := range items {
			items[i] = queue.Item{PostID: itoa(int64(i + 1)), Num: i + 1}
		}
		return items
	}
	render := func(items []queue.Item) string {
		rec := httptest.NewRecorder()
		srv.render(rec, "queue_items_capped", map[string]any{"Items": items, "MonbooruURL": ""})
		return rec.Body.String()
	}

	body := render(mk(maxQueueItems + 5))
	if n := strings.Count(body, "item-row"); n != maxQueueItems+5 {
		t.Errorf("all items should render, got %d rows", n)
	}
	if !strings.Contains(body, "more-items") || !strings.Contains(body, "+5 more") {
		t.Errorf("over the cap should show a +5 more toggle, got %q", body)
	}
	if body := render(mk(maxQueueItems)); strings.Contains(body, "more-items") {
		t.Error("a job at the cap should not show a more toggle")
	}
}

type holdingProc struct {
	items   int
	started chan struct{}
	release chan struct{}
}

func (p holdingProc) Process(_ context.Context, job *queue.Job) error {
	items := make([]queue.Item, p.items)
	for i := range items {
		items[i] = queue.Item{PostID: itoa(int64(i + 1)), URL: "https://danbooru.donmai.us/posts/" + itoa(int64(i+1))}
	}
	job.SetItems(items)
	close(p.started)
	<-p.release
	return nil
}

func TestLiveRowPollSendsOnlyTheCapUntilMoreIsOpen(t *testing.T) {
	proc := holdingProc{items: 5000, started: make(chan struct{}), release: make(chan struct{})}
	srv, ts := serveProc(t, proc)
	id := srv.queue.Enqueue("https://danbooru.donmai.us/pools/1", queue.Options{})
	<-proc.started
	defer close(proc.release)

	_, body := get(t, ts, "/internal/queue-rows")
	if n := strings.Count(body, "item-row"); n != maxQueueItems {
		t.Errorf("the poll sent %d item rows, want the cap, %d", n, maxQueueItems)
	}
	if want := "+" + itoa(5000-maxQueueItems) + " more"; !strings.Contains(body, want) {
		t.Errorf("poll should count the rest as %q, got %q", want, truncate(body))
	}
	if !strings.Contains(body, "5000 items") || !strings.Contains(body, "downloading...") {
		t.Errorf("the row should still report the whole job, got %q", truncate(body))
	}

	_, opened := get(t, ts, "/internal/queue-rows?more="+itoa(id))
	if n := strings.Count(opened, "item-row"); n != 5000 {
		t.Errorf("with its +N more open the poll sent %d item rows, want all", n)
	}
}

func TestQueueItemFailedLookupLinksImage(t *testing.T) {
	srv := serverWith(t, noopProc{})
	rec := httptest.NewRecorder()
	items := []queue.Item{{Status: queue.ItemFailed, Outcome: outcome.Failed, ErrorCode: outcome.ErrCodeHashNotFound}}
	srv.render(rec, "queue_items", map[string]any{"Items": items, "MonbooruURL": "http://mb", "ImageID": int64(42), "Kind": queue.KindLookup})
	body := rec.Body.String()
	if !strings.Contains(body, `href="http://mb/images/42"`) || !strings.Contains(body, ">monbooru</a>") {
		t.Errorf("failed lookup should link to its monbooru image, got %q", body)
	}
	if !strings.Contains(body, "no match found") {
		t.Errorf("failed lookup should still show its error, got %q", body)
	}
	if !strings.Contains(body, "image #42") {
		t.Errorf("failed lookup subject should read image #42, got %q", body)
	}
}

func TestAFoldedPTRLookupRowReloadsItsItemList(t *testing.T) {
	srv, ts := serveWeb(t, "")
	matched := func(id int64) []queue.Item {
		return []queue.Item{{MonbooruID: id, Status: queue.ItemDone, Outcome: outcome.Matched}}
	}
	srv.queue.RecordPTRLookup("", false, 1, matched(1))
	_, first := get(t, ts, "/internal/queue-rows")
	srv.queue.RecordPTRLookup("", false, 1, matched(2))
	_, second := get(t, ts, "/internal/queue-rows")

	if !strings.Contains(first, `-1" hx-preserve="true"`) || !strings.Contains(second, `-2" hx-preserve="true"`) {
		t.Errorf("the preserved list should key on the item count:\n%s\n%s", truncate(first), truncate(second))
	}
}

func TestMoreSummary(t *testing.T) {
	items := []queue.Item{
		{Status: queue.ItemPending},
		{Outcome: outcome.Created},
		{Outcome: outcome.Created},
		{Outcome: outcome.Duplicate},
		{Outcome: outcome.Enriched},
		{Outcome: outcome.Matched},
		{Outcome: outcome.Replaced},
		{Outcome: outcome.SkippedBlacklist},
		{Outcome: outcome.Failed},
		{Outcome: outcome.Failed, ErrorCode: outcome.ErrCodeCanceled},
	}
	want := "1 downloading, 2 created, 1 duplicate, 1 enriched, 1 matched, 1 replaced, 1 blacklisted, 1 failed, 1 canceled"
	if got := moreSummary(items); got != want {
		t.Errorf("moreSummary = %q, want %q", got, want)
	}
	if got := moreSummary(nil); got != "" {
		t.Errorf("moreSummary(nil) = %q, want empty", got)
	}
}

func TestContinueLabelFollowsLoweredCap(t *testing.T) {
	srv, ts := serveProc(t, cappedProc{})

	finishJob(t, srv, "http://danbooru/posts?tags=x")

	storeCfg(srv, func(c *config.Config) { c.Downloader.MaxItemsPerJob = 3 })
	_, body := get(t, ts, "/internal/queue-rows")
	if !strings.Contains(body, "Get next 3") || strings.Contains(body, "Get next 8") {
		t.Errorf("row should offer the lowered window, got %q", body)
	}
}

func TestCancelPendingJobsSettlesTheFIFO(t *testing.T) {
	srv, ts := serveProc(t, skipProc{})
	done := finishJob(t, srv, "http://danbooru/posts/1")
	srv.queue.Pause()
	waiting := srv.queue.Enqueue("http://danbooru/posts/2", queue.Options{})

	resp := postForm(t, ts, srv, "/queue/cancel-pending", url.Values{})
	body := readBody(t, resp)
	if !strings.Contains(body, "posts/2") {
		t.Errorf("the canceled row should keep its url in the swap, got %q", body)
	}
	snap, err := srv.queue.Get(waiting)
	if err != nil || snap.Status != queue.JobCanceled {
		t.Errorf("a pending job = %v (err %v), want a canceled row", snap.Status, err)
	}
	if _, err := srv.queue.Get(done); err != nil {
		t.Errorf("the finished job's history should survive: %v", err)
	}
}

func TestQueuePollLeavesFinishedItemsAlone(t *testing.T) {
	srv, ts := serveProc(t, skipProc{})
	id := finishJob(t, srv, "http://danbooru/posts?tags=x")

	_, body := get(t, ts, "/internal/queue-rows")
	if !strings.Contains(body, "2 items") {
		t.Errorf("row should size itself from the summary, got %q", body)
	}
	if !strings.Contains(body, "force=1") {
		t.Errorf("row should offer force download from the summary, got %q", body)
	}
	if strings.Contains(body, "item-row") {
		t.Errorf("a finished row should render no items until it is expanded, got %q", body)
	}
	_, items := get(t, ts, "/internal/queue-rows/"+itoa(id)+"/items")
	if n := strings.Count(items, "item-row"); n != 2 {
		t.Errorf("expanding should load %d item rows, got %d", 2, n)
	}
}
