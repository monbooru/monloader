package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/monbooru"
	"github.com/leqwin/monloader/internal/ptr"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/similarity"
	"github.com/leqwin/monloader/internal/sitestate"
	"golang.org/x/crypto/bcrypt"
)

// fakeRunner is the web tests' gdl.Runner. A zero probe reports ok; set probe
// to drive testSite down its auth/blocked/failed branches.
type fakeRunner struct{ probe gdl.ProbeResult }

func (fakeRunner) Resolve(context.Context, string, string, bool) (gdl.ResolveResult, error) {
	return gdl.ResolveResult{}, nil
}
func (fakeRunner) FetchMeta(context.Context, string) (map[string]any, error) { return nil, nil }
func (fakeRunner) Download(context.Context, string, string, string, bool, func(int, gdl.Downloaded), bool) ([]gdl.Downloaded, error) {
	return nil, nil
}
func (fakeRunner) ListExtractors(context.Context) ([]gdl.Extractor, error) { return nil, nil }
func (f fakeRunner) Probe(context.Context, string) (gdl.ProbeResult, error) {
	if f.probe.Status == "" {
		return gdl.ProbeResult{Status: gdl.ProbeOK}, nil
	}
	return f.probe, nil
}
func (fakeRunner) Version(context.Context) string { return "1.32.1" }

type noopProc struct{}

func (noopProc) Process(context.Context, *queue.Job) error { return nil }

// monbooruStub answers the two endpoints the UI calls.
func monbooruStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/galleries":
			_, _ = w.Write([]byte(`[{"name":"default","images":3,"tags":2,"active":true}]`))
		default:
			_, _ = w.Write([]byte(`{"api":"monbooru","version":"v9.9.9"}`))
		}
	}))
}

// serveWeb is the stub-monbooru-backed server most handler tests start from;
// cleanup rides t. Tests that need the stub's URL afterwards build the pieces
// themselves.
func serveWeb(t *testing.T, password string) (*Server, *httptest.Server) {
	t.Helper()
	mb := monbooruStub()
	t.Cleanup(mb.Close)
	srv := newWebServer(t, mb.URL, password)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func newWebServer(t *testing.T, monbooruURL, password string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Monbooru.APIURL = monbooruURL
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
	mapper, err := mapping.New(provider)
	if err != nil {
		t.Fatal(err)
	}
	extractors := []gdl.Extractor{{Category: "danbooru", Subcategory: "post", Example: "https://example.com/posts/1"}}
	srv, err := NewServer(provider, filepath.Join(t.TempDir(), "monloader.toml"), q, monbooru.New(provider), fakeRunner{}, mapper, extractors, "1.32.1", sitestate.New(), ptr.NewEngine(cfg.PTR), similarity.New(provider, mapper.CanonicalPostURL, mapper.PostURLFor))
	if err != nil {
		t.Fatal(err)
	}
	return srv
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

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func TestAddScreen(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	ts := httptest.NewServer(newWebServer(t, mb.URL, "").Handler())
	defer ts.Close()

	status, body := get(t, ts, "/")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{`id="add-input"`, "search-input-wrap", "monloader", "/queue", "/settings", `name="_csrf"`, ">download<"} {
		if !strings.Contains(body, want) {
			t.Errorf("add screen missing %q", want)
		}
	}
}

func TestQueueScreenHasPoll(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	ts := httptest.NewServer(newWebServer(t, mb.URL, "").Handler())
	defer ts.Close()

	status, body := get(t, ts, "/queue")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, `hx-trigger="load, every 2s [!confirmOpen()]"`) {
		t.Error("queue screen should poll every 2s, held while the confirm pop-in is open")
	}
	if !strings.Contains(body, "queue-rows") {
		t.Error("queue screen should have the rows container")
	}
}

func TestSettingsScreenSections(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	pairMB(t, srv)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := get(t, ts, "/settings")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{">monloader authentication<", ">monbooru<", ">downloads<", ">sites ", ">advanced<", ">stats<", "Goroutines", `name="_csrf"`, "danbooru"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}
	// monbooru is ordered above the (renamed) authentication section.
	if i, j := strings.Index(body, ">monbooru<"), strings.Index(body, ">monloader authentication<"); i < 0 || j < 0 || i > j {
		t.Errorf("monbooru should render above monloader authentication (monbooru=%d, auth=%d)", i, j)
	}
	// The default gallery dropdown is populated from the monbooru stub.
	if !strings.Contains(body, "default (3 img)") {
		t.Error("settings should list galleries from monbooru")
	}
}

func TestDefaultGalleryWarning(t *testing.T) {
	srv, ts := serveWeb(t, "")

	// Without a monbooru pairing the default-gallery field is hidden entirely.
	if _, body := get(t, ts, "/settings"); strings.Contains(body, `name="default_gallery"`) {
		t.Error("default gallery field should not render before pairing")
	}
	pairMB(t, srv)

	// The default config sets no gallery, so settings warns and prompts a pick.
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, "no default gallery set") {
		t.Error("an unset default gallery should warn in settings")
	}
	// A gallery the monbooru stub does not list warns it will be rejected.
	if err := srv.updateConfig(func(c *config.Config) error { c.Monbooru.DefaultGallery = "ghost"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, "is not in monbooru") {
		t.Error("an unknown default gallery should warn in settings")
	}
	// A gallery the stub lists is fine - no warning.
	if err := srv.updateConfig(func(c *config.Config) error { c.Monbooru.DefaultGallery = "default"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, ts, "/settings"); strings.Contains(body, "hint-warn") {
		t.Error("a valid default gallery should not warn")
	}
}

func TestEnqueueViaForm(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	web := newWebServer(t, mb.URL, "")
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	// Grab a valid CSRF token from the rendered add screen.
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
	// A good submit redirects the operator to the queue screen.
	if loc := resp.Header.Get("HX-Redirect"); loc != "/queue" {
		t.Errorf("enqueue should HX-Redirect to /queue, got %q", loc)
	}
	if _, total := web.queue.List(queue.ListOptions{}); total != 1 {
		t.Errorf("queue should have 1 job, got %d", total)
	}
}

func TestPauseToggle(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	web := newWebServer(t, mb.URL, "")
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	// The topbar shows the running control by default.
	_, body := get(t, ts, "/queue")
	if !strings.Contains(body, `id="pause-toggle"`) || !strings.Contains(body, `aria-label="pause downloads"`) {
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
	if !strings.Contains(frag, `aria-label="resume downloads"`) || !strings.Contains(frag, "/queue/resume") {
		t.Errorf("pause fragment should show the paused/resume control, got %q", frag)
	}

	// The poll fragment reflects the live state.
	if _, poll := get(t, ts, "/internal/queue-pause"); !strings.Contains(poll, `aria-label="resume downloads"`) {
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

// Adding from the queue screen refreshes the rows in place instead of
// redirecting, so a full-page reload does not drop the operator's collapse
// state.
func TestEnqueueFromQueueRefreshesInPlace(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	web := newWebServer(t, mb.URL, "")
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	_, body := get(t, ts, "/queue")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token in the add form")
	}
	// The form clears itself after an in-place add (but not on a validation
	// error); the handler must render verbatim, quotes unescaped.
	if !strings.Contains(body, "getResponseHeader('HX-Retarget')) this.reset()") {
		t.Error("the queue add form should reset after an in-place add")
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
	mb := monbooruStub()
	defer mb.Close()
	ts := httptest.NewServer(newWebServer(t, mb.URL, "").Handler())
	defer ts.Close()
	resp, err := http.PostForm(ts.URL+"/", url.Values{"url": {"http://x"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a tokenless POST should be 403, got %d", resp.StatusCode)
	}
}

func TestPasswordRedirectsToLogin(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	ts := httptest.NewServer(newWebServer(t, mb.URL, "secret").Handler())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/queue")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("redirect to %q, want /login", loc)
	}
}

// pairMB completes a monbooru pairing - the issued token plus the push token -
// so the pairing-gated UI renders (the footer connectivity light, the settings
// default-gallery field) and the connectivity probe has a token to send.
func pairMB(t *testing.T, srv *Server) *Server {
	t.Helper()
	tok, _ := config.GenerateToken("monbooru (paired)", config.AllScopes)
	tok.Paired = "monbooru"
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		c.Monbooru.APIToken = "paired-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestConnLight(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	up := httptest.NewServer(pairMB(t, newWebServer(t, mb.URL, "")).Handler())
	defer up.Close()
	if _, body := get(t, up, "/internal/monbooru-status"); !strings.Contains(body, "dot-ok") {
		t.Errorf("reachable monbooru should render a green dot, got %q", body)
	} else if !strings.Contains(body, "connected to monbooru v9.9.9") {
		t.Errorf("reachable monbooru should show the reported version, got %q", body)
	}

	// Point a fresh server at a closed monbooru to get the red dot.
	dead := monbooruStub()
	deadURL := dead.URL
	dead.Close()
	down := httptest.NewServer(pairMB(t, newWebServer(t, deadURL, "")).Handler())
	defer down.Close()
	if _, body := get(t, down, "/internal/monbooru-status"); !strings.Contains(body, "dot-down") {
		t.Errorf("unreachable monbooru should render a red dot, got %q", body)
	}

	// A reachable monbooru that refuses the token shows the amber "rejected"
	// dot, distinct from the red "unreachable".
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejecting.Close()
	rej := httptest.NewServer(pairMB(t, newWebServer(t, rejecting.URL, "")).Handler())
	defer rej.Close()
	if _, body := get(t, rej, "/internal/monbooru-status"); !strings.Contains(body, "dot-rejected") {
		t.Errorf("a rejected token should render the amber rejected dot, got %q", body)
	}
}

// A full page seeds its footer light from the last cached probe, so the light
// shows its known state at once instead of flickering to "checking" (and
// re-probing) on every navigation.
func TestConnLightSeedsCachedStatus(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	ts := httptest.NewServer(pairMB(t, newWebServer(t, mb.URL, "")).Handler())
	defer ts.Close()

	// Cold cache: the initial page seeds the "checking" shell.
	if _, body := get(t, ts, "/queue"); !strings.Contains(body, "checking monbooru") {
		t.Fatal("cold cache should seed the checking shell")
	}

	// The poll warms the cache; the next page seeds the connected state.
	get(t, ts, "/internal/monbooru-status")
	if _, body := get(t, ts, "/queue"); !strings.Contains(body, "connected to monbooru") {
		t.Errorf("warm cache should seed the connected light, got %q", body)
	}
}

// TestConnLightUnconfigured checks the synchronous unreachable path: a blank
// API URL reports down without a probe and exposes the machine-readable state
// the add-bar JS reads. Unpaired, the light shows no visual - only data-conn.
func TestConnLightUnconfigured(t *testing.T) {
	srv := newWebServer(t, "", "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, body := get(t, ts, "/internal/monbooru-status")
	if !strings.Contains(body, `data-conn="down"`) {
		t.Errorf("conn light should expose data-conn for the add-bar JS, got %q", body)
	}
	if strings.Contains(body, "dot-") {
		t.Errorf("unpaired light should render no visual dot, got %q", body)
	}
}

// TestConnLightUnpaired checks a configured-but-unpaired instance: with an API
// URL set but no token, the probe is skipped and the state reads "unpaired", so
// a first-run instance never claims its token was rejected.
func TestConnLightUnpaired(t *testing.T) {
	_, ts := serveWeb(t, "")
	_, body := get(t, ts, "/internal/monbooru-status")
	if !strings.Contains(body, `data-conn="unpaired"`) {
		t.Errorf("configured-but-unpaired light should report unpaired, got %q", body)
	}
	if strings.Contains(body, "dot-") {
		t.Errorf("unpaired light should render no visual dot, got %q", body)
	}
}

// TestMonbooruBannerAndSubmitGate covers the unreachable guardrail on the add
// and queue screens: with no monbooru configured the banner shows and the add
// bar is disabled; once a monbooru is configured the banner is hidden and the
// add bar is enabled.
func TestMonbooruBannerAndSubmitGate(t *testing.T) {
	// Unconfigured: banner visible, add bar disabled.
	down := httptest.NewServer(newWebServer(t, "", "").Handler())
	defer down.Close()
	for _, path := range []string{"/", "/queue"} {
		_, body := get(t, down, path)
		if !strings.Contains(body, `id="monbooru-banner"`) {
			t.Errorf("%s should render the monbooru banner, got %q", path, body)
		}
		if !strings.Contains(body, `id="monbooru-banner-msg"`) {
			t.Errorf("%s banner should carry the message span the add-bar JS sets, got %q", path, body)
		}
		if strings.Contains(body, `class="monbooru-banner" hidden`) {
			t.Errorf("%s banner should be visible when unconfigured", path)
		}
		if !strings.Contains(body, `class="needs-monbooru" disabled`) {
			t.Errorf("%s should disable the add bar when unconfigured, got %q", path, body)
		}
	}

	// Configured + reachable: banner hidden, add bar enabled.
	mb := monbooruStub()
	defer mb.Close()
	up := httptest.NewServer(newWebServer(t, mb.URL, "").Handler())
	defer up.Close()
	for _, path := range []string{"/", "/queue"} {
		_, body := get(t, up, path)
		if !strings.Contains(body, `class="monbooru-banner" hidden`) {
			t.Errorf("%s banner should be hidden when configured, got %q", path, body)
		}
		if strings.Contains(body, `class="needs-monbooru" disabled`) {
			t.Errorf("%s add bar should be enabled when configured", path)
		}
	}
}

func TestConfigSaveIsRaceFree(t *testing.T) {
	// A settings save publishes a new snapshot instead of mutating in place, so
	// it must not race readers on other goroutines (the worker mapping a push,
	// the connectivity poll). Overlap saves with reads of the same fields;
	// -race fails the build if the swap is not atomic.
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")

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
		err := srv.updateConfig(func(c *config.Config) error {
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
