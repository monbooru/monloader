package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
)

type createdProc struct {
	site  string
	sub   string
	items int
}

func (p createdProc) Process(_ context.Context, j *queue.Job) error {
	j.SetSite(p.site)
	j.SetSubcategory(p.sub)
	j.SetItems(make([]queue.Item, p.items))
	for i := range p.items {
		j.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		j.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemUploaded })
		j.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDone; it.Outcome = outcome.Created })
	}
	return nil
}

func serveWatches(t *testing.T, proc queue.Processor) (*Server, *httptest.Server) {
	t.Helper()
	srv := stubbedServer(t, "")
	st, err := queue.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q := queue.New(proc, 1, 100)
	q.UseStore(st)
	q.Start()
	t.Cleanup(q.Close)
	srv.queue = q
	return srv, newTestServer(t, srv)
}

func waitWatchChecked(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ws := srv.queue.Watches(); len(ws) > 0 && ws[0].Last.JobID != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the watch was never checked")
}

func deleteReq(t *testing.T, ts *httptest.Server, srv *Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWatchesScreen(t *testing.T) {
	_, ts := serveWatches(t, noopProc{})
	status, body := get(t, ts, "/watches")
	if status != http.StatusOK {
		t.Fatalf("GET /watches = %d", status)
	}
	for _, want := range []string{`href="/watches" class="nav-watches active">Watched</a>`, `placeholder="Paste a search URL to watch"`, "No watches", `hx-get="/internal/watch-rows"`} {
		if !strings.Contains(body, want) {
			t.Errorf("watched page lacks %q", want)
		}
	}
}

func TestWatchesScreenWithoutAStore(t *testing.T) {
	_, ts := serveWeb(t, "")
	_, body := get(t, ts, "/watches")
	if !strings.Contains(body, "Watches can't be saved") {
		t.Error("the page does not say watches need the store")
	}
	if !strings.Contains(body, `autocomplete="off" disabled placeholder="Paste a search URL to watch"`) {
		t.Error("the add bar is not disabled, or carries the class that re-enables it")
	}
}

func TestAddWatchFromTheAddBar(t *testing.T) {
	srv, ts := serveWatches(t, noopProc{})
	for _, tc := range []struct{ url, hours, want string }{
		{"not a url", "24", "valid http(s) URL"},
		{"https://danbooru.donmai.us/posts?tags=a", "5", "Pick how often"},
	} {
		body := readBody(t, postForm(t, ts, srv, "/watches", url.Values{"url": {tc.url}, "interval_hours": {tc.hours}}))
		if !strings.Contains(body, tc.want) {
			t.Errorf("add %q every %s = %q, want %q", tc.url, tc.hours, truncate(body), tc.want)
		}
	}
	if len(srv.queue.Watches()) != 0 {
		t.Fatal("a refused add stored a watch")
	}

	target := "https://danbooru.donmai.us/posts?tags=a"
	resp := postForm(t, ts, srv, "/watches", url.Values{"url": {target}, "interval_hours": {"72"}})
	if got := resp.Header.Get("HX-Retarget"); got != "#watch-rows" {
		t.Errorf("HX-Retarget = %q, want #watch-rows", got)
	}
	if body := readBody(t, resp); !strings.Contains(body, target) {
		t.Errorf("rows after the add lack the URL: %q", truncate(body))
	}
	ws := srv.queue.Watches()
	if len(ws) != 1 || ws[0].IntervalHours != 72 {
		t.Fatalf("watches = %+v, want one every 72h", ws)
	}
	waitWatchChecked(t, srv)
	if body := readBody(t, postForm(t, ts, srv, "/watches", url.Values{"url": {target}, "interval_hours": {"24"}})); !strings.Contains(body, "already watched") {
		t.Errorf("a duplicate add = %q, want the already-watched refusal", truncate(body))
	}
}

func TestQueueRowWatchButton(t *testing.T) {
	srv, ts := serveWatches(t, createdProc{site: "danbooru", items: 2})
	search := finishJob(t, srv, "https://danbooru.donmai.us/posts?tags=a")
	button := `hx-post="/queue/` + itoa(search) + `/watch"`
	if _, body := get(t, ts, "/internal/queue-rows"); !strings.Contains(body, button) {
		t.Fatalf("a finished search row has no watch button")
	}

	body := readBody(t, postForm(t, ts, srv, "/queue/"+itoa(search)+"/watch", url.Values{}))
	if strings.Contains(body, button) || !strings.Contains(body, `<span class="job-lane">watched</span></td>`) || strings.Contains(body, `href="/watches"`) {
		t.Errorf("the row after watching = %q, want watched in the status cell, with no button and no link", truncate(body))
	}
	ws := srv.queue.Watches()
	if len(ws) != 1 || ws[0].IntervalHours != defaultWatchHours || ws[0].Last.JobID != search || ws[0].Last.Created != 2 {
		t.Fatalf("watches = %+v, want one daily watch carrying the row's run", ws)
	}
	if body := readBody(t, postForm(t, ts, srv, "/queue/"+itoa(search)+"/watch", url.Values{})); !strings.Contains(body, "already watched") {
		t.Errorf("watching twice = %q, want the refusal", truncate(body))
	}
	if body := readBody(t, postForm(t, ts, srv, "/queue/99999/watch", url.Values{})); !strings.Contains(body, "no longer in the queue") {
		t.Errorf("watching a gone job = %q, want the gone notice", truncate(body))
	}
}

func TestQueueRowWatchButtonOnAMangaSitesListing(t *testing.T) {
	srv, ts := serveWatches(t, createdProc{site: "exhentai", sub: "search", items: 1})
	id := finishJob(t, srv, "https://e-hentai.org/?f_search=a")
	if _, body := get(t, ts, "/internal/queue-rows"); !strings.Contains(body, `hx-post="/queue/`+itoa(id)+`/watch"`) {
		t.Fatalf("a finished manga search row has no watch button")
	}
	postForm(t, ts, srv, "/queue/"+itoa(id)+"/watch", url.Values{})
	if ws := srv.queue.Watches(); len(ws) != 1 || ws[0].Last.JobID != id {
		t.Errorf("watches = %+v, want one carrying the row's run", ws)
	}
}

func TestQueueRowWatchButtonSkipsWhatCannotBeWatched(t *testing.T) {
	for name, tc := range map[string]struct {
		proc queue.Processor
		url  string
	}{
		"single post": {createdProc{site: "danbooru", items: 1}, "https://danbooru.donmai.us/posts/1"},
		"manga":       {createdProc{site: "nhentai", sub: "gallery", items: 2}, "https://nhentai.net/g/1/"},
		"manga title": {createdProc{site: "mangadex", sub: "manga", items: 2}, "https://mangadex.org/title/x"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, ts := serveWatches(t, tc.proc)
			id := finishJob(t, srv, tc.url)
			if _, body := get(t, ts, "/internal/queue-rows"); strings.Contains(body, "/watch\"") {
				t.Errorf("a %s row offers watch", name)
			}
			if body := readBody(t, postForm(t, ts, srv, "/queue/"+itoa(id)+"/watch", url.Values{})); !strings.Contains(body, "Only a finished search") {
				t.Errorf("watching a %s = %q, want the refusal", name, truncate(body))
			}
		})
	}
}

func TestWatchRowActions(t *testing.T) {
	srv, ts := serveWatches(t, noopProc{})
	id, err := srv.queue.AddWatch("https://danbooru.donmai.us/posts?tags=a", 24, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/watches/" + strconv.FormatInt(id, 10)

	readBody(t, postForm(t, ts, srv, path+"/interval", url.Values{"interval_hours": {"168"}}))
	if got := srv.queue.Watches()[0].IntervalHours; got != 168 {
		t.Errorf("interval after the change = %d, want 168", got)
	}
	resp := postForm(t, ts, srv, path+"/interval", url.Values{"interval_hours": {"3"}})
	if body := readBody(t, resp); resp.Header.Get("HX-Retarget") != "#watch-flash" || !strings.Contains(body, "Pick how often") {
		t.Errorf("an off-list interval = %q, want the refusal in the watch flash", truncate(body))
	}

	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.Paused = true })
	if body := readBody(t, postForm(t, ts, srv, path+"/check", url.Values{})); !strings.Contains(body, "paused") {
		t.Errorf("check now while paused = %q, want the paused refusal", truncate(body))
	}
	if _, body := get(t, ts, "/internal/watch-rows"); !strings.Contains(body, ">held<") {
		t.Error("a watch held by the paused link does not read held")
	}
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.Paused = false })
	readBody(t, postForm(t, ts, srv, path+"/check", url.Values{}))
	waitWatchChecked(t, srv)

	readBody(t, deleteReq(t, ts, srv, path))
	if ws := srv.queue.Watches(); len(ws) != 0 {
		t.Errorf("watches after remove = %+v, want none", ws)
	}
	if body := readBody(t, deleteReq(t, ts, srv, path)); !strings.Contains(body, "That watch is gone") {
		t.Errorf("removing twice = %q, want the gone notice", truncate(body))
	}
}

func TestWatchRowResults(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    queue.Watch
		want string
	}{
		{"unchecked", queue.Watch{IntervalHours: 24}, "-"},
		{"live", queue.Watch{Live: true}, "checking..."},
		{"new", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Created: 3, Total: 20}}, "3 new"},
		{"nothing", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Total: 20}}, "nothing new"},
		{"empty", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded}}, "no posts matched"},
		{"error", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobFailed, ErrorCode: "rate_limited"}}, "rate limited"},
		{"crashed", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobRunning}}, "interrupted"},
		{"single post", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Created: 1, Total: 1, Subcategory: "post"}}, "a single post - nothing to watch"},
		{"search with one match", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Created: 1, Total: 1, Subcategory: "tag"}}, "1 new"},
		{"unknown extractor", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Total: 1}}, "nothing new"},
		{"past a low pick", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Total: 5, More: true}}, "nothing new - more past the limit"},
		{"partial past a low pick", queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobPartial, Total: 5, More: true}}, "partial"},
	} {
		if got := newWatchRow(tc.w, false).Result; got != tc.want {
			t.Errorf("%s: result = %q, want %q", tc.name, got, tc.want)
		}
	}
	empty := newWatchRow(queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded}}, false)
	if !strings.Contains(empty.ResultTitle, "need a login") {
		t.Errorf("an empty check's title = %q, want the left-out posts named", empty.ResultTitle)
	}
	if got := newWatchRow(queue.Watch{}, false).Next; got != "-" {
		t.Errorf("a manual watch's next = %q, want -", got)
	}
}

func TestItemsChoices(t *testing.T) {
	srv := stubbedServer(t, "")
	values := func(cs []itemsChoice) (vals []string, selected string) {
		for _, c := range cs {
			vals = append(vals, c.Value)
			if c.Selected {
				selected = c.Value
			}
		}
		return vals, selected
	}
	for _, tc := range []struct {
		limit, current int
		want           string
		selected       string
	}{
		{200, 0, "1,5,10,20,50,100,,500,1000,all", ""},
		{200, queue.ItemsAll, "1,5,10,20,50,100,,500,1000,all", "all"},
		{250, 0, "1,5,10,20,50,100,200,,500,1000,all", ""},
		{200, 300, "1,5,10,20,50,100,,300,500,1000,all", "300"},
	} {
		mustUpdate(t, srv, func(c *config.Config) { c.Downloader.MaxItemsPerJob = tc.limit })
		vals, selected := values(srv.itemsChoices(tc.current))
		if got := strings.Join(vals, ","); got != tc.want || selected != tc.selected {
			t.Errorf("cap %d stored %d = %q selected %q, want %q selected %q", tc.limit, tc.current, got, selected, tc.want, tc.selected)
		}
	}
	var labels []string
	for _, c := range srv.itemsChoices(0) {
		labels = append(labels, c.Label)
	}
	if got, want := strings.Join(labels, ","), "up to 1,up to 5,up to 10,up to 20,up to 50,up to 100,up to 200,up to 500,up to 1000,all"; got != want {
		t.Errorf("labels = %q, want %q", got, want)
	}
}

func TestAddBarTakesTheItemCount(t *testing.T) {
	srv, ts := serveWeb(t, "")
	for _, page := range []string{"/", "/queue"} {
		if _, body := get(t, ts, page); !strings.Contains(body, `<select name="items"`) {
			t.Errorf("%s has no items select", page)
		}
	}
	postForm(t, ts, srv, "/", url.Values{"url": {"https://danbooru.donmai.us/posts?tags=a"}, "items": {"10"}}).Body.Close()
	postForm(t, ts, srv, "/", url.Values{"url": {"https://danbooru.donmai.us/posts?tags=b"}, "items": {"all"}}).Body.Close()
	postForm(t, ts, srv, "/", url.Values{"url": {"https://danbooru.donmai.us/posts?tags=d"}, "items": {"500"}}).Body.Close()
	if body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {"https://danbooru.donmai.us/posts?tags=c"}, "items": {"-3"}})); !strings.Contains(body, "Pick how many posts") {
		t.Errorf("a bad item count = %q, want the refusal", truncate(body))
	}
	jobs, _ := srv.queue.List(queue.ListOptions{})
	got := map[string]*queue.Job{}
	for _, j := range jobs {
		got[j.URL] = j
	}
	if j := got["https://danbooru.donmai.us/posts?tags=a"]; j == nil || j.MaxItems != 10 || j.Auto {
		t.Errorf("items=10 queued %+v, want MaxItems 10 without the chain", j)
	}
	if j := got["https://danbooru.donmai.us/posts?tags=b"]; j == nil || !j.Auto {
		t.Errorf("items=all queued %+v, want the chain", j)
	}
	if j := got["https://danbooru.donmai.us/posts?tags=d"]; j == nil || j.Target != 500 || j.Auto {
		t.Errorf("items=500 queued %+v, want a 500-post target", j)
	}
	if got["https://danbooru.donmai.us/posts?tags=c"] != nil {
		t.Error("a refused item count queued a job")
	}
}

func TestWatchItems(t *testing.T) {
	srv, ts := serveWatches(t, noopProc{})
	readBody(t, postForm(t, ts, srv, "/watches", url.Values{"url": {"https://danbooru.donmai.us/posts?tags=a"}, "interval_hours": {"24"}, "items": {"all"}}))
	ws := srv.queue.Watches()
	if len(ws) != 1 || ws[0].MaxItems != queue.ItemsAll {
		t.Fatalf("watches = %+v, want one fetching all", ws)
	}
	path := "/watches/" + strconv.FormatInt(ws[0].ID, 10) + "/items"
	body := readBody(t, postForm(t, ts, srv, path, url.Values{"items": {"50"}}))
	if got := srv.queue.Watches()[0].MaxItems; got != 50 {
		t.Errorf("items after the change = %d, want 50", got)
	}
	if !strings.Contains(body, `<option value="50" selected>`) {
		t.Errorf("the row does not show its pick: %q", truncate(body))
	}
	if body := readBody(t, postForm(t, ts, srv, path, url.Values{"items": {"x"}})); !strings.Contains(body, "Pick how many posts") {
		t.Errorf("a bad count = %q, want the refusal", truncate(body))
	}
}

func TestRowWatchKeepsTheRowsItemCount(t *testing.T) {
	for name, tc := range map[string]struct {
		opts queue.Options
		want int
	}{
		"lowered":  {queue.Options{MaxItems: 10}, 10},
		"past cap": {queue.Options{}.WithItems(500), 500},
		"chained":  {queue.Options{Auto: true}, queue.ItemsAll},
		"default":  {queue.Options{}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			srv, ts := serveWatches(t, createdProc{site: "danbooru", items: 2})
			id := srv.queue.Enqueue("https://danbooru.donmai.us/posts?tags=a", tc.opts)
			waitJob(t, srv, id)
			readBody(t, postForm(t, ts, srv, "/queue/"+itoa(id)+"/watch", url.Values{}))
			if ws := srv.queue.Watches(); len(ws) != 1 || ws[0].MaxItems != tc.want {
				t.Errorf("watches = %+v, want items %d", ws, tc.want)
			}
		})
	}
}

func TestGroupKeepsTheWidestWindow(t *testing.T) {
	first := leadJob(1, queue.JobSucceeded, "https://x/posts?tags=a")
	first.Root, first.Cap, first.Capped = 1, 4, true
	last := leadJob(2, queue.JobSucceeded, first.URL)
	last.Root, last.Cap, last.Capped = 1, 2, true
	g := groupJobs([]*queue.Job{last, first})
	if len(g) != 1 || g[0].widest != 4 || g[0].Continue != last {
		t.Fatalf("group = %+v, want one row continuing the last window at the widest size 4", g)
	}
}

func TestWatchableJobJudgesSinglePosts(t *testing.T) {
	srv := stubbedServer(t, "")
	job := func(id, root int64, url string, status queue.JobStatus, capped bool) *queue.Job {
		j := &queue.Job{}
		j.ID, j.Root, j.URL, j.Status, j.Capped = id, root, url, status, capped
		return j
	}
	for _, tc := range []struct {
		name  string
		job   *queue.Job
		total int
		want  bool
	}{
		{"single post", job(1, 1, "https://x/posts/1", queue.JobSucceeded, false), 1, false},
		{"canceled before it resolved", job(1, 1, "https://x/posts/1", queue.JobCanceled, false), 0, false},
		{"search that matched nothing", job(1, 1, "https://x/posts?tags=a", queue.JobSucceeded, false), 0, true},
		{"search", job(1, 1, "https://x/posts?tags=a", queue.JobSucceeded, false), 5, true},
		{"one-post window of a series", job(2, 1, "https://x/posts?tags=a", queue.JobSucceeded, false), 1, true},
		{"capped one-post window", job(1, 1, "https://x/posts?tags=a", queue.JobSucceeded, true), 1, true},
	} {
		if got := srv.watchableJob(tc.job, tc.total); got != tc.want {
			t.Errorf("%s: watchable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUnpairedRefusesEverythingThatFetches(t *testing.T) {
	srv, ts := serveWatches(t, createdProc{})
	wid, err := srv.queue.AddWatch("https://danbooru.donmai.us/posts?tags=a", 24, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.APIToken = "" })
	for _, path := range []string{"/", "/watches/" + itoa(wid) + "/check"} {
		body := readBody(t, postForm(t, ts, srv, path, url.Values{"url": {"https://danbooru.donmai.us/posts/1"}}))
		if !strings.Contains(body, "Not paired with monbooru") {
			t.Errorf("POST %s while unpaired = %q, want the refusal", path, truncate(body))
		}
	}
	if jobs, _ := srv.queue.List(queue.ListOptions{OmitItems: true}); len(jobs) != 0 {
		t.Errorf("unpaired queued %d jobs", len(jobs))
	}
}

func TestAnAllBlacklistedCheckSaysSo(t *testing.T) {
	row := newWatchRow(queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Total: 5, Blacklisted: 5}}, false)
	if row.Result != "only blacklisted posts" {
		t.Errorf("result = %q", row.Result)
	}
	row = newWatchRow(queue.Watch{Last: queue.WatchCheck{JobID: 1, Status: queue.JobSucceeded, Total: 5, Blacklisted: 2, Duplicate: 3}}, false)
	if row.Result != "nothing new" {
		t.Errorf("a mixed check = %q, want nothing new", row.Result)
	}
}
