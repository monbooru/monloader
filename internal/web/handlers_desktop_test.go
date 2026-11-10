package web

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/desktop"
)

func isolateDesktopHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("FLATPAK_ID", "")
}

func desktopServer(t *testing.T) *Server {
	t.Helper()
	isolateDesktopHome(t)
	srv := newWebServer(t, "", "")
	storeCfg(srv, func(c *config.Config) { c.Server.BindAddress = "127.0.0.1:8456" })
	srv.desktop = true
	srv.logDir = filepath.Join(t.TempDir(), "logs")
	return srv
}

func TestTestServersLeaveTheRealEntriesAlone(t *testing.T) {
	if got := desktopServer(t).DesktopHook().App; got == "monloader" {
		t.Errorf("a test server drives the real %q entries", got)
	}
}

func waitQuit(t *testing.T, srv *Server) {
	t.Helper()
	select {
	case <-srv.QuitRequested():
	case <-time.After(5 * time.Second):
		t.Fatal("QuitRequested never fired")
	}
}

func localRequest(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.RemoteAddr = "127.0.0.1:54321"
	r.Host = "127.0.0.1:8456"
	return r
}

func TestBrowseListsSubdirectoriesOnly(t *testing.T) {
	srv := desktopServer(t)
	root := t.TempDir()
	for _, d := range []string{"alpha", "beta", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a-file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := record(srv.browseDirs, localRequest("GET", "/internal/browse?into=ptr-path&path="+url.QueryEscape(root), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"alpha/", "beta/"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "a-file.txt") {
		t.Error("files must never be listed")
	}
	if strings.Contains(body, ".hidden") {
		t.Error("dot directories must not be listed")
	}
}

func TestBrowseLinksEncodeAPathWithQueryCharacters(t *testing.T) {
	srv := desktopServer(t)
	root := t.TempDir()
	for _, d := range []string{"R&D", "a+b", "50%"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := record(srv.browseDirs, localRequest("GET", "/internal/browse?into=ptr-path&path="+url.QueryEscape(root), ""))
	body := w.Body.String()

	for _, name := range []string{"R&D", "a+b", "50%"} {
		want := "path=" + url.QueryEscape(filepath.Join(root, name))
		if !strings.Contains(body, want) {
			t.Errorf("no encoded link for %q (want %q):\n%s", name, want, truncate(body))
		}
		if strings.Contains(body, "path="+filepath.Join(root, name)+`"`) {
			t.Errorf("%q is linked unencoded", name)
		}
	}
}

func TestBrowseRejectsABadTargetField(t *testing.T) {
	srv := desktopServer(t)
	w := record(srv.browseDirs, localRequest("GET", "/internal/browse?into="+url.QueryEscape(`" onload="x`), ""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDesktopControlsAreGated(t *testing.T) {
	cases := []struct {
		name    string
		profile bool
		bind    string
		remote  string
		host    string
	}{
		{"no desktop profile", false, "127.0.0.1:8456", "127.0.0.1:5000", ""},
		{"remote is not loopback", true, "127.0.0.1:8456", "192.168.1.20:5000", ""},
		{"remote is not loopback on a network bind", true, "0.0.0.0:8456", "192.168.1.20:5000", ""},

		{"renamed host", true, "0.0.0.0:8456", "127.0.0.1:5000", "nas.lan:8456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := desktopServer(t)
			srv.desktop = tc.profile
			storeCfg(srv, func(c *config.Config) {
				c.Server.BindAddress = tc.bind

				c.SetupDone = true
			})
			csrf := srv.sessions.CSRFToken("anon")

			h := srv.Handler()
			for _, probe := range []struct {
				path, method, body string
			}{
				{"/internal/browse?into=x", "GET", ""},
				{"/internal/open-folder", "POST", "kind=config&_csrf=" + csrf},
				{"/settings/desktop", "POST", "_csrf=" + csrf},
				{"/settings/quit", "POST", "_csrf=" + csrf},
				{"/settings/restart", "POST", "_csrf=" + csrf},
				{"/internal/desktop-actions", "GET", ""},
			} {
				w := httptest.NewRecorder()
				r := localRequest(probe.method, probe.path, probe.body)
				r.RemoteAddr = tc.remote
				if tc.host != "" {
					r.Host = tc.host
				}
				h.ServeHTTP(w, r)
				if w.Code != http.StatusNotFound {
					t.Errorf("%s %s: status = %d, want 404", probe.method, probe.path, w.Code)
				}
			}
		})
	}
}

func TestDesktopControlsRefuseAForwardedHop(t *testing.T) {
	srv := desktopServer(t)
	storeCfg(srv, func(c *config.Config) { c.SetupDone = true })
	for _, header := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "Via"} {
		r := localRequest("GET", "/internal/browse?into=x", "")
		r.Header.Set(header, "192.168.1.20")
		if srv.desktopLocal(r) {
			t.Errorf("%s: a proxied request passed as the local machine", header)
		}
		w := record(srv.Handler().ServeHTTP, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: browse status = %d, want 404", header, w.Code)
		}
	}
}

func TestDesktopControlsSurviveANetworkBind(t *testing.T) {
	srv := desktopServer(t)
	storeCfg(srv, func(c *config.Config) { c.Server.BindAddress = "0.0.0.0:8456" })

	w := record(srv.settingsScreen, localRequest("GET", "/settings", ""))
	if !strings.Contains(w.Body.String(), `action="/settings/restart"`) {
		t.Errorf("no restart control on a network bind:\n%s", truncate(w.Body.String()))
	}
	w = record(srv.browseDirs, localRequest("GET", "/internal/browse?into=x", ""))
	if w.Code != http.StatusOK {
		t.Errorf("browse status = %d, want 200", w.Code)
	}
}

func TestOpenFolderRefusesAnUnknownKind(t *testing.T) {
	srv := desktopServer(t)
	w := record(srv.openFolder, localRequest("POST", "/internal/open-folder", "kind=/etc"))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a flash redirect", w.Code)
	}
	if !strings.Contains(w.Header().Get("Location"), "kind=err") {
		t.Errorf("location = %q, want an error flash", w.Header().Get("Location"))
	}
}

func TestAvailableFoldersDropsTheUnsetPTRPath(t *testing.T) {
	srv := desktopServer(t)
	storeCfg(srv, func(c *config.Config) { c.PTR.DataPath = "" })
	if got := srv.availableFolders(); slices.Contains(got, "ptr") {
		t.Errorf("availableFolders = %v, want no ptr entry while the path is unset", got)
	}
	storeCfg(srv, func(c *config.Config) { c.PTR.DataPath = t.TempDir() })
	if got := srv.availableFolders(); !slices.Contains(got, "ptr") {
		t.Errorf("availableFolders = %v, want a ptr entry once the path is set", got)
	}
}

func TestQuitNamesWhatIsInFlight(t *testing.T) {
	srv := desktopServer(t)
	w := record(srv.settingsQuit, localRequest("POST", "/settings/quit", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "has stopped") {
		t.Errorf("no stopped page rendered:\n%s", w.Body.String())
	}
	waitQuit(t, srv)
}

func TestQuitIsIdempotentAndKeepsAPendingRestart(t *testing.T) {
	srv := desktopServer(t)
	srv.RequestRestart()
	srv.RequestQuit()
	srv.RequestQuit()
	waitQuit(t, srv)
	if !srv.RestartRequested() {
		t.Error("a second quit dropped the restart")
	}
}

func TestRestartWarnsLikeTheStopBesideIt(t *testing.T) {
	srv := desktopServer(t)
	srv.queue.Pause()
	srv.queue.Enqueue("https://example.com/posts/1", queue.Options{})

	got := srv.desktopRestartPrompt()
	if !strings.Contains(got, "1 job is") || !strings.Contains(got, "cancels it") {
		t.Errorf("prompt = %q, want the waiting job counted and said to be cancelled", got)
	}
	w := record(srv.settingsScreen, localRequest("GET", "/settings", ""))
	if body := w.Body.String(); !strings.Contains(body, `hx-confirm="Restart monloader? 1 job is`) {
		t.Errorf("the restart form carries no counted confirmation:\n%s", truncate(body))
	}
}

func TestDesktopActionsFragmentCountsTheQueueNow(t *testing.T) {
	srv := desktopServer(t)
	w := record(srv.settingsScreen, localRequest("GET", "/settings", ""))
	if body := w.Body.String(); strings.Contains(body, "job is") {
		t.Fatalf("an idle queue should count nothing:\n%s", truncate(body))
	}

	srv.queue.Pause()
	for range 4 {
		srv.queue.Enqueue("https://example.com/posts/1", queue.Options{})
	}
	w = record(srv.desktopActionsFragment, localRequest("GET", "/internal/desktop-actions", ""))
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, want := range []string{"Stop monloader? 4 jobs are", "Restart monloader? 4 jobs are"} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment missing %q:\n%s", want, truncate(body))
		}
	}
}

func TestQuitPromptCountsNothingWhenTheQueueIsEmpty(t *testing.T) {
	srv := desktopServer(t)
	if got := srv.desktopQuitPrompt(); strings.Contains(got, "job is") {
		t.Errorf("prompt = %q, want no job count on an empty queue", got)
	}
}

func TestQuitPromptSaysAWaitingJobIsCancelled(t *testing.T) {
	srv := desktopServer(t)
	srv.queue.Pause()
	srv.queue.Enqueue("https://example.com/posts/1", queue.Options{})
	got := srv.desktopQuitPrompt()
	if !strings.Contains(got, "1 job is") {
		t.Errorf("prompt = %q, want the waiting job counted", got)
	}
	if !strings.Contains(got, "cancels it") {
		t.Errorf("prompt = %q, want the stop to say it cancels it", got)
	}
	if strings.Contains(got, "resume") {
		t.Errorf("prompt = %q, want no resume promise the queue cannot keep", got)
	}
}

func TestTraySwitchFollowsTheBuild(t *testing.T) {
	srv := desktopServer(t)
	w := record(srv.settingsScreen, localRequest("GET", "/settings", ""))
	shown := strings.Contains(w.Body.String(), `name="tray"`)
	if want := desktop.TrayAvailable(); shown != want {
		t.Errorf("tray switch rendered = %v, want %v", shown, want)
	}
	if !srv.TrayEnabled() {
		t.Error("the tray should start on")
	}

	record(srv.settingsDesktopPost, localRequest("POST", "/settings/desktop", ""))
	if got, want := srv.TrayEnabled(), !desktop.TrayAvailable(); got != want {
		t.Errorf("tray after an unticked save = %v, want %v", got, want)
	}
}

func TestRestartAsksForAFreshProcess(t *testing.T) {
	srv := desktopServer(t)
	if srv.RestartRequested() {
		t.Fatal("nothing has asked for a restart yet")
	}
	w := record(srv.settingsRestart, localRequest("POST", "/settings/restart", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "is restarting") {
		t.Errorf("no restarting page rendered:\n%s", w.Body.String())
	}
	waitQuit(t, srv)
	if !srv.RestartRequested() {
		t.Error("the command would stop instead of starting again")
	}
}

func TestQuitDoesNotAskForARestart(t *testing.T) {
	srv := desktopServer(t)
	record(srv.settingsQuit, localRequest("POST", "/settings/quit", ""))
	waitQuit(t, srv)
	if srv.RestartRequested() {
		t.Error("a stop asked for a restart")
	}
}

func TestMonbooruWebURLHintNamesTheCompanionPort(t *testing.T) {
	srv := desktopServer(t)
	for _, desktop := range []bool{true, false} {
		srv.desktop = desktop
		w := record(srv.settingsScreen, localRequest("GET", "/settings", ""))
		if !strings.Contains(w.Body.String(), `placeholder="http://localhost:8455"`) {
			t.Errorf("desktop=%v: no hint at monbooru's port:\n%s", desktop, truncate(w.Body.String()))
		}
	}
}

func TestMonbooruAPIHintFollowsTheProfile(t *testing.T) {
	srv := desktopServer(t)
	if got := srv.monbooruAPIHint(); got != "http://127.0.0.1:8455" {
		t.Errorf("desktop hint = %q", got)
	}
	srv.desktop = false
	if got := srv.monbooruAPIHint(); got != config.Default().Monbooru.APIURL {
		t.Errorf("container hint = %q, want the shipped default", got)
	}
}

var hicolorSizes = []int{16, 22, 24, 32, 36, 48, 64, 72, 96, 128, 192, 256, 512}

func TestDesktopHookIconFitsTheIconTheme(t *testing.T) {
	srv := desktopServer(t)
	cfg, err := png.DecodeConfig(bytes.NewReader(srv.DesktopHook().Icon))
	if err != nil {
		t.Fatalf("decoding the hook icon: %v", err)
	}
	if cfg.Width != cfg.Height {
		t.Fatalf("icon is %dx%d, want a square", cfg.Width, cfg.Height)
	}
	if !slices.Contains(hicolorSizes, cfg.Width) {
		t.Errorf("icon is %dpx, want one of %v", cfg.Width, hicolorSizes)
	}
}

func TestLoopbackBindRefusesAReboundHost(t *testing.T) {
	srv := desktopServer(t)
	storeCfg(srv, func(c *config.Config) {
		c.SetupDone = true
		c.Server.BaseURL = "http://monloader.lan:8456"
	})
	h := srv.Handler()
	for _, tc := range []struct {
		host string
		want int
	}{
		{"evil.example:8456", http.StatusMisdirectedRequest},
		{"127.0.0.1:8456", http.StatusOK},
		{"localhost:8456", http.StatusOK},
		{"[::1]:8456", http.StatusOK},
		{"monloader.lan:8456", http.StatusOK},
	} {
		w := httptest.NewRecorder()
		r := localRequest("GET", "/settings", "")
		r.Host = tc.host
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("Host %s: status = %d, want %d", tc.host, w.Code, tc.want)
		}
	}

	storeCfg(srv, func(c *config.Config) { c.Server.BindAddress = "0.0.0.0:8456" })
	w := httptest.NewRecorder()
	r := localRequest("GET", "/settings", "")
	r.Host = "nas.lan:8456"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("a network bind refused a LAN name: status = %d", w.Code)
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/settings", http.StatusMisdirectedRequest},
		{"/health", http.StatusOK},
		{"/api/v1/queue", http.StatusServiceUnavailable},
	} {
		w := httptest.NewRecorder()
		r := localRequest("GET", tc.path, "")
		r.Host = "evil.example:8456"
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("a network bind, public name, %s: status = %d, want %d", tc.path, w.Code, tc.want)
		}
	}
}

func TestDesktopHookFilesMonloaderUnderPhotography(t *testing.T) {
	if got := desktopServer(t).DesktopHook().Categories; got != "Graphics;Photography;" {
		t.Errorf("menu categories = %q", got)
	}
}
