package ptr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/logx"
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
	UpdateIndex     uint64 `json:"update_index"`
	UpdateCount     uint64 `json:"update_count"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
	// DownloadRate is the current pass's average fetch speed in bytes per
	// second, reported only while syncing. Downloaded bytes are the compressed
	// wire size, so this tracks the network stream, not how fast the on-disk
	// index grows.
	DownloadRate int64 `json:"download_rate"`
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
}

// Engine owns the optional PTR sync: one background goroutine streaming the
// repository into the local index, plus the queries the lookup path runs
// against it. It is created for every run but does nothing until enabled.
type Engine struct {
	cfg    config.PTRConfig
	client *Client

	// retryBackoff and readyPoll are the wait after a failed pass and the
	// fallback poll when caught up; fields so tests can shorten them.
	retryBackoff time.Duration
	readyPoll    time.Duration

	baseCtx context.Context

	// enableMu serializes Enable end to end: its store-open runs off mu (it is
	// slow), and without this two concurrent enables would both pass the
	// store==nil guard and launch two sync loops against one index.
	enableMu sync.Mutex

	mu       sync.Mutex
	store    *Store
	state    State
	progress Progress
	// passStart and passStartBytes anchor the download-rate window to the start
	// of the current sync pass; a resume begins a new pass and so a new window,
	// which keeps paused time out of the rate.
	passStart      time.Time
	passStartBytes int64
	counts         Counts
	nextDue        int64
	covered        int64
	errMsg         string
	paused         bool
	cancel         context.CancelFunc
	wake           chan struct{} // buffered: nudges the loop to re-check (resume/retry)
	done           chan struct{} // closed when the goroutine exits
}

// NewEngine builds the engine from the PTR config.
func NewEngine(cfg config.PTRConfig) *Engine {
	return &Engine{
		cfg:          cfg,
		client:       NewClient(cfg),
		retryBackoff: 30 * time.Second,
		readyPoll:    10 * time.Minute,
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
	e.enableMu.Lock()
	defer e.enableMu.Unlock()

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
	if !indexExists(e.cfg.DataPath) {
		if err := e.checkFreeSpace(); err != nil {
			return err
		}
	}
	store, err := OpenStore(e.cfg.DataPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(e.baseCtx)
	e.mu.Lock()
	e.store = store
	e.state = StateSyncing
	e.errMsg = ""
	e.cancel = cancel
	e.wake = make(chan struct{}, 1)
	e.done = make(chan struct{})
	e.mu.Unlock()

	go e.loop(ctx)
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

// Delete stops the sync and removes the index files entirely.
func (e *Engine) Delete() error {
	e.Disable()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(indexPath(e.cfg.DataPath) + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
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
	if e.state == StateSyncing && !e.passStart.IsZero() {
		if secs := time.Since(e.passStart).Seconds(); secs > 0 {
			st.Progress.DownloadRate = int64(float64(e.progress.DownloadedBytes-e.passStartBytes) / secs)
		}
	}
	return st
}

// TagsForHash answers a PTR hash lookup, or ok=false when the backend is not
// available or the hash is unknown.
func (e *Engine) TagsForHash(hashHex string) (tags []string, ok bool, err error) {
	e.mu.Lock()
	store := e.store
	e.mu.Unlock()
	if store == nil {
		return nil, false, nil
	}
	return store.TagsForHash(hashHex)
}

// TagGraph answers alias / implication queries, or nil when disabled.
func (e *Engine) TagGraph(names []string) (map[string]TagInfo, error) {
	e.mu.Lock()
	store := e.store
	e.mu.Unlock()
	if store == nil {
		return nil, nil
	}
	return store.TagGraph(names)
}

// loop is the sync goroutine: drive to caught-up, then poll for the next
// update; back off and retry on error; hold while paused. It exits on ctx.
func (e *Engine) loop(ctx context.Context) {
	defer close(e.done)
	// The census seed can scan a 40 GB index for minutes; it belongs here on
	// the sync goroutine, after the boot path has already returned.
	if err := e.store.EnsureCounts(ctx); err != nil && ctx.Err() == nil {
		logx.Warnf("ptr: seeding the row census failed: %v", err)
	}
	e.loadCounts()
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
		err := e.syncPass(ctx)
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
			e.setState(StateReady, "")
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
func (e *Engine) syncPass(ctx context.Context) error {
	e.setState(StateSyncing, "")
	e.markPassStart()
	for {
		since := uint64(0)
		if cur, ok, err := e.store.Cursor(); err != nil {
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
			return e.backfillCovered(ctx) // caught up
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
		e.setProgressTotal(last + 1)
		for _, entry := range slice.Updates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e.isPaused() {
				return errPaused
			}
			if err := e.applyEntry(ctx, entry); err != nil {
				return err
			}
		}
	}
}

// backfillCovered fills the coverage timestamp on an index synced before it
// was recorded: its caught-up passes never replay an update, so nothing would
// set it until the server publishes the next one. The cursor's own manifest
// entry is re-fetched once for its window end - no blob is downloaded or
// re-applied. A no-op once the timestamp exists.
func (e *Engine) backfillCovered(ctx context.Context) error {
	e.mu.Lock()
	covered := e.covered
	e.mu.Unlock()
	if covered > 0 {
		return nil
	}
	cur, ok, err := e.store.Cursor()
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
		if err := e.store.SetCoveredThrough(entry.End); err != nil {
			return err
		}
		e.mu.Lock()
		e.covered = entry.End
		e.mu.Unlock()
		return nil
	}
	return nil
}

// maxEntryBlobs bounds how many blobs one manifest entry may list. A real
// index carries a couple, but the count is server-controlled and every blob
// (each up to the inflate cap) is buffered until the entry replays, so an
// unbounded entry could grow memory with the manifest.
const maxEntryBlobs = 64

// applyEntry fetches one update index's blobs, parses them, and replays them as
// one transaction, then advances the progress and the census Replay returned.
func (e *Engine) applyEntry(ctx context.Context, entry UpdateEntry) error {
	if len(entry.Hashes) > maxEntryBlobs {
		return fmt.Errorf("update %d lists %d blobs, over the %d cap", entry.Index, len(entry.Hashes), maxEntryBlobs)
	}
	updates := make([]*Update, 0, len(entry.Hashes))
	var bytes int64
	for _, h := range entry.Hashes {
		up, n, err := e.client.updateWithSize(ctx, h)
		if err != nil {
			return err
		}
		updates = append(updates, up)
		bytes += n
	}
	counts, err := e.store.Replay(entry.Index, entry.End, updates)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.progress.UpdateIndex = entry.Index
	e.progress.DownloadedBytes += bytes
	e.counts = counts
	if entry.End > 0 {
		e.covered = entry.End
	}
	e.mu.Unlock()
	return nil
}

func (e *Engine) isPaused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused
}

func (e *Engine) setState(s State, msg string) {
	e.mu.Lock()
	e.state = s
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

// markPassStart stamps the current pass's start time and byte baseline, the
// window Status divides to report the download rate.
func (e *Engine) markPassStart() {
	e.mu.Lock()
	e.passStart = time.Now()
	e.passStartBytes = e.progress.DownloadedBytes
	e.mu.Unlock()
}

// loadCounts publishes the census and coverage persisted in the index. The
// loop reads them once at start; from then on each Replay hands the updated
// values back, so no table is ever re-counted while the engine runs.
func (e *Engine) loadCounts() {
	e.mu.Lock()
	store := e.store
	e.mu.Unlock()
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

// freeBytes returns the bytes available to an unprivileged user on the volume
// holding path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// indexPath is the sqlite file path for a data dir.
func indexPath(dataPath string) string { return dataPath + "/" + dbFile }

// indexExists reports whether an index file is already present (a resume).
func indexExists(dataPath string) bool {
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
