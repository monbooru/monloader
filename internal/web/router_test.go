package web

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/internal/sitestate"
	webFS "github.com/monbooru/monloader/web"
	"golang.org/x/crypto/bcrypt"
)

type fakeRunner struct{ probe gdl.ProbeResult }

func (fakeRunner) Resolve(context.Context, string, string, bool) (gdl.ResolveResult, error) {
	return gdl.ResolveResult{}, nil
}
func (fakeRunner) FetchMeta(context.Context, string) (map[string]any, error) { return nil, nil }
func (fakeRunner) FirstPost(context.Context, string) (map[string]any, error) { return nil, nil }
func (fakeRunner) Download(context.Context, string, string, string, gdl.Archive, func(gdl.Downloaded), bool) ([]gdl.Downloaded, error) {
	return nil, nil
}
func (fakeRunner) ListExtractors(context.Context) ([]gdl.Extractor, error) { return nil, nil }
func (f fakeRunner) Probe(context.Context, string) (gdl.ProbeResult, error) {
	if f.probe.Status == "" {
		return gdl.ProbeResult{Status: gdl.ProbeOK}, nil
	}
	return f.probe, nil
}
func (fakeRunner) DebugLog(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (fakeRunner) Version(context.Context) string { return "1.32.1" }

func (fakeRunner) SetSiteLists(gdl.SiteLists) {}

func (fakeRunner) ExtractorInfo(context.Context, string) (string, string) { return "", "" }

type noopProc struct{}

func (noopProc) Process(context.Context, *queue.Job) error { return nil }

func monbooruStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/galleries":
			_, _ = w.Write([]byte(`[{"name":"default","images":3,"tags":2,"active":true}]`))
		case "/api/v1/categories":
			_, _ = w.Write([]byte(`[{"name":"general","is_builtin":true},{"name":"va"}]`))
		default:
			_, _ = w.Write([]byte(`{"api":"monbooru","version":"v9.9.9"}`))
		}
	}))
}

func stubbedServer(t *testing.T, password string) *Server {
	t.Helper()
	mb := monbooruStub()
	t.Cleanup(mb.Close)
	return newWebServer(t, mb.URL, password)
}

func serveEngine(t *testing.T, eng ptrEngine) (*Server, *httptest.Server) {
	t.Helper()
	srv := stubbedServer(t, "")
	srv.ptr = eng
	return srv, newTestServer(t, srv)
}

func serveWeb(t *testing.T, password string) (*Server, *httptest.Server) {
	t.Helper()
	srv := stubbedServer(t, password)
	return srv, newTestServer(t, srv)
}

func scratchConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(dir, "archive.sqlite")
	cfg.GalleryDL.CookiesDir = filepath.Join(dir, "cookies")
	return cfg
}

func newWebServer(t *testing.T, monbooruURL, password string) *Server {
	t.Helper()
	cfg := scratchConfig(t)
	cfg.Monbooru.APIURL = monbooruURL
	cfg.Monbooru.APIToken = "tok"
	if password != "" {
		cfg.Auth.EnablePassword = true
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Auth.PasswordHash = string(hash)
	}
	provider := config.NewProvider(cfg)
	q := queue.New(noopProc{}, 1, 100)
	q.Start()
	t.Cleanup(q.Close)
	mapper, err := mapping.New(provider, filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	extractors := []gdl.Extractor{{Category: "danbooru", Subcategory: "post", Example: "https://example.com/posts/1"}}
	configPath := filepath.Join(t.TempDir(), "monloader.toml")
	provider.Persist(configPath, "")
	srv, err := NewServer(provider, configPath, t.TempDir(), t.TempDir(), q, monbooru.New(provider), fakeRunner{}, mapper, gdl.NewCatalog("1.32.1", "1.32.1", false, extractors, map[string]gdl.SupportedSite{"twitter": {Category: "twitter", Name: "Twitter", URL: "https://x.com/", Auth: "Cookies"}}), sitestate.New(), ptr.NewEngine(cfg.PTR), similarity.New(provider, mapper.CanonicalPostURL, mapper.PostURLFor), Desktop{})
	if err != nil {
		t.Fatal(err)
	}

	srv.desktopApp = "monloader-test"
	t.Cleanup(func() { _ = srv.DesktopHook().SetAutostart(false) })
	return srv
}

func record(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestStaticAssetsCarryTheirOwnContentType(t *testing.T) {
	h := newWebServer(t, "", "").Handler()
	for path, want := range map[string]string{
		"/static/main.css":           "text/css",
		"/static/main.js":            "text/javascript",
		"/static/favicon.png":        "image/png",
		"/static/shared/htmx.min.js": "text/javascript",
	} {
		w := record(h.ServeHTTP, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Errorf("GET %s served Content-Type %q, want %s", path, ct, want)
		}
	}
}

func TestPagesLinkTheStaticFilesThePageCacheKeeps(t *testing.T) {
	_, ts := serveProc(t, noopProc{})
	_, queue := get(t, ts, "/queue")
	for _, path := range []string{"/static/shared/base.css", "/static/main.css", "/static/shared/htmx.min.js", "/static/shared/base.js", "/static/main.js"} {
		if !strings.Contains(queue, `"`+path+"?v="+assetVersion+`"`) {
			t.Errorf("the queue page links %s without the version", path)
		}
	}
	h := newWebServer(t, "", "").Handler()
	for path, want := range map[string]string{
		"/static/main.css?v=" + assetVersion:        "max-age=31536000, immutable",
		"/static/shared/base.css?v=" + assetVersion: "max-age=31536000, immutable",
		"/static/light/logo.png?v=" + assetVersion:  "max-age=31536000, immutable",
		"/static/shared/jetbrains-mono.woff2":       "no-cache",
	} {
		w := record(h.ServeHTTP, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != want {
			t.Errorf("GET %s: %d %q, want 200 %q", path, w.Code, w.Header().Get("Cache-Control"), want)
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1/static/shared/jetbrains-mono.woff2", nil)
	r.Header.Set("If-None-Match", `"`+assetVersion+`"`)
	if w := record(h.ServeHTTP, r); w.Code != 304 {
		t.Errorf("revalidation: %d, want 304", w.Code)
	}
}

var csrfRe = regexp.MustCompile(`(?:name="_csrf" value|name="csrf-token" content)="([^"]+)"`)

func newTestServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func waitJob(t *testing.T, srv *Server, id int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := srv.queue.Wait(ctx, id); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestAddScreen(t *testing.T) {
	_, ts := serveWeb(t, "")

	status, body := get(t, ts, "/")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{`id="add-input"`, "search-input-wrap", "monloader", "/queue", `href="/watches">[watched]</a>`, "/settings", `<meta name="csrf-token"`, ">Download<"} {
		if !strings.Contains(body, want) {
			t.Errorf("add screen missing %q", want)
		}
	}
}

func TestQueueScreenHasPoll(t *testing.T) {
	_, ts := serveWeb(t, "")

	status, body := get(t, ts, "/queue")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, `hx-trigger="load, every 2s [!pollHeld('#queue-rows')], more-opened"`) {
		t.Error("queue screen should poll every 2s, held while the confirm pop-in is open or a row holds the selection or focus")
	}
	if !strings.Contains(body, "queue-rows") {
		t.Error("queue screen should have the rows container")
	}
}

func TestSettingsScreenSections(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)

	status, body := get(t, ts, "/settings")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{">Authentication<", ">Monbooru<", ">Downloads<", ">Sites<", ">Advanced<", ">Stats<", "Goroutines", `name="_csrf"`, "danbooru"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}
	if i, j := strings.Index(body, `id="sites"`), strings.Index(body, `id="ptr"`); i < 0 || j < 0 || i > j {
		t.Errorf("ptr should render below sites (sites=%d, ptr=%d)", i, j)
	}
	if i, j := strings.Index(body, ">Monbooru<"), strings.Index(body, ">Authentication<"); i < 0 || j < 0 || i > j {
		t.Errorf("monbooru should render above monloader authentication (monbooru=%d, auth=%d)", i, j)
	}
	if !strings.Contains(body, "default (3 images)") {
		t.Error("settings should list galleries from monbooru")
	}
}

func TestDefaultGalleryWarning(t *testing.T) {
	srv, ts := serveWeb(t, "")

	if _, body := get(t, ts, "/settings"); strings.Contains(body, `name="default_gallery"`) {
		t.Error("default gallery field should not render before pairing")
	}
	pairMB(t, srv)

	if _, body := get(t, ts, "/settings"); strings.Contains(body, `class="hint-warn"`) {
		t.Error("an unset default gallery should not warn in settings")
	}
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.DefaultGallery = "ghost" })
	_, body := get(t, ts, "/settings")
	if !strings.Contains(body, "is not in monbooru") {
		t.Error("an unknown default gallery should warn in settings")
	}
	if !strings.Contains(body, `<option value="ghost" selected>ghost (not in monbooru)</option>`) {
		t.Error("a stale default gallery should round-trip through the dropdown")
	}
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.DefaultGallery = "default" })
	if _, body := get(t, ts, "/settings"); strings.Contains(body, "hint-warn") {
		t.Error("a valid default gallery should not warn")
	}
}

func TestSiteDialogGroupsCategories(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	_, body := get(t, ts, "/settings/sites/newgrounds/dialog")
	if !strings.Contains(body, `<optgroup label="built-in">`) || !strings.Contains(body, `<optgroup label="custom"><option value="va">va</option></optgroup>`) {
		t.Errorf("the category map should group built-in and custom categories, got %q", truncate(body))
	}
	if strings.Contains(body, "monbooru is unreachable") {
		t.Error("a monbooru that answered should not read as unreachable")
	}

	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"cat_from": {"voice", "gone"}, "cat_to": {"va", "ghost"},
	}).Body.Close()
	if got := mustProfile(t, srv, "newgrounds").CategoryOverrides; got["voice"] != "va" || got["gone"] != "ghost" {
		t.Fatalf("category overrides = %v, want custom targets stored", got)
	}
	_, body = get(t, ts, "/settings/sites/newgrounds/dialog")
	if !strings.Contains(body, `<option value="va" selected>va</option>`) {
		t.Error("a custom target should show selected")
	}
	if !strings.Contains(body, `<option value="ghost" selected>ghost (not in monbooru)</option>`) {
		t.Error("a target monbooru does not list should stay selected and marked")
	}
}

func TestCategoryNamedDropIsNotTheDropOption(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"cat_from": {"kept", "gone"}, "cat_to": {"drop", ""},
	}).Body.Close()
	got := mustProfile(t, srv, "newgrounds").CategoryOverrides
	if to, ok := got["kept"]; !ok || to != "drop" {
		t.Errorf("kept = %q, want the custom category named drop", to)
	}
	if to, ok := got["gone"]; !ok || to != "" {
		t.Errorf("gone = %q, want the empty target that drops the tags", to)
	}
	_, body := get(t, ts, "/settings/sites/newgrounds/dialog")
	if !strings.Contains(body, `<option value="drop" selected>drop (not in monbooru)</option>`) {
		t.Errorf("the category named drop should show selected, got %q", truncate(body))
	}
	if !strings.Contains(body, `<option value="" selected>(drop these tags)</option>`) {
		t.Errorf("the drop option should show selected on its own row, got %q", truncate(body))
	}
}

func TestSiteDialogWithMonbooruDownKeepsACustomTarget(t *testing.T) {
	srv := newWebServer(t, "http://127.0.0.1:1", "")
	ts := newTestServer(t, srv)
	_, body := get(t, ts, "/settings/sites/newgrounds/dialog")
	if !strings.Contains(body, "monbooru is unreachable, so only the built-in categories are listed") {
		t.Errorf("a dead monbooru should say why the list is short, got %q", truncate(body))
	}
	if strings.Contains(body, `<optgroup label="custom">`) {
		t.Error("no custom group should render with nothing to list")
	}
	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"cat_from": {"voice"}, "cat_to": {"va"},
	}).Body.Close()
	_, body = get(t, ts, "/settings/sites/newgrounds/dialog")
	if !strings.Contains(body, `<option value="va" selected>va</option>`) {
		t.Error("a stored custom target should stay selected, unmarked, while monbooru cannot say")
	}
}

func mustProfile(t *testing.T, srv *Server, name string) mapping.Profile {
	t.Helper()
	p, ok := srv.mapper.Lookup(name)
	if !ok {
		t.Fatalf("no profile for %s", name)
	}
	return p
}

func TestSiteDialogKeepsAGalleryMonbooruDoesNotList(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	setGallery := func(name string) {
		t.Helper()
		mustUpdate(t, srv, func(c *config.Config) {
			if site := c.FindSite("danbooru"); site != nil {
				site.Gallery = name
			} else {
				c.Sites = append(c.Sites, config.Site{Name: "danbooru", Gallery: name})
			}
		})
	}

	setGallery("ghost")
	if _, body := get(t, ts, "/settings/sites/danbooru/dialog"); !strings.Contains(body, `<option value="ghost" selected>ghost (not in monbooru)</option>`) {
		t.Error("a stale site gallery should round-trip through the dropdown")
	}
	body := refusedSave(t, postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"gallery": {"default"}, "cookies_paste": {"not a cookies file"},
	}))
	if strings.Contains(body, "not in monbooru") {
		t.Error("a refused save marked the target it was sent as missing")
	}
	setGallery("default")
	if _, body := get(t, ts, "/settings/sites/danbooru/dialog"); strings.Contains(body, "not in monbooru") {
		t.Error("a gallery monbooru lists should not be marked missing")
	}
}

func TestQueuePaginates(t *testing.T) {
	web, ts := serveWeb(t, "")

	web.queue.Pause()

	for i := 0; i < 25; i++ {
		web.queue.Enqueue("https://example.com/posts/"+strconv.Itoa(i), queue.Options{})
	}

	_, body := get(t, ts, "/internal/queue-rows")
	if !strings.Contains(body, "Page 1 / 2") {
		t.Errorf("page 1 missing pager: %q", body)
	}
	if n := strings.Count(body, "job-row"); n != 20 {
		t.Errorf("page 1 should show 20 rows, got %d", n)
	}
	_, body = get(t, ts, "/internal/queue-rows?page=2")
	if !strings.Contains(body, "Page 2 / 2") {
		t.Errorf("page 2 missing pager: %q", body)
	}
	if n := strings.Count(body, "job-row"); n != 5 {
		t.Errorf("page 2 should show 5 rows, got %d", n)
	}
	body = readBody(t, postForm(t, ts, web, "/queue/clear-succeeded", url.Values{"page": {"2"}}))
	if !strings.Contains(body, "Page 2 / 2") {
		t.Errorf("a bulk action answered off page 2: %q", body)
	}
}

func TestEnqueueViaForm(t *testing.T) {
	web, ts := serveWeb(t, "")

	_, body := get(t, ts, "/")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token in the add form")
	}
	token := m[1]

	resp, err := http.PostForm(ts.URL+"/", url.Values{
		"_csrf": {token},
		"url":   {"https://example.com/posts/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("enqueue status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("HX-Redirect"); loc != "/queue" {
		t.Errorf("enqueue should HX-Redirect to /queue, got %q", loc)
	}
	if _, total := web.queue.List(queue.ListOptions{}); total != 1 {
		t.Errorf("queue should have 1 job, got %d", total)
	}
}

func TestPauseToggle(t *testing.T) {
	web, ts := serveWeb(t, "")

	_, body := get(t, ts, "/queue")
	if !strings.Contains(body, `id="pause-toggle"`) || !strings.Contains(body, `aria-label="Pause downloads"`) {
		t.Fatalf("topbar should show the running pause control, got %q", body)
	}
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token on the queue screen")
	}
	token := m[1]

	resp, err := http.PostForm(ts.URL+"/queue/pause", url.Values{"_csrf": {token}})
	if err != nil {
		t.Fatal(err)
	}
	frag := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("pause status = %d", resp.StatusCode)
	}
	if !web.queue.Paused() {
		t.Error("queue should be paused after POST /queue/pause")
	}
	if !strings.Contains(frag, `aria-label="Resume downloads"`) || !strings.Contains(frag, "/queue/resume") {
		t.Errorf("pause fragment should show the paused/resume control, got %q", frag)
	}

	if _, poll := get(t, ts, "/internal/queue-pause"); !strings.Contains(poll, `aria-label="Resume downloads"`) {
		t.Errorf("poll fragment should show paused, got %q", poll)
	}

	resp, err = http.PostForm(ts.URL+"/queue/resume", url.Values{"_csrf": {token}})
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, resp)
	if web.queue.Paused() {
		t.Error("queue should not be paused after POST /queue/resume")
	}
}

func TestEnqueueFromQueueRefreshesInPlace(t *testing.T) {
	_, ts := serveWeb(t, "")

	_, body := get(t, ts, "/queue")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token in the add form")
	}

	if !strings.Contains(body, "getResponseHeader('HX-Retarget')) { this.reset(); document.getElementById('add-flash').textContent = ''; }") {
		t.Error("the queue add form should reset and clear its flash after an in-place add")
	}

	form := url.Values{"_csrf": {m[1]}, "url": {"https://example.com/posts/1"}}
	req, _ := http.NewRequest("POST", ts.URL+"/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Current-URL", ts.URL+"/queue")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("HX-Redirect"); loc != "" {
		t.Errorf("queue-screen add should not redirect, got HX-Redirect %q", loc)
	}
	if tgt := resp.Header.Get("HX-Retarget"); tgt != "#queue-rows" {
		t.Errorf("queue-screen add should retarget #queue-rows, got %q", tgt)
	}
	if b := readBody(t, resp); !strings.Contains(b, "example.com/posts/1") {
		t.Errorf("in-place refresh should render the new job row, got %q", b)
	}
}

func TestCSRFRejectsMissingToken(t *testing.T) {
	srv, ts := serveWeb(t, "")

	for _, tc := range []struct{ name, token string }{
		{"missing", ""},
		{"garbage", "not-a-token"},
		{"another session", srv.sessions.CSRFToken("some-other-session")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"url": {"http://x"}}
			if tc.token != "" {
				form.Set("_csrf", tc.token)
			}
			resp, err := http.PostForm(ts.URL+"/", form)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST should be 403, got %d", resp.StatusCode)
			}
		})
	}

	form := url.Values{"url": {"http://x"}, "_csrf": {srv.sessions.CSRFToken("anon")}}
	resp, err := http.PostForm(ts.URL+"/", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Error("this session's own token should be accepted")
	}
}

func TestCSRFAcceptsAStaleTokenOnlyFromThisOrigin(t *testing.T) {
	_, ts := serveWeb(t, "")
	for site, want := range map[string]bool{"same-origin": true, "cross-site": false} {
		form := url.Values{"url": {"http://x"}, "_csrf": {"from-before-the-restart"}}
		req, _ := http.NewRequest("POST", ts.URL+"/", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", site)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.StatusCode != http.StatusForbidden; got != want {
			t.Errorf("%s with a stale token: status %d", site, resp.StatusCode)
		}
	}
}

func TestPasswordRedirectsToLogin(t *testing.T) {
	_, ts := serveWeb(t, "secret")

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for target, want := range map[string]string{
		"/":                    "/login",
		"/queue":               "/login?next=%2Fqueue",
		"/settings?tab=x#auth": "/login?next=%2Fsettings%3Ftab%3Dx",
	} {
		resp, err := client.Get(ts.URL + target)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s: status = %d, want 303", target, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != want {
			t.Errorf("GET %s: redirect to %q, want %q", target, loc, want)
		}
	}
	for _, path := range []string{"/login", "/health", "/static/main.css", "/api/v1/queue", "/theme.css", "/theme.logo", "/theme.favicon"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusSeeOther {
			t.Errorf("%s redirected to login; it must bypass the session gate", path)
		}
	}
}

func TestAnHTMXRequestWithoutASessionStaysOnItsPage(t *testing.T) {
	_, ts := serveWeb(t, "secret")
	req, err := http.NewRequest("GET", ts.URL+"/internal/queue-rows", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("HX-Redirect") != "" {
		t.Errorf("status %d, HX-Redirect %q: want a bare 401", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
}

func TestLoginReturnsToTheRequestedPageOnly(t *testing.T) {
	srv, ts := serveWeb(t, "secret")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	_, body := get(t, ts, "/login?next=%2Fwatches")
	if !strings.Contains(body, `name="next" value="/watches"`) {
		t.Error("the login form does not carry the requested page")
	}
	for next, want := range map[string]string{
		"/watches?x=1":          "/watches?x=1",
		"":                      "/",
		"//evil.example":        "/",
		`/\evil.example`:        "/",
		"https://evil.example/": "/",
		"/\t/evil.example":      "/",
		"javascript:alert(1)":   "/",
	} {
		resp, err := client.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {"secret"}, "next": {next}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != want {
			t.Errorf("next=%q: %d to %q, want 303 to %q", next, resp.StatusCode, loc, want)
		}
	}
}

func pairMBWith(t *testing.T, srv *Server, apiToken string) string {
	t.Helper()
	tok, secret := apitoken.New("monbooru (paired)", config.AllScopes)
	tok.Paired = "monbooru"
	mustUpdate(t, srv, func(c *config.Config) {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		c.Monbooru.APIToken = apiToken
	})
	return secret
}

func storeCfg(srv *Server, fn func(*config.Config)) {
	cfg := srv.cfg.Current().Clone()
	fn(cfg)
	srv.cfg.Store(cfg)
}

func mustUpdate(t *testing.T, srv *Server, fn func(*config.Config)) {
	t.Helper()
	if err := srv.cfg.Update(func(c *config.Config) error { fn(c); return nil }); err != nil {
		t.Fatal(err)
	}
}

func addToken(t *testing.T, srv *Server, tok apitoken.Token) {
	t.Helper()
	mustUpdate(t, srv, func(c *config.Config) {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
	})
}

func pairMB(t *testing.T, srv *Server) *Server {
	t.Helper()
	pairMBWith(t, srv, "paired-token")
	return srv
}

func hangUpURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestConnLight(t *testing.T) {
	up := newTestServer(t, pairMB(t, stubbedServer(t, "")))
	if _, body := get(t, up, "/internal/monbooru-status"); !strings.Contains(body, "dot-ok") {
		t.Errorf("reachable monbooru should render a green dot, got %q", body)
	} else if !strings.Contains(body, "connected to monbooru v9.9.9") {
		t.Errorf("reachable monbooru should show the reported version, got %q", body)
	}

	down := newTestServer(t, pairMB(t, newWebServer(t, hangUpURL(t), "")))
	if _, body := get(t, down, "/internal/monbooru-status"); !strings.Contains(body, "dot-down") {
		t.Errorf("unreachable monbooru should render a red dot, got %q", body)
	}

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejecting.Close()
	rej := newTestServer(t, pairMB(t, newWebServer(t, rejecting.URL, "")))
	if _, body := get(t, rej, "/internal/monbooru-status"); !strings.Contains(body, "dot-rejected") {
		t.Errorf("a rejected token should render the amber rejected dot, got %q", body)
	}
}

func TestConnLightNamesAnAPIMismatch(t *testing.T) {
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = io.WriteString(w, `{"app":"monbooru","api":9,"min_peer_api":9}`)
			return
		}
		_, _ = io.WriteString(w, `{"version":"v9.9.9"}`)
	}))
	t.Cleanup(mb.Close)
	ts := newTestServer(t, pairMB(t, newWebServer(t, mb.URL, "")))
	_, body := get(t, ts, "/internal/monbooru-status")
	if !strings.Contains(body, ">api mismatch</span>") || !strings.Contains(body, "it speaks api 9 and drives 9 or newer") {
		t.Errorf("light should name the api mismatch, got %q", body)
	}
}

func TestConnLightPausesTheLink(t *testing.T) {
	srv := pairMB(t, stubbedServer(t, ""))
	ts := newTestServer(t, srv)

	if _, body := get(t, ts, "/internal/monbooru-status"); !strings.Contains(body, "/internal/monbooru/pause") {
		t.Errorf("a connected light should offer the kill switch, got %q", body)
	}

	frag := readBody(t, postForm(t, ts, srv, "/internal/monbooru/pause", url.Values{}))
	if !srv.cfg.Current().Monbooru.Paused {
		t.Error("pause should persist the hold")
	}
	if !strings.Contains(frag, "connection paused") || !strings.Contains(frag, "/internal/monbooru/resume") {
		t.Errorf("paused light should offer to resume, got %q", frag)
	}
	if _, body := get(t, ts, "/internal/monbooru-status"); !strings.Contains(body, `data-conn="paused"`) {
		t.Errorf("poll should keep reporting paused, got %q", body)
	}
	if _, page := get(t, ts, "/queue"); !strings.Contains(page, `data-conn="paused"`) {
		t.Errorf("page seed should render paused, got %q", page)
	}
	flash := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {"https://danbooru.donmai.us/posts/1"}}))
	if !strings.Contains(flash, "connection is paused") {
		t.Errorf("a paused link should refuse the add bar, got %q", flash)
	}
	for _, path := range []string{"/queue/1/retry", "/queue/1/retry?force=1", "/queue/1/continue", "/queue/1/continue-all"} {
		row := readBody(t, postForm(t, ts, srv, path, url.Values{}))
		if !strings.Contains(row, "connection is paused") {
			t.Errorf("a paused link should refuse %s, got %q", path, row)
		}
	}

	frag = readBody(t, postForm(t, ts, srv, "/internal/monbooru/resume", url.Values{}))
	if srv.cfg.Current().Monbooru.Paused {
		t.Error("resume should lift the hold")
	}
	if !strings.Contains(frag, "checking monbooru") || !strings.Contains(frag, "load delay:1s") {
		t.Errorf("resumed light should re-check within a second, got %q", frag)
	}
}

func TestConnLightSeedsCachedStatus(t *testing.T) {
	ts := newTestServer(t, pairMB(t, stubbedServer(t, "")))

	if _, body := get(t, ts, "/queue"); !strings.Contains(body, "checking monbooru") {
		t.Fatal("cold cache should seed the checking shell")
	}

	get(t, ts, "/internal/monbooru-status")
	if _, body := get(t, ts, "/queue"); !strings.Contains(body, "connected to monbooru") {
		t.Errorf("warm cache should seed the connected light, got %q", body)
	}
}

func TestConnLightUnconfigured(t *testing.T) {
	srv := newWebServer(t, "", "")
	ts := newTestServer(t, srv)
	_, body := get(t, ts, "/internal/monbooru-status")
	if !strings.Contains(body, `data-conn="unconfigured"`) {
		t.Errorf("conn light should expose data-conn for the add-bar JS, got %q", body)
	}
	if strings.Contains(body, "dot-") {
		t.Errorf("unpaired light should render no visual dot, got %q", body)
	}
}

func TestConnLightUnpaired(t *testing.T) {
	srv, ts := serveWeb(t, "")
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.APIToken = "" })
	_, body := get(t, ts, "/internal/monbooru-status")
	if !strings.Contains(body, `data-conn="unpaired"`) {
		t.Errorf("configured-but-unpaired light should report unpaired, got %q", body)
	}
	if strings.Contains(body, "dot-") {
		t.Errorf("unpaired light should render no visual dot, got %q", body)
	}
}

func TestMonbooruBannerAndSubmitGate(t *testing.T) {
	down := newTestServer(t, newWebServer(t, "", ""))
	for _, path := range []string{"/", "/queue"} {
		_, body := get(t, down, path)
		if !strings.Contains(body, `id="monbooru-banner"`) {
			t.Errorf("%s should render the monbooru banner, got %q", path, body)
		}
		if !strings.Contains(body, `id="monbooru-banner-msg"`) {
			t.Errorf("%s banner should carry the message span the add-bar JS sets, got %q", path, body)
		}
		if !strings.Contains(body, "No monbooru address set") {
			t.Errorf("%s banner should name an unset address, not an unreachable one, got %q", path, body)
		}
		if strings.Contains(body, `id="monbooru-banner" class="degraded-banner" hidden`) {
			t.Errorf("%s banner should be visible when unconfigured", path)
		}
		if !strings.Contains(body, `data-peer-title="" disabled`) {
			t.Errorf("%s should disable the add bar when unconfigured, got %q", path, body)
		}
	}

	_, up := serveWeb(t, "")
	for _, path := range []string{"/", "/queue"} {
		_, body := get(t, up, path)
		if !strings.Contains(body, `id="monbooru-banner" class="degraded-banner" hidden`) {
			t.Errorf("%s banner should be hidden when configured, got %q", path, body)
		}
		if strings.Contains(body, `data-peer-title="" disabled`) {
			t.Errorf("%s add bar should be enabled when configured", path)
		}
	}
}

func TestConfigSaveIsRaceFree(t *testing.T) {

	srv := stubbedServer(t, "")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = srv.cfg.Current().Monbooru.APIToken
					_ = len(srv.cfg.Current().Sites)
					_ = srv.mapper.Gallery("gelbooru")
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		err := srv.cfg.Update(func(c *config.Config) error {
			c.Monbooru.APIToken = "tok" + strconv.Itoa(i)
			c.Sites = append(c.Sites, config.Site{Name: "gelbooru", Gallery: "g"})
			return nil
		})
		if err != nil {
			t.Fatalf("updateConfig: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestEveryPollLogsAtDebug(t *testing.T) {
	polls := []string{"GET /internal/monbooru-status"}
	tagRe := regexp.MustCompile(`(?s)<[a-z][^<>]*hx-trigger="[^"]*every[^"]*"[^<>]*>`)
	reqRe := regexp.MustCompile(`hx-(get|post)="([^"?{]+)`)
	err := fs.WalkDir(webFS.FS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(webFS.FS, path)
		if err != nil {
			return err
		}
		for _, tag := range tagRe.FindAllString(string(b), -1) {
			if m := reqRe.FindStringSubmatch(tag); m != nil {
				polls = append(polls, strings.ToUpper(m[1])+" "+m[2])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(polls) < 10 {
		t.Fatalf("found only %d polls, the scan is broken: %v", len(polls), polls)
	}
	for _, p := range append(polls, "GET /health", "GET /static/main.css") {
		method, path, _ := strings.Cut(p, " ")
		if !isNoisyPath(httptest.NewRequest(method, path, nil)) {
			t.Errorf("%s logs at info", p)
		}
	}
	for _, p := range []string{"POST /queue/pause", "GET /settings", "POST /internal/open-folder"} {
		method, path, _ := strings.Cut(p, " ")
		if isNoisyPath(httptest.NewRequest(method, path, nil)) {
			t.Errorf("%s, an operator action, logs at debug", p)
		}
	}
}
