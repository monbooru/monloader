package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/queue"
)

const fakeGDLVersion = "1.32.1"

func (s *Server) gdlRunning() bool {
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	return s.gdlJob != nil && s.gdlJob.running
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the install to settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func plantManagedInstall(t *testing.T, srv *Server) string {
	t.Helper()
	root := gdl.ManagedRoot(srv.stateDir)
	bin := filepath.Join(root, "venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"python3", "gallery-dl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGDLSettingsSection(t *testing.T) {
	_, ts := serveWeb(t, "")
	status, body := get(t, ts, "/settings")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{`id="advanced"`, "/settings/gallerydl/install", ">bundled<", "1.32.1"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	if strings.Contains(body, "/settings/gallerydl/revert") {
		t.Error("revert form rendered without a managed install")
	}
	if n := strings.Count(body, `id="gdl-stats"`); n != 1 || strings.Contains(body, `id="gdl-stats" hx-swap-oob`) {
		t.Errorf("the settings page renders %d stats blocks, want its own one in place", n)
	}
}

func TestGDLPanelRefreshesTheStats(t *testing.T) {
	srv, ts := serveWeb(t, "")
	srv.gdlJob = &gdlJob{version: "2026.11.12", kind: "ok", msg: "gallery-dl nightly 2026.11.12 installed and active."}

	_, body := get(t, ts, "/internal/gallerydl-status")
	stats := body[strings.Index(body, `id="gdl-stats"`)+1:]
	if !strings.Contains(body, `id="gdl-stats" hx-swap-oob="true"`) || !strings.Contains(stats, fakeGDLVersion) {
		t.Errorf("the finished panel should carry the stats out of band with the probed version, got %s", body)
	}
}

func postFollow(t *testing.T, ts *httptest.Server, srv *Server, path string, vals url.Values) *http.Response {
	t.Helper()
	vals.Set("_csrf", srv.sessions.CSRFToken("anon"))
	resp, err := http.PostForm(ts.URL+path, vals)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGDLInstallRejectsBadVersion(t *testing.T) {
	srv, ts := serveWeb(t, "")
	resp := postFollow(t, ts, srv, "/settings/gallerydl/install", url.Values{"version": {"1.2.3; rm -rf /"}})
	panel := readBody(t, resp)
	if !strings.Contains(panel, "flash-err") || !strings.Contains(panel, "not a release number") {
		t.Fatalf("expected the panel to refuse the version, got %s", panel)
	}
	if srv.gdlJob != nil {
		t.Error("a refused version claimed the install slot")
	}
}

func TestGDLInstallNightly(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		lookup  error
		version string // what reaches the installer; empty when nothing should
		want    string
	}{
		{"the keyword takes the newest build", "nightly", nil, "2026.11.12", "nightly 2026.11.12"},
		{"the keyword ignores case", "Nightly", nil, "2026.11.12", "nightly 2026.11.12"},
		{"a date pins a build", "2026.11.10", nil, "2026.11.10", "nightly 2026.11.10"},
		{"a failed lookup claims nothing", "nightly", errors.New("rate limited"), "", "could not ask GitHub for the latest nightly: rate limited"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, ts := serveWeb(t, "")
			oldLatest, oldInstall := latestNightly, installManaged
			latestNightly = func(context.Context) (string, error) { return "2026.11.12", c.lookup }
			got := make(chan string, 1)
			installManaged = func(_ context.Context, _, version string, _ gdl.InstallProgress) error {
				got <- version
				return errors.New("no network")
			}
			t.Cleanup(func() { latestNightly, installManaged = oldLatest, oldInstall })

			resp := postFollow(t, ts, srv, "/settings/gallerydl/install", url.Values{"version": {c.input}})
			if panel := readBody(t, resp); !strings.Contains(panel, c.want) {
				t.Errorf("panel missing %q: %s", c.want, panel)
			}
			if c.version == "" {
				if srv.gdlJob != nil {
					t.Error("a failed lookup claimed the install slot")
				}
				return
			}
			if v := <-got; v != c.version {
				t.Errorf("installer got %q, want %q", v, c.version)
			}
			waitFor(t, func() bool { return !srv.gdlRunning() })
		})
	}
}

func TestGDLInstallProgressPanel(t *testing.T) {
	srv, ts := serveWeb(t, "")
	srv.gdlJob = &gdlJob{version: "1.32.9", step: "downloading gallery-dl 1.32.9", percent: 44, running: true}

	status, body := get(t, ts, "/internal/gallerydl-status")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{"Installing", "1.32.9", "width:44%", "downloading gallery-dl", "/internal/gallerydl-status"} {
		if !strings.Contains(body, want) {
			t.Errorf("progress panel missing %q", want)
		}
	}
	if strings.Contains(body, "/settings/gallerydl/install") {
		t.Error("install form rendered during an install")
	}

	srv.gdlDone("ok", "gallery-dl 1.32.9 installed and active")
	_, body = get(t, ts, "/internal/gallerydl-status")
	if !strings.Contains(body, "installed and active") || strings.Contains(body, "hx-trigger") {
		t.Errorf("expected a final panel carrying the outcome, got %s", body)
	}
	if _, body = get(t, ts, "/internal/gallerydl-status"); strings.Contains(body, "installed and active") {
		t.Error("the outcome message survived the panel that showed it")
	}
}

func TestGDLRevertRefusedDuringInstall(t *testing.T) {
	srv, ts := serveWeb(t, "")
	plantManagedInstall(t, srv)
	srv.catalog = gdl.NewCatalog("1.32.9", "1.32.1", true, nil, nil)
	srv.gdlJob = &gdlJob{version: "1.32.9", running: true}

	resp := postFollow(t, ts, srv, "/settings/gallerydl/revert", url.Values{})
	defer resp.Body.Close()
	if loc := resp.Request.URL.String(); !strings.Contains(loc, "kind=err") || !strings.Contains(loc, "install+or+revert+is+running") {
		t.Fatalf("expected the revert refused while installing, landed on %s", loc)
	}
}

func TestGDLInstallRefusedDuringRevert(t *testing.T) {
	srv, ts := serveWeb(t, "")

	done, reason := srv.gdlSwitchGuard()
	if reason != "" {
		t.Fatalf("the guard refused on an idle server: %s", reason)
	}
	defer done()

	resp := postFollow(t, ts, srv, "/settings/gallerydl/install", url.Values{"version": {"1.30.0"}})
	if panel := readBody(t, resp); !strings.Contains(panel, "flash-err") || !strings.Contains(panel, "install or revert is running") {
		t.Errorf("expected the install refused during a revert, got %s", panel)
	}
	if srv.gdlRunning() {
		t.Error("an install started under a revert")
	}
}

func TestGDLPanelReadsTheStateDirectory(t *testing.T) {
	srv, ts := serveWeb(t, "")
	configRoot := gdl.ManagedRoot(filepath.Dir(srv.configPath))
	if err := os.MkdirAll(filepath.Join(configRoot, "venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, ts, "/settings")
	if strings.Contains(body, "/settings/gallerydl/revert") {
		t.Error("an install beside the config is not one the runner uses; the panel must not offer to revert it")
	}

	plantManagedInstall(t, srv)
	if _, body = get(t, ts, "/settings"); !strings.Contains(body, "/settings/gallerydl/revert") {
		t.Error("an install under the state dir is the one that runs; the panel must offer to revert it")
	}
}

func TestGDLPanelInstallShadowedByBinaryPath(t *testing.T) {
	srv, ts := serveWeb(t, "")
	plantManagedInstall(t, srv)
	mustUpdate(t, srv, func(c *config.Config) {
		c.GalleryDL.BinaryPath = "/opt/custom/gallery-dl"
	})

	_, body := get(t, ts, "/settings")
	if !strings.Contains(body, "/settings/gallerydl/revert") {
		t.Error("no revert control for an install that is on disk but not in use")
	}
	if !strings.Contains(body, "binary_path names another one") {
		t.Errorf("panel does not say the configured binary outranks the install: %s", body)
	}
}

func TestGDLPanelReprobesAnInstallThatStoppedRunning(t *testing.T) {
	srv, ts := serveWeb(t, "")
	root := plantManagedInstall(t, srv)

	srv.catalog = gdl.NewCatalog("1.30.0", fakeGDLVersion, true, nil, nil)
	if err := os.Remove(filepath.Join(root, "venv", "bin", "python3")); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, ts, "/settings")
	if !strings.Contains(body, "no longer runs") {
		t.Errorf("panel does not report an install that cannot start: %s", body)
	}
	if !strings.Contains(body, "/settings/gallerydl/revert") {
		t.Error("a broken install must still be removable from the panel")
	}
	if srv.catalog.Managed() || srv.catalog.Version() != fakeGDLVersion {
		t.Errorf("catalog = %q managed=%v, want a re-probe naming the binary that answers now",
			srv.catalog.Version(), srv.catalog.Managed())
	}
}

func TestGDLPanelDoesNotReprobeUnderARevert(t *testing.T) {
	srv, ts := serveWeb(t, "")
	root := plantManagedInstall(t, srv)
	srv.catalog = gdl.NewCatalog("1.30.0", fakeGDLVersion, true, nil, nil)

	if err := os.Remove(filepath.Join(root, "venv", "bin", "python3")); err != nil {
		t.Fatal(err)
	}
	done, reason := srv.gdlSwitchGuard()
	if reason != "" {
		t.Fatalf("the guard refused on an idle server: %s", reason)
	}
	defer done()

	get(t, ts, "/settings")
	if !srv.catalog.Managed() || srv.catalog.Version() != "1.30.0" {
		t.Errorf("catalog = %q managed=%v, want it left for the revert to re-probe",
			srv.catalog.Version(), srv.catalog.Managed())
	}
}

func TestGDLRevertWithoutManagedInstall(t *testing.T) {
	srv, ts := serveWeb(t, "")
	resp := postFollow(t, ts, srv, "/settings/gallerydl/revert", url.Values{})
	defer resp.Body.Close()
	if loc := resp.Request.URL.String(); !strings.Contains(loc, "kind=err") {
		t.Fatalf("expected an error flash, landed on %s", loc)
	}
}

func TestGDLInstallResumesTheQueueBeforeItReadsAsFinished(t *testing.T) {
	srv, _ := serveWeb(t, "")
	old := installManaged
	installManaged = func(context.Context, string, string, gdl.InstallProgress) error {
		return errors.New("no network")
	}
	t.Cleanup(func() { installManaged = old })

	if !srv.gdlClaim("1.30.0") {
		t.Fatal("the claim failed on an idle server")
	}
	var runningAtResume bool
	srv.runGDLInstall("1.30.0", func() { runningAtResume = srv.gdlRunning() })
	if !runningAtResume {
		t.Error("the queue was resumed after the job read as finished, so a second switch could start under it")
	}
}

func TestGDLInstallHoldsTheQueueAndSwapsTheCatalog(t *testing.T) {
	srv, ts := serveWeb(t, "")

	installed := make(chan struct{})
	pausedDuring := make(chan bool, 1)
	old := installManaged
	installManaged = func(_ context.Context, stateDir, version string, progress gdl.InstallProgress) error {
		pausedDuring <- srv.queue.Held()
		progress("downloading gallery-dl "+version, 20)
		bin := filepath.Join(gdl.ManagedRoot(stateDir), "venv", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return err
		}
		for _, name := range []string{"python3", "gallery-dl"} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
				return err
			}
		}
		close(installed)
		return nil
	}
	t.Cleanup(func() { installManaged = old })

	resp := postFollow(t, ts, srv, "/settings/gallerydl/install", url.Values{"version": {"1.30.0"}})
	panel := readBody(t, resp)
	if !strings.Contains(panel, "Installing") || !strings.Contains(panel, "1.30.0") {
		t.Errorf("the starting request should answer the progress panel, got %s", panel)
	}

	<-installed
	if !<-pausedDuring {
		t.Error("the queue must be held for the switch, so no job resolves with one binary and downloads with another")
	}
	waitFor(t, func() bool { return !srv.gdlRunning() })

	if !srv.catalog.Managed() || srv.catalog.Version() != fakeGDLVersion {
		t.Errorf("catalog = %q managed=%v, want the installed binary probed", srv.catalog.Version(), srv.catalog.Managed())
	}
	if srv.queue.Held() {
		t.Error("the queue should be released once the switch is done")
	}
	_, body := get(t, ts, "/internal/gallerydl-status")
	if !strings.Contains(body, "installed and active") {
		t.Errorf("panel should report the outcome once, got %s", body)
	}
}

func TestGDLInstallKeepsAnOperatorPause(t *testing.T) {
	srv, ts := serveWeb(t, "")
	srv.queue.Pause()

	old := installManaged
	installManaged = func(context.Context, string, string, gdl.InstallProgress) error { return nil }
	t.Cleanup(func() { installManaged = old })

	resp := postFollow(t, ts, srv, "/settings/gallerydl/install", url.Values{"version": {"1.30.0"}})
	resp.Body.Close()
	waitFor(t, func() bool { return !srv.gdlRunning() })
	if !srv.queue.Paused() {
		t.Error("an operator's own pause must survive the switch")
	}
}

func TestTheTopbarCannotUndoTheSwitchesHold(t *testing.T) {
	srv, ts := serveWeb(t, "")

	mid := make(chan struct{})
	release := make(chan struct{})
	old := installManaged
	installManaged = func(context.Context, string, string, gdl.InstallProgress) error {
		close(mid)
		<-release
		return nil
	}
	t.Cleanup(func() { installManaged = old })

	resp := postFollow(t, ts, srv, "/settings/gallerydl/install", url.Values{"version": {"1.30.0"}})
	resp.Body.Close()
	<-mid

	postForm(t, ts, srv, "/queue/resume", url.Values{}).Body.Close()
	if !srv.queue.Held() {
		t.Error("the topbar's resume released the switch's hold")
	}
	id := srv.queue.Enqueue("http://x/1", queue.Options{})
	time.Sleep(50 * time.Millisecond)
	if j, _ := srv.queue.Get(id); j.Status != queue.JobQueued {
		t.Errorf("a job started mid-switch: %s", j.Status)
	}

	postForm(t, ts, srv, "/queue/pause", url.Values{}).Body.Close()
	close(release)
	waitFor(t, func() bool { return !srv.gdlRunning() })
	if !srv.queue.Paused() {
		t.Error("a pause made during the switch must survive it")
	}
	if srv.queue.Held() {
		t.Error("the switch's hold should be gone")
	}
}

func TestGDLInstallFailureReprobes(t *testing.T) {
	srv, ts := serveWeb(t, "")
	srv.catalog = gdl.NewCatalog("9.9.9", "9.9.9", true, nil, nil)

	old := installManaged
	installManaged = func(context.Context, string, string, gdl.InstallProgress) error {
		return errors.New("no matching distribution")
	}
	t.Cleanup(func() { installManaged = old })

	resp := postFollow(t, ts, srv, "/settings/gallerydl/install", url.Values{"version": {"9.9.9"}})
	resp.Body.Close()
	waitFor(t, func() bool { return !srv.gdlRunning() })

	if srv.catalog.Managed() || srv.catalog.Version() != fakeGDLVersion {
		t.Errorf("catalog = %q managed=%v, want a re-probe of the surviving binary", srv.catalog.Version(), srv.catalog.Managed())
	}
	_, body := get(t, ts, "/internal/gallerydl-status")
	if !strings.Contains(body, "Install failed") || !strings.Contains(body, "no matching distribution") {
		t.Errorf("panel should carry the failure, got %s", body)
	}
	if srv.queue.Held() {
		t.Error("the queue should be released after a failed switch too")
	}
}
