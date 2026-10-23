package ptr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/desktop"
	"github.com/monbooru/monloader/internal/shared/logx"
)

type State string

const (
	StateDisabled State = "disabled"
	StateSyncing  State = "syncing"
	StateReady    State = contract.PTRReady
	StatePaused   State = "paused"
	StateError    State = "error"
)

const gib = 1 << 30

type Progress struct {
	UpdateIndex uint64 `json:"update_index"`
	UpdateCount uint64 `json:"update_count"`
	// BlobsDone over BlobsTotal is the bar's fraction: blobs are row-capped,
	// so their count tracks the work where the update count does not.
	BlobsDone       uint64 `json:"blobs_done"`
	BlobsTotal      uint64 `json:"blobs_total"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
	// DownloadRate counts fetch time only, pacing sleep included: a
	// disk-bound sync must not read as a slow network.
	DownloadRate  int64 `json:"download_rate"`
	ProcessRate   int64 `json:"process_rate"`
	LastAppliedAt int64 `json:"last_applied_at,omitempty"`
}

type Status struct {
	Enabled        bool           `json:"enabled"`
	State          State          `json:"state"`
	Progress       Progress       `json:"progress"`
	Counts         Counts         `json:"counts"`
	DiskBytes      int64          `json:"disk_bytes"`
	NextUpdateDue  int64          `json:"next_update_due,omitempty"`
	CoveredThrough int64          `json:"covered_through,omitempty"`
	Queryable      bool           `json:"queryable"`
	Error          string         `json:"error,omitempty"`
	Contrib        *ContribStatus `json:"contrib,omitempty"`
}

type Engine struct {
	cfg    config.PTRConfig
	client *client

	// Fields so tests can shrink them.
	retryBackoff time.Duration
	readyPoll    time.Duration
	entryBytes   int64

	baseCtx context.Context

	// Serializes enable, disable and delete end to end. Their slow parts run
	// off mu, so without it two enables both pass the store == nil guard.
	lifecycleMu sync.Mutex

	// Without it two concurrent creations both pass the no-key check and
	// mint two accounts.
	createMu sync.Mutex

	mu    sync.Mutex
	store *Store
	state State
	// State alone cannot tell a paused or failed first build from a
	// complete index.
	everReady     bool
	progress      Progress
	passBytes     int64
	passFetchDur  time.Duration
	passReplayDur time.Duration
	passRows      int64
	counts        Counts
	nextDue       int64
	covered       int64
	errMsg        string
	paused        bool
	cancel        context.CancelFunc
	wake          chan struct{} // buffered, so a nudge sent mid-pass is not lost
	done          chan struct{}
	account       *Account
	accountAt     time.Time
	contrib       *ContribStore
	tagFilter     *TagFilter
}

func NewEngine(cfg config.PTRConfig) *Engine {
	return &Engine{
		cfg:          cfg,
		client:       newClient(cfg),
		retryBackoff: 30 * time.Second,
		readyPoll:    10 * time.Minute,
		entryBytes:   maxEntryBytes,
		state:        StateDisabled,
		paused:       cfg.Paused,
	}
}

func (e *Engine) Start(ctx context.Context) {
	e.baseCtx = ctx
	if e.cfg.Enabled {
		if err := e.Enable(); err != nil {
			logx.Warnf("ptr: could not start sync: %v", err)
			e.setState(StateError, err.Error())
		}
	}
}

func (e *Engine) Enable() error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	e.mu.Lock()
	if e.store != nil {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	if err := CheckDataPath(e.cfg.DataPath); err != nil {
		return err
	}
	// Only the initial build needs the floor; a resume is past the big
	// download.
	if !IndexExists(e.cfg.DataPath) {
		if err := e.checkFreeSpace(); err != nil {
			return err
		}
	}
	store, err := OpenStore(e.cfg.DataPath)
	if err != nil {
		return err
	}
	// Another repository's updates resume at this one's cursor, and the
	// mixed index would read ready while missing what the new one maps.
	built, err := store.BuiltFrom(e.cfg.Address)
	if err == nil && strings.TrimRight(built, "/") != strings.TrimRight(e.cfg.Address, "/") {
		err = fmt.Errorf("the index in %s was built from %s; delete the ptr data to sync from %s", e.cfg.DataPath, built, e.cfg.Address)
	}
	if err != nil {
		_ = store.Close()
		return err
	}
	contrib, err := OpenContribStore(e.cfg.DataPath)
	if err != nil {
		// Contributions degrade without their store; the sync must not.
		logx.Warnf("ptr: contribution store unavailable: %v", err)
	}

	// A complete index opens ready: refusing lookups while this pass
	// polls for news would fail the work a restart just restored.
	complete, err := store.EverReady()
	if err != nil {
		logx.Warnf("ptr: reading the caught-up mark failed: %v", err)
	}
	state := StateSyncing
	if complete {
		state = StateReady
	}

	ctx, cancel := context.WithCancel(e.baseCtx)
	done := make(chan struct{})
	e.mu.Lock()
	e.store = store
	e.contrib = contrib
	e.state = state
	e.everReady = complete
	e.errMsg = ""
	e.cancel = cancel
	e.wake = make(chan struct{}, 1)
	e.done = done
	e.mu.Unlock()

	// Passed in, not read off e: Disable clears both fields before it waits.
	go e.loop(ctx, store, done)
	return nil
}

func (e *Engine) checkFreeSpace() error {
	if e.cfg.MinFreeGB <= 0 {
		return nil
	}
	_, free, err := desktop.DiskUsage(e.cfg.DataPath)
	if err != nil {
		return fmt.Errorf("checking free space on %s: %w", e.cfg.DataPath, err)
	}
	if free < uint64(e.cfg.MinFreeGB)*gib {
		return fmt.Errorf("only %.1f GiB free on %s, below the %d GiB floor for the initial sync",
			float64(free)/gib, e.cfg.DataPath, e.cfg.MinFreeGB)
	}
	return nil
}

func (e *Engine) Disable() {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.disableLocked()
}

// Caller must hold lifecycleMu.
func (e *Engine) disableLocked() {
	e.mu.Lock()
	cancel, done, store := e.cancel, e.done, e.store
	e.cancel, e.done = nil, nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
	e.mu.Lock()
	if store != nil {
		_ = store.Close()
	}
	if e.contrib != nil {
		_ = e.contrib.Close()
		e.contrib = nil
	}
	e.store = nil
	e.state = StateDisabled
	e.progress = Progress{}
	e.counts = Counts{}
	e.nextDue = 0
	e.covered = 0
	e.mu.Unlock()
}

func (e *Engine) Delete() error {
	// Held across the unlink, so an enable cannot open the index before
	// its files are gone.
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.disableLocked()
	e.mu.Lock()
	e.everReady = false
	e.mu.Unlock()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(indexPath(e.cfg.DataPath) + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return removeContribStore(e.cfg.DataPath)
}

// Pause and Resume set the state themselves rather than leave it to the
// loop: the page renders right after them, and only a syncing page polls.
// Held without an index too, so a retry after a boot-time open failure
// opens it paused, as the saved setting says.
func (e *Engine) Pause() {
	e.mu.Lock()
	e.paused = true
	if e.store != nil {
		e.state, e.errMsg = StatePaused, ""
		e.nudgeLocked()
	}
	e.mu.Unlock()
}

func (e *Engine) Resume() {
	e.mu.Lock()
	e.paused = false
	if e.state == StatePaused {
		e.state = StateSyncing
		if e.everReady {
			e.state = StateReady
		}
	}
	e.nudgeLocked()
	e.mu.Unlock()
}

// Retry re-opens the index after a boot-time open failure, which leaves
// no loop to nudge.
func (e *Engine) Retry() {
	e.mu.Lock()
	closed := e.store == nil
	e.mu.Unlock()
	if closed {
		if err := e.Enable(); err != nil {
			e.setState(StateError, err.Error())
		}
		return
	}
	e.Resume()
}

func (e *Engine) nudgeLocked() {
	if e.wake == nil {
		return
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) Enabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store != nil
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{
		Enabled:        e.store != nil,
		State:          e.state,
		Progress:       e.progress,
		Counts:         e.counts,
		NextUpdateDue:  e.nextDue,
		CoveredThrough: e.covered,
		Queryable:      e.store != nil && e.caughtUpLocked(),
		Error:          e.errMsg,
	}
	if e.store != nil {
		st.DiskBytes = indexDiskBytes(e.cfg.DataPath)
	}
	if e.state == StateSyncing {
		if secs := e.passFetchDur.Seconds(); secs > 0 {
			st.Progress.DownloadRate = int64(float64(e.passBytes) / secs)
		}
		if secs := e.passReplayDur.Seconds(); secs > 0 {
			st.Progress.ProcessRate = int64(float64(e.passRows) / secs)
		}
	}
	if st.Enabled {
		st.Contrib = e.contribStatusLocked()
	}
	return st
}

func (e *Engine) currentStore() *Store {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store
}

// DataPath is the path callers read, not their own config copy, which may
// name a different index than the open one.
func (e *Engine) DataPath() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.DataPath
}

// SetDataPath answers false under a running sync: an open index keeps its
// path until the next boot.
func (e *Engine) SetDataPath(path string) bool {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.store != nil {
		return e.cfg.DataPath == path
	}
	e.cfg.DataPath = path
	return true
}

// SetAddress repoints the sync while no index is open; it reports whether
// the running sync already uses addr.
func (e *Engine) SetAddress(addr string) bool {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.store != nil {
		return e.cfg.Address == addr
	}
	e.cfg.Address = addr
	e.client.setAddress(addr)
	return true
}

func (e *Engine) TagsForHash(hashHex string) (tags []string, ok bool, err error) {
	store := e.currentStore()
	if store == nil {
		return nil, false, nil
	}
	return store.TagsForHash(hashHex)
}

func (e *Engine) TagGraph(ctx context.Context, names []string) (map[string]TagInfo, error) {
	store := e.currentStore()
	if store == nil {
		return nil, nil
	}
	return store.TagGraph(ctx, names)
}

func (e *Engine) SearchTags(ctx context.Context, ranges []TagRange, scanCap int) ([]TagMatch, bool, error) {
	store := e.currentStore()
	if store == nil {
		return nil, false, nil
	}
	return store.SearchTags(ctx, ranges, scanCap)
}

func (e *Engine) loop(ctx context.Context, store *Store, done chan struct{}) {
	defer close(done)
	// Before the census seed, so the page never shows update 0 against a
	// populated index while it runs.
	e.seedPosition(store)
	// Here, not on the boot path: the scan can take minutes, and a
	// healthcheck that cannot reach /health kills the app.
	if err := store.EnsureCounts(ctx); err != nil && ctx.Err() == nil {
		logx.Warnf("ptr: seeding the row census failed: %v", err)
	}
	e.loadCounts(store)
	seeded := false
	for {
		if ctx.Err() != nil {
			return
		}
		if e.isPaused() {
			e.setState(StatePaused, "")
			if !e.waitWake(ctx, e.readyPoll) {
				return
			}
			continue
		}
		// After the pause check: the seed is a repository request.
		if !seeded {
			seeded = true
			if err := e.seedBlobBaseline(ctx, store); err != nil && ctx.Err() == nil {
				// Only the bar's blob fraction needs the baseline; it falls back
				// to the update fraction.
				logx.Warnf("ptr: seeding the blob census failed: %v", err)
			}
		}
		err := e.syncPass(ctx, store)
		switch {
		case ctx.Err() != nil:
			return
		case err == errPaused:
			continue
		case err != nil:
			e.setState(StateError, err.Error())
			logx.Warnf("ptr: sync pass failed: %v", err)
			backoff := e.retryBackoff
			if !IsRetryable(err) {
				// A permanent refusal will not heal: wait for a manual
				// retry rather than hammer the repository.
				backoff = 0
			}
			if !e.waitWake(ctx, backoff) {
				return
			}
		default:
			if err := store.MarkReady(); err != nil {
				logx.Warnf("ptr: recording the caught-up mark failed: %v", err)
			}
			e.setState(StateReady, "")
			e.trackContribOutcomes()
			if !e.waitWake(ctx, e.readyWait()) {
				return
			}
		}
	}
}

// Waiting for next_update_due keeps a caught-up client from polling the
// donated server every few minutes.
func (e *Engine) readyWait() time.Duration {
	e.mu.Lock()
	due := e.nextDue
	e.mu.Unlock()
	if due > 0 {
		return max(e.readyPoll, time.Until(time.Unix(due, 0)))
	}
	return e.readyPoll
}

var errPaused = fmt.Errorf("sync paused")

func (e *Engine) syncPass(ctx context.Context, store *Store) error {
	// A complete index stays ready until the manifest names updates to replay.
	if e.Provisional() {
		e.setState(StateSyncing, "")
	}
	e.markPassStart()
	for {
		since := uint64(0)
		if cur, ok, err := store.Cursor(); err != nil {
			return err
		} else if ok {
			since = cur + 1
		}
		slice, err := e.client.Metadata(ctx, since)
		if err != nil {
			return err
		}
		e.setNextDue(slice.NextUpdateDue)
		if len(slice.Updates) == 0 {
			return e.backfillCovered(ctx, store)
		}
		// The last entry is the newest published index; if it sits below
		// the cursor the manifest never empties, so the pass fails rather
		// than loop forever.
		last := slice.Updates[len(slice.Updates)-1].Index
		if last < since {
			return fmt.Errorf("repository manifest did not advance past update %d", since)
		}
		e.setState(StateSyncing, "")
		e.setProgressTotal(last + 1)
		if applied, ok, err := store.BlobsApplied(); err == nil && ok {
			var remaining uint64
			for _, entry := range slice.Updates {
				remaining += uint64(len(entry.Hashes))
			}
			e.setBlobProgress(applied, applied+remaining)
		}
		prev, havePrev := uint64(0), false
		for _, entry := range slice.Updates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e.isPaused() {
				return errPaused
			}
			// Replay moves the cursor to each entry's index, so an entry
			// out of order would drive it backward.
			if entry.Index < since || (havePrev && entry.Index <= prev) {
				return fmt.Errorf("repository manifest entry %d out of order (cursor %d)", entry.Index, since)
			}
			prev, havePrev = entry.Index, true
			if err := e.applyEntry(ctx, store, entry); err != nil {
				return err
			}
		}
	}
}

// seedBlobBaseline fills the blob census on an index synced before it was
// kept, from one manifest fetch.
func (e *Engine) seedBlobBaseline(ctx context.Context, store *Store) error {
	if _, ok, err := store.BlobsApplied(); err != nil || ok {
		return err
	}
	cur, ok, err := store.Cursor()
	if err != nil {
		return err
	}
	if !ok {
		return store.SeedBlobsApplied(0)
	}
	slice, err := e.client.Metadata(ctx, 0)
	if err != nil {
		return err
	}
	var n uint64
	for _, entry := range slice.Updates {
		if entry.Index <= cur {
			n += uint64(len(entry.Hashes))
		}
	}
	return store.SeedBlobsApplied(n)
}

// backfillCovered fills the coverage on an index synced before it was
// kept: a caught-up pass replays nothing, so nothing else would set it.
func (e *Engine) backfillCovered(ctx context.Context, store *Store) error {
	e.mu.Lock()
	covered := e.covered
	e.mu.Unlock()
	if covered > 0 {
		return nil
	}
	cur, ok, err := store.Cursor()
	if err != nil || !ok {
		return err
	}
	slice, err := e.client.Metadata(ctx, cur)
	if err != nil {
		return err
	}
	for _, entry := range slice.Updates {
		if entry.Index != cur || entry.End <= 0 {
			continue
		}
		if err := store.SetCoveredThrough(entry.End); err != nil {
			return err
		}
		e.mu.Lock()
		e.covered = entry.End
		e.mu.Unlock()
		return nil
	}
	return nil
}

// An entry is buffered whole before it replays. The count alone bounds no
// memory, as each blob may be maxUpdateBytes; both sit far above the real
// PTR.
const (
	maxEntryBlobs = 256
	maxEntryBytes = 1 << 30
)

func (e *Engine) applyEntry(ctx context.Context, store *Store, entry updateEntry) error {
	if len(entry.Hashes) > maxEntryBlobs {
		return fmt.Errorf("update %d lists %d blobs, over the %d cap", entry.Index, len(entry.Hashes), maxEntryBlobs)
	}
	blobs := make([][]byte, 0, len(entry.Hashes))
	var fetchDur time.Duration
	var buffered int64
	for _, h := range entry.Hashes {
		// An entry replays whole, so a pause drops what it buffered.
		if e.isPaused() {
			return errPaused
		}
		fetchStart := time.Now()
		raw, err := e.client.updateRaw(ctx, h)
		d := time.Since(fetchStart)
		if err != nil {
			return err
		}
		if buffered += int64(len(raw)); buffered > e.entryBytes {
			return fmt.Errorf("update %d buffers over the %d byte cap", entry.Index, e.entryBytes)
		}
		blobs = append(blobs, raw)
		fetchDur += d
		// Per blob, not per entry: a many-blob entry fetches for minutes.
		e.mu.Lock()
		e.progress.DownloadedBytes += int64(len(raw))
		e.passBytes += int64(len(raw))
		e.passFetchDur += d
		e.mu.Unlock()
	}
	replayStart := time.Now()
	counts, rows, err := store.Replay(ctx, entry.Index, entry.End, blobs, e.blobApplied)
	if err != nil {
		return err
	}
	replayDur := time.Since(replayStart)
	e.mu.Lock()
	e.progress.UpdateIndex = entry.Index
	e.progress.LastAppliedAt = time.Now().Unix()
	e.passRows += rows
	e.passReplayDur += replayDur
	e.counts = counts
	if entry.End > 0 {
		e.covered = entry.End
	}
	e.mu.Unlock()
	logx.Debugf("ptr: applied update %d: %d blobs, %d rows, fetch %s, replay %s",
		entry.Index, len(blobs), rows, fetchDur.Round(time.Millisecond), replayDur.Round(time.Millisecond))
	return nil
}

// Ticks from an entry that then rolls back are corrected at the next pass top.
func (e *Engine) blobApplied() {
	e.mu.Lock()
	if e.progress.BlobsTotal > 0 {
		e.progress.BlobsDone++
	}
	e.mu.Unlock()
}

func (e *Engine) isPaused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused
}

func (e *Engine) setState(s State, msg string) {
	e.mu.Lock()
	e.state = s
	if s == StateReady {
		e.everReady = true
	}
	e.errMsg = msg
	e.mu.Unlock()
}

func (e *Engine) setNextDue(due int64) {
	e.mu.Lock()
	e.nextDue = due
	e.mu.Unlock()
}

func (e *Engine) setProgressTotal(n uint64) {
	e.mu.Lock()
	e.progress.UpdateCount = n
	e.mu.Unlock()
}

func (e *Engine) setBlobProgress(done, total uint64) {
	e.mu.Lock()
	e.progress.BlobsDone = done
	e.progress.BlobsTotal = total
	e.mu.Unlock()
}

func (e *Engine) markPassStart() {
	e.mu.Lock()
	e.passBytes, e.passRows = 0, 0
	e.passFetchDur, e.passReplayDur = 0, 0
	e.mu.Unlock()
}

func (e *Engine) seedPosition(store *Store) {
	cur, ok, err := store.Cursor()
	if err != nil || !ok {
		return
	}
	e.mu.Lock()
	e.progress.UpdateIndex = cur
	e.mu.Unlock()
}

func (e *Engine) loadCounts(store *Store) {
	c, err := store.StoredCounts()
	if err != nil {
		return
	}
	covered, err := store.CoveredThrough()
	if err != nil {
		return
	}
	e.mu.Lock()
	e.counts = c
	e.covered = covered
	e.mu.Unlock()
}

func (e *Engine) waitWake(ctx context.Context, d time.Duration) bool {
	e.mu.Lock()
	wake := e.wake
	e.mu.Unlock()
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timeout:
		return true
	}
}

func FreeBytes(path string) int64 {
	for p := path; ; {
		if _, n, err := desktop.DiskUsage(p); err == nil {
			return int64(n)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0
		}
		p = parent
	}
}

func indexPath(dataPath string) string { return dataPath + "/" + dbFile }

// CheckDataPath never creates the folder: a missing path is an unmounted
// volume, and an index built there would die with the container.
func CheckDataPath(path string) error {
	if path == "" {
		return errors.New("no ptr data path is set")
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return fmt.Errorf("the ptr data path %s does not exist", path)
	}
	return nil
}

func IndexExists(dataPath string) bool {
	_, err := os.Stat(indexPath(dataPath))
	return err == nil
}

func indexDiskBytes(dataPath string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(indexPath(dataPath) + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}
