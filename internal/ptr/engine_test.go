package ptr

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
)

// seedFixtureStream loads a two-index update stream into the repo. Index 0
// defines ids; index 1 adds mappings and a sibling so a lookup resolves.
func seedFixtureStream(t *testing.T, fr *fixtureRepo) {
	t.Helper()
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue eyes", 11: "blue_eyes"})
	contentBlob := newContent().mappingAdd(10, 1).siblingAdd(10, 11).build(t)
	h0 := fr.addUpdate(defsBlob)
	h1 := fr.addUpdate(contentBlob)
	fr.setEntries(4102444800, []UpdateEntry{
		{Index: 0, Hashes: []string{h0}, Begin: 1, End: 2},
		{Index: 1, Hashes: []string{h1}, Begin: 2, End: 3},
	})
}

// newTestEngine builds an engine pointed at the fixture repo with short waits.
func newTestEngine(t *testing.T, fr *fixtureRepo, dataPath string) *Engine {
	e := NewEngine(config.PTRConfig{
		Enabled: true, DataPath: dataPath, Address: fr.URL, FetchSleep: 0, MinFreeGB: 0,
	})
	e.retryBackoff = 20 * time.Millisecond
	e.readyPoll = 20 * time.Millisecond
	return e
}

// waitState polls the engine until it reaches want or the deadline passes.
func waitState(t *testing.T, e *Engine, want State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.Status().State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", e.Status().State, want)
}

func TestEngineInitialSyncToReady(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	e := newTestEngine(t, fr, t.TempDir())
	e.Start(context.Background())
	defer e.Disable()

	waitState(t, e, StateReady)
	st := e.Status()
	if st.Counts.Mappings != 1 || st.Counts.Hashes != 1 {
		t.Errorf("counts = %+v, want the seeded rows", st.Counts)
	}
	if st.Progress.UpdateIndex != 1 || st.Progress.UpdateCount != 2 {
		t.Errorf("progress = %d/%d, want 1/2", st.Progress.UpdateIndex, st.Progress.UpdateCount)
	}
	if st.CoveredThrough != 3 {
		t.Errorf("covered = %d, want the last entry's window end (3)", st.CoveredThrough)
	}
	// The lookup resolves through the synced graph (alias -> ideal).
	tags, ok, err := e.TagsForHash(h32("aa"))
	if err != nil || !ok {
		t.Fatalf("TagsForHash: ok=%v err=%v", ok, err)
	}
	if len(tags) != 1 || tags[0] != "blue_eyes" {
		t.Errorf("tags = %v, want [blue_eyes]", tags)
	}
}

func TestEngineResumeAcrossRestart(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	dir := t.TempDir()

	e := newTestEngine(t, fr, dir)
	e.Start(context.Background())
	waitState(t, e, StateReady)
	e.Disable()

	// A second engine on the same data dir resumes: the cursor is at 1, the
	// manifest offers nothing new, so it reaches ready without re-applying.
	e2 := newTestEngine(t, fr, dir)
	e2.Start(context.Background())
	defer e2.Disable()
	waitState(t, e2, StateReady)
	if got := e2.Status().Counts.Mappings; got != 1 {
		t.Errorf("resumed mappings = %d, want 1 (no double-apply)", got)
	}
	if got := e2.Status().CoveredThrough; got != 3 {
		t.Errorf("resumed covered = %d, want 3 read from the index", got)
	}
}

func TestEngineResumedProgressCountsAbsolute(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue_eyes"})
	contentBlob := newContent().mappingAdd(10, 1).build(t)
	h0 := fr.addUpdate(defsBlob)
	h1 := fr.addUpdate(contentBlob)
	fr.setEntries(4102444800, []UpdateEntry{{Index: 0, Hashes: []string{h0}, Begin: 1, End: 2}})
	dir := t.TempDir()

	e := newTestEngine(t, fr, dir)
	e.Start(context.Background())
	waitState(t, e, StateReady)
	e.Disable()

	// The server publishes one more update; the resumed sync fetches only the
	// remainder, but the progress must still read as absolute position over
	// the published total, never over the remainder (which would run past
	// 100%).
	fr.setEntries(4102444800, []UpdateEntry{
		{Index: 0, Hashes: []string{h0}, Begin: 1, End: 2},
		{Index: 1, Hashes: []string{h1}, Begin: 2, End: 3},
	})
	e2 := newTestEngine(t, fr, dir)
	e2.Start(context.Background())
	defer e2.Disable()
	waitState(t, e2, StateReady)
	if p := e2.Status().Progress; p.UpdateIndex != 1 || p.UpdateCount != 2 {
		t.Errorf("resumed progress = %d/%d, want 1/2", p.UpdateIndex, p.UpdateCount)
	}
}

// An index synced before the coverage timestamp was recorded is caught up and
// never replays, so the timestamp is backfilled from the cursor's own manifest
// entry and persisted for the next run.
func TestEngineBackfillsCoveredThrough(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	dir := t.TempDir()

	// Leave the index the way an older version did: cursor set, no coverage.
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if _, err := s.Replay(1, 0, nil); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	s.Close()

	e := newTestEngine(t, fr, dir)
	e.Start(context.Background())
	waitState(t, e, StateReady)
	if got := e.Status().CoveredThrough; got != 3 {
		t.Errorf("backfilled covered = %d, want 3 from the cursor's manifest entry", got)
	}
	e.Disable()

	e2 := newTestEngine(t, fr, dir)
	e2.Start(context.Background())
	defer e2.Disable()
	waitState(t, e2, StateReady)
	if got := e2.Status().CoveredThrough; got != 3 {
		t.Errorf("covered after restart = %d, want 3 persisted", got)
	}
}

func TestEnginePauseAndResume(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	e := newTestEngine(t, fr, t.TempDir())
	e.Start(context.Background())
	defer e.Disable()
	waitState(t, e, StateReady)

	e.Pause()
	waitState(t, e, StatePaused)
	e.Resume()
	waitState(t, e, StateReady)
}

func TestEngineErrorThenRetry(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	fr.setFailMeta(true) // /metadata answers 500, so the first pass errors

	e := newTestEngine(t, fr, t.TempDir())
	e.Start(context.Background())
	defer e.Disable()

	waitState(t, e, StateError)
	fr.setFailMeta(false)
	e.Retry()
	waitState(t, e, StateReady)
}

// A repository that ignores since and keeps re-publishing old indexes must
// land the sync in the error state, not spin the pass forever.
func TestEngineErrorsOnNonAdvancingManifest(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	fr.setIgnoreSince(true)

	e := newTestEngine(t, fr, t.TempDir())
	e.Start(context.Background())
	defer e.Disable()

	waitState(t, e, StateError)
	if msg := e.Status().Error; !strings.Contains(msg, "did not advance") {
		t.Errorf("error = %q, want the non-advancing manifest named", msg)
	}

	// A permanent error holds for a manual retry: fixing the repository alone
	// must not restart the sync.
	fr.setIgnoreSince(false)
	time.Sleep(10 * e.retryBackoff)
	if st := e.Status().State; st != StateError {
		t.Fatalf("state = %s, want error held until a manual retry", st)
	}
	e.Retry()
	waitState(t, e, StateReady)
}

// A boot-time enable failure surfaces on the page's state (not only the log)
// and the retry button re-opens the index once the cause is cleared.
func TestEngineBootFailureSurfacesAndRetryRecovers(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	e := newTestEngine(t, fr, t.TempDir())
	e.cfg.MinFreeGB = 1_000_000_000 // an impossibly high floor refuses the initial sync

	e.Start(context.Background())
	defer e.Disable()

	if st := e.Status().State; st != StateError {
		t.Fatalf("state = %s, want error after a refused boot", st)
	}
	if e.Enabled() {
		t.Error("a refused boot must leave the index closed")
	}

	e.cfg.MinFreeGB = 0
	e.Retry()
	waitState(t, e, StateReady)
}

// An entry listing more blobs than the cap must fail the pass instead of
// buffering them all.
func TestEngineCapsBlobsPerEntry(t *testing.T) {
	fr := newFixtureRepo(t)
	hashes := make([]string, maxEntryBlobs+1)
	for i := range hashes {
		hashes[i] = h32("ff")
	}
	fr.setEntries(0, []UpdateEntry{{Index: 0, Hashes: hashes, Begin: 1, End: 2}})

	e := newTestEngine(t, fr, t.TempDir())
	e.Start(context.Background())
	defer e.Disable()

	waitState(t, e, StateError)
	if msg := e.Status().Error; !strings.Contains(msg, "cap") {
		t.Errorf("error = %q, want the blob cap named", msg)
	}
}

func TestEngineDeleteRemovesIndex(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	dir := t.TempDir()
	e := newTestEngine(t, fr, dir)
	e.Start(context.Background())
	waitState(t, e, StateReady)

	if err := e.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if indexExists(dir) {
		t.Error("index file should be gone after Delete")
	}
	if e.Enabled() {
		t.Error("engine should read disabled after Delete")
	}
	st := e.Status()
	if st.Counts != (Counts{}) || st.NextUpdateDue != 0 || st.CoveredThrough != 0 {
		t.Errorf("deleted status still reports counts=%+v due=%d covered=%d, want zeros", st.Counts, st.NextUpdateDue, st.CoveredThrough)
	}
}

// Concurrent enables (a double-submitted form) must open one store and one
// sync loop, not two writers on the same index.
func TestEngineEnableConcurrent(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	e := newTestEngine(t, fr, t.TempDir())
	e.baseCtx = context.Background()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Enable(); err != nil {
				t.Errorf("Enable: %v", err)
			}
		}()
	}
	wg.Wait()
	waitState(t, e, StateReady)
	e.Disable()
	if e.Enabled() {
		t.Error("engine should be disabled after Disable")
	}
}

func TestEngineLowDiskRefused(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	// An absurd floor no real volume meets forces the refusal.
	e := NewEngine(config.PTRConfig{
		Enabled: true, DataPath: t.TempDir(), Address: fr.URL, MinFreeGB: 1 << 20,
	})
	e.baseCtx = context.Background()
	if err := e.Enable(); err == nil {
		t.Fatal("Enable should refuse below the free-space floor")
	}
	if e.Enabled() {
		t.Error("engine should not be enabled after a refused Enable")
	}
}

func TestEngineSyncRate(t *testing.T) {
	e := NewEngine(config.PTRConfig{Address: "http://x"})
	if r := e.Status().Progress.DownloadRate; r != 0 {
		t.Errorf("rate = %d, want 0 when not syncing", r)
	}
	// A pass 10 s in having fetched 10 MiB reads as ~1 MiB/s.
	e.state = StateSyncing
	e.passStart = time.Now().Add(-10 * time.Second)
	e.progress.DownloadedBytes = 10 << 20
	if r := e.Status().Progress.DownloadRate; r < 900<<10 || r > 1<<20 {
		t.Errorf("rate = %d B/s, want ~1 MiB/s", r)
	}
}

func TestReadyWaitHonorsNextUpdateDue(t *testing.T) {
	e := NewEngine(config.PTRConfig{Address: "http://x"})
	e.readyPoll = time.Minute
	if d := e.readyWait(); d != time.Minute {
		t.Errorf("no due time: wait = %v, want the fallback poll", d)
	}
	e.setNextDue(time.Now().Add(time.Hour).Unix())
	if d := e.readyWait(); d < 59*time.Minute || d > time.Hour {
		t.Errorf("future due: wait = %v, want ~1h", d)
	}
	e.setNextDue(time.Now().Add(-time.Hour).Unix())
	if d := e.readyWait(); d != time.Minute {
		t.Errorf("past due: wait = %v, want the fallback poll", d)
	}
}

func TestFreeBytesFallsBackToExistingAncestor(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ptr", "data")
	if got := FreeBytes(missing); got <= 0 {
		t.Errorf("FreeBytes(%q) = %d, want the ancestor volume's headroom", missing, got)
	}
}

func TestEngineDisabledQueriesAreEmpty(t *testing.T) {
	e := NewEngine(config.PTRConfig{Address: "http://x"})
	if _, ok, _ := e.TagsForHash(h32("aa")); ok {
		t.Error("a disabled engine should resolve no hash")
	}
	if g, _ := e.TagGraph([]string{"x"}); g != nil {
		t.Error("a disabled engine should return no graph")
	}
	if st := e.Status(); st.Enabled || st.State != StateDisabled {
		t.Errorf("disabled status = %+v", st)
	}
}
