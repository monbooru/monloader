package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/ptr"
)

// stubEngine is a ptrEngine that reports a fixed state and records the lifecycle
// calls the page's forms make.
type stubEngine struct {
	status ptr.Status
	calls  []string
}

func (s *stubEngine) Enabled() bool                                     { return s.status.Enabled }
func (s *stubEngine) Status() ptr.Status                                { return s.status }
func (s *stubEngine) TagGraph([]string) (map[string]ptr.TagInfo, error) { return nil, nil }
func (s *stubEngine) Enable() error                                     { s.calls = append(s.calls, "enable"); return nil }
func (s *stubEngine) Pause()                                            { s.calls = append(s.calls, "pause") }
func (s *stubEngine) Resume()                                           { s.calls = append(s.calls, "resume") }
func (s *stubEngine) Retry()                                            { s.calls = append(s.calls, "retry") }
func (s *stubEngine) Delete() error                                     { s.calls = append(s.calls, "delete"); return nil }

// withStubEngine builds a normal web server and swaps in a stub PTR engine.
func withStubEngine(t *testing.T, monbooruURL string, eng *stubEngine) *Server {
	t.Helper()
	srv := newWebServer(t, monbooruURL, "")
	srv.ptr = eng
	return srv
}

func TestPTRScreenDisabledRendersEnable(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := withStubEngine(t, mb.URL, &stubEngine{status: ptr.Status{State: ptr.StateDisabled}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := httpGet(t, ts.URL+"/ptr")
	if !strings.Contains(body, "enable ptr sync") || !strings.Contains(body, "disabled") {
		t.Errorf("disabled ptr page should offer to enable, got %q", truncate(body))
	}
}

// A data path with no directory behind it (a docker run without the volume
// mounted) is flagged so the operator mounts one before enabling.
func TestPTRScreenFlagsMissingDataPath(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := withStubEngine(t, mb.URL, &stubEngine{status: ptr.Status{State: ptr.StateDisabled}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	dir := t.TempDir()
	if err := srv.updateConfig(func(c *config.Config) error { c.PTR.DataPath = filepath.Join(dir, "absent"); return nil }); err != nil {
		t.Fatal(err)
	}
	if body := httpGet(t, ts.URL+"/ptr"); !strings.Contains(body, "does not exist") {
		t.Errorf("missing data path should be flagged, got %q", truncate(body))
	}

	if err := srv.updateConfig(func(c *config.Config) error { c.PTR.DataPath = dir; return nil }); err != nil {
		t.Fatal(err)
	}
	if body := httpGet(t, ts.URL+"/ptr"); strings.Contains(body, "does not exist") {
		t.Errorf("existing data path should not be flagged, got %q", truncate(body))
	}
}

func TestPTRScreenShowsSyncState(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	eng := &stubEngine{status: ptr.Status{
		Enabled: true, State: ptr.StateSyncing,
		Progress: ptr.Progress{UpdateIndex: 5, UpdateCount: 20},
	}}
	srv := withStubEngine(t, mb.URL, eng)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := httpGet(t, ts.URL+"/ptr")
	if !strings.Contains(body, "ptr-state-syncing") || !strings.Contains(body, "ptr-bar-fill") {
		t.Errorf("syncing page should show the progress bar, got %q", truncate(body))
	}
	if !strings.Contains(body, `hx-get="/internal/ptr-status"`) {
		t.Error("syncing page should poll the status fragment")
	}
	if !strings.Contains(body, "width:25%") {
		t.Errorf("progress bar should read 25%% (5/20), got %q", truncate(body))
	}
}

// The delete form is a plain (non-htmx) form, so it needs the full confirm
// markup the submit interceptor reads: the question plus the OK-label and
// destructive markers.
func TestPTRDeleteConfirmMarkup(t *testing.T) {
	_, ts := serveWeb(t, "")

	body := httpGet(t, ts.URL+"/settings")
	for _, want := range []string{
		`action="/settings/ptr/delete" hx-confirm=`, `data-confirm-ok="delete"`, "data-confirm-danger",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings ptr delete form missing %q", want)
		}
	}
}

func TestPTRStatusFragmentReady(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	eng := &stubEngine{status: ptr.Status{
		Enabled: true, State: ptr.StateReady, Counts: ptr.Counts{Mappings: 42},
		CoveredThrough: 1769558400, // 2026-01-28 UTC
	}}
	srv := withStubEngine(t, mb.URL, eng)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := httpGet(t, ts.URL+"/internal/ptr-status")
	if !strings.Contains(body, "ptr-state-ready") || !strings.Contains(body, "caught up") {
		t.Errorf("ready fragment wrong: %q", truncate(body))
	}
	if !strings.Contains(body, "42") {
		t.Error("ready fragment should show the mapping count")
	}
	if !strings.Contains(body, "data through 2026-01-28") {
		t.Error("ready fragment should show how far the synced data reaches")
	}
}

// An index that has not applied an update yet has no coverage to show.
func TestPTRStatusFragmentNoCoverage(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}}
	srv := withStubEngine(t, mb.URL, eng)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if body := httpGet(t, ts.URL+"/internal/ptr-status"); strings.Contains(body, "coverage") {
		t.Errorf("fragment shows a coverage row without a timestamp: %q", truncate(body))
	}
}

func TestPTRLifecycleForms(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateSyncing}}
	srv := withStubEngine(t, mb.URL, eng)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, action := range []string{"pause", "resume", "retry"} {
		resp := postForm(t, ts, srv, "/ptr/"+action, url.Values{})
		resp.Body.Close()
	}
	if strings.Join(eng.calls, ",") != "pause,resume,retry" {
		t.Errorf("lifecycle calls = %v, want pause,resume,retry", eng.calls)
	}
}

func TestPTREnablePersistsAndDeleteClears(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv := withStubEngine(t, mb.URL, eng)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	postForm(t, ts, srv, "/ptr/enable", url.Values{}).Body.Close()
	if !srv.cfg.Current().PTR.Enabled {
		t.Error("a successful enable should persist enabled=true")
	}
	postForm(t, ts, srv, "/settings/ptr/delete", url.Values{}).Body.Close()
	if srv.cfg.Current().PTR.Enabled {
		t.Error("delete should persist enabled=false")
	}
	if strings.Join(eng.calls, ",") != "enable,delete" {
		t.Errorf("calls = %v, want enable,delete", eng.calls)
	}
}

func TestSavePTRSettings(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/ptr", url.Values{
		"data_path":   {" /mnt/ptr "},
		"address":     {"https://ptr.example:45871"},
		"access_key":  {"abc123"},
		"fetch_sleep": {"2.5"},
		"min_free_gb": {"120"},
	}).Body.Close()
	got := srv.cfg.Current().PTR
	if got.DataPath != "/mnt/ptr" || got.Address != "https://ptr.example:45871" ||
		got.AccessKey != "abc123" || got.FetchSleep != 2.5 || got.MinFreeGB != 120 {
		t.Errorf("saved ptr config = %+v", got)
	}

	// Blank secret keeps the stored key; invalid numbers keep the stored values.
	postForm(t, ts, srv, "/settings/ptr", url.Values{
		"data_path":   {"/mnt/ptr"},
		"access_key":  {""},
		"fetch_sleep": {"-1"},
		"min_free_gb": {"abc"},
	}).Body.Close()
	got = srv.cfg.Current().PTR
	if got.AccessKey != "abc123" || got.FetchSleep != 2.5 || got.MinFreeGB != 120 {
		t.Errorf("blank/invalid fields should keep the stored values, got %+v", got)
	}
}

func TestSavePTRRequiresDataPath(t *testing.T) {
	srv, ts := serveWeb(t, "")
	resp := postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {"  "}})
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("blank data path should flash an error, got %q", loc)
	}
	if srv.cfg.Current().PTR.DataPath != "/ptr" {
		t.Errorf("blank data path should keep the default, got %q", srv.cfg.Current().PTR.DataPath)
	}
}

func httpGet(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	return readBody(t, resp)
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
