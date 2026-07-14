package web

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/similarity"
)

// readBody drains and closes a response body, returning it as a string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// postForm posts with the anon CSRF token and returns the response without
// following redirects, so a 303 + flash can be asserted.
func postForm(t *testing.T, ts *httptest.Server, srv *Server, path string, vals url.Values) *http.Response {
	t.Helper()
	vals.Set("_csrf", srv.csrfToken("anon"))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.URL+path, vals)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEnqueueEmptyURL(t *testing.T) {
	srv, ts := serveWeb(t, "")
	resp := postForm(t, ts, srv, "/", url.Values{"url": {""}})
	if body := readBody(t, resp); !strings.Contains(body, "enter a URL") {
		t.Errorf("empty url should flash an error, got %q", body)
	}
}

func TestEnqueueRejectsInvalidURL(t *testing.T) {
	srv, ts := serveWeb(t, "")
	for _, bad := range []string{"not a url", "gelbooru.com/post/1", "ftp://x/y", "https://"} {
		body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {bad}}))
		if !strings.Contains(body, "valid") {
			t.Errorf("%q should flash an invalid-url error, got %q", bad, body)
		}
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("invalid urls should not enqueue, queue has %d", total)
	}
}

// TestEnqueueBlockedWhenMonbooruUnconfigured guards the unreachable case: with
// no monbooru to push to, a submit is refused with a flash and nothing is
// queued (the add bar is also disabled client-side; this covers a stale page).
func TestEnqueueBlockedWhenMonbooruUnconfigured(t *testing.T) {
	srv := newWebServer(t, "", "") // blank API URL: no monbooru configured
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {"https://example.com/posts/1"}}))
	if !strings.Contains(body, "not configured") {
		t.Errorf("submit with no monbooru should flash a not-configured error, got %q", body)
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("no job should enqueue when monbooru is unconfigured, queue has %d", total)
	}
}

// TestSettingsNeverEchoesSecrets guards the most security-critical UI property:
// the settings page renders only whether a secret is set, never its value. A
// future template edit that printed a token/key/hash would fail this.
func TestSettingsNeverEchoesSecrets(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "loginpw") // enables the UI password
	const (
		mbToken = "MB-SECRET-TOKEN-zzz"
		siteKey = "SITE-SECRET-KEY-zzz"
		snaoKey = "SAUCENAO-SECRET-KEY-zzz"
	)
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Monbooru.APIToken = mbToken
		c.Sites = append(c.Sites, config.Site{Name: "gelbooru", APIKey: siteKey})
		c.Lookup.Saucenao.APIKey = snaoKey
		return nil
	}); err != nil {
		t.Fatalf("updateConfig: %v", err)
	}
	pwHash := srv.cfg.Current().Auth.PasswordHash

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	id, err := srv.sessions.New(7)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/settings", nil)
	req.AddCookie(&http.Cookie{Name: "monloader_session", Value: id})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200 (authenticated)", resp.StatusCode)
	}
	for name, secret := range map[string]string{
		"monbooru token":   mbToken,
		"site api key":     siteKey,
		"saucenao api key": snaoKey,
		"password hash":    pwHash,
	} {
		if strings.Contains(body, secret) {
			t.Errorf("settings page leaked the %s", name)
		}
	}
}

func TestSaveMonbooru(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/monbooru", url.Values{
		"api_url":         {"http://mb2:8080"},
		"web_url":         {"http://booru.example.com"},
		"api_token":       {"newtoken"},
		"default_gallery": {"art"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	c := srv.cfg.Current().Monbooru
	if c.APIURL != "http://mb2:8080" || c.DefaultGallery != "art" || c.WebURL != "http://booru.example.com" {
		t.Errorf("monbooru config not saved: %+v", c)
	}
	if c.APIToken != "" {
		t.Errorf("api token must not be settable from the form (pairing only), got %q", c.APIToken)
	}
}

func TestMonbooruWebBase(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	// Without web_url the image links fall back to the API URL.
	if got, want := srv.monbooruWebBase(), strings.TrimRight(mb.URL, "/"); got != want {
		t.Errorf("web base without web_url = %q, want %q", got, want)
	}
	// A configured web_url wins and is trailing-slash trimmed.
	srv.cfg.Current().Monbooru.WebURL = "http://booru.example.com/"
	if got := srv.monbooruWebBase(); got != "http://booru.example.com" {
		t.Errorf("web base with web_url = %q, want http://booru.example.com", got)
	}
}

func TestSaveDownloader(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/downloader", url.Values{
		"concurrency":            {"3"},
		"sleep_request":          {"2.5"},
		"max_items_per_job":      {"42"},
		"default_folder":         {"incoming"},
		"history_retention_days": {"3"},
	})
	resp.Body.Close()
	if srv.cfg.Current().Downloader.Concurrency != 3 || srv.cfg.Current().GalleryDL.SleepRequest != 2.5 ||
		srv.cfg.Current().Downloader.MaxItemsPerJob != 42 || srv.cfg.Current().Downloader.DefaultFolder != "incoming" ||
		srv.cfg.Current().Downloader.HistoryRetentionDays != 3 {
		t.Errorf("downloader config not saved: %+v %+v", srv.cfg.Current().Downloader, srv.cfg.Current().GalleryDL.SleepRequest)
	}

	// Zero turns the age limit off, so it must be saved rather than read as an
	// unset field the way the caps above are.
	resp = postForm(t, ts, srv, "/settings/downloader", url.Values{"history_retention_days": {"0"}})
	resp.Body.Close()
	if got := srv.cfg.Current().Downloader.HistoryRetentionDays; got != 0 {
		t.Errorf("history_retention_days = %d, want 0 to be accepted as no age limit", got)
	}
}

func TestSaveSite(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites", url.Values{
		"name":    {"gelbooru"},
		"api_key": {"K"},
		"user_id": {"U"},
		"gallery": {"art"},
	}).Body.Close()
	site := srv.cfg.Current().FindSite("gelbooru")
	if site == nil || site.APIKey != "K" || site.UserID != "U" || site.Gallery != "art" {
		t.Errorf("site not saved: %+v", site)
	}
	// Updating the same site mutates it in place rather than duplicating.
	postForm(t, ts, srv, "/settings/sites", url.Values{"name": {"gelbooru"}, "gallery": {"newart"}}).Body.Close()
	n := 0
	for _, s := range srv.cfg.Current().Sites {
		if s.Name == "gelbooru" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected 1 gelbooru site after update, got %d", n)
	}
	if srv.cfg.Current().FindSite("gelbooru").Gallery != "newart" {
		t.Errorf("site gallery not updated")
	}
}

func TestResetSite(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites", url.Values{"name": {"gelbooru"}, "api_key": {"K"}, "gallery": {"art"}}).Body.Close()
	if srv.cfg.Current().FindSite("gelbooru") == nil {
		t.Fatal("site should be configured before reset")
	}
	resp := postForm(t, ts, srv, "/settings/sites/gelbooru/reset", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset status = %d, want 303", resp.StatusCode)
	}
	if srv.cfg.Current().FindSite("gelbooru") != nil {
		t.Errorf("site should be removed after reset, got %+v", srv.cfg.Current().FindSite("gelbooru"))
	}
}

func TestSaveRawValidAndInvalid(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {`{"extractor":{"timeout":30}}`}}).Body.Close()
	if srv.cfg.Current().GalleryDL.RawConfig == "" {
		t.Error("valid raw config should be saved")
	}
	// Invalid JSON must not overwrite the saved value.
	resp := postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {"{not json"}})
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("invalid raw config should redirect with an error, loc=%q", loc)
	}
	if srv.cfg.Current().GalleryDL.RawConfig != `{"extractor":{"timeout":30}}` {
		t.Error("invalid raw config should not have overwritten the saved value")
	}
}

func TestTestMonbooruButton(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postForm(t, ts, srv, "/settings/monbooru/test", url.Values{"api_url": {mb.URL}})
	if body := readBody(t, resp); !strings.Contains(body, "flash-ok") {
		t.Errorf("reachable monbooru test should flash ok, body=%q", body)
	}
}

func TestTestSiteButton(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/sites/danbooru/test", url.Values{})
	got := readBody(t, resp)
	if !strings.Contains(got, `flash-ok">ok<`) {
		t.Errorf("site probe should report ok in the row state cell, got %q", got)
	}
	// A successful probe records the reach and shows it beside the result.
	if !strings.Contains(got, "site-last") || !strings.Contains(got, "just now") {
		t.Errorf("a successful probe should show the last-reached time, got %q", got)
	}
	if srv.siteState.LastReached("danbooru").IsZero() {
		t.Error("a successful probe should record the danbooru reach")
	}
	// The recorded time then renders on a fresh settings load.
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, "last ok") {
		t.Error("settings should show the last-reached note after a successful probe")
	}

	// nhentai is not in the cached extractor list, but its curated profile
	// carries an example, so the probe still runs (not "no example URL").
	resp2 := postForm(t, ts, srv, "/settings/sites/nhentai/test", url.Values{})
	if got := readBody(t, resp2); !strings.Contains(got, `flash-ok">ok<`) {
		t.Errorf("profile example should let the probe run, got %q", got)
	}
}

// TestTestSiteStateWords covers the non-ok probe wording: a bot-protection wall
// reads "blocked", and a site missing a required credential reads "needs ..."
// regardless of the raw gallery-dl error.
func TestTestSiteStateWords(t *testing.T) {
	srv, ts := serveWeb(t, "")

	probe := func(site string, res gdl.ProbeResult) string {
		srv.runner = fakeRunner{probe: res}
		return readBody(t, postForm(t, ts, srv, "/settings/sites/"+site+"/test", url.Values{}))
	}

	// e926 needs no credential, so a Cloudflare wall reads "blocked", not "auth
	// required" (the old 403 -> auth_required behavior).
	if got := probe("e926", gdl.ProbeResult{Status: gdl.ProbeBlocked, Detail: "Cloudflare challenge (403 Forbidden)"}); !strings.Contains(got, ">blocked<") {
		t.Errorf("cloudflare probe should read blocked, got %q", got)
	}
	// A cookies site with no cookies reads "needs cookies" even though the raw
	// error is an unclassifiable generic failure.
	if got := probe("sankaku", gdl.ProbeResult{Status: gdl.ProbeFailed, Detail: "invalid id"}); !strings.Contains(got, "needs cookies") {
		t.Errorf("cookies site without cookies should read needs cookies, got %q", got)
	}
	// An api-key site with no key reads "needs api key".
	if got := probe("gelbooru", gdl.ProbeResult{Status: gdl.ProbeAuthRequired, Detail: "missing authentication"}); !strings.Contains(got, "needs api key") {
		t.Errorf("api-key site without key should read needs api key, got %q", got)
	}
	// With a key set but the booru still refusing it, the diagnosis flips to
	// "auth rejected" (the key is wrong), not "needs api key" (missing).
	postForm(t, ts, srv, "/settings/sites", url.Values{"name": {"gelbooru"}, "api_key": {"WRONGKEY"}}).Body.Close()
	if got := probe("gelbooru", gdl.ProbeResult{Status: gdl.ProbeAuthRequired, Detail: "401 Unauthorized"}); !strings.Contains(got, "auth rejected") {
		t.Errorf("api-key site with a rejected key should read auth rejected, got %q", got)
	}
}

func TestQueueActions(t *testing.T) {
	srv, ts := serveWeb(t, "")

	id := srv.queue.Enqueue("http://x", queue.Options{})
	// Retry and cancel both render the rows partial (200).
	resp := postForm(t, ts, srv, "/queue/"+itoa(id)+"/retry", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("retry status = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("DELETE", ts.URL+"/queue/"+itoa(id), nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("cancel status = %d", resp2.StatusCode)
	}
}

// TestForceDownloadRetry checks that the queue's force-download button
// (retry?force=1) re-queues the job with the archive bypass set.
func TestForceDownloadRetry(t *testing.T) {
	srv, ts := serveWeb(t, "")

	id := srv.queue.Enqueue("http://x", queue.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := srv.queue.Wait(ctx, id); err != nil {
		t.Fatalf("waiting for first run: %v", err)
	}

	resp := postForm(t, ts, srv, "/queue/"+itoa(id)+"/retry?force=1", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("force download status = %d", resp.StatusCode)
	}
	job, err := srv.queue.Get(id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !job.Force {
		t.Error("force download did not set job.Force")
	}
}

// loginClient authenticates against a password-protected server and returns a
// cookie-jar client plus the CSRF token matching that session.
func loginClient(t *testing.T, ts *httptest.Server, srv *Server, password string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.csrfToken("anon")}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var sid string
	for _, c := range resp.Cookies() {
		if c.Name == "monloader_session" {
			sid = c.Value
		}
	}
	if sid == "" {
		t.Fatal("login did not set a session cookie")
	}
	return client, srv.csrfToken(sid)
}

func TestAuthSetInitialPassword(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "") // auth starts disabled
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// An empty new password is rejected.
	if body := readBody(t, postForm(t, ts, srv, "/settings/auth/password", url.Values{"new_password": {""}})); !strings.Contains(body, "required") {
		t.Errorf("empty new password should error, got %q", body)
	}
	// Setting a password also turns auth on and re-renders the section.
	body := readBody(t, postForm(t, ts, srv, "/settings/auth/password", url.Values{"new_password": {"hunter2"}}))
	if !srv.cfg.Current().Auth.EnablePassword || srv.cfg.Current().Auth.PasswordHash == "" {
		t.Fatalf("setting a password should enable auth, got %+v", srv.cfg.Current().Auth)
	}
	if !strings.Contains(body, "password updated") || !strings.Contains(body, "auth-password-section") {
		t.Errorf("expected a flash and the OOB section, got %q", body)
	}
}

func TestAuthPasswordChangeRemove(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "secret") // auth on, password "secret"
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client, csrf := loginClient(t, ts, srv, "secret")
	post := func(path string, vals url.Values) string {
		vals.Set("_csrf", csrf)
		resp, err := client.PostForm(ts.URL+path, vals)
		if err != nil {
			t.Fatal(err)
		}
		return readBody(t, resp)
	}

	// A wrong current password is rejected; the hash is unchanged.
	initialHash := srv.cfg.Current().Auth.PasswordHash
	if body := post("/settings/auth/password", url.Values{"current_password": {"wrong"}, "new_password": {"x"}}); !strings.Contains(body, "incorrect") {
		t.Errorf("a wrong current password should be rejected, got %q", body)
	}
	if srv.cfg.Current().Auth.PasswordHash != initialHash {
		t.Error("the hash should be unchanged after a rejected change")
	}
	// The correct current password lets it change.
	post("/settings/auth/password", url.Values{"current_password": {"secret"}, "new_password": {"newsecret"}})
	if srv.cfg.Current().Auth.PasswordHash == initialHash {
		t.Error("the hash should change after a confirmed change")
	}
	// Removing with the correct current password disables auth.
	post("/settings/auth/remove-password", url.Values{"current_password": {"newsecret"}})
	if srv.cfg.Current().Auth.EnablePassword || srv.cfg.Current().Auth.PasswordHash != "" {
		t.Errorf("remove should disable auth, got %+v", srv.cfg.Current().Auth)
	}
}

func TestAuthTokenCreate(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/auth/tokens", url.Values{"name": {"ci"}})
	if trig := resp.Header.Get("HX-Trigger"); trig != "token-created" {
		t.Errorf("a created token should trigger token-created to reset the form, got %q", trig)
	}
	body := readBody(t, resp)
	toks := srv.cfg.Current().Auth.Tokens
	if len(toks) != 1 || toks[0].Name != "ci" {
		t.Fatalf("want one token named ci, got %+v", toks)
	}
	m := regexp.MustCompile(`value="([a-f0-9]{32})"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("response should reveal a 32-hex secret once, got %q", body)
	}
	if config.HashToken(m[1]) != toks[0].TokenHash {
		t.Error("revealed secret does not match the stored hash")
	}
}

func TestNotFound(t *testing.T) {
	_, ts := serveWeb(t, "")

	if code, body := get(t, ts, "/no-such-page"); code != http.StatusNotFound {
		t.Errorf("GET /no-such-page = %d, want 404", code)
	} else if !strings.Contains(body, "404 - not found") || !strings.Contains(body, "back to monloader") {
		t.Errorf("themed 404 page expected, got %q", body)
	}

	if code, body := get(t, ts, "/api/v1/no-such-endpoint"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/no-such-endpoint = %d, want 404", code)
	} else if !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("JSON 404 expected for an API path, got %q", body)
	}

	if code, _ := get(t, ts, "/"); code != http.StatusOK {
		t.Errorf("GET / = %d, want 200 (add screen still resolves)", code)
	}
}

func TestAuthTokenPrivileges(t *testing.T) {
	srv, ts := serveWeb(t, "")

	readBody(t, postForm(t, ts, srv, "/settings/auth/tokens", url.Values{"name": {"ci"}}))
	id := srv.cfg.Current().Auth.Tokens[0].ID
	readBody(t, postForm(t, ts, srv, "/settings/auth/tokens/"+id+"/privileges", url.Values{"scope": {"read"}}))
	got := srv.cfg.Current().Auth.Tokens[0].Scopes
	if len(got) != 1 || got[0] != config.ScopeRead {
		t.Fatalf("scopes = %v, want [read]", got)
	}
}

func TestPairedTokenNotRevocable(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	tok, _ := config.GenerateToken("monbooru (paired)", config.AllScopes)
	tok.Paired = "monbooru"
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("DELETE", "/settings/auth/tokens/"+tok.ID, nil)
	req.SetPathValue("id", tok.ID)
	w := httptest.NewRecorder()
	srv.settingsTokenRevoke(w, req)

	if len(srv.cfg.Current().Auth.Tokens) != 1 {
		t.Fatalf("paired token was revoked directly; want it kept (manage via pairing)")
	}
	if !strings.Contains(w.Body.String(), "pairing") {
		t.Errorf("expected a pairing hint, got %q", w.Body.String())
	}
}

func TestPairedTokenPrivilegesLocked(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	tok, _ := config.GenerateToken("monbooru (paired)", config.AllScopes)
	tok.Paired = "monbooru"
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"scope": {config.ScopeRead}}
	req := httptest.NewRequest("POST", "/settings/auth/tokens/"+tok.ID+"/privileges", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", tok.ID)
	w := httptest.NewRecorder()
	srv.settingsTokenPrivilegesPost(w, req)

	if got := strings.Join(srv.cfg.Current().Auth.Tokens[0].Scopes, " "); got != strings.Join(config.AllScopes, " ") {
		t.Fatalf("paired token scopes changed to %q; want them unchanged (manage via pairing)", got)
	}
	if !strings.Contains(w.Body.String(), "pairing") {
		t.Errorf("expected a pairing hint, got %q", w.Body.String())
	}
}

func TestClearQueueButton(t *testing.T) {
	srv, ts := serveWeb(t, "")
	resp := postForm(t, ts, srv, "/queue/clear", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("clear status = %d, want 200", resp.StatusCode)
	}
}

func TestStatsRSS(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	st := srv.gatherStats()
	if st.Mem.RSS == 0 {
		t.Skip("RSS unavailable (non-Linux)")
	}
	// RSS is the whole resident process, so it must be at least the live heap.
	if st.Mem.RSS < st.Mem.HeapAlloc {
		t.Errorf("RSS %d should be >= live heap %d", st.Mem.RSS, st.Mem.HeapAlloc)
	}
}

func TestLoginFlow(t *testing.T) {
	srv, ts := serveWeb(t, "secret")

	// Login page renders.
	if code, body := get(t, ts, "/login"); code != 200 || !strings.Contains(body, "password") {
		t.Fatalf("login page: code=%d", code)
	}
	jar := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Wrong password -> 401.
	resp, _ := jar.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.csrfToken("anon")}, "password": {"wrong"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password status = %d, want 401", resp.StatusCode)
	}
	// Correct password -> 303 + session cookie.
	resp, _ = jar.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.csrfToken("anon")}, "password": {"secret"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct password status = %d, want 303", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "monloader_session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("no session cookie set on login")
	}
	// The cookie now grants access to a guarded page.
	req, _ := http.NewRequest("GET", ts.URL+"/queue", nil)
	req.AddCookie(cookie)
	resp, err := jar.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("authenticated /queue status = %d, want 200", resp.StatusCode)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAddBarHashImport(t *testing.T) {
	srv, ts := serveWeb(t, "")

	// A bare md5 and the prefixed form both enqueue a hash import; the hash is
	// lower-cased.
	for _, target := range []string{"0123456789ABCDEF0123456789abcdef", "md5:0123456789abcdef0123456789abcdef"} {
		postForm(t, ts, srv, "/", url.Values{"url": {target}}).Body.Close()
	}
	jobs, total := srv.queue.List(queue.ListOptions{})
	if total != 2 {
		t.Fatalf("want 2 hash-import jobs, got %d", total)
	}
	for _, j := range jobs {
		if j.Kind != queue.KindHashImport || j.MD5 != "0123456789abcdef0123456789abcdef" {
			t.Errorf("job kind=%s md5=%q, want a hash import on the lower-cased md5", j.Kind, j.MD5)
		}
	}
}

func TestAddBarSHA256Rejected(t *testing.T) {
	srv, ts := serveWeb(t, "")

	sha := strings.Repeat("a", 64)
	body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {sha}}))
	// With the PTR off (the default), the hint points at enabling it.
	if !strings.Contains(body, "sha256") || !strings.Contains(body, "ptr page") {
		t.Errorf("a sha256 paste should point at the ptr page, got %q", body)
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("a sha256 must not enqueue, queue has %d", total)
	}
}

func TestAddBarMD5RejectedWhenNoLookupSite(t *testing.T) {
	srv, ts := serveWeb(t, "")
	// Clear danbooru's seeded order so no site is queried.
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Sites = []config.Site{{Name: "danbooru", LookupOrder: 0}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {"0123456789abcdef0123456789abcdef"}}))
	if !strings.Contains(body, "md5 lookup is off") {
		t.Errorf("md5 import with no lookup site should flash the off hint, got %q", body)
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("nothing should enqueue when md5 lookup is off, queue has %d", total)
	}
}

func TestAddBarShowsLookupStatus(t *testing.T) {
	_, ts := serveWeb(t, "")

	body := httpGet(t, ts.URL+"/")
	// Default seeds danbooru, so the booru chain is on; the PTR is off.
	if !strings.Contains(body, "lookup-status") || !strings.Contains(body, "booru lookup:") || !strings.Contains(body, "sha256 lookup:") {
		t.Errorf("home page should show the lookup status line, got %q", truncate(body))
	}
	// The hover title lists the queryable chain: danbooru works anonymously,
	// the services and the keyed sites are skipped without credentials.
	if !strings.Contains(body, "queries danbooru") || strings.Contains(body, "iqdb (similarity)") {
		t.Errorf("status title should list only the queryable sources, got %q", truncate(body))
	}
}

func TestSaveSiteIgnoresStaleLookupOrder(t *testing.T) {
	srv, ts := serveWeb(t, "")

	// The site dialog no longer carries the field; a stale form posting one
	// must not disturb the chain (gelbooru keeps its seeded position).
	postForm(t, ts, srv, "/settings/sites", url.Values{"name": {"gelbooru"}, "api_key": {"k"}, "lookup_order": {"9"}}).Body.Close()
	site := srv.cfg.Current().FindSite("gelbooru")
	if site.APIKey != "k" || site.LookupOrder != 4 {
		t.Errorf("gelbooru = %+v, want the key saved and the seeded order 4 kept", site)
	}
}

func TestSaveLookupChain(t *testing.T) {
	srv, ts := serveWeb(t, "")

	// Reorder across both number spaces; a blank clears saucenao out.
	postForm(t, ts, srv, "/settings/lookup/chain", url.Values{
		"order-iqdb":     {"1"},
		"order-danbooru": {"2"},
		"order-yandere":  {"3"},
		"order-saucenao": {""},
	}).Body.Close()
	cfg := srv.cfg.Current()
	if cfg.Lookup.Iqdb.Order != 1 || cfg.Lookup.Saucenao.Order != 0 {
		t.Errorf("service orders = %+v, want iqdb 1 and saucenao cleared", cfg.Lookup)
	}
	if cfg.FindSite("danbooru").LookupOrder != 2 || cfg.FindSite("yandere").LookupOrder != 3 {
		t.Errorf("site orders wrong: %+v", cfg.Sites)
	}

	// A templateless site cannot join the chain.
	resp := postForm(t, ts, srv, "/settings/lookup/chain", url.Values{"order-nhentai": {"1"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "does+not+support+md5+lookup") {
		t.Errorf("order on a templateless site should be refused, got redirect %q", loc)
	}
	if s := srv.cfg.Current().FindSite("nhentai"); s != nil && s.LookupOrder != 0 {
		t.Errorf("nhentai should not have kept an order: %+v", s)
	}
}

func TestSaveLookup(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/lookup", url.Values{"min_similarity": {"90"}}).Body.Close()
	if got := srv.cfg.Current().Lookup.MinSimilarity; got != 90 {
		t.Errorf("min_similarity = %d, want 90", got)
	}
	resp := postForm(t, ts, srv, "/settings/lookup", url.Values{"min_similarity": {"150"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "kind=err") || srv.cfg.Current().Lookup.MinSimilarity != 90 {
		t.Errorf("an out-of-range floor should be refused, got %q and %d", loc, srv.cfg.Current().Lookup.MinSimilarity)
	}

	// The saucenao key saves like a site credential: blank keeps it.
	postForm(t, ts, srv, "/settings/lookup", url.Values{"api_key": {"sk-1"}}).Body.Close()
	postForm(t, ts, srv, "/settings/lookup", url.Values{"api_key": {""}, "min_similarity": {"85"}}).Body.Close()
	if cfg := srv.cfg.Current(); cfg.Lookup.Saucenao.APIKey != "sk-1" || cfg.Lookup.MinSimilarity != 85 {
		t.Errorf("lookup = %+v, want the key kept and the floor updated", cfg.Lookup)
	}
}

// stubSim is a simService for the panel and its test button.
type stubSim struct {
	missing map[string]string
	res     similarity.Result
	err     error
}

func (s *stubSim) Missing(service string) (string, bool) {
	label, ok := s.missing[service]
	return label, ok
}

func (s *stubSim) Search(context.Context, string, []byte) (similarity.Result, error) {
	return s.res, s.err
}

func (s *stubSim) LimitedUntil(service string) time.Time { return time.Time{} }

// stubMetaRunner scripts FetchMeta for the exact-md5 test button.
type stubMetaRunner struct {
	fakeRunner
	err  error
	urls []string
}

func (s *stubMetaRunner) FetchMeta(_ context.Context, url string) (map[string]any, error) {
	s.urls = append(s.urls, url)
	return nil, s.err
}

func TestLookupPanelRenders(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	srv.sim = &stubSim{missing: map[string]string{"iqdb": "danbooru api key", "saucenao": "api key"}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := httpGet(t, ts.URL+"/settings")
	// The default chain renders both source kinds with their states.
	for _, want := range []string{
		"exact md5", "similarity",
		"needs danbooru api key", "needs api key",
		"uses the danbooru site credentials", "api key: not set",
		"min similarity", "chain-dialog", "saucenao-dialog",
		`id="chain-on"`, `id="chain-off"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	// The sites tables no longer carry the order column.
	if strings.Contains(body, "se-field-lookup") || strings.Contains(body, `name="lookup_order"`) {
		t.Error("the site dialog should have dropped its lookup-order field")
	}
}

func TestLookupTestButton(t *testing.T) {
	srv, ts := serveWeb(t, "")

	post := func(service string) string {
		req, _ := http.NewRequest("POST", ts.URL+"/settings/lookup/test/"+service, nil)
		req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return readBody(t, resp)
	}

	srv.sim = &stubSim{missing: map[string]string{"saucenao": "api key"}}
	if body := post("saucenao"); !strings.Contains(body, "needs api key") {
		t.Errorf("keyless saucenao test = %q", body)
	}
	srv.sim = &stubSim{res: similarity.Result{DailyRemaining: 94}}
	if body := post("saucenao"); !strings.Contains(body, "ok, 94 queries left today") {
		t.Errorf("saucenao test = %q, want the quota note", body)
	}
	srv.sim = &stubSim{res: similarity.Result{DailyRemaining: -1}}
	if body := post("iqdb"); !strings.Contains(body, ">ok<") {
		t.Errorf("iqdb test = %q, want a bare ok", body)
	}
	srv.sim = &stubSim{err: &queue.CodedError{Code: queue.ErrCodeRateLimited, Msg: "cooling down"}}
	if body := post("iqdb"); !strings.Contains(body, "rate limited") {
		t.Errorf("limited test = %q", body)
	}
	// An exact-md5 site is testable too: its search runs with the probe
	// image's md5, and "no match" (mapping_failed) is the successful round
	// trip.
	mr := &stubMetaRunner{err: &queue.CodedError{Code: queue.ErrCodeMappingFailed, Msg: "no post metadata"}}
	srv.runner = mr
	if body := post("danbooru"); !strings.Contains(body, ">ok<") {
		t.Errorf("danbooru md5 test = %q, want ok on a clean miss", body)
	}
	sum := md5.Sum(similarity.ProbeImage())
	if len(mr.urls) != 1 || !strings.Contains(mr.urls[0], hex.EncodeToString(sum[:])) {
		t.Errorf("fetches = %v, want one search for the probe md5", mr.urls)
	}
	srv.runner = &stubMetaRunner{err: &queue.CodedError{Code: queue.ErrCodeAuthRequired, Msg: "401"}}
	if body := post("danbooru"); !strings.Contains(body, "auth rejected") {
		t.Errorf("danbooru md5 test = %q, want auth rejected", body)
	}
	if body := post("gelbooru"); !strings.Contains(body, "needs api key") {
		t.Errorf("keyless gelbooru test = %q", body)
	}
	if resp, err := http.Post(ts.URL+"/settings/lookup/test/nosuch", "", nil); err == nil {
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
			t.Errorf("unknown service = %d, want a refusal", resp.StatusCode)
		}
		resp.Body.Close()
	}
}
