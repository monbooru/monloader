package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func setupServer(t *testing.T) *Server {
	t.Helper()
	srv := desktopServer(t)
	storeCfg(srv, func(c *config.Config) { c.SetupDone = false })
	return srv
}

func TestSetupGateRedirectsEveryPage(t *testing.T) {
	srv := setupServer(t)
	h := srv.Handler()
	for _, path := range []string{"/", "/queue", "/settings", "/ptr"} {
		w := record(h.ServeHTTP, localRequest("GET", path, ""))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
			t.Errorf("%s: status = %d location = %q, want 303 -> /setup", path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestSetupGateAndPasswordDoNotLockTheOperatorOut(t *testing.T) {
	srv := setupServer(t)
	storeCfg(srv, func(c *config.Config) {
		c.Auth.EnablePassword = true
		c.Auth.PasswordHash = "$2a$10$notarealhashbutlongenoughtolooklikeone"
	})
	h := srv.Handler()

	w := record(h.ServeHTTP, localRequest("GET", "/login", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("/login status = %d location = %q, want the login page", w.Code, w.Header().Get("Location"))
	}
	w = record(h.ServeHTTP, localRequest("GET", "/queue", ""))
	if !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("/queue location = %q, want the session gate first", w.Header().Get("Location"))
	}
}

func TestSetupGateRefusesTheAPIInJSON(t *testing.T) {
	srv := setupServer(t)
	h := srv.Handler()
	for _, path := range []string{"/api/v1/queue", "/api/v1/ptr/status", "/api/v1/pair/request"} {
		w := record(h.ServeHTTP, localRequest("GET", path, ""))
		if w.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"code":"setup_required"`) {
			t.Errorf("%s: body = %s, want a setup_required code", path, w.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/openapi.json", "/api/v1/docs"} {
		w := record(h.ServeHTTP, localRequest("GET", path, ""))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
	}
}

func TestSetupGateRefusalIsReadableCrossOrigin(t *testing.T) {
	srv := setupServer(t)
	h := srv.Handler()
	const origin = "chrome-extension://abc"
	for _, path := range []string{"/api/v1/queue", "/api/v1/pair/request", "/api/v1/pair/status"} {
		w := httptest.NewRecorder()
		r := localRequest("OPTIONS", path, "")
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("OPTIONS %s: status = %d ACAO = %q, want 204 with %q", path, w.Code, w.Header().Get("Access-Control-Allow-Origin"), origin)
		}
	}
	for _, method := range []string{"GET", "POST"} {
		w := httptest.NewRecorder()
		r := localRequest(method, "/api/v1/queue", "")
		r.Header.Set("Origin", origin)
		h.ServeHTTP(w, r)
		if w.Code != http.StatusConflict || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("%s /api/v1/queue: status = %d ACAO = %q, want 409 with %q", method, w.Code, w.Header().Get("Access-Control-Allow-Origin"), origin)
		}
		if !strings.Contains(w.Body.String(), `"code":"setup_required"`) {
			t.Errorf("%s /api/v1/queue: body = %s, want a setup_required code", method, w.Body.String())
		}
	}
}

func TestSetupGateExemptsWhatTheWizardNeeds(t *testing.T) {
	srv := setupServer(t)
	h := srv.Handler()
	for _, path := range []string{"/health", "/setup", "/static/main.css"} {
		w := record(h.ServeHTTP, localRequest("GET", path, ""))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
	}
}

func TestSetupGateIsOffWithoutTheDesktopProfile(t *testing.T) {
	srv := setupServer(t)
	srv.desktop = false
	h := srv.Handler()
	w := record(h.ServeHTTP, localRequest("GET", "/", ""))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	w = record(h.ServeHTTP, localRequest("GET", "/setup", ""))
	if w.Code != http.StatusNotFound {
		t.Errorf("/setup status = %d, want 404 off the desktop profile", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Back to") {
		t.Errorf("/setup 404 should render the styled page, got %q", w.Body.String())
	}
}

func TestSetupStoresTheAddressAndClearsTheGate(t *testing.T) {
	srv := setupServer(t)
	w := record(srv.setupPost, localRequest("POST", "/setup", "api_url="+url.QueryEscape("http://127.0.0.1:8455")+"&reach=local"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("status = %d location = %q: %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if got := srv.cfg.Current().Monbooru.APIURL; got != "http://127.0.0.1:8455" {
		t.Errorf("api_url = %q", got)
	}
	if srv.setupPending() {
		t.Error("the gate should be down once the wizard is through")
	}
}

func TestSetupPairingFailureFinishesTheSetup(t *testing.T) {
	srv := setupServer(t)
	w := httptest.NewRecorder()

	srv.setupPost(w, localRequest("POST", "/setup", "api_url="+url.QueryEscape("http://127.0.0.1:1")+"&reach=local&pair=on"))

	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/settings?") {
		t.Fatalf("status = %d location = %q, want 303 to settings: %s", w.Code, loc, w.Body.String())
	}
	if !strings.Contains(loc, "kind=err") || !strings.Contains(loc, "section=pairing") || !strings.Contains(loc, "pairing+request+failed") {
		t.Errorf("location = %q, want the pairing failure flashed in its section", loc)
	}
	page := record(srv.settingsScreen, localRequest("GET", loc, ""))
	body := page.Body.String()
	if at := strings.Index(body, "flash flash-err"); at < strings.Index(body, `id="pairing"`) {
		t.Errorf("the flash renders outside the Pairing section, at %d", at)
	}
	if srv.setupPending() {
		t.Error("the setup is finished; leaving the gate up would send the operator back to a wizard with nothing left to do")
	}
}

func pairingPeer(t *testing.T, answer *string) *httptest.Server {
	t.Helper()
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/pair/request":
			_, _ = w.Write([]byte(`{"request_id":"r1","status":"pending"}`))
		case "/api/v1/pair/status":
			_, _ = w.Write([]byte(*answer))
		case "/api/v1/galleries":
			_, _ = w.Write([]byte(`[{"name":"main"}]`))
		default:
			_, _ = w.Write([]byte(`{"status":"ok","version":"v9"}`))
		}
	}))
	t.Cleanup(mb.Close)
	return mb
}

func TestSetupPairingCommitsFromTheFooterLight(t *testing.T) {
	answer := `{"status":"pending"}`
	mb := pairingPeer(t, &answer)
	srv := setupServer(t)
	w := record(srv.setupPost, localRequest("POST", "/setup", "api_url="+url.QueryEscape(mb.URL)+"&reach=local&pair=on"))
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("location = %q, want the landing", loc)
	}

	light := func() string {
		w := record(srv.monbooruStatus, httptest.NewRequest("GET", "/internal/monbooru-status", nil))
		return w.Body.String()
	}
	light()
	if srv.hasPairedToken(contract.AppMonbooru) {
		t.Fatal("committed before the approval")
	}

	answer = `{"status":"approved","token":"mb-issued"}`
	if body := light(); !strings.Contains(body, `data-conn="ok"`) {
		t.Errorf("the light after an approval reads %q, want ok", body)
	}
	if !srv.hasPairedToken(contract.AppMonbooru) || srv.cfg.Current().Monbooru.APIToken != "mb-issued" {
		t.Errorf("not committed: token %q", srv.cfg.Current().Monbooru.APIToken)
	}

	w = record(srv.monbooruPairPoll, httptest.NewRequest("POST", "/settings/monbooru/pair/poll", nil))
	if !strings.Contains(w.Body.String(), `id="auth-tokens"`) {
		t.Errorf("a panel still polling should refresh the token list: %s", w.Body.String())
	}
}

func TestFooterLightClearsARequestMonbooruDropped(t *testing.T) {
	answer := `{"status":"pending"}`
	mb := pairingPeer(t, &answer)
	srv := setupServer(t)
	srv.setupPost(httptest.NewRecorder(), localRequest("POST", "/setup", "api_url="+url.QueryEscape(mb.URL)+"&reach=local&pair=on"))
	if srv.getPairAttempt() == nil {
		t.Fatal("no attempt after the setup")
	}
	answer = `{"status":"not_found"}`
	srv.monbooruStatus(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monbooru-status", nil))
	if srv.getPairAttempt() != nil {
		t.Error("the attempt should clear once monbooru no longer has it")
	}
}

func TestFooterLightLeavesAPausedLinkUnprobed(t *testing.T) {
	answer := `{"status":"approved","token":"mb-issued"}`
	mb := pairingPeer(t, &answer)
	srv := setupServer(t)
	srv.setupPost(httptest.NewRecorder(), localRequest("POST", "/setup", "api_url="+url.QueryEscape(mb.URL)+"&reach=local&pair=on"))
	if err := srv.cfg.Update(func(c *config.Config) error { c.Monbooru.Paused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	srv.monbooruStatus(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monbooru-status", nil))
	if srv.hasPairedToken(contract.AppMonbooru) {
		t.Error("a paused link should not be asked about the attempt")
	}
}

func TestSetupRefusesANonURL(t *testing.T) {
	srv := setupServer(t)
	w := record(srv.setupPost, localRequest("POST", "/setup", "api_url=not-a-url&reach=local"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "http(s) address") {
		t.Errorf("status = %d body = %s", w.Code, w.Body.String())
	}
	if !srv.setupPending() {
		t.Error("a refused submit must not clear the gate")
	}
}

func TestSetupWritesTheBind(t *testing.T) {
	srv := setupServer(t)
	w := record(srv.setupPost, localRequest("POST", "/setup", "reach=lan"))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := srv.cfg.Current().Server.BindAddress; got != "0.0.0.0:8456" {
		t.Errorf("bind = %q", got)
	}
}

func TestSetupLeavesThePasswordAlone(t *testing.T) {
	srv := setupServer(t)
	cfg := srv.cfg.Current().Clone()
	cfg.Auth.EnablePassword = true
	cfg.Auth.PasswordHash = "$2a$10$notarealhashbutlongenoughtolooklikeone"
	srv.cfg.Store(cfg)

	w := record(srv.setupPost, localRequest("POST", "/setup", "reach=local"))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := srv.cfg.Current().Auth; !got.EnablePassword || got.PasswordHash != cfg.Auth.PasswordHash {
		t.Errorf("auth = %+v, want it untouched", got)
	}
}

func TestSetupRefusalKeepsWhatWasEntered(t *testing.T) {
	srv := setupServer(t)
	w := record(srv.setupPost, localRequest("POST", "/setup", "api_url=monbooru.lan:8455&reach=lan"))
	body := w.Body.String()
	if !strings.Contains(body, "monbooru.lan:8455") {
		t.Errorf("the typed address is gone:\n%s", body)
	}
	if !strings.Contains(body, `value="lan" checked`) {
		t.Errorf("the chosen reach is gone:\n%s", body)
	}
}

func TestSetupIsGoneOnceDone(t *testing.T) {
	srv := desktopServer(t)
	storeCfg(srv, func(c *config.Config) { c.SetupDone = true })
	w := record(srv.Handler().ServeHTTP, localRequest("GET", "/setup", ""))
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /setup status = %d, want 404", w.Code)
	}
	w = record(srv.setupPost, localRequest("POST", "/setup", "reach=lan"))
	if w.Code != http.StatusNotFound {
		t.Errorf("POST /setup status = %d, want 404", w.Code)
	}
	if got := srv.cfg.Current().Server.BindAddress; got != "127.0.0.1:8456" {
		t.Errorf("bind = %q, want the address left alone", got)
	}
}

func TestSetupRefusesARemotePeer(t *testing.T) {
	srv := setupServer(t)
	r := localRequest("POST", "/setup", "reach=lan")
	r.RemoteAddr = "192.168.1.20:54321"
	w := record(srv.setupPost, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !srv.setupPending() {
		t.Error("a remote submit must not clear the gate")
	}
	if got := srv.cfg.Current().Server.BindAddress; got != "127.0.0.1:8456" {
		t.Errorf("bind = %q, want the address left alone", got)
	}
}

func TestHostPort(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8455":  "127.0.0.1:8455",
		"http://127.0.0.1:8455/": "127.0.0.1:8455",
		"http://monbooru":        "monbooru:80",
		"https://example.com":    "example.com:443",
		"http://[::1]":           "[::1]:80",
		"http://[::1]:8455/":     "[::1]:8455",
		"":                       "",
	}
	for in, want := range cases {
		if got := hostPort(in); got != want {
			t.Errorf("hostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetupPageDrawsTheThemesWaitingFaceOrNone(t *testing.T) {
	srv := setupServer(t)
	h := srv.Handler()
	page := func() string {
		w := record(h.ServeHTTP, localRequest("GET", "/setup", ""))
		return w.Body.String()
	}
	for theme, want := range map[string]string{"dark": "/static/mascot-sleep.png", "light": "/static/light/mascot-sleep.png"} {
		storeCfg(srv, func(c *config.Config) { c.Server.Theme = theme })
		body := page()
		for _, s := range []string{`<div id="setup-page" class="setup-drawn">`, `src="` + want + `"`, `<h1>mon<span class="wordmark-accent">loader</span></h1>`} {
			if !strings.Contains(body, s) {
				t.Errorf("%s setup page missing %q", theme, s)
			}
		}
	}
	storeCfg(srv, func(c *config.Config) { c.Server.Theme = "legacy-light" })
	body := page()
	if strings.Contains(body, `class="mascot"`) || !strings.Contains(body, `<link rel="stylesheet" href="/theme.css">`) {
		t.Error("legacy-light's setup page should link its sheet and show no drawing")
	}
}

func TestSetupGateLetsTheWizardLoadAThemesDrawing(t *testing.T) {
	srv := setupServer(t)
	installTheme(t, srv, map[string]string{"aurora/theme.css": ":root{}", "aurora/mascot-sleep.png": "sleep-bytes"})
	storeCfg(srv, func(c *config.Config) { c.Server.Theme = "aurora" })
	w := record(srv.Handler().ServeHTTP, localRequest("GET", "/theme.mascot/sleep", ""))
	if w.Code != http.StatusOK || w.Body.String() != "sleep-bytes" {
		t.Errorf("/theme.mascot/sleep during setup = %d %q", w.Code, w.Body.String())
	}
}
