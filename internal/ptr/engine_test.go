package ptr

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

func seedFixtureStream(t *testing.T, fr *fixtureRepo) {
	t.Helper()
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue eyes", 11: "blue_eyes"})
	contentBlob := newContent().mappingAdd(10, 1).siblingAdd(10, 11).build(t)
	h0 := fr.addUpdate(defsBlob)
	h1 := fr.addUpdate(contentBlob)
	fr.setEntries(4102444800, []updateEntry{
		{Index: 0, Hashes: []string{h0}, Begin: 1, End: 2},
		{Index: 1, Hashes: []string{h1}, Begin: 2, End: 3},
	})
}

func seededRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	return fr
}

func startAndWait(e *Engine) {
	e.Start(context.Background())
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
}

func startedEngine(t *testing.T, fr *fixtureRepo, dataPath string) *Engine {
	t.Helper()
	e := newTestEngine(t, fr, dataPath)
	startAndWait(e)
	t.Cleanup(e.Disable)
	return e
}

func syncIndex(t *testing.T, fr *fixtureRepo, dir string) {
	t.Helper()
	e := newTestEngine(t, fr, dir)
	startAndWait(e)
	waitState(t, e, StateReady)
	e.Disable()
}

func newTestEngine(t *testing.T, fr *fixtureRepo, dataPath string) *Engine {
	e := NewEngine(config.PTRConfig{
		Enabled: true, DataPath: dataPath, Address: fr.URL, FetchSleep: 0, MinFreeGB: 0,
	})
	e.retryBackoff = 20 * time.Millisecond
	e.readyPoll = 20 * time.Millisecond
	return e
}

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

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEngineInitialSyncToReady(t *testing.T) {
	fr := seededRepo(t)
	e := startedEngine(t, fr, t.TempDir())

	waitState(t, e, StateReady)
	st := e.Status()
	if st.Counts.Mappings != 1 || st.Counts.Hashes != 1 {
		t.Errorf("counts = %+v, want the seeded rows", st.Counts)
	}
	if st.Progress.UpdateIndex != 1 || st.Progress.UpdateCount != 2 {
		t.Errorf("progress = %d/%d, want 1/2", st.Progress.UpdateIndex, st.Progress.UpdateCount)
	}
	if st.Progress.BlobsDone != 2 || st.Progress.BlobsTotal != 2 {
		t.Errorf("blob progress = %d/%d, want 2/2", st.Progress.BlobsDone, st.Progress.BlobsTotal)
	}
	if st.Progress.LastAppliedAt == 0 {
		t.Error("applying an entry should stamp the last-applied time")
	}
	if st.CoveredThrough != 3 {
		t.Errorf("covered = %d, want the last entry's window end (3)", st.CoveredThrough)
	}
	tags, ok, err := e.TagsForHash(h32("aa"))
	if err != nil || !ok {
		t.Fatalf("TagsForHash: ok=%v err=%v", ok, err)
	}
	if len(tags) != 1 || tags[0] != "blue_eyes" {
		t.Errorf("tags = %v, want [blue_eyes]", tags)
	}
	matches, truncated, err := e.SearchTags(context.Background(), []TagRange{{Prefix: "blue"}}, 10)
	if err != nil || truncated {
		t.Fatalf("SearchTags: truncated=%v err=%v", truncated, err)
	}
	if len(matches) != 1 || matches[0].Ideal != "blue_eyes" || len(matches[0].Matched) != 2 {
		t.Errorf("matches = %+v, want one cluster of both spellings under blue_eyes", matches)
	}
}

func TestEngineResumeAcrossRestart(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()

	syncIndex(t, fr, dir)

	e2 := startedEngine(t, fr, dir)
	waitFor(t, "the resumed position and counts", func() bool {
		st := e2.Status()
		return st.Progress.UpdateIndex > 0 && st.CoveredThrough > 0
	})
	if got := e2.Status().Counts.Mappings; got != 1 {
		t.Errorf("resumed mappings = %d, want 1 (no double-apply)", got)
	}
	if got := e2.Status().CoveredThrough; got != 3 {
		t.Errorf("resumed covered = %d, want 3 read from the index", got)
	}

	if got := e2.Status().Progress.UpdateIndex; got != 1 {
		t.Errorf("resumed update index = %d, want 1 seeded from the cursor", got)
	}
}

func TestEngineRefusesAnIndexBuiltFromAnotherRepository(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()
	syncIndex(t, fr, dir)

	other := seededRepo(t)
	e2 := newTestEngine(t, other, dir)
	err := e2.Enable()
	if err == nil {
		e2.Disable()
		t.Fatal("Enable on another repository's index succeeded")
	}
	if !strings.Contains(err.Error(), fr.URL) || !strings.Contains(err.Error(), "delete the ptr data") {
		t.Errorf("err = %v, want it to name the index's repository and the way out", err)
	}
}

func TestEngineSetAddressRepointsOnlyWhileClosed(t *testing.T) {
	fr := seededRepo(t)
	e := NewEngine(config.PTRConfig{DataPath: t.TempDir(), Address: "https://old.example"})
	e.baseCtx = context.Background()
	if !e.SetAddress(fr.URL) {
		t.Fatal("SetAddress on a closed engine was not applied")
	}
	if err := e.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	defer e.Disable()
	waitState(t, e, StateReady)
	if e.SetAddress("https://other.example") {
		t.Error("SetAddress repointed a running sync")
	}
}

func TestEngineAnswersDuringTheBootManifestCheck(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()

	syncIndex(t, fr, dir)

	release := fr.holdMetadata()
	defer release()
	e2 := startedEngine(t, fr, dir)
	if e2.Provisional() {
		t.Error("a complete index must not read as provisional after a restart")
	}
	fr.waitMetaHit(t)
	if !e2.CaughtUp() {
		t.Error("a complete index must answer while its boot check is still in flight")
	}
	tags, ok, err := e2.TagsForHash(h32("aa"))
	if !ok || err != nil || len(tags) != 1 {
		t.Errorf("TagsForHash = %v ok=%v err=%v, want the index to answer", tags, ok, err)
	}

	release()
	fr.setFailMeta(true)
	e3 := startedEngine(t, fr, dir)
	waitState(t, e3, StateError)
	if !e3.CaughtUp() {
		t.Error("a complete index must answer even when its poll could not")
	}
}

func TestEngineRefusesAHalfBuiltIndexAfterRestart(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()

	s := openStoreAt(t, dir)
	mustReplay(t, s, 0, 0, nil)
	s.Close()

	fr.setFailMeta(true)
	e := startedEngine(t, fr, dir)
	if e.CaughtUp() {
		t.Error("a half-built index must keep refusing")
	}
}

func TestEngineResumedProgressCountsAbsolute(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue_eyes"})
	contentBlob := newContent().mappingAdd(10, 1).build(t)
	h0 := fr.addUpdate(defsBlob)
	h1 := fr.addUpdate(contentBlob)
	fr.setEntries(4102444800, []updateEntry{{Index: 0, Hashes: []string{h0}, Begin: 1, End: 2}})
	dir := t.TempDir()

	syncIndex(t, fr, dir)

	fr.setEntries(4102444800, []updateEntry{
		{Index: 0, Hashes: []string{h0}, Begin: 1, End: 2},
		{Index: 1, Hashes: []string{h1}, Begin: 2, End: 3},
	})
	e2 := startedEngine(t, fr, dir)
	waitFor(t, "the remaining update to apply", func() bool {
		p := e2.Status().Progress
		return p.UpdateIndex == 1 && p.UpdateCount == 2
	})
	if p := e2.Status().Progress; p.UpdateIndex != 1 || p.UpdateCount != 2 {
		t.Errorf("resumed progress = %d/%d, want 1/2", p.UpdateIndex, p.UpdateCount)
	}
}

func TestEngineBackfillsCoveredThrough(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()

	s := openStoreAt(t, dir)
	mustReplay(t, s, 1, 0, nil)
	s.Close()

	e := newTestEngine(t, fr, dir)
	startAndWait(e)
	waitState(t, e, StateReady)
	if got := e.Status().CoveredThrough; got != 3 {
		t.Errorf("backfilled covered = %d, want 3 from the cursor's manifest entry", got)
	}
	e.Disable()

	e2 := startedEngine(t, fr, dir)
	waitFor(t, "the coverage to be read back", func() bool { return e2.Status().CoveredThrough > 0 })
	if got := e2.Status().CoveredThrough; got != 3 {
		t.Errorf("covered after restart = %d, want 3 persisted", got)
	}
}

func TestEngineSeedsBlobBaseline(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()

	syncIndex(t, fr, dir)

	s := openStoreAt(t, dir)
	if _, err := s.db.Exec(`DELETE FROM sync WHERE key='blobs'`); err != nil {
		t.Fatalf("deleting the census: %v", err)
	}
	s.Close()

	e2 := startedEngine(t, fr, dir)
	waitFor(t, "the blob census backfill", func() bool {
		_, ok, _ := e2.store.BlobsApplied()
		return ok
	})
	if got, ok, _ := e2.store.BlobsApplied(); !ok || got != 2 {
		t.Errorf("backfilled blobs = %d ok=%v, want 2/true", got, ok)
	}
}

func TestEnginePauseAndResume(t *testing.T) {
	fr := seededRepo(t)
	e := startedEngine(t, fr, t.TempDir())
	waitState(t, e, StateReady)

	e.Pause()
	waitState(t, e, StatePaused)
	e.Resume()
	waitState(t, e, StateReady)
}

func TestEnginePauseAndResumeShowAtOnce(t *testing.T) {
	fr := seededRepo(t)
	e := startedEngine(t, fr, t.TempDir())
	waitState(t, e, StateReady)

	e.Pause()
	if st := e.Status().State; st != StatePaused {
		t.Errorf("state right after Pause = %s, want paused", st)
	}
	waitState(t, e, StatePaused)
	e.Resume()
	if st := e.Status().State; st != StateReady {
		t.Errorf("state right after Resume = %s, want ready", st)
	}
}

func TestEngineResumeMidBuildShowsSyncing(t *testing.T) {
	fr := seededRepo(t)
	e := NewEngine(config.PTRConfig{Enabled: true, Paused: true, DataPath: t.TempDir(), Address: fr.URL})
	startAndWait(e)
	t.Cleanup(e.Disable)
	waitState(t, e, StatePaused)

	e.Resume()
	if st := e.Status().State; st != StateSyncing {
		t.Errorf("state right after Resume = %s, want syncing on an index that never caught up", st)
	}
	waitState(t, e, StateReady)
}

func TestEngineStartsPausedWithoutARequest(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()
	syncIndex(t, fr, dir)

	s := openStoreAt(t, dir)
	if _, err := s.db.Exec(`DELETE FROM sync WHERE key='blobs'`); err != nil {
		t.Fatalf("deleting the census: %v", err)
	}
	s.Close()
	meta, update := fr.hits()

	e2 := NewEngine(config.PTRConfig{Enabled: true, Paused: true, DataPath: dir, Address: fr.URL})
	startAndWait(e2)
	t.Cleanup(e2.Disable)
	waitState(t, e2, StatePaused)
	time.Sleep(50 * time.Millisecond)
	if m, u := fr.hits(); m != meta || u != update {
		t.Errorf("a paused boot sent %d metadata and %d update requests", m-meta, u-update)
	}
	if !e2.Status().Queryable || !e2.CaughtUp() {
		t.Error("a paused complete index should stay queryable")
	}
	if _, ok, _ := e2.TagsForHash(h32("aa")); !ok {
		t.Error("a paused complete index should still answer a lookup")
	}
}

func TestEnginePausedFirstSyncIsNotQueryable(t *testing.T) {
	fr := seededRepo(t)
	e := NewEngine(config.PTRConfig{Enabled: true, Paused: true, DataPath: t.TempDir(), Address: fr.URL})
	startAndWait(e)
	t.Cleanup(e.Disable)
	waitState(t, e, StatePaused)
	if e.Status().Queryable {
		t.Error("an index that never finished a sync must not read queryable")
	}
	if m, u := fr.hits(); m != 0 || u != 0 {
		t.Errorf("a paused first sync sent %d metadata and %d update requests", m, u)
	}
}

func TestEnginePauseStopsBetweenBlobs(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue eyes"})
	contentBlob := newContent().mappingAdd(10, 1).build(t)
	fr.setEntries(4102444800, []updateEntry{
		{Index: 0, Hashes: []string{fr.addUpdate(defsBlob), fr.addUpdate(contentBlob)}, Begin: 1, End: 2},
	})
	e := newTestEngine(t, fr, t.TempDir())
	fr.onUpdate = sync.OnceFunc(e.Pause)
	startAndWait(e)
	t.Cleanup(e.Disable)

	waitState(t, e, StatePaused)
	if _, u := fr.hits(); u != 1 {
		t.Errorf("update requests = %d, want the pause to stop the entry after its first blob", u)
	}
	if st := e.Status(); st.Counts.Hashes != 0 || st.CoveredThrough != 0 {
		t.Errorf("a paused entry must not apply, got counts %+v covered %d", st.Counts, st.CoveredThrough)
	}

	e.Resume()
	waitState(t, e, StateReady)
	if _, u := fr.hits(); u != 3 {
		t.Errorf("update requests = %d, want the entry re-fetched whole on resume", u)
	}
	if _, ok, _ := e.TagsForHash(h32("aa")); !ok {
		t.Error("the resumed entry should have applied")
	}
}

func TestEngineErrorThenRetry(t *testing.T) {
	fr := seededRepo(t)
	fr.setFailMeta(true)

	e := startedEngine(t, fr, t.TempDir())

	waitState(t, e, StateError)
	fr.setFailMeta(false)
	e.Retry()
	waitState(t, e, StateReady)
}

func TestEngineErrorsOnNonAdvancingManifest(t *testing.T) {
	fr := seededRepo(t)
	fr.setIgnoreSince(true)

	e := startedEngine(t, fr, t.TempDir())

	waitState(t, e, StateError)
	if msg := e.Status().Error; !strings.Contains(msg, "did not advance") {
		t.Errorf("error = %q, want the non-advancing manifest named", msg)
	}

	fr.setIgnoreSince(false)
	time.Sleep(10 * e.retryBackoff)
	if st := e.Status().State; st != StateError {
		t.Fatalf("state = %s, want error held until a manual retry", st)
	}
	e.Retry()
	waitState(t, e, StateReady)
}

func TestEngineProvisionalUntilCaughtUp(t *testing.T) {
	fr := seededRepo(t)
	e := newTestEngine(t, fr, t.TempDir())
	if !e.Provisional() {
		t.Error("a fresh engine should read provisional")
	}
	startAndWait(e)
	defer e.Disable()
	waitState(t, e, StateReady)
	if e.Provisional() {
		t.Error("a caught-up index should not read provisional")
	}
	e.Pause()
	if e.Provisional() {
		t.Error("a paused index that already caught up must stay non-provisional")
	}
	if err := e.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !e.Provisional() {
		t.Error("a deleted index leaves nothing caught up, so answers are provisional again")
	}
}

func TestEngineErrorsOnOutOfOrderManifest(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue eyes"})
	contentBlob := newContent().mappingAdd(10, 1).build(t)
	h0 := fr.addUpdate(defsBlob)
	h1 := fr.addUpdate(contentBlob)
	fr.setEntries(4102444800, []updateEntry{
		{Index: 1, Hashes: []string{h1}, Begin: 2, End: 3},
		{Index: 0, Hashes: []string{h0}, Begin: 1, End: 2},
	})

	e := startedEngine(t, fr, t.TempDir())

	waitState(t, e, StateError)
	if msg := e.Status().Error; !strings.Contains(msg, "out of order") {
		t.Errorf("error = %q, want the out-of-order manifest named", msg)
	}
}

func TestEngineBootFailureSurfacesAndRetryRecovers(t *testing.T) {
	fr := seededRepo(t)
	e := newTestEngine(t, fr, t.TempDir())
	e.cfg.MinFreeGB = 1_000_000_000

	startAndWait(e)
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

func TestPauseAfterABootFailureHoldsTheRetry(t *testing.T) {
	fr := seededRepo(t)
	e := newTestEngine(t, fr, t.TempDir())
	e.cfg.MinFreeGB = 1_000_000_000
	startAndWait(e)
	defer e.Disable()

	e.Pause()
	e.cfg.MinFreeGB = 0
	e.Retry()
	waitState(t, e, StatePaused)
}

func TestEngineAppliesManyBlobEntry(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue_eyes"})
	hashes := []string{fr.addUpdate(defsBlob)}
	for i := uint64(0); i < 69; i++ {
		defsBlob = buildDefinitions(t, nil, map[uint64]string{100 + i: "extra_tag"})
		contentBlob := newContent().mappingAdd(100+i, 1).build(t)
		hashes = append(hashes, fr.addUpdate(defsBlob), fr.addUpdate(contentBlob))
	}
	fr.setEntries(4102444800, []updateEntry{{Index: 0, Hashes: hashes, Begin: 1, End: 2}})

	e := startedEngine(t, fr, t.TempDir())

	waitState(t, e, StateReady)
	if got := e.Status().Counts.Mappings; got != 69 {
		t.Errorf("mappings = %d, want all 69 blobs replayed", got)
	}
}

func TestATransientFailureKeepsTheBlobsFetched(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue_eyes"})
	first := fr.addUpdate(defsBlob)
	last := fr.addUpdate(newContent().mappingAdd(10, 1).build(t))
	fr.failBlob = map[string]int{last: 1}
	fr.setEntries(4102444800, []updateEntry{{Index: 0, Hashes: []string{first, last}, Begin: 1, End: 2}})

	e := startedEngine(t, fr, t.TempDir())

	waitState(t, e, StateReady)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.hitsByBlob[first] != 1 {
		t.Errorf("the first blob was fetched %d times, want it kept across the retry", fr.hitsByBlob[first])
	}
	if fr.hitsByBlob[last] != 2 {
		t.Errorf("the failing blob was fetched %d times, want 2", fr.hitsByBlob[last])
	}
	if got := e.Status().Counts.Mappings; got != 1 {
		t.Errorf("mappings = %d, want the entry replayed", got)
	}
}

func TestAPauseDropsTheBlobsKeptForARetry(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue_eyes"})
	first := fr.addUpdate(defsBlob)
	last := fr.addUpdate(newContent().mappingAdd(10, 1).build(t))
	fr.failBlob = map[string]int{last: 1}
	fr.setEntries(4102444800, []updateEntry{{Index: 0, Hashes: []string{first, last}, Begin: 1, End: 2}})

	e := newTestEngine(t, fr, t.TempDir())
	e.retryBackoff = time.Hour
	startAndWait(e)
	t.Cleanup(e.Disable)
	waitState(t, e, StateError)

	e.Pause()

	time.Sleep(10 * e.readyPoll)
	e.Resume()
	waitState(t, e, StateReady)

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.hitsByBlob[first] != 2 {
		t.Errorf("the first blob was fetched %d times, want it refetched after the pause", fr.hitsByBlob[first])
	}
}

func TestADisableDropsTheBlobsKeptForARetry(t *testing.T) {
	fr := newFixtureRepo(t)
	defsBlob := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue_eyes"})
	first := fr.addUpdate(defsBlob)
	last := fr.addUpdate(newContent().mappingAdd(10, 1).build(t))
	fr.failBlob = map[string]int{last: 1}
	fr.setEntries(4102444800, []updateEntry{{Index: 0, Hashes: []string{first, last}, Begin: 1, End: 2}})

	e := newTestEngine(t, fr, t.TempDir())
	e.retryBackoff = time.Hour
	startAndWait(e)
	waitState(t, e, StateError)
	e.Disable()

	if len(e.fetched) != 0 {
		t.Errorf("fetched holds %d blobs after a disable, want none", len(e.fetched))
	}
}

func TestRetryWaitDoublesToACeiling(t *testing.T) {
	base := 30 * time.Second
	for failures, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if got := retryWait(base, failures); got != want {
			t.Errorf("retryWait after %d failures = %v, want %v", failures, got, want)
		}
	}
	if got := retryWait(base, 100); got != maxRetryBackoff {
		t.Errorf("retryWait after many failures = %v, want the ceiling", got)
	}
}

func TestEngineCapsBlobsPerEntry(t *testing.T) {
	fr := newFixtureRepo(t)
	hashes := make([]string, maxEntryBlobs+1)
	for i := range hashes {
		hashes[i] = h32("ff")
	}
	fr.setEntries(0, []updateEntry{{Index: 0, Hashes: hashes, Begin: 1, End: 2}})

	e := startedEngine(t, fr, t.TempDir())

	waitState(t, e, StateError)
	if msg := e.Status().Error; !strings.Contains(msg, "cap") {
		t.Errorf("error = %q, want the blob cap named", msg)
	}
}

func TestEngineCapsBytesPerEntry(t *testing.T) {
	fr := newFixtureRepo(t)
	defs := buildDefinitions(t, map[uint64]string{1: h32("aa")}, map[uint64]string{10: "blue eyes"})
	h := fr.addUpdate(defs)
	fr.setEntries(0, []updateEntry{{Index: 0, Hashes: []string{h, h}, Begin: 1, End: 2}})

	e := newTestEngine(t, fr, t.TempDir())
	e.entryBytes = 1
	startAndWait(e)
	defer e.Disable()

	waitState(t, e, StateError)
	if msg := e.Status().Error; !strings.Contains(msg, "byte cap") {
		t.Errorf("error = %q, want the byte cap named", msg)
	}
}

func TestEngineDeleteRemovesIndex(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()
	e := newTestEngine(t, fr, dir)
	startAndWait(e)
	waitState(t, e, StateReady)

	if err := e.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if IndexExists(dir) {
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

func TestEngineEnableConcurrent(t *testing.T) {
	fr := seededRepo(t)
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
	fr := seededRepo(t)

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

func TestEngineStartDoesNotWaitForTheIndexToOpen(t *testing.T) {
	fr := seededRepo(t)
	e := newTestEngine(t, fr, t.TempDir())
	t.Cleanup(e.Disable)

	e.mu.Lock()
	started := make(chan struct{})
	go func() {
		e.Start(context.Background())
		close(started)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Error("Start waited for the open")
	}
	e.mu.Unlock()

	<-started
	waitState(t, e, StateReady)
}

func TestEngineLowDiskRefusesAnEmptyIndexFile(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	s.Close()
	e := NewEngine(config.PTRConfig{Enabled: true, DataPath: dir, Address: fr.URL, MinFreeGB: 1 << 20})
	e.baseCtx = context.Background()
	if err := e.Enable(); err == nil {
		e.Disable()
		t.Fatal("Enable should refuse a half-built index below the floor")
	}
	if e.Enabled() {
		t.Error("engine should not be enabled after a refused Enable")
	}
}

func TestEngineLowDiskLetsACompleteIndexOpen(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()
	syncIndex(t, fr, dir)
	e := NewEngine(config.PTRConfig{Enabled: true, DataPath: dir, Address: fr.URL, MinFreeGB: 1 << 20})
	e.baseCtx = context.Background()
	if err := e.Enable(); err != nil {
		t.Fatalf("Enable on a complete index: %v", err)
	}
	e.Disable()
}

func TestEngineALoweredFloorAppliesWithNoRestart(t *testing.T) {
	fr := seededRepo(t)
	e := NewEngine(config.PTRConfig{
		Enabled: true, DataPath: t.TempDir(), Address: fr.URL, MinFreeGB: 1 << 20,
	})
	e.baseCtx = context.Background()
	if err := e.Enable(); err == nil {
		t.Fatal("Enable should refuse below the free-space floor")
	}
	e.SetMinFreeGB(0)
	if err := e.Enable(); err != nil {
		t.Fatalf("Enable after lowering the floor: %v", err)
	}
	t.Cleanup(e.Disable)
}

func TestEngineFetchSleepAppliesWithNoRestart(t *testing.T) {
	e := NewEngine(config.PTRConfig{FetchSleep: 1})
	e.SetFetchSleep(2.5)
	if got := e.client.fetchSleep(); got != 2500*time.Millisecond {
		t.Errorf("fetchSleep = %v, want 2.5s", got)
	}
}

func TestEngineMissingDataPathRefused(t *testing.T) {
	fr := newFixtureRepo(t)
	e := newTestEngine(t, fr, filepath.Join(t.TempDir(), "not-mounted"))
	e.baseCtx = context.Background()
	if err := e.Enable(); err == nil {
		t.Fatal("Enable should refuse a data path that does not exist")
	}
	if e.Enabled() {
		t.Error("engine should not be enabled after a refused Enable")
	}
}

func TestEngineSettersDoNotWaitOnALifecycleChange(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(config.PTRConfig{DataPath: dir, Address: "https://old.example"})
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	done := make(chan [2]bool, 1)
	go func() { done <- [2]bool{e.SetDataPath(t.TempDir()), e.SetAddress("https://new.example")} }()
	select {
	case got := <-done:
		if got[0] || got[1] {
			t.Errorf("SetDataPath, SetAddress = %v, want both refused while an enable or disable holds the engine", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a setter waited on an enable or disable in progress")
	}
	if e.DataPath() != dir || e.Address() != "https://old.example" {
		t.Errorf("DataPath, Address = %q, %q, want them unchanged", e.DataPath(), e.Address())
	}
}

func TestEngineSetDataPathEnablesWithoutARestart(t *testing.T) {
	fr := seededRepo(t)
	e := newTestEngine(t, fr, "")
	e.baseCtx = context.Background()
	if err := e.Enable(); err == nil {
		t.Fatal("Enable should refuse an unset data path")
	}

	dir := t.TempDir()
	if !e.SetDataPath(dir) {
		t.Fatal("SetDataPath should take with no index open")
	}
	if err := e.Enable(); err != nil {
		t.Fatalf("Enable after SetDataPath: %v", err)
	}
	t.Cleanup(e.Disable)
	if e.DataPath() != dir {
		t.Errorf("DataPath() = %q, want %q", e.DataPath(), dir)
	}

	if e.SetDataPath(t.TempDir()) {
		t.Error("SetDataPath should refuse to move the index a sync has open")
	}
	if e.DataPath() != dir {
		t.Errorf("DataPath() = %q after a refused move, want the open index %q", e.DataPath(), dir)
	}
}

func TestEngineSyncRate(t *testing.T) {
	e := NewEngine(config.PTRConfig{Address: "http://x"})
	st := e.Status()
	if st.Progress.DownloadRate != 0 || st.Progress.ProcessRate != 0 {
		t.Errorf("rates = %d/%d, want 0/0 when not syncing", st.Progress.DownloadRate, st.Progress.ProcessRate)
	}
	e.state = StateSyncing
	e.passBytes = 10 << 20
	e.passFetchDur = 10 * time.Second
	e.passRows = 500000
	e.passReplayDur = 5 * time.Second
	st = e.Status()
	if st.Progress.DownloadRate != 1<<20 {
		t.Errorf("download rate = %d B/s, want 1 MiB/s", st.Progress.DownloadRate)
	}
	if st.Progress.ProcessRate != 100000 {
		t.Errorf("process rate = %d rows/s, want 100000", st.Progress.ProcessRate)
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
	if g, _ := e.TagGraph(context.Background(), []string{"x"}); g != nil {
		t.Error("a disabled engine should return no graph")
	}
	if m, tr, err := e.SearchTags(context.Background(), []TagRange{{Prefix: "b"}}, 10); m != nil || tr || err != nil {
		t.Errorf("a disabled engine's search = %v truncated=%v err=%v, want nothing", m, tr, err)
	}
	if st := e.Status(); st.Enabled || st.State != StateDisabled {
		t.Errorf("disabled status = %+v", st)
	}
}

func TestEngineEnableDisableAreSerialized(t *testing.T) {
	fr := seededRepo(t)
	dir := t.TempDir()
	e := newTestEngine(t, fr, dir)
	startAndWait(e)
	waitState(t, e, StateReady)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				e.Disable()
				return
			}
			if err := e.Enable(); err != nil {
				t.Errorf("Enable: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if e.Enabled() {
		e.Disable()
	}
	if st := e.Status(); st.Enabled || st.State != StateDisabled {
		t.Errorf("after the last disable: %+v", st)
	}
	if err := e.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if IndexExists(dir) {
		t.Error("the index file should be gone")
	}
}
