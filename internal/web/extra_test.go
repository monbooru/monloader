package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/monbooru"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/sitestate"
)

// leadJob builds a snapshot-shaped job for rendering tests; a composite
// literal cannot set the fields promoted from the job's embedded state.
func leadJob(id int64, status queue.JobStatus, url string) *queue.Job {
	j := &queue.Job{}
	j.ID = id
	j.Status = status
	j.URL = url
	return j
}

// itemsProc drives a job to a created + a failed item so the queue_rows
// partial exercises the monbooru-link and error-code branches.
type itemsProc struct{}

func (itemsProc) Process(_ context.Context, job *queue.Job) error {
	job.SetItems([]queue.Item{
		{PostID: "a", Num: 1, URL: "https://example.com/posts/100"},
		{PostID: "b", Num: 2},
	})
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = queue.OutcomeCreated
		it.MonbooruID = 5
		it.SHA256 = "abc123"
	})
	job.UpdateItem(1, func(it *queue.Item) {
		it.Status = queue.ItemFailed
		it.Outcome = queue.OutcomeFailed
		it.ErrorCode = queue.ErrCodeMonbooruRejected
	})
	return nil
}

func serverWith(t *testing.T, proc queue.Processor) *Server {
	t.Helper()
	mb := monbooruStub()
	t.Cleanup(mb.Close)
	cfg := config.Default()
	cfg.Monbooru.APIURL = mb.URL
	provider := config.NewProvider(cfg)
	q := queue.New(proc, 1, 100)
	q.Start()
	t.Cleanup(q.Close)
	mapper, err := mapping.New(provider)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(provider, filepath.Join(t.TempDir(), "monloader.toml"), q, monbooru.New(provider), fakeRunner{}, mapper, []gdl.Extractor{}, "1.32.1", sitestate.New())
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// cappedProc drives a job to a created item and flags it capped, so the
// queue_rows partial exercises the "capped at N" note.
type cappedProc struct{}

func (cappedProc) Process(_ context.Context, job *queue.Job) error {
	job.SetItems([]queue.Item{{PostID: "1"}})
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = queue.OutcomeCreated
	})
	job.SetCapped(8)
	return nil
}

// cappedOnceProc caps only the first window (offset 0), so a fetch-all chain
// advances a single window and then stops.
type cappedOnceProc struct{}

func (cappedOnceProc) Process(_ context.Context, job *queue.Job) error {
	s := job.Snapshot()
	job.SetItems([]queue.Item{{PostID: "1"}})
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = queue.OutcomeCreated
	})
	if s.Offset == 0 {
		job.SetCapped(8)
	}
	return nil
}

func TestQueueRowsShowsCap(t *testing.T) {
	srv := serverWith(t, cappedProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	id := srv.queue.Enqueue("http://danbooru/posts?tags=x", queue.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2e9)
	defer cancel()
	if _, err := srv.queue.Wait(ctx, id); err != nil {
		t.Fatalf("wait: %v", err)
	}
	_, body := get(t, ts, "/internal/queue-rows")
	for _, want := range []string{"more available", "get next 8"} {
		if !strings.Contains(body, want) {
			t.Errorf("a capped job should show %q, got %q", want, body)
		}
	}
}

// TestQueueGroupsContinuations checks a capped search and its continuation
// collapse into one row that offers continue only on the newest window.
func TestQueueGroupsContinuations(t *testing.T) {
	srv := serverWith(t, cappedProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2e9)
	defer cancel()

	id := srv.queue.Enqueue("http://danbooru/posts?tags=x", queue.Options{})
	if _, err := srv.queue.Wait(ctx, id); err != nil {
		t.Fatalf("wait: %v", err)
	}
	nid, err := srv.queue.Continue(id)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if _, err := srv.queue.Wait(ctx, nid); err != nil {
		t.Fatalf("wait continuation: %v", err)
	}

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
}

// TestContinueButtons checks a capped row offers the continue and fetch-all
// actions and that posting each queues a follow-up window.
func TestContinueButtons(t *testing.T) {
	for _, action := range []string{"continue", "continue-all"} {
		t.Run(action, func(t *testing.T) {
			srv := serverWith(t, cappedOnceProc{})
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			id := srv.queue.Enqueue("http://danbooru/posts?tags=x", queue.Options{})
			ctx, cancel := context.WithTimeout(context.Background(), 2e9)
			defer cancel()
			if _, err := srv.queue.Wait(ctx, id); err != nil {
				t.Fatalf("wait: %v", err)
			}
			if _, body := get(t, ts, "/internal/queue-rows"); !strings.Contains(body, "/queue/"+itoa(id)+"/"+action) {
				t.Errorf("a capped job row should offer a %s button", action)
			}
			// Posting it queues a follow-up job for the next window.
			before, _ := srv.queue.List(queue.ListOptions{})
			resp := postForm(t, ts, srv, "/queue/"+itoa(id)+"/"+action, map[string][]string{})
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s status = %d", action, resp.StatusCode)
			}
			if after, _ := srv.queue.List(queue.ListOptions{}); len(after) != len(before)+1 {
				t.Errorf("%s should queue one follow-up job: before=%d after=%d", action, len(before), len(after))
			}
		})
	}
}

// The clear button routes through the shared confirm pop-in (it carries the
// hx-confirm question plus the OK-label and destructive markers), and the
// dialog itself rides in the layout.
func TestClearConfirmPopin(t *testing.T) {
	srv := serverWith(t, noopProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, body := get(t, ts, "/queue")
	for _, want := range []string{
		`id="confirm-dialog"`, `id="confirm-dialog-ok"`, `id="confirm-dialog-cancel"`,
		`hx-confirm="clear recent history?"`, `data-confirm-ok="clear"`, "data-confirm-danger",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue page missing %q", want)
		}
	}
}

func TestQueueRowsRendersItems(t *testing.T) {
	srv := serverWith(t, itemsProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	id := srv.queue.Enqueue("http://danbooru/pools/1", queue.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2e9)
	defer cancel()
	if _, err := srv.queue.Wait(ctx, id); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// A finished job's row carries the lazy loader and the summary counts, not
	// the items themselves.
	_, rows := get(t, ts, "/internal/queue-rows")
	for _, want := range []string{"items-row", "data-job=", "hx-preserve", "/internal/queue-rows/" + itoa(id) + "/items", "created</span>", "total</span>"} {
		if !strings.Contains(rows, want) {
			t.Errorf("queue rows missing %q", want)
		}
	}
	if strings.Contains(rows, "/i/abc123") {
		t.Error("a finished job's items should load lazily, not inline in the poll")
	}

	// The items load from the per-group endpoint.
	_, items := get(t, ts, "/internal/queue-rows/"+itoa(id)+"/items")
	for _, want := range []string{"/i/abc123", ">view</a>", `<a href="https://example.com/posts/100"`, "o-created", "o-failed", "monbooru_rejected"} {
		if !strings.Contains(items, want) {
			t.Errorf("items fragment missing %q", want)
		}
	}
}

// Both the active and finished jobs render expanded by default; the active
// job's items render inline (live) while a finished job's load lazily.
func TestQueueRowDefaultOpenState(t *testing.T) {
	srv := serverWith(t, noopProc{})
	groups := []jobGroup{
		{Root: 1, Lead: leadJob(1, queue.JobRunning, "u1"), Items: []queue.Item{{PostID: "a"}}},
		{Root: 2, Lead: leadJob(2, queue.JobSucceeded, "u2"), Items: []queue.Item{{PostID: "b"}}},
	}
	rec := httptest.NewRecorder()
	srv.render(rec, "queue_rows", map[string]any{"Groups": groups, "MonbooruURL": "", "CSRFToken": "t"})
	body := rec.Body.String()
	if !strings.Contains(body, `data-job="1" open`) {
		t.Error("the active (running) job should render expanded")
	}
	if !strings.Contains(body, `data-job="2" open`) {
		t.Error("a finished job should also render expanded by default")
	}
	// The active job's items render inline (live); the finished job's load lazily.
	if n := strings.Count(body, "item-row"); n != 1 {
		t.Errorf("only the active job's items should be inline, got %d item rows", n)
	}
	if !strings.Contains(body, `hx-get="/internal/queue-rows/2/items"`) {
		t.Error("the finished job should carry a lazy items loader")
	}
}

// The row summary labels the batch's progress: downloading while any item is
// still pending, pushing once the files are down and going to monbooru, and
// finished once the series stops running.
func TestQueueRowPhase(t *testing.T) {
	srv := serverWith(t, noopProc{})
	groups := []jobGroup{
		{Root: 1, Lead: leadJob(1, queue.JobRunning, "u1"), Items: []queue.Item{{PostID: "a", Status: queue.ItemPending}}},
		{Root: 2, Lead: leadJob(2, queue.JobRunning, "u2"), Items: []queue.Item{{PostID: "b", Status: queue.ItemDownloaded}}},
		{Root: 3, Lead: leadJob(3, queue.JobSucceeded, "u3"), Items: []queue.Item{{PostID: "c", Status: queue.ItemDone}}},
	}
	rec := httptest.NewRecorder()
	srv.render(rec, "queue_rows", map[string]any{"Groups": groups, "MonbooruURL": "", "CSRFToken": "t"})
	body := rec.Body.String()
	for _, want := range []string{"downloading...", "pushing...", "finished"} {
		if !strings.Contains(body, want) {
			t.Errorf("queue row phase %q missing from render", want)
		}
	}
	if !strings.Contains(body, "job-phase-downloading") || !strings.Contains(body, "job-phase-finished") {
		t.Error("phase span should carry its state class")
	}
}

// Finished-row actions are gated: retry shows only when the job did not fully
// succeed (partial / failed / canceled); force download shows only when the row
// has skipped items; remove always shows.
func TestQueueActionGating(t *testing.T) {
	srv := serverWith(t, noopProc{})
	render := func(g jobGroup) string {
		rec := httptest.NewRecorder()
		srv.render(rec, "queue_rows", map[string]any{"Groups": []jobGroup{g}, "MonbooruURL": "", "CSRFToken": "t"})
		return rec.Body.String()
	}
	hasRetry := func(s string) bool { return strings.Contains(s, `/retry"`) }
	hasForce := func(s string) bool { return strings.Contains(s, "force=1") }
	hasRemove := func(s string) bool { return strings.Contains(s, `hx-delete="/queue/`) }

	// Clean success, no skips: neither retry nor force download, but remove.
	ok := render(jobGroup{Root: 1, Lead: leadJob(1, queue.JobSucceeded, "u"), Summary: queue.Summary{Created: 1, Total: 1}})
	if hasRetry(ok) || hasForce(ok) {
		t.Errorf("a clean success should offer neither retry nor force download, got %q", ok)
	}
	if !hasRemove(ok) {
		t.Error("remove should always be offered")
	}

	// Succeeded but with skipped items: force download appears, retry does not.
	skip := render(jobGroup{Root: 2, Lead: leadJob(2, queue.JobSucceeded, "u"), Summary: queue.Summary{Skipped: 2, Total: 2}})
	if !hasForce(skip) {
		t.Errorf("a job with skipped items should offer force download, got %q", skip)
	}
	if hasRetry(skip) {
		t.Errorf("a succeeded job should not offer retry, got %q", skip)
	}

	// Partial (some failed), no skips: retry appears, force download does not.
	partial := render(jobGroup{Root: 3, Lead: leadJob(3, queue.JobPartial, "u"), Summary: queue.Summary{Created: 1, Failed: 1, Total: 2}})
	if !hasRetry(partial) {
		t.Errorf("a partial job should offer retry, got %q", partial)
	}
	if hasForce(partial) {
		t.Errorf("a job with no skips should not offer force download, got %q", partial)
	}

	// Canceled counts as not-succeeded, so retry stays available.
	canceled := render(jobGroup{Root: 4, Lead: leadJob(4, queue.JobCanceled, "u"), Summary: queue.Summary{Canceled: 1, Total: 1}})
	if !hasRetry(canceled) {
		t.Errorf("a canceled job should offer retry, got %q", canceled)
	}
}

// Items past the cap render behind a "+N more" toggle so a large pool or
// search stays bounded; a job at or under the cap shows no toggle.
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

func TestMoreSummary(t *testing.T) {
	items := []queue.Item{
		{Status: queue.ItemPending},
		{Outcome: queue.OutcomeCreated},
		{Outcome: queue.OutcomeCreated},
		{Outcome: queue.OutcomeDuplicate},
		{Outcome: queue.OutcomeFailed},
		{Outcome: queue.OutcomeFailed, ErrorCode: queue.ErrCodeCanceled},
	}
	if got, want := moreSummary(items), "1 downloading, 2 created, 1 duplicate, 1 failed, 1 canceled"; got != want {
		t.Errorf("moreSummary = %q, want %q", got, want)
	}
	if got := moreSummary(nil); got != "" {
		t.Errorf("moreSummary(nil) = %q, want empty", got)
	}
}

func TestSettingsSiteGroups(t *testing.T) {
	srv := serverWith(t, noopProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	_, body := get(t, ts, "/settings")
	// Two grouped tables, an action column with per-row edit buttons, and the
	// shared edit pop-in; a booru and a manga site land in their groups.
	for _, want := range []string{
		">boorus<", "manga / comics", "<th>action</th>",
		`class="edit-site"`, `id="site-edit-dialog"`,
		"danbooru", "nhentai",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}
}

func TestLoginInfo(t *testing.T) {
	withKey := &config.Site{APIKey: "k"}
	withCookies := &config.Site{Cookies: "/c"}
	cases := []struct {
		auth      string
		site      *config.Site
		label     string
		needsCred bool
	}{
		{"api_optional", nil, "api (opt)", false},
		{"api_required", nil, "api key", true},
		{"api_required", withKey, "api key", false},
		{"cookies", nil, "cookies", true},
		{"cookies", withCookies, "cookies", false},
		{"none", nil, "none", false},
		{"", nil, "none", false},
	}
	for _, c := range cases {
		label, needs := loginInfo(c.auth, c.site)
		if label != c.label || needs != c.needsCred {
			t.Errorf("loginInfo(%q,%v) = (%q,%v), want (%q,%v)", c.auth, c.site, label, needs, c.label, c.needsCred)
		}
	}
}

func TestTestMonbooruFailure(t *testing.T) {
	dead := monbooruStub()
	deadURL := dead.URL
	dead.Close()
	srv := serverWith(t, noopProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp := postForm(t, ts, srv, "/settings/monbooru/test", map[string][]string{"api_url": {deadURL}})
	if body := readBody(t, resp); !strings.Contains(body, "flash-err") {
		t.Errorf("unreachable monbooru test should flash err, body=%q", body)
	}
}

// The operator-supplied CSS and logo overrides serve only while configured;
// unset they 404 so the layout falls back to the bundled assets.
func TestServeCustomAssets(t *testing.T) {
	cases := []struct {
		name, route, file, content string
		set                        func(s *Server, path string)
	}{
		{"css", "/custom.css", "custom.css", ":root{--accent:#abc}",
			func(s *Server, path string) { s.cfg.Current().Server.CustomCSS = path }},
		{"logo", "/custom.logo", "logo.png", "PNGBYTES",
			func(s *Server, path string) { s.cfg.Current().Server.BooruLogo = path }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serverWith(t, noopProc{})
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			if code, _ := get(t, ts, tc.route); code != 404 {
				t.Errorf("unset %s = %d, want 404", tc.route, code)
			}
			path := filepath.Join(t.TempDir(), tc.file)
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.set(srv, path)
			if code, body := get(t, ts, tc.route); code != 200 || !strings.Contains(body, tc.content) {
				t.Errorf("%s = %d %q", tc.route, code, body)
			}
		})
	}
}

// Unset branding leaves the bundled assets and the "monloader" wordmark in
// place across the landing (h1) and topbar (span) surfaces; nothing points at
// the override route.
func TestBrandingDefaults(t *testing.T) {
	srv := serverWith(t, noopProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, landing := get(t, ts, "/")
	for _, want := range []string{`href="/static/favicon.png"`, `src="/static/logo.png"`, `<h1>monloader</h1>`, `<title>monloader</title>`} {
		if !strings.Contains(landing, want) {
			t.Errorf("default landing page missing %q", want)
		}
	}
	_, queue := get(t, ts, "/queue")
	for _, want := range []string{`src="/static/logo.png"`, `<span>monloader</span>`, `<title>queue - monloader</title>`} {
		if !strings.Contains(queue, want) {
			t.Errorf("default queue page missing %q", want)
		}
	}
	if strings.Contains(landing, "/custom.logo") || strings.Contains(queue, "/custom.logo") {
		t.Error("default pages must not reference /custom.logo")
	}
}

// A configured name and logo reach the title, the wordmark, and the favicon
// link across the landing, topbar, and login surfaces; the single logo path
// backs both the favicon and the logo image.
func TestBrandingOverride(t *testing.T) {
	srv := serverWith(t, noopProc{})
	srv.cfg.Current().Server.BooruName = "Privloader"
	srv.cfg.Current().Server.BooruLogo = "/some/path/logo.png"
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, landing := get(t, ts, "/")
	for _, want := range []string{`href="/custom.logo"`, `src="/custom.logo"`, `<h1>Privloader</h1>`, `<title>Privloader</title>`} {
		if !strings.Contains(landing, want) {
			t.Errorf("branded landing page missing %q", want)
		}
	}
	if strings.Contains(landing, "/static/logo.png") || strings.Contains(landing, "/static/favicon.png") {
		t.Error("branded landing page should not link the bundled assets")
	}
	_, queue := get(t, ts, "/queue")
	for _, want := range []string{`src="/custom.logo"`, `<span>Privloader</span>`, `<title>queue - Privloader</title>`} {
		if !strings.Contains(queue, want) {
			t.Errorf("branded queue page missing %q", want)
		}
	}

	// The login page is served before auth, so it carries the same brand.
	mb := monbooruStub()
	defer mb.Close()
	authed := newWebServer(t, mb.URL, "secret")
	authed.cfg.Current().Server.BooruName = "Privloader"
	authed.cfg.Current().Server.BooruLogo = "/some/path/logo.png"
	lts := httptest.NewServer(authed.Handler())
	defer lts.Close()
	_, login := get(t, lts, "/login")
	if !strings.Contains(login, "<h1>Privloader</h1>") {
		t.Errorf("branded login heading missing, got %q", login)
	}
	if !strings.Contains(login, `href="/custom.logo"`) {
		t.Error("branded login favicon should point at /custom.logo")
	}
}

// A configured web_url turns the footer connection indicator into a link
// (trailing slash trimmed); the poll endpoint that re-renders the light carries
// the link too so it survives the swap. An empty web_url renders the word plain.
func TestMonbooruLink(t *testing.T) {
	srv := serverWith(t, noopProc{})
	pairMB(t, srv)
	srv.cfg.Current().Monbooru.WebURL = "http://booru.example.com/"
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The poll handler re-renders the light on its own; without the web base it
	// would drop the link after the first swap.
	_, light := get(t, ts, "/internal/monbooru-status")
	if !strings.Contains(light, `connected to <a href="http://booru.example.com" target="_blank" rel="noopener">monbooru</a>`) {
		t.Errorf("connection light should read 'connected to' and link only the word monbooru, got %q", light)
	}

	srv.cfg.Current().Monbooru.WebURL = ""
	_, plainLight := get(t, ts, "/internal/monbooru-status")
	if strings.Contains(plainLight, "<a href=") {
		t.Error("connection light should not be linked without web_url")
	}
	if !strings.Contains(plainLight, "connected to monbooru") {
		t.Errorf("unlinked light should still read 'connected to monbooru', got %q", plainLight)
	}
}

func TestLoginPageRedirectsWhenNoPassword(t *testing.T) {
	srv := serverWith(t, noopProc{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("login with no password set should redirect to /, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLogoutClearsSession(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "secret")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Establish a session.
	id, err := srv.sessions.New(7)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "monloader_session", Value: id}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", ts.URL+"/logout", strings.NewReader("_csrf="+srv.csrfToken(id)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("logout status = %d, want 303", resp.StatusCode)
	}
	if _, ok := srv.sessions.Get(id); ok {
		t.Error("session should be deleted after logout")
	}
}
