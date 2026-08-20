package ptr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

// State is the sync engine's lifecycle state, surfaced verbatim by the status
// endpoint and the ptr page.
type State string

const (
	StateDisabled State = "disabled"
	StateSyncing  State = "syncing"
	StateReady    State = "ready"
	StatePaused   State = "paused"
	StateError    State = "error"
)

// gib is one gibibyte, for the free-space floor.
const gib = 1 << 30

// Progress reports how far an in-flight sync has come. UpdateIndex is the
// last applied update's absolute index and UpdateCount the server's published
// total, so index over count is a true fraction even on a resumed sync.
type Progress struct {
	UpdateIndex uint64 `json:"update_index"`
	UpdateCount uint64 `json:"update_count"`
	// BlobsDone over BlobsTotal is the volume-weighted fraction the progress
	// bar shows: blob count tracks the repository's row volume (blobs are
	// row-capped) where the update count does not, so the update fraction ran
	// far ahead of the real work. Zero until the applied-blob census is known.
	BlobsDone       uint64 `json:"blobs_done"`
	BlobsTotal      uint64 `json:"blobs_total"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
	// DownloadRate is the current pass's average fetch speed in bytes per
	// second, over time spent fetching only (the pacing sleep included, so it
	// is the effective stream rate). Replay time is excluded: a disk-bound
	// sync must not read as a stalled network.
	DownloadRate int64 `json:"download_rate"`
	// ProcessRate is rows replayed into the index per second of replay time.
	// Against DownloadRate it says which side - the network or the index
	// build - bounds a slow sync.
	ProcessRate int64 `json:"process_rate"`
	// LastAppliedAt dates the last entry this process committed: on a slow
	// disk an entry replays for an hour, and a page frozen that long reads as
	// a hang unless it can say the sync is still alive. Zero until one lands.
	LastAppliedAt int64 `json:"last_applied_at,omitempty"`
}

// Status is the engine snapshot the API and page render.
type Status struct {
	Enabled       bool     `json:"enabled"`
	State         State    `json:"state"`
	Progress      Progress `json:"progress"`
	Counts        Counts   `json:"counts"`
	DiskBytes     int64    `json:"disk_bytes"`
	NextUpdateDue int64    `json:"next_update_due,omitempty"`
	// CoveredThrough is the end of the last applied update's server-side time
	// window: how far in time the synced data reaches, which a caught-up state
	// alone does not say.
	CoveredThrough int64  `json:"covered_through,omitempty"`
	Error          string `json:"error,omitempty"`
	// Contrib is the contribution gate (personal account present, ban
	// state, unsent backlog); absent while the PTR is off.
	Contrib *ContribStatus `json:"contrib,omitempty"`
}

// Engine owns the optional PTR sync: one background goroutine streaming the
// repository into the local index, plus the queries the lookup path runs
// against it. It is created for every run but does nothing until enabled.
type Engine struct {
	cfg    config.PTRConfig
	client *Client

	// retryBackoff and readyPoll are the wait after a failed pass and the
	// fallback poll when caught up, entryBytes the budget one manifest entry
	// may buffer; fields so tests can shrink them.
	retryBackoff time.Duration
	readyPoll    time.Duration
	entryBytes   int64

	baseCtx context.Context

	// lifecycleMu serializes enable / disable / delete end to end: their slow
	// parts (opening the store, waiting for the goroutine to exit, unlinking
	// the files) run off mu, and without this two concurrent enables would both
	// pass the store==nil guard, or an enable landing inside a disable's wait
	// would leave a sync goroutine the engine no longer tracks.
	lifecycleMu sync.Mutex

	// createMu serializes account auto-creation so two concurrent calls can't
	// both pass the no-key check and mint two accounts under one operator.
	createMu sync.Mutex

	mu    sync.Mutex
	store *Store
	state State
	// everReady records that the index reached the caught-up state at least
	// once, so a partial initial sync that is now paused or errored still
	// reads as provisional while a fully-synced index never does.
	everReady bool
	progress  Progress
	// The rate window: bytes, fetch and replay time, and rows since the
	// current sync pass began. A resume begins a new pass and so a new
	// window, which keeps paused time out of the rates.
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
	wake          chan struct{} // buffered: nudges the loop to re-check (resume/retry)
	done          chan struct{} // closed when the goroutine exits
	// account is the last-fetched contribution account state, cached so
	// the page poll and the status gate read it without a network trip.
	account   *Account
	accountAt time.Time
	// contrib is the staged-contributions store, opened beside the index.
	contrib *ContribStore
	// tagFilter caches the repository's filter from the last commit's
	// fetch, feeding the preview's filtered verdicts without a request.
	tagFilter *TagFilter
}

// NewEngine builds the engine from the PTR config.
func NewEngine(cfg config.PTRConfig) *Engine {
	return &Engine{
		cfg:          cfg,
		client:       NewClient(cfg),
		retryBackoff: 30 * time.Second,
		readyPoll:    10 * time.Minute,
		entryBytes:   maxEntryBytes,
		state:        StateDisabled,
	}
}

// Start records the base context and, when the config has the PTR enabled,
// begins syncing. Called once at boot.
func (e *Engine) Start(ctx context.Context) {
	e.baseCtx = ctx
	if e.cfg.Enabled {
		if err := e.Enable(); err != nil {
			// A boot-time open failure (corrupt index, unwritable path, disk
			// below the floor) has no sync goroutine to report it, so surface
			// it on the page's state instead of only the log; the retry button
			// re-opens via Retry.
			logx.Warnf("ptr: could not start sync: %v", err)
			e.setState(StateError, err.Error())
		}
	}
}

// Enable opens the index and launches the sync goroutine. It refuses to begin
// an initial sync when the data volume has less than the configured free-space
// floor, so a multi-tens-of-GB stream cannot fill the disk. Idempotent.
func (e *Engine) Enable() error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	e.mu.Lock()
	if e.store != nil {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	if err := os.MkdirAll(e.cfg.DataPath, 0o755); err != nil {
		return fmt.Errorf("creating ptr data dir: %w", err)
	}
	// The floor only guards the initial build; an existing index (a resume) is
	// past the big download and may legitimately sit on a fuller disk.
	if !IndexExists(e.cfg.DataPath) {
		if err := e.checkFreeSpace(); err != nil {
			return err
		}
	}
	store, err := OpenStore(e.cfg.DataPath)
	if err != nil {
		return err
	}
	contrib, err := OpenContribStore(e.cfg.DataPath)
	if err != nil {
		// Contributions degrade without their store; the sync must not.
		logx.Warnf("ptr: contribution store unavailable: %v", err)
	}

	// A complete index opens ready: it answers everything it answered before
	// the process stopped, and the pass below is only checking for new
	// updates. Refusing for the length of that check would fail the work a
	// restart just restored.
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

	// The store and the done channel are handed over rather than read back off
	// the engine: Disable clears both before it waits, so a loop reading them
	// through the fields would signal the wrong channel and outlive its index.
	go e.loop(ctx, store, done)
	return nil
}

// checkFreeSpace refuses the initial sync below the free-space floor.
func (e *Engine) checkFreeSpace() error {
	if e.cfg.MinFreeGB <= 0 {
		return nil
	}
	free, err := freeBytes(e.cfg.DataPath)
	if err != nil {
		return fmt.Errorf("checking free space on %s: %w", e.cfg.DataPath, err)
	}
	if free < uint64(e.cfg.MinFreeGB)*gib {
		return fmt.Errorf("only %.1f GB free on %s, below the %d GB floor for the initial sync",
			float64(free)/gib, e.cfg.DataPath, e.cfg.MinFreeGB)
	}
	return nil
}

// Disable stops the sync goroutine and closes the index, keeping the files so a
// later Enable resumes. Idempotent.
func (e *Engine) Disable() {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.disableLocked()
}

// disableLocked is Disable's body; the caller holds lifecycleMu.
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
	// The census, due time, and coverage describe the closed index; a status
	// snapshot of a disabled engine must not keep advertising them.
	e.counts = Counts{}
	e.nextDue = 0
	e.covered = 0
	e.mu.Unlock()
}

// Delete stops the sync and removes the index files entirely, the
// contribution store included: pending and historical contributions go
// with the data, as the settings confirm warns.
func (e *Engine) Delete() error {
	// Held across the unlink too, so an enable cannot open a fresh index in the
	// window between the sync stopping and its files going away.
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.disableLocked()
	// The caught-up mark described the index being removed; a re-enable builds
	// a new one from nothing, and its partial answers are provisional again.
	e.mu.Lock()
	e.everReady = false
	e.mu.Unlock()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(indexPath(e.cfg.DataPath) + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return RemoveContribStore(e.cfg.DataPath)
}

// Pause holds the sync after the slice in flight; Resume lifts it. The nudge
// wakes a loop parked until next_update_due so the paused state shows now, not
// at the next poll.
func (e *Engine) Pause() {
	e.mu.Lock()
	if e.store != nil {
		e.paused = true
		e.nudgeLocked()
	}
	e.mu.Unlock()
}

// Resume lifts a pause and wakes the loop. Retry is its alias for the error
// state: both just nudge the loop to try again now.
func (e *Engine) Resume() {
	e.mu.Lock()
	e.paused = false
	e.nudgeLocked()
	e.mu.Unlock()
}

// Retry re-attempts a failed sync now. A boot-time open failure left the index
// closed with no goroutine to nudge, so it re-opens the index; a running loop
// parked on an error is just woken.
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

// Enabled reports whether the index is open (syncing, ready, paused, or error).
func (e *Engine) Enabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store != nil
}

// Status snapshots the engine for the API and page.
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

// currentStore is the open index or nil, for the readers that answer "nothing
// known" while the PTR is off rather than erroring.
func (e *Engine) currentStore() *Store {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store
}

// DataPath is the index directory this run opened. The engine snapshots its
// config at boot, so a path edited in settings only takes effect on the next
// one; a caller that reads its own copy would describe a different index than
// the one the status, the disk figure and delete all act on.
func (e *Engine) DataPath() string { return e.cfg.DataPath }

// TagsForHash answers a PTR hash lookup, or ok=false when the backend is not
// available or the hash is unknown.
func (e *Engine) TagsForHash(hashHex string) (tags []string, ok bool, err error) {
	store := e.currentStore()
	if store == nil {
		return nil, false, nil
	}
	return store.TagsForHash(hashHex)
}

// TagGraph answers alias / implication queries, or nil when disabled.
func (e *Engine) TagGraph(ctx context.Context, names []string) (map[string]TagInfo, error) {
	store := e.currentStore()
	if store == nil {
		return nil, nil
	}
	return store.TagGraph(ctx, names)
}

// SearchTags answers spelling searches, or nothing when disabled.
func (e *Engine) SearchTags(ctx context.Context, ranges []TagRange, scanCap int) ([]TagMatch, bool, error) {
	store := e.currentStore()
	if store == nil {
		return nil, false, nil
	}
	return store.SearchTags(ctx, ranges, scanCap)
}

// loop is the sync goroutine: drive to caught-up, then poll for the next
// update; back off and retry on error; hold while paused. It exits on ctx.
func (e *Engine) loop(ctx context.Context, store *Store, done chan struct{}) {
	defer close(done)
	// Seed the position before the census scan below: on a first boot that
	// scan can run for minutes, and the page would otherwise claim update 0
	// against a populated index for its whole duration.
	e.seedPosition()
	// The census seed can scan a 40 GB index for minutes; it belongs here on
	// the sync goroutine, after the boot path has already returned.
	if err := store.EnsureCounts(ctx); err != nil && ctx.Err() == nil {
		logx.Warnf("ptr: seeding the row census failed: %v", err)
	}
	e.loadCounts()
	if err := e.seedBlobBaseline(ctx, store); err != nil && ctx.Err() == nil {
		// Without the baseline the pass skips the blob fraction and the bar
		// falls back to the update fraction; nothing else degrades.
		logx.Warnf("ptr: seeding the blob census failed: %v", err)
	}
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
		err := e.syncPass(ctx, store)
		switch {
		case ctx.Err() != nil:
			return
		case err == errPaused:
			continue // pause was requested mid-pass; the top handles it
		case err != nil:
			e.setState(StateError, err.Error())
			logx.Warnf("ptr: sync pass failed: %v", err)
			backoff := e.retryBackoff
			if !IsRetryable(err) {
				// A permanent refusal (bad access key, malformed blob) will not
				// heal on its own; hold for the page's manual retry instead of
				// re-hammering the repository on every backoff.
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

// readyWait is how long a caught-up loop sleeps before the next /metadata
// poll: until the server's published next_update_due (roughly daily for the
// real PTR, so a caught-up client does not poll the donated server every few
// minutes), floored at readyPoll so a past or absent due time still re-polls
// on the fallback cadence.
func (e *Engine) readyWait() time.Duration {
	e.mu.Lock()
	due := e.nextDue
	e.mu.Unlock()
	d := e.readyPoll
	if due > 0 {
		if until := time.Until(time.Unix(due, 0)); until > d {
			d = until
		}
	}
	return d
}

// errPaused signals that a pass stopped early because a pause was requested.
var errPaused = fmt.Errorf("sync paused")

// syncPass fetches the manifest from the cursor onward and replays every
// pending update index in order, stopping early if paused or canceled. It
// returns nil once caught up (an empty manifest).
func (e *Engine) syncPass(ctx context.Context, store *Store) error {
	// An index still being built is syncing for the whole pass. A complete one
	// stays ready until the manifest below actually names updates to replay,
	// so a poll - at boot or on the daily cadence - does not refuse the
	// lookups the index can already answer.
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
			return e.backfillCovered(ctx, store) // caught up
		}
		// The manifest runs from the cursor through the newest published index,
		// so the last entry names the server's total. A manifest whose newest
		// index sits below the cursor can never empty the loop; failing the
		// pass (error state, backoff) beats re-hammering a buggy or hostile
		// repository forever.
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
			// Replay must run in ascending index order (each pass advances the
			// cursor to the entry's index): a manifest entry below the cursor
			// or out of order would drive the cursor backward, so fail the
			// pass instead of applying it.
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

// seedBlobBaseline fills the applied-blob census on an index synced before it
// was recorded: without an anchor the blob fraction cannot be computed, so the
// cursor's manifest is re-fetched once from zero and the already-applied
// entries' blobs are counted - nothing is downloaded or re-applied. A no-op
// once the census exists; a fresh index starts it at zero.
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

// backfillCovered fills the coverage timestamp on an index synced before it
// was recorded: its caught-up passes never replay an update, so nothing would
// set it until the server publishes the next one. The cursor's own manifest
// entry is re-fetched once for its window end - no blob is downloaded or
// re-applied. A no-op once the timestamp exists.
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

// maxEntryBlobs and maxEntryBytes bound one manifest entry, which is buffered
// compressed in full before any of it replays. The count alone is not a memory
// bound - each blob may be maxUpdateBytes - so the byte budget is what actually
// guards against a hostile manifest, and it is set far above the real PTR (a
// couple of MB per blob, entries past 100 blobs).
const (
	maxEntryBlobs = 256
	maxEntryBytes = 1 << 30
)

// applyEntry fetches one update index's blobs and replays them as one
// transaction, then advances the progress, the census, and the rate window
// with what Replay returned.
func (e *Engine) applyEntry(ctx context.Context, store *Store, entry UpdateEntry) error {
	if len(entry.Hashes) > maxEntryBlobs {
		return fmt.Errorf("update %d lists %d blobs, over the %d cap", entry.Index, len(entry.Hashes), maxEntryBlobs)
	}
	blobs := make([][]byte, 0, len(entry.Hashes))
	var fetchDur time.Duration
	var buffered int64
	for _, h := range entry.Hashes {
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
		// Credited per blob, not per entry: a many-blob entry fetches for
		// minutes, and a byte counter frozen that long reads as a hang.
		e.mu.Lock()
		e.progress.DownloadedBytes += int64(len(raw))
		e.passBytes += int64(len(raw))
		e.passFetchDur += d
		e.mu.Unlock()
	}
	replayStart := time.Now()
	counts, rows, err := store.Replay(entry.Index, entry.End, blobs, e.blobApplied)
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

// blobApplied advances the bar as Replay lands each blob: a single large
// entry can replay for an hour on an HDD, and the bar must move within it.
// A tick rolled back with a failed entry is corrected by the census resync
// at the next pass top.
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

// markPassStart resets the rate window Status divides to report the pass's
// download and processing rates.
func (e *Engine) markPassStart() {
	e.mu.Lock()
	e.passBytes, e.passRows = 0, 0
	e.passFetchDur, e.passReplayDur = 0, 0
	e.mu.Unlock()
}

// seedPosition publishes the persisted cursor as the progress position so a
// resumed sync shows where it stopped from the first render, instead of
// claiming update 0 until its first entry commits - hours, on a large entry.
func (e *Engine) seedPosition() {
	store := e.currentStore()
	if store == nil {
		return
	}
	cur, ok, err := store.Cursor()
	if err != nil || !ok {
		return
	}
	e.mu.Lock()
	e.progress.UpdateIndex = cur
	e.mu.Unlock()
}

// loadCounts publishes the census and coverage persisted in the index. The
// loop reads them once at start; from then on each Replay hands the updated
// values back, so no table is ever re-counted while the engine runs.
func (e *Engine) loadCounts() {
	store := e.currentStore()
	if store == nil {
		return
	}
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

// waitWake sleeps up to d (with no deadline when d <= 0), returning early
// (true) when nudged (resume / retry), or false when the context is done.
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

// FreeBytes returns the bytes available on the volume holding path, or 0 when
// it cannot be determined. The ptr page shows it so an operator sees the
// headroom before enabling the sync; the data dir is only created on the first
// enable, so a path that does not exist yet falls back to its nearest existing
// ancestor (the same volume the dir would land on).
func FreeBytes(path string) int64 {
	for p := path; ; {
		if n, err := freeBytes(p); err == nil {
			return int64(n)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0
		}
		p = parent
	}
}

// indexPath is the sqlite file path for a data dir.
func indexPath(dataPath string) string { return dataPath + "/" + dbFile }

// IndexExists reports whether an index file is already present, so a caller can
// tell a resume from a first sync: the ptr page words its disabled state on it,
// and Enable applies the free-space floor only to a first sync.
func IndexExists(dataPath string) bool {
	_, err := os.Stat(indexPath(dataPath))
	return err == nil
}

// indexDiskBytes sums the sqlite file and its WAL/SHM sidecars.
func indexDiskBytes(dataPath string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(indexPath(dataPath) + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}
