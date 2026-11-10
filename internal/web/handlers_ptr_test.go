package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/ptr"
)

type stubEngine struct {
	status    ptr.Status
	calls     []string
	hasKey    bool
	accessKey string
	account   *ptr.Account
	createErr error
	contrib   *ptr.ContribStore
	rawKnown  map[string]bool
	dataPath  string
	address   string

	minFreeGB  int
	fetchSleep float64

	statusCalls atomic.Int64
}

func (s *stubEngine) Enabled() bool { return s.status.Enabled }
func (s *stubEngine) Status() ptr.Status {
	s.statusCalls.Add(1)
	return s.status
}
func (s *stubEngine) DataPath() string { return s.dataPath }
func (s *stubEngine) TagGraph(context.Context, []string) (map[string]ptr.TagInfo, error) {
	return nil, nil
}
func (s *stubEngine) SearchTags(context.Context, []ptr.TagRange, int) ([]ptr.TagMatch, bool, error) {
	return nil, false, nil
}
func (s *stubEngine) TagsForHash(string) ([]string, bool, error) { return nil, false, nil }
func (s *stubEngine) Enable() error {
	s.calls = append(s.calls, "enable")
	s.status.Enabled = true
	return nil
}
func (s *stubEngine) Disable() {
	s.calls = append(s.calls, "disable")
	s.status.Enabled = false
}
func (s *stubEngine) Pause()                  { s.calls = append(s.calls, "pause") }
func (s *stubEngine) Resume()                 { s.calls = append(s.calls, "resume") }
func (s *stubEngine) Retry()                  { s.calls = append(s.calls, "retry") }
func (s *stubEngine) Delete() error           { s.calls = append(s.calls, "delete"); return nil }
func (s *stubEngine) HasPersonalKey() bool    { return s.hasKey }
func (s *stubEngine) AccessKey() string       { return s.accessKey }
func (s *stubEngine) SetAccessKey(key string) { s.accessKey = key; s.hasKey = key != "" }
func (s *stubEngine) SetDataPath(path string) bool {
	if s.status.Enabled {
		return s.dataPath == path
	}
	s.dataPath = path
	return true
}
func (s *stubEngine) Address() string { return s.address }
func (s *stubEngine) SetAddress(addr string) bool {
	if s.status.Enabled {
		return s.address == addr
	}
	s.address = addr
	return true
}
func (s *stubEngine) SetMinFreeGB(gb int)           { s.minFreeGB = gb }
func (s *stubEngine) SetFetchSleep(seconds float64) { s.fetchSleep = seconds }
func (s *stubEngine) CreateContribAccount(context.Context) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	s.accessKey, s.hasKey = "cafe1234", true
	return "cafe1234", nil
}
func (s *stubEngine) ContribAccount(context.Context) (*ptr.Account, error) {
	if s.account == nil {
		return nil, errors.New("no account state")
	}
	return s.account, nil
}
func (s *stubEngine) RefreshAccount(ctx context.Context) (*ptr.Account, error) {
	return s.ContribAccount(ctx)
}
func (s *stubEngine) Provisional() bool                              { return s.status.State != ptr.StateReady }
func (s *stubEngine) CaughtUp() bool                                 { return s.status.State == ptr.StateReady }
func (s *stubEngine) Contrib() *ptr.ContribStore                     { return s.contrib }
func (s *stubEngine) TagFilterCached() *ptr.TagFilter                { return nil }
func (s *stubEngine) HashHasIdeal(string, string) (bool, error)      { return false, nil }
func (s *stubEngine) HashHasRaw(h, tag string) (bool, error)         { return s.rawKnown[h+"|"+tag], nil }
func (s *stubEngine) RawTagsForHash(string) ([]string, error)        { return nil, nil }
func (s *stubEngine) IdealTag(string) (string, bool, error)          { return "", false, nil }
func (s *stubEngine) SiblingCurrent(string, string) (bool, error)    { return false, nil }
func (s *stubEngine) ParentCurrent(string, string) (bool, error)     { return false, nil }
func (s *stubEngine) ParentEdgeCovered(string, string) (bool, error) { return false, nil }

func (s *stubEngine) HashHasIdeals(string, []string) (map[string]bool, error) {
	return nil, nil
}

func TestPTRScreenDisabledRendersEnable(t *testing.T) {
	_, ts := serveEngine(t, &stubEngine{status: ptr.Status{State: ptr.StateDisabled}, dataPath: t.TempDir()})

	_, body := get(t, ts, "/ptr")
	if !strings.Contains(body, "Enable PTR sync") || !strings.Contains(body, "Disabled") {
		t.Errorf("disabled ptr page should offer to enable, got %q", truncate(body))
	}
	if !strings.Contains(body, "guides/ptr.html#starting-from-a-snapshot") {
		t.Errorf("disabled ptr page should link the snapshot guide, got %q", truncate(body))
	}
}

func TestTheQueuePollLeavesTheEngineAlone(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	_, ts := serveEngine(t, eng)
	get(t, ts, "/internal/queue-rows")
	if n := eng.statusCalls.Load(); n != 0 {
		t.Errorf("the rows poll read the engine's status %d times", n)
	}
}

func TestPTRScreenDisablesEnableBelowTheFloor(t *testing.T) {
	dir := t.TempDir()
	srv, ts := serveEngine(t, &stubEngine{status: ptr.Status{State: ptr.StateDisabled}, dataPath: dir})
	mustUpdate(t, srv, func(c *config.Config) { c.PTR.DataPath, c.PTR.MinFreeGB = dir, 1<<20 })

	_, body := get(t, ts, "/ptr")
	if !strings.Contains(body, `title="Needs 1048576 GiB free to start" disabled>Enable PTR sync</button>`) {
		t.Errorf("Enable should be off with its reason below the floor, got %q", truncate(body))
	}
	if !strings.Contains(body, "(needs 1048576 GiB to start)") {
		t.Errorf("the floor should read in GiB like the free space, got %q", truncate(body))
	}
}

func TestPTRScreenDisabledWithIndexReadsAsResume(t *testing.T) {
	dir := t.TempDir()
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}, dataPath: dir}
	_, ts := serveEngine(t, eng)

	store, err := ptr.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	_, body := get(t, ts, "/ptr")
	if !strings.Contains(body, "Resume PTR sync") || !strings.Contains(body, "still on disk") {
		t.Errorf("a kept index should read as a resume, got %q", truncate(body))
	}
	if strings.Contains(body, "full tag history") || strings.Contains(body, "GiB to start") {
		t.Errorf("a kept index should not repeat the first-sync cost, got %q", truncate(body))
	}
}

func TestPTRScreenFlagsMissingDataPath(t *testing.T) {
	dir := t.TempDir()
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}, dataPath: filepath.Join(dir, "absent")}
	_, ts := serveEngine(t, eng)

	if _, body := get(t, ts, "/ptr"); !strings.Contains(body, "does not exist") {
		t.Errorf("missing data path should be flagged, got %q", truncate(body))
	}

	if _, body := get(t, ts, "/ptr"); strings.Contains(body, "Enable PTR sync") {
		t.Errorf("missing data path should not offer enable, got %q", truncate(body))
	}

	eng.dataPath = dir
	_, body := get(t, ts, "/ptr")
	if strings.Contains(body, "does not exist") {
		t.Errorf("existing data path should not be flagged, got %q", truncate(body))
	}
	if !strings.Contains(body, "Enable PTR sync") {
		t.Errorf("existing data path should offer enable, got %q", truncate(body))
	}
}

func TestPTRScreenShowsSyncState(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{
		Enabled: true, State: ptr.StateSyncing,
		Progress: ptr.Progress{UpdateIndex: 5, UpdateCount: 20, BlobsDone: 1, BlobsTotal: 10, DownloadRate: 2 << 20, ProcessRate: 41200, LastAppliedAt: time.Now().Add(-5 * time.Minute).Unix()},
	}}
	_, ts := serveEngine(t, eng)

	_, body := get(t, ts, "/ptr")
	if !strings.Contains(body, "ptr-state-syncing") || !strings.Contains(body, "ptr-bar-fill") {
		t.Errorf("syncing page should show the progress bar, got %q", truncate(body))
	}
	if !strings.Contains(body, `hx-get="/internal/ptr-status"`) {
		t.Error("syncing page should poll the status fragment")
	}

	if !strings.Contains(body, "width:10%") {
		t.Errorf("progress bar should read 10%% (1/10 blobs), got %q", truncate(body))
	}
	if !strings.Contains(body, "2.0 MiB/s") || !strings.Contains(body, "41200 rows/s") {
		t.Errorf("syncing page should show the network and row rates, got %q", truncate(body))
	}
	if !strings.Contains(body, "Update 5 of 20") || !strings.Contains(body, " downloaded") {
		t.Errorf("syncing page should show the update position and the session byte counter, got %q", truncate(body))
	}
	if !strings.Contains(body, "last applied 5m ago") {
		t.Errorf("syncing page should show when the last update landed, got %q", truncate(body))
	}
	if !strings.Contains(body, "guides/ptr.html#starting-from-a-snapshot") {
		t.Errorf("syncing page should link the snapshot guide, got %q", truncate(body))
	}
}

func TestPTRSyncLineOmitsUnknownTotal(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{
		Enabled: true, State: ptr.StateSyncing,
		Progress: ptr.Progress{UpdateIndex: 5},
	}}
	_, ts := serveEngine(t, eng)

	_, body := get(t, ts, "/internal/ptr-status")
	if !strings.Contains(body, "Update 5") || strings.Contains(body, "Update 5 of") {
		t.Errorf("sync line should render the position without a zero total, got %q", truncate(body))
	}
}

func TestPTRBarFallsBackToUpdateFraction(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{
		Enabled: true, State: ptr.StateSyncing,
		Progress: ptr.Progress{UpdateIndex: 5, UpdateCount: 20},
	}}
	_, ts := serveEngine(t, eng)

	if _, body := get(t, ts, "/internal/ptr-status"); !strings.Contains(body, "width:25%") {
		t.Errorf("bar should fall back to 25%% (5/20), got %q", truncate(body))
	}
}

func TestPTRDeleteConfirmMarkup(t *testing.T) {
	_, ts := serveWeb(t, "")

	_, body := get(t, ts, "/settings")
	for _, want := range []string{
		`action="/settings/ptr/delete" hx-confirm=`, `data-confirm-ok="Delete"`, "data-confirm-danger",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings ptr delete form missing %q", want)
		}
	}
}

func TestPTRStatusFragmentReady(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{
		Enabled: true, State: ptr.StateReady, Counts: ptr.Counts{Mappings: 42},
		CoveredThrough: 1769558400,
	}}
	_, ts := serveEngine(t, eng)

	_, body := get(t, ts, "/internal/ptr-status")
	if !strings.Contains(body, "ptr-state-ready") || !strings.Contains(body, "Up to date") {
		t.Errorf("ready fragment wrong: %q", truncate(body))
	}
	if !strings.Contains(body, "42") {
		t.Error("ready fragment should show the mapping count")
	}
	if !strings.Contains(body, "data through 2026-01-28") {
		t.Error("ready fragment should show how far the synced data reaches")
	}
}

func TestPTRStatusFragmentNoCoverage(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}}
	_, ts := serveEngine(t, eng)

	if _, body := get(t, ts, "/internal/ptr-status"); strings.Contains(body, "coverage") {
		t.Errorf("fragment shows a coverage row without a timestamp: %q", truncate(body))
	}
}

func TestPTRLifecycleForms(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateSyncing}}
	srv, ts := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) { c.PTR.Enabled = true })

	for _, action := range []string{"pause", "resume", "retry"} {
		postClosed(t, ts, srv, "/ptr/"+action, url.Values{})
	}
	if strings.Join(eng.calls, ",") != "pause,resume,retry" {
		t.Errorf("lifecycle calls = %v, want pause,resume,retry", eng.calls)
	}
}

func TestPTRPauseButtonOutsideSyncing(t *testing.T) {
	for _, state := range []ptr.State{ptr.StateReady, ptr.StateError} {
		eng := &stubEngine{status: ptr.Status{Enabled: true, State: state}}
		_, ts := serveEngine(t, eng)
		if _, body := get(t, ts, "/ptr"); !strings.Contains(body, `action="/ptr/pause"`) {
			t.Errorf("%s page should offer pause, got %q", state, truncate(body))
		}
	}
}

func TestPTRPausePersists(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}}
	srv, ts := serveEngine(t, eng)

	postForm(t, ts, srv, "/ptr/pause", url.Values{}).Body.Close()
	if !srv.cfg.Current().PTR.Paused {
		t.Error("pause should persist paused=true")
	}
	postForm(t, ts, srv, "/ptr/resume", url.Values{}).Body.Close()
	if srv.cfg.Current().PTR.Paused {
		t.Error("resume should persist paused=false")
	}
}

func TestPTREnableClearsAHeldPause(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, ts := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) { c.PTR.Paused = true })

	postForm(t, ts, srv, "/ptr/enable", url.Values{}).Body.Close()
	if cfg := srv.cfg.Current().PTR; !cfg.Enabled || cfg.Paused {
		t.Errorf("enable should start the sync unpaused, got enabled=%v paused=%v", cfg.Enabled, cfg.Paused)
	}
	if strings.Join(eng.calls, ",") != "resume,enable" {
		t.Errorf("calls = %v, want resume,enable", eng.calls)
	}
}

func TestPTREnablePersistsAndDeleteClears(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, ts := serveEngine(t, eng)

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

func TestSavePTRToggleStartsAndStopsSync(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, ts := serveEngine(t, eng)
	dataPath := t.TempDir()

	postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {dataPath}, "enabled": {"1"}}).Body.Close()
	if !srv.cfg.Current().PTR.Enabled || !eng.Enabled() {
		t.Errorf("checked box should start the sync and persist it, got cfg=%v engine=%v",
			srv.cfg.Current().PTR.Enabled, eng.Enabled())
	}
	if _, settings := get(t, ts, "/settings"); !strings.Contains(settings, `name="enabled" value="1" checked`) {
		t.Errorf("settings should show the box checked, got %q", truncate(settings))
	}

	postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {dataPath}}).Body.Close()
	if srv.cfg.Current().PTR.Enabled || eng.Enabled() {
		t.Error("unchecked box should stop the sync and persist it off")
	}
	if strings.Join(eng.calls, ",") != "enable,disable" {
		t.Errorf("calls = %v, want enable,disable (never delete)", eng.calls)
	}
}

func TestSavePTRStartsTheSyncOnThePathTheSameSubmitSet(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, ts := serveEngine(t, eng)

	dir := t.TempDir()
	postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {dir}, "enabled": {"1"}}).Body.Close()
	if eng.DataPath() != dir {
		t.Errorf("engine data path = %q, want the path the submit saved (%q)", eng.DataPath(), dir)
	}
	if !eng.Enabled() {
		t.Error("the sync should have started on the path the same submit set")
	}
}

func TestSavePTRStartsTheSyncFromTheAddressTheSameSubmitSet(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, ts := serveEngine(t, eng)

	postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {t.TempDir()}, "address": {"https://repo.example:45871"}, "enabled": {"1"}}).Body.Close()
	if eng.address != "https://repo.example:45871" || !eng.Enabled() {
		t.Errorf("engine address = %q, enabled %v; want the sync started on the address the submit set", eng.address, eng.Enabled())
	}
}

func TestSavePTRSaysAPathChangeUnderARunningSyncWaits(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, dataPath: "/old/ptr"}
	srv, ts := serveEngine(t, eng)
	eng.address = srv.cfg.Current().PTR.Address

	resp := postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {t.TempDir()}, "enabled": {"1"}})
	defer resp.Body.Close()
	if eng.DataPath() != "/old/ptr" {
		t.Errorf("engine data path = %q, want the index it has open", eng.DataPath())
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "data+path+applies+after+a+restart") || !strings.Contains(loc, "index+is+open") {
		t.Errorf("the flash should say the new path waits for a restart, got %s", loc)
	}

	both := postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {t.TempDir()}, "address": {"https://other.example"}, "enabled": {"1"}})
	defer both.Body.Close()
	if loc := both.Header.Get("Location"); !strings.Contains(loc, "data+path+and+address+apply+after+a+restart") {
		t.Errorf("two waiting values should read in the plural, got %s", loc)
	}
}

func TestSavePTRRefusesAPathThatIsNotThereUnderARunningSync(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, dataPath: t.TempDir()}
	srv, ts := serveEngine(t, eng)
	stored := srv.cfg.Current().PTR.DataPath

	absent := filepath.Join(t.TempDir(), "not-mounted")
	body := refusedSave(t, postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {absent}, "enabled": {"1"}}))
	if !strings.Contains(body, "does not exist") {
		t.Errorf("a path that is not there should be refused, got %q", truncate(body))
	}
	if got := srv.cfg.Current().PTR.DataPath; got != stored {
		t.Errorf("data_path = %q, want the stored %q", got, stored)
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

	postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {"/mnt/ptr"}, "access_key": {""}}).Body.Close()
	got = srv.cfg.Current().PTR
	if got.AccessKey != "abc123" || got.Address != "https://ptr.example:45871" || got.FetchSleep != 2.5 || got.MinFreeGB != 120 {
		t.Errorf("absent fields should keep the stored values, got %+v", got)
	}
}

func TestAnySaveReachesTheEngineWithItsPathAndAddress(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, _ := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) { c.PTR.Address, c.PTR.DataPath = "https://direct.example", "/mnt/direct" })
	if eng.Address() != "https://direct.example" || eng.DataPath() != "/mnt/direct" {
		t.Errorf("the engine holds %q and %q", eng.Address(), eng.DataPath())
	}
}

func TestAnAddressSavedUnderAnOpenIndexLandsOnceItCloses(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, address: "https://old.example"}
	srv, _ := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) { c.PTR.Address = "https://new.example" })
	if eng.Address() != "https://old.example" {
		t.Fatalf("an open index was repointed to %q", eng.Address())
	}
	eng.Disable()
	mustUpdate(t, srv, func(c *config.Config) { c.Downloader.Concurrency = 2 })
	if eng.Address() != "https://new.example" {
		t.Errorf("the owed address did not land: %q", eng.Address())
	}
}

func TestSavePTRRefusesWhatDoesNotParseByName(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, ts := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) {
		c.PTR.Address, c.PTR.FetchSleep, c.PTR.MinFreeGB = "https://kept.example", 2.5, 120
	})
	body := refusedSave(t, postForm(t, ts, srv, "/settings/ptr", url.Values{
		"data_path":   {t.TempDir()},
		"enabled":     {"1"},
		"address":     {""},
		"fetch_sleep": {"abc"},
		"min_free_gb": {"-1"},
	}))
	for _, want := range []string{"address must be an http(s) URL", "delay between updates must be a number", "free space needed must be a whole number", "nothing was saved"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal lacks %q", want)
		}
	}
	if !strings.Contains(body, `value="abc"`) || !strings.Contains(body, `name="enabled" value="1" checked`) {
		t.Error("the refusal should keep what was typed, the enable box included")
	}
	if got := srv.cfg.Current().PTR; got.Address != "https://kept.example" || got.FetchSleep != 2.5 || got.MinFreeGB != 120 || eng.Enabled() {
		t.Errorf("a refused save moved something: %+v, enabled %v", got, eng.Enabled())
	}
	if refused := refusedSave(t, postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {t.TempDir()}, "fetch_sleep": {"Inf"}})); !strings.Contains(refused, "delay between updates must be a number") {
		t.Error("an infinite sleep should be refused")
	}
}

func TestSavePTRReachesTheEngineWithNoRestart(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{State: ptr.StateDisabled}}
	srv, ts := serveEngine(t, eng)
	postForm(t, ts, srv, "/settings/ptr", url.Values{
		"data_path":   {t.TempDir()},
		"fetch_sleep": {"2.5"},
		"min_free_gb": {"12"},
	}).Body.Close()
	if eng.minFreeGB != 12 || eng.fetchSleep != 2.5 {
		t.Errorf("the engine holds floor %d and sleep %v, want 12 and 2.5", eng.minFreeGB, eng.fetchSleep)
	}
}

func TestSavePTRRequiresDataPath(t *testing.T) {
	srv, ts := serveWeb(t, "")
	body := refusedSave(t, postForm(t, ts, srv, "/settings/ptr", url.Values{"data_path": {"  "}, "address": {"http://kept.example"}}))
	if !strings.Contains(body, "Data path required") {
		t.Errorf("blank data path should be refused, got %q", truncate(body))
	}
	if !strings.Contains(body, `value="http://kept.example"`) {
		t.Errorf("the refusal dropped what was typed, got %q", truncate(body))
	}
	if srv.cfg.Current().PTR.DataPath != "/ptr" {
		t.Errorf("blank data path should keep the default, got %q", srv.cfg.Current().PTR.DataPath)
	}
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

func TestPTRAccountCardStates(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}}
	_, ts := serveEngine(t, eng)

	_, body := get(t, ts, "/internal/ptr-account")
	if !strings.Contains(body, "Create contribution account") {
		t.Errorf("no-key card should offer creation, got %q", truncate(body))
	}

	eng.hasKey, eng.accessKey = true, "cafe1234"
	eng.account = &ptr.Account{
		Type: ptr.AccountType{
			Title:       "Public User: Read Write",
			Permissions: map[int]int{0: 1, 1: 0, 2: 0},
		},
		Created: 1720000000,
	}
	_, body = get(t, ts, "/internal/ptr-account")
	for _, want := range []string{`<td class="stats-path">Public User: Read Write`, `<td class="stats-path">add tags`, "Reveal key", "Losing this key"} {
		if !strings.Contains(body, want) {
			t.Errorf("account card missing %q, got %q", want, truncate(body))
		}
	}
	if strings.Contains(body, "cafe1234") {
		t.Error("the key must not render without the explicit reveal")
	}

	eng.account.Banned, eng.account.BanReason = true, "junk spammer"
	_, body = get(t, ts, "/internal/ptr-account")
	if !strings.Contains(body, "junk spammer") {
		t.Errorf("banned card should carry the reason verbatim, got %q", truncate(body))
	}
}

func TestPTRAccountCreatePersistsKey(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}}
	srv, ts := serveEngine(t, eng)

	resp := postForm(t, ts, srv, "/ptr/account", url.Values{})
	if resp.StatusCode != 200 {
		t.Fatalf("create answered %d", resp.StatusCode)
	}
	if got := srv.cfg.Current().PTR.AccessKey; got != "cafe1234" {
		t.Errorf("persisted key = %q, want the created one", got)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `id="ptr-contrib-card" class="settings-section" hx-swap-oob="true"`) {
		t.Errorf("create should carry the contributions card, got %q", truncate(body))
	}
	if strings.Contains(body, "needs a personal account") {
		t.Errorf("the swapped card still asks for an account, got %q", truncate(body))
	}
}

func TestPTRRevealKeyIsTheOnlyKeySurface(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, hasKey: true, accessKey: "cafe1234"}
	srv, ts := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) { c.PTR.AccessKey = "cafe1234" })

	resp := postForm(t, ts, srv, "/internal/ptr-reveal-key", url.Values{})
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, "cafe1234") {
		t.Errorf("reveal = %d %q", resp.StatusCode, truncate(body))
	}
	_, settings := get(t, ts, "/settings")
	if strings.Contains(settings, "cafe1234") {
		t.Error("settings page leaks the key")
	}
	if !strings.Contains(settings, "leave blank to keep") {
		t.Errorf("settings should show the key as set, got %q", truncate(settings))
	}
}

func TestPTRClearAccessKeyRevertsToPublic(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, hasKey: true, accessKey: "cafe1234"}
	srv, ts := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) { c.PTR.AccessKey = "cafe1234" })

	resp := postForm(t, ts, srv, "/settings/ptr/clear-key", url.Values{})
	if resp.StatusCode >= 400 {
		t.Fatalf("clear answered %d", resp.StatusCode)
	}
	if got := srv.cfg.Current().PTR.AccessKey; got != "" {
		t.Errorf("access key after clear = %q, want empty", got)
	}
	if eng.hasKey {
		t.Error("engine not re-pointed after the clear")
	}
}

func contribStoreForWeb(t *testing.T) *ptr.ContribStore {
	t.Helper()
	store, err := ptr.OpenContribStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPTRContribCardStates(t *testing.T) {
	store := contribStoreForWeb(t)
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, contrib: store}
	srv, ts := serveEngine(t, eng)
	mb := "http://mb.example"
	srv.cfg.Current().Monbooru.WebURL = mb

	_, body := get(t, ts, "/internal/ptr-contrib")
	if !strings.Contains(body, `id="ptr-contrib-card" class="settings-section" hidden`) {
		t.Errorf("no-account card = %q", truncate(body))
	}

	eng.hasKey = true
	_, body = get(t, ts, "/internal/ptr-contrib")
	if !strings.Contains(body, "Send contributions from monbooru's") {
		t.Errorf("empty card = %q", truncate(body))
	}

	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "blond hair", Tag2: "blonde hair", Reason: "misspelling", Origin: "tags page"})
	_ = mustFail(t, store, id, "bandwidth refusal")
	_, body = get(t, ts, "/internal/ptr-contrib")
	for _, want := range []string{"1 unsent", "last send failed", "retry", "blond hair -&gt; blonde hair", "tags page", mb + "/tags?q=blond_hair",
		`<table class="stats-table contrib-table stack-table">`, `<td class="stack-head">alias</td>`, `<td data-label="Origin">tags page</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("failed card missing %q: %q", want, truncate(body))
		}
	}

	resp := postForm(t, ts, srv, "/ptr/contrib/"+strconv.FormatInt(id, 10)+"/rescind", url.Values{})
	if resp.StatusCode != 200 {
		t.Fatalf("rescind = %d", resp.StatusCode)
	}
	if unsent, _, _ := store.Counts(); unsent != 0 {
		t.Errorf("unsent after rescind = %d", unsent)
	}
	body = readBody(t, postForm(t, ts, srv, "/ptr/contrib/"+strconv.FormatInt(id, 10)+"/rescind", url.Values{}))
	if !strings.Contains(body, "already on its way out") {
		t.Errorf("rescind of a gone row = %q", truncate(body))
	}

	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "blonde hair", Hash: testHash32(0xaa)})
	sibID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "a", Tag2: "b", Reason: "r"})
	claimed, _ := store.ClaimIDs([]int64{addID, sibID})
	for _, it := range claimed {
		_, _ = store.Complete(it)
	}
	_, body = get(t, ts, "/internal/ptr-contrib")
	for _, want := range []string{"Ask to remove", "pending 0d", "applied", mb + "/i/" + strings.Repeat("aa", 32),
		`<td class="ptr-sent stack-head">`, `<td data-label="Outcome">`} {
		if !strings.Contains(body, want) {
			t.Errorf("history card missing %q: %q", want, truncate(body))
		}
	}
}

func TestPTRContribActivityPanel(t *testing.T) {
	store := contribStoreForWeb(t)
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, hasKey: true, contrib: store}
	_, ts := serveEngine(t, eng)

	_, body := get(t, ts, "/internal/ptr-contrib")
	if !strings.Contains(body, "0 contributions in the last year") || strings.Count(body, `class="ptr-day"`) < 300 {
		t.Errorf("empty activity panel missing: %q", truncate(body))
	}

	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "a", Tag2: "b", Reason: "r"})
	claimed, _ := store.ClaimIDs([]int64{id})
	if _, err := store.Complete(claimed[0]); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, ts, "/internal/ptr-contrib")
	today := time.Now().Format("2006-01-02")
	for _, want := range []string{"1 contribution in the last year", `class="ptr-day ptr-day-4" title="` + today + ` - 1 contribution"`} {
		if !strings.Contains(body, want) {
			t.Errorf("activity panel missing %q: %q", want, truncate(body))
		}
	}
}

func TestPTRActivityShape(t *testing.T) {
	now := time.Date(2026, 7, 28, 15, 0, 0, 0, time.Local)
	v := ptrActivity(map[string]int{"2026-07-28": 2, "2026-07-27": 8, "2025-08-03": 1}, now)
	if len(v.Weeks) != 53 {
		t.Errorf("weeks = %d, want 53", len(v.Weeks))
	}
	if v.Total != 11 {
		t.Errorf("total = %d, want 11", v.Total)
	}

	first := v.Weeks[0]
	if len(first.Days) != 7 || !strings.HasPrefix(first.Days[0].Title, "2025-07-27 - 0") {
		t.Errorf("first week = %+v", first)
	}

	last := v.Weeks[len(v.Weeks)-1]
	if len(last.Days) != 3 || last.Days[2].Title != "2026-07-28 - 2 contributions" {
		t.Errorf("last week = %+v", last)
	}
	if lv := last.Days[2].Level; lv != 1 {
		t.Errorf("level for 2 of max 8 = %d, want 1", lv)
	}
	if lv := last.Days[1].Level; lv != 4 {
		t.Errorf("busiest day level = %d, want 4", lv)
	}
	var months []string
	for _, wk := range v.Weeks {
		if wk.Month != "" {
			months = append(months, wk.Month)
		}
	}
	if len(months) != 12 || months[0] != "aug" || months[11] != "jul" {
		t.Errorf("month labels = %v", months)
	}
}

func TestPTRContribCardPollsWhileSending(t *testing.T) {
	store := contribStoreForWeb(t)
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, hasKey: true, contrib: store}
	srv, ts := serveEngine(t, eng)

	if _, body := get(t, ts, "/internal/ptr-contrib"); strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Errorf("an idle card should not poll: %q", truncate(body))
	}

	srv.queue.Pause()
	defer srv.queue.Resume()
	srv.queue.EnqueueContrib(nil, true)
	_, body := get(t, ts, "/internal/ptr-contrib")
	if !strings.Contains(body, `hx-trigger="every 2s [!confirmOpen()]"`) || !strings.Contains(body, `hx-target="#ptr-contrib"`) {
		t.Errorf("a card rendered while a send is in flight should poll: %q", truncate(body))
	}
}

func TestPTRContribHistoryPaginates(t *testing.T) {
	store := contribStoreForWeb(t)
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, contrib: store, hasKey: true}
	_, ts := serveEngine(t, eng)

	for i := 0; i < 25; i++ {
		id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: fmt.Sprintf("tag%02d", i), Hash: testHash32(byte(i))})
		claimed, _ := store.ClaimIDs([]int64{id})
		_, _ = store.Complete(claimed[0])
	}

	_, body := get(t, ts, "/internal/ptr-contrib")
	if !strings.Contains(body, "Page 1 / 2") {
		t.Errorf("page 1 missing pager: %q", truncate(body))
	}
	if !strings.Contains(body, "tag24") || strings.Contains(body, "tag04") {
		t.Errorf("page 1 should hold the newest 20 rows: %q", truncate(body))
	}
	_, body = get(t, ts, "/internal/ptr-contrib?page=2")
	if !strings.Contains(body, "Page 2 / 2") || !strings.Contains(body, "tag00") {
		t.Errorf("page 2 should hold the oldest rows: %q", truncate(body))
	}

	for _, want := range []string{`id="ptr-contrib-card" class="settings-section" hx-vals='{"page": 2}'`, `hx-vals='{"page": 1}'`} {
		if !strings.Contains(body, want) {
			t.Errorf("page 2 is missing %s: %q", want, truncate(body))
		}
	}
}

func mustFail(t *testing.T, store *ptr.ContribStore, id int64, msg string) int64 {
	t.Helper()
	if _, err := store.ClaimIDs([]int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := store.FailItem(id, msg); err != nil {
		t.Fatal(err)
	}
	return id
}

func testHash32(b byte) []byte {
	return bytes.Repeat([]byte{b}, 32)
}

func TestPTRContribLogRescindWeb(t *testing.T) {
	store := contribStoreForWeb(t)
	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: testHash32(0xaa)})
	claimed, _ := store.ClaimIDs([]int64{addID})
	logID, _ := store.Complete(claimed[0])
	eng := &stubEngine{
		status: ptr.Status{Enabled: true, State: ptr.StateReady}, hasKey: true, contrib: store,
		rawKnown: map[string]bool{strings.Repeat("aa", 32) + "|a tag": true},
	}
	srv, ts := serveEngine(t, eng)

	resp := postForm(t, ts, srv, "/ptr/contrib/log/"+strconv.FormatInt(logID, 10)+"/rescind", url.Values{})
	if resp.StatusCode != 200 {
		t.Fatalf("log rescind = %d", resp.StatusCode)
	}
	row, _ := store.LogRow(logID)
	if row.Outcome != ptr.OutcomeApplied {
		t.Errorf("outcome = %q, want applied until the petition is sent", row.Outcome)
	}
	unsent, err := store.Unsent()
	if err != nil || len(unsent) != 1 || unsent[0].Kind != ptr.ContribMappingPetition {
		t.Errorf("unsent = %+v, %v, want the staged petition", unsent, err)
	}
}

func TestPTRContribSendGatedOnWeb(t *testing.T) {
	store := contribStoreForWeb(t)
	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: testHash32(0xcc)})
	claimed, _ := store.ClaimIDs([]int64{addID})
	logID, _ := store.Complete(claimed[0])

	eng := &stubEngine{hasKey: true, contrib: store,
		rawKnown: map[string]bool{strings.Repeat("cc", 32) + "|a tag": true}}
	srv, ts := serveEngine(t, eng)
	post := func(path string) string {
		resp := postForm(t, ts, srv, path, url.Values{})
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	rescindPath := "/ptr/contrib/log/" + strconv.FormatInt(logID, 10) + "/rescind"

	eng.status = ptr.Status{Enabled: true, State: ptr.StateSyncing}
	for _, path := range []string{rescindPath, "/ptr/contrib/retry"} {
		if got := post(path); !strings.Contains(got, "not fully synced yet") {
			t.Errorf("%s while syncing: %q", path, truncate(got))
		}
	}

	eng.status = ptr.Status{Enabled: true, State: ptr.StateReady}
	eng.account = &ptr.Account{Banned: true}
	for _, path := range []string{rescindPath, "/ptr/contrib/retry"} {
		if got := post(path); !strings.Contains(got, "account is banned") {
			t.Errorf("%s while banned: %q", path, truncate(got))
		}
	}
	if row, _ := store.LogRow(logID); row.Outcome == ptr.OutcomeRescinded {
		t.Error("a refused send stamped the ledger row")
	}
}

func TestPTRContribLogRescindWebRefusals(t *testing.T) {
	store := contribStoreForWeb(t)
	hash := testHash32(0xbb)

	sibID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "bad", Tag2: "good", Reason: "misspelling"})
	sibClaimed, _ := store.ClaimIDs([]int64{sibID})
	sibLog, _ := store.Complete(sibClaimed[0])

	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: hash})
	addClaimed, _ := store.ClaimIDs([]int64{addID})
	addLog, _ := store.Complete(addClaimed[0])

	eng := &stubEngine{
		status:  ptr.Status{Enabled: true, State: ptr.StateReady, CoveredThrough: time.Now().Unix() + 10},
		hasKey:  true,
		contrib: store,
	}
	srv, ts := serveEngine(t, eng)
	rescind := func(id int64) string {
		resp := postForm(t, ts, srv, "/ptr/contrib/log/"+strconv.FormatInt(id, 10)+"/rescind", url.Values{})
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	if got := rescind(9999); !strings.Contains(got, "no ledger row with that id") {
		t.Errorf("unknown id: %q", truncate(got))
	}
	if got := rescind(sibLog); !strings.Contains(got, "only a committed mapping add can be rescinded") {
		t.Errorf("sibling row: %q", truncate(got))
	}

	if got := rescind(addLog); !strings.Contains(got, "already gone from the index") {
		t.Errorf("absent mapping: %q", truncate(got))
	}
	if row, _ := store.LogRow(addLog); row.Outcome == ptr.OutcomeRescinded {
		t.Error("a refused rescind stamped the ledger row")
	}

	eng.rawKnown = map[string]bool{strings.Repeat("bb", 32) + "|a tag": true}
	if got := rescind(addLog); strings.Contains(got, "flash-warn") {
		t.Errorf("first rescind refused: %q", truncate(got))
	}
	if got := rescind(addLog); !strings.Contains(got, "already staged or awaiting review") {
		t.Errorf("second rescind: %q", truncate(got))
	}
}

func TestPortableSettingsSaveStoresPathsUnderTheFolderRelative(t *testing.T) {
	srv := newWebServer(t, "", "")
	dir := t.TempDir()
	srv.configPath = filepath.Join(dir, "monloader.toml")
	srv.cfg.Persist(srv.configPath, dir)

	inside := filepath.Join(dir, "index")
	mustUpdate(t, srv, func(c *config.Config) {
		c.PTR.DataPath = inside
	})
	written, err := os.ReadFile(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `data_path = "index"`) {
		t.Errorf("a path inside the folder was stored absolute:\n%s", written)
	}

	if strings.Contains(string(written), "/config/") {
		t.Errorf("a save wrote the container volume into a portable config:\n%s", written)
	}
	if got := srv.cfg.Current().PTR.DataPath; got != inside {
		t.Errorf("runtime ptr.data_path = %q, want %q", got, inside)
	}

	outside := filepath.Join(t.TempDir(), "big-disk")
	mustUpdate(t, srv, func(c *config.Config) {
		c.PTR.DataPath = outside
	})
	written, err = os.ReadFile(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `data_path = "`+outside+`"`) {
		t.Errorf("a path outside the folder should stay absolute:\n%s", written)
	}
}

func TestPTRRetryRefusesWhileDisabled(t *testing.T) {
	srv, ts := serveWeb(t, "")
	if srv.cfg.Current().PTR.Enabled {
		t.Fatal("the test config should start with the ptr off")
	}
	postClosed(t, ts, srv, "/ptr/retry", url.Values{})
	if srv.ptr.Enabled() {
		t.Error("a retry started a sync the operator disabled")
	}
}

func TestContribOutcomeLabelNamesANotTakenAdd(t *testing.T) {
	if got := contribOutcomeLabel(ptr.OutcomeNotTaken, 0); got != "not taken" {
		t.Errorf("label = %q, want not taken", got)
	}
}

func TestRevealsAreNotCached(t *testing.T) {
	eng := &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, hasKey: true, accessKey: "cafe1234"}
	srv, ts := serveEngine(t, eng)
	mustUpdate(t, srv, func(c *config.Config) {
		c.GalleryDL.RawConfig = `{"extractor":{"timeout":30}}`
		c.Sites = []config.Site{{Name: "twitter", Options: `{"access-token":"x"}`}}
	})
	for _, path := range []string{"/settings/raw", "/settings/sites/twitter/options"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); resp.StatusCode != http.StatusOK || got != "no-store" {
			t.Errorf("%s = %d, Cache-Control %q", path, resp.StatusCode, got)
		}
	}
	resp := postForm(t, ts, srv, "/internal/ptr-reveal-key", url.Values{})
	_ = resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("the key's reveal: Cache-Control %q", got)
	}
}
