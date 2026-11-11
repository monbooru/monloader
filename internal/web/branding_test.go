package web

import (
	"bytes"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/shared/webui"
)

func installTheme(t *testing.T, srv *Server, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(srv.themesDir(), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThemeRoutesFollowTheActiveTheme(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	for _, route := range []string{"/theme.css", "/theme.logo", "/theme.favicon"} {
		if code, _ := get(t, ts, route); code != 404 {
			t.Errorf("no theme: %s = %d, want 404", route, code)
		}
	}
	installTheme(t, srv, map[string]string{
		"aurora/theme.css":   ":root{--accent:#abc}",
		"aurora/logo.png":    "LOGOBYTES",
		"aurora/favicon.png": "ICONBYTES",
	})
	srv.cfg.Current().Server.Theme = "aurora"
	for route, want := range map[string]string{"/theme.css": "--accent:#abc", "/theme.logo": "LOGOBYTES", "/theme.favicon": "ICONBYTES"} {
		if code, body := get(t, ts, route); code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s = %d %q", route, code, body)
		}
	}
	_, landing := get(t, ts, "/")
	for _, want := range []string{`<link rel="stylesheet" href="/theme.css">`, `src="/theme.logo"`, `href="/theme.favicon?v=aurora-`} {
		if !strings.Contains(landing, want) {
			t.Errorf("themed landing page missing %q", want)
		}
	}

	srv.cfg.Current().Server.Theme = "../aurora/theme.css"
	if code, _ := get(t, ts, "/theme.css"); code != 404 {
		t.Errorf("a theme name that is a path served %d, want the shipped look", code)
	}
}

func TestThemePickerListsAndSwitches(t *testing.T) {
	srv := serverWith(t, noopProc{})
	installTheme(t, srv, map[string]string{"midnight.css": ":root{--bg:#000}"})
	ts := newTestServer(t, srv)
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, `<span class="theme-cluster-active">[dark]</span>`) || !strings.Contains(body, `value="midnight"`) {
		t.Errorf("settings should offer midnight beside the active dark theme")
	}
	resp := postClosed(t, ts, srv, "/settings/theme", url.Values{"theme": {"midnight"}})
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Refresh") != "true" || srv.cfg.Current().Server.Theme != "midnight" {
		t.Errorf("switch = %d, refresh %q, theme %q", resp.StatusCode, resp.Header.Get("HX-Refresh"), srv.cfg.Current().Server.Theme)
	}
	resp = postForm(t, ts, srv, "/settings/theme", url.Values{"theme": {"gone"}})
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Retarget") != "#theme-flash" ||
		!strings.Contains(body, `class="flash-err">No theme named gone.`) || srv.cfg.Current().Server.Theme != "midnight" {
		t.Errorf("an unknown theme = %d, retarget %q, body %q, theme %q",
			resp.StatusCode, resp.Header.Get("HX-Retarget"), body, srv.cfg.Current().Server.Theme)
	}
}

func TestThemeSaveFailureShowsItsFlash(t *testing.T) {
	srv := serverWith(t, noopProc{})
	installTheme(t, srv, map[string]string{"midnight.css": ":root{--bg:#000}"})
	ts := newTestServer(t, srv)
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, `id="theme-flash"`) {
		t.Fatal("the Themes section has no slot for a refusal")
	}
	if err := os.WriteFile(srv.configPath, []byte("not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := postForm(t, ts, srv, "/settings/theme", url.Values{"theme": {"midnight"}})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Retarget") != "#theme-flash" || !strings.Contains(body, `class="flash-err">Could not save:`) {
		t.Errorf("a failed save = %d, retarget %q, body %q", resp.StatusCode, resp.Header.Get("HX-Retarget"), body)
	}
	if resp.Header.Get("HX-Refresh") != "" || srv.cfg.Current().Server.Theme != "" {
		t.Errorf("a failed save refreshed (%q) or switched the theme to %q", resp.Header.Get("HX-Refresh"), srv.cfg.Current().Server.Theme)
	}
}

func TestListThemesLogsARepeatedNoteOnce(t *testing.T) {
	srv := serverWith(t, noopProc{})
	installTheme(t, srv, map[string]string{"twin/theme.css": ":root{}", "twin.css": ":root{}"})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for range 3 {
		srv.listThemes()
	}
	if n := strings.Count(buf.String(), "WARN"); n != 1 {
		t.Errorf("three listings logged %d warning(s), want one:\n%s", n, buf.String())
	}
}

func TestBuiltinLightThemeIsSeededAndServed(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	seeded := filepath.Join(srv.themesDir(), "light")
	for _, file := range []string{"theme.css", webui.SeedFile} {
		if _, err := os.Stat(filepath.Join(seeded, file)); err != nil {
			t.Errorf("seeded light copy: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(seeded, "logo.png")); err == nil {
		t.Error("the seeded copy carries a logo, which the app brings itself")
	}
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, `<span class="theme-cluster-active">[dark]</span>`) || !strings.Contains(body, `value="light"`) {
		t.Errorf("settings should offer light beside the active dark theme")
	}

	if err := os.RemoveAll(seeded); err != nil {
		t.Fatal(err)
	}
	srv.seedThemesDir()
	if _, err := os.Stat(seeded); err == nil {
		t.Errorf("a deleted light copy was rebuilt")
	}
	srv.cfg.Current().Server.Theme = "light"
	if code, css := get(t, ts, "/theme.css"); code != 200 || !strings.Contains(css, `:root[data-app="monloader"]`) {
		t.Errorf("built-in light: /theme.css = %d, want the sheet with the monloader block", code)
	}
	_, landing := get(t, ts, "/")
	for _, want := range []string{`href="/static/light/favicon.png"`, `src="/static/light/mascot-sparkle.png"`} {
		if !strings.Contains(landing, want) {
			t.Errorf("light landing page missing %q", want)
		}
	}
}

func TestLegacyThemesKeepTheLookBeforeTheMascot(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	for _, name := range []string{"legacy-dark", "legacy-light"} {
		srv.cfg.Current().Server.Theme = name
		for _, route := range []string{"/theme.css", "/theme.logo", "/theme.favicon"} {
			if code, _ := get(t, ts, route); code != 200 {
				t.Errorf("%s: %s = %d, want 200", name, route, code)
			}
		}
		_, landing := get(t, ts, "/")
		if !strings.Contains(landing, `class="landing-logo"`) || strings.Contains(landing, `class="mascot"`) {
			t.Errorf("%s landing should show the logo and no drawing", name)
		}
		if _, nf := get(t, ts, "/no-such-page"); strings.Contains(nf, `class="mascot"`) {
			t.Errorf("%s 404 shows a drawing", name)
		}
	}
}

func TestLandingFaceFollowsTheMonbooruLink(t *testing.T) {
	for conn, want := range map[string]string{
		webui.ConnOK: "sparkle", connChecking: "sparkle",
		webui.ConnUnconfigured: "sleep", connUnpaired: "sleep", webui.ConnPaused: "sleep",
		webui.ConnDown: "panic", webui.ConnRejected: "panic",
	} {
		if got := landingFace(conn); got != want {
			t.Errorf("landingFace(%q) = %q, want %q", conn, got, want)
		}
	}
}

func TestThemeMascotServesOnlyTheFoldersOwnDrawings(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	installTheme(t, srv, map[string]string{"aurora/theme.css": ":root{}", "aurora/mascot-sleep.png": "sleep-bytes"})
	srv.cfg.Current().Server.Theme = "aurora"
	if code, body := get(t, ts, "/theme.mascot/sleep"); code != 200 || body != "sleep-bytes" {
		t.Errorf("/theme.mascot/sleep = %d %q", code, body)
	}
	if code, _ := get(t, ts, "/theme.mascot/lost"); code != 404 {
		t.Errorf("/theme.mascot/lost = %d, want 404", code)
	}
	if _, queue := get(t, ts, "/queue"); !strings.Contains(queue, `src="/theme.mascot/sleep"`) {
		t.Error("the empty queue should show the theme's sleep drawing")
	}
}

func TestBrandingDefaults(t *testing.T) {
	_, ts := serveProc(t, noopProc{})
	_, landing := get(t, ts, "/")
	for _, want := range []string{`href="/static/favicon.png"`, `src="/static/mascot-sparkle.png"`, `<h1>mon<span class="wordmark-accent">loader</span></h1>`, `<title>Monloader</title>`} {
		if !strings.Contains(landing, want) {
			t.Errorf("default landing page missing %q", want)
		}
	}
	_, queue := get(t, ts, "/queue")
	for _, want := range []string{`src="/static/logo.png?v=` + assetVersion + `"`, `<span>mon<span class="wordmark-accent">loader</span></span>`, `src="/static/mascot-sleep.png"`, `<title>Queue - Monloader</title>`} {
		if !strings.Contains(queue, want) {
			t.Errorf("default queue page missing %q", want)
		}
	}
	if strings.Contains(landing, "/theme.") || strings.Contains(queue, "/theme.") {
		t.Error("default pages must not reference a theme file")
	}
}

func TestBrandingOverride(t *testing.T) {
	srv := serverWith(t, noopProc{})
	srv.cfg.Current().Server.BooruName = "Privloader"
	ts := newTestServer(t, srv)
	_, landing := get(t, ts, "/")
	for _, want := range []string{`<h1>Privloader</h1>`, `<title>Privloader</title>`} {
		if !strings.Contains(landing, want) {
			t.Errorf("branded landing page missing %q", want)
		}
	}
	_, queue := get(t, ts, "/queue")
	for _, want := range []string{`<span>Privloader</span>`, `<title>Queue - Privloader</title>`} {
		if !strings.Contains(queue, want) {
			t.Errorf("branded queue page missing %q", want)
		}
	}

	authed := stubbedServer(t, "secret")
	authed.cfg.Current().Server.BooruName = "Privloader"
	lts := newTestServer(t, authed)
	_, login := get(t, lts, "/login")
	if !strings.Contains(login, "<h1>Privloader</h1>") {
		t.Errorf("branded login heading missing, got %q", login)
	}
}

func TestMonbooruLink(t *testing.T) {
	srv := serverWith(t, noopProc{})
	pairMB(t, srv)
	srv.cfg.Current().Monbooru.WebURL = "http://booru.example.com/"
	ts := newTestServer(t, srv)

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

func TestTheHeadDeclaresTheActiveThemesColorScheme(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	installTheme(t, srv, map[string]string{"plain.css": ":root{--bg:#222}"})
	for theme, want := range map[string]string{"": "dark", "light": "light", "legacy-light": "light", "legacy-dark": "dark", "plain": "dark"} {
		srv.cfg.Current().Server.Theme = theme
		if _, queue := get(t, ts, "/queue"); !strings.Contains(queue, `<meta name="color-scheme" content="`+want+`">`) {
			t.Errorf("%q: the head does not declare %s", theme, want)
		}
	}
}
