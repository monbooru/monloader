package queue

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// defaultMaxFinished bounds the recent-history ring.
const defaultMaxFinished = 100

// ErrNotFound is returned by Get/Retry/Cancel for an unknown job id.
var ErrNotFound = errors.New("job not found")

// ErrNotCapped is returned by Continue for a job that did not hit the cap, so
// there is no next window to fetch.
var ErrNotCapped = errors.New("job was not capped")

// Options carries per-job settings supplied at enqueue time.
// Empty fields fall back to the configured defaults inside the processor.
type Options struct {
	Gallery  string
	Folder   string
	MaxItems int
	// Offset skips this many leading posts, so a continued capped search fetches
	// the next window rather than re-resolving from the start.
	Offset int
	// Root ties a continuation to its originating job's series; zero starts a
	// new series (the queue fills it with the new job's id).
	Root int64
	// Site pre-labels a continuation with its series' site: a window past the
	// last post resolves no items and never learns one, which would blank the
	// collapsed row's site cell. A fresh add leaves it for the resolve pass.
	Site string
	// Auto chains a "fetch all" run: a capped auto window enqueues the next one
	// itself until the source runs short.
	Auto bool
	// Priority marks a single-post / wait request so it jumps ahead of bulk
	// jobs in the FIFO.
	Priority bool
	// Kind + ImageID mark a metadata-only source refetch (see EnqueueMetadata);
	// a zero-value Kind is a normal download.
	Kind    JobKind
	ImageID int64
}

// Processor runs a job's full pipeline (resolve, download, map, push). It is
// injected so the queue is unit-testable with a fake. Process should return
// nil on normal completion even when some
// items failed (per-item failures live on the items); it returns an error
// only for a job-level abort such as a failed resolve.
type Processor interface {
	Process(ctx context.Context, job *Job) error
}

// Queue is the in-memory download queue: a FIFO of pending jobs, the set of
// running jobs, and a bounded ring of recently finished jobs, all guarded
// by a single mutex. Worker goroutines pull pending jobs and
// drive them through the injected Processor.
type Queue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending []*Job
	running map[int64]*Job
	cancels map[int64]context.CancelFunc
	// finished is the recent-history ring, oldest first; index maps every
	// tracked job id to its live *Job.
	finished    []*Job
	index       map[int64]*Job
	maxFinished int
	nextID      int64
	closed      bool
	// paused holds a global download pause: workers finish the job in flight
	// but pick up no new one while set, so submissions queue behind a manual
	// resume. It survives no restart (the queue itself is in memory).
	paused bool

	proc    Processor
	workers int
	wg      sync.WaitGroup

	// now is the clock, overridable in tests.
	now func() time.Time
}

// New builds a queue with the given worker count and recent-history bound.
// A workers value below 1 is snapped to 1; a maxFinished below 1 uses the
// default. Call Start to launch the workers.
func New(proc Processor, workers, maxFinished int) *Queue {
	if workers < 1 {
		workers = 1
	}
	if maxFinished < 1 {
		maxFinished = defaultMaxFinished
	}
	q := &Queue{
		running:     map[int64]*Job{},
		cancels:     map[int64]context.CancelFunc{},
		index:       map[int64]*Job{},
		maxFinished: maxFinished,
		proc:        proc,
		workers:     workers,
		now:         time.Now,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Workers returns the number of running worker goroutines. It is fixed when
// the queue is built, so changing downloader.concurrency takes effect only on
// restart.
func (q *Queue) Workers() int { return q.workers }

// Pause holds the queue: workers finish any job already running but start no
// new one until Resume. Submissions still enqueue and wait their turn.
func (q *Queue) Pause() {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
}

// Resume lifts a pause and wakes the idle workers so pending jobs run.
func (q *Queue) Resume() {
	q.mu.Lock()
	q.paused = false
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Paused reports whether the queue is holding new work.
func (q *Queue) Paused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

// Enqueue creates a queued job for url and returns its id. The job jumps
// ahead of bulk jobs when opts.Priority is set.
func (q *Queue) Enqueue(url string, opts Options) int64 {
	q.mu.Lock()
	q.nextID++
	id := q.nextID
	j := newJob(id, url, opts, q.now())
	if j.Root == 0 {
		j.Root = id
	}
	// A fresh add (not a continuation) of a URL a recent job failed to fully
	// import re-downloads past the archive, so a post whose push failed before is
	// re-pushed rather than archive-skipped - the recovery a retry already does.
	if opts.Root == 0 && q.lastRunUnimportedLocked(url) {
		j.Force = true
	}
	q.index[id] = j
	q.pushPendingLocked(j)
	q.cond.Broadcast()
	q.mu.Unlock()
	return id
}

// EnqueueMetadata queues a metadata-only source refetch: it re-reads url for
// its tags / commentary / notes and enriches monbooru image imageID (already in
// gallery) instead of downloading a new file. Prioritized so a single refetch
// does not wait behind a bulk download.
func (q *Queue) EnqueueMetadata(imageID int64, gallery, url string) int64 {
	return q.Enqueue(url, Options{Kind: KindMetadata, ImageID: imageID, Gallery: gallery, Priority: true})
}

// Get returns a snapshot of the job with the given id.
func (q *Queue) Get(id int64) (*Job, error) {
	q.mu.Lock()
	j := q.index[id]
	q.mu.Unlock()
	if j == nil {
		return nil, ErrNotFound
	}
	return j.Snapshot(), nil
}

// ListOptions filters and paginates List.
type ListOptions struct {
	Status JobStatus // "" = every status
	Limit  int       // 0 = no limit
	Page   int       // 1-based; <=1 means the first page
}

// List returns job snapshots newest-first, filtered by status and
// paginated, plus the total number of matching jobs (pre-pagination).
func (q *Queue) List(opts ListOptions) ([]*Job, int) {
	q.mu.Lock()
	all := make([]*Job, 0, len(q.index))
	for _, j := range q.index {
		all = append(all, j)
	}
	q.mu.Unlock()

	// Snapshot outside the queue lock, then filter on the stable copy.
	snaps := make([]*Job, 0, len(all))
	for _, j := range all {
		s := j.Snapshot()
		if opts.Status == "" || s.Status == opts.Status {
			snaps = append(snaps, s)
		}
	}
	sort.Slice(snaps, func(i, k int) bool { return snaps[i].ID > snaps[k].ID })

	total := len(snaps)
	if opts.Limit > 0 {
		page := opts.Page
		if page < 1 {
			page = 1
		}
		start := (page - 1) * opts.Limit
		if start >= len(snaps) {
			return []*Job{}, total
		}
		end := start + opts.Limit
		if end > len(snaps) {
			end = len(snaps)
		}
		snaps = snaps[start:end]
	}
	return snaps, total
}

// Retry re-queues a finished job by id, clearing its prior run. force re-runs
// it with the download-archive bypassed so already-fetched posts are fetched
// again.
func (q *Queue) Retry(id int64, force bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.index[id]
	if j == nil {
		return ErrNotFound
	}
	if err := j.reset(force); err != nil {
		return err
	}
	q.removeFromFinishedLocked(id)
	q.pushPendingLocked(j)
	q.cond.Broadcast()
	return nil
}

// Continue enqueues a follow-up job for the window after a capped job's, so the
// next batch of a truncated search comes down without re-resolving the part
// already fetched. It returns the new job id; the source must have been capped.
func (q *Queue) Continue(id int64) (int64, error) { return q.continueFrom(id, false) }

// ContinueAll is Continue marked auto: the queue keeps fetching each next
// window as one comes back capped, until a window returns short of the cap - a
// one-click "fetch the rest" past the per-job cap.
func (q *Queue) ContinueAll(id int64) (int64, error) { return q.continueFrom(id, true) }

func (q *Queue) continueFrom(id int64, auto bool) (int64, error) {
	src, err := q.Get(id)
	if err != nil {
		return 0, err
	}
	if !src.Capped {
		return 0, ErrNotCapped
	}
	// Advance past the furthest window the series has fetched, not just the one
	// this call names, so a continue from a non-latest window cannot re-fetch a
	// window an earlier one already took.
	return q.Enqueue(src.URL, Options{
		Gallery:  src.Gallery,
		Folder:   src.Folder,
		MaxItems: src.Cap,
		Offset:   q.seriesHighWater(src.seriesKey()),
		Root:     src.seriesKey(),
		Site:     src.Site,
		Auto:     auto,
	}), nil
}

// seriesHighWater returns the offset just past the furthest window any job in
// the series has fetched.
func (q *Queue) seriesHighWater(root int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	high := 0
	for _, j := range q.index {
		if j.seriesKey() != root {
			continue
		}
		if end := j.windowEnd(); end > high {
			high = end
		}
	}
	return high
}

// autoContinue advances a fetch-all chain once a window finishes: a capped auto
// window means more remain, so enqueue the next (also auto). The chain ends when
// a window comes back short of the cap, or when the user cancels or the window
// fails - a window keeps the capped flag its resolve set even if the download is
// then canceled, so the terminal status, not the flag alone, decides the next hop.
func (q *Queue) autoContinue(j *Job) {
	s := j.Snapshot()
	if !s.Auto || !s.Capped {
		return
	}
	if s.Status != JobSucceeded && s.Status != JobPartial {
		return
	}
	_, _ = q.continueFrom(s.ID, true)
}

// CancelSeries stops id's continue-series (the originating capped job and its
// continuations), so the collapsed queue row acts on them as one unit. A live
// cancel - the clicked window is still queued or running - stops the in-flight
// and pending windows but leaves the windows that already finished, so their
// imported items stay in the history; once every window has finished, a remove
// drops the whole series.
func (q *Queue) CancelSeries(id int64) error {
	q.mu.Lock()
	src := q.index[id]
	if src == nil {
		q.mu.Unlock()
		return ErrNotFound
	}
	root := src.seriesKey()
	live := q.isLiveLocked(id)
	var ids []int64
	for jid, j := range q.index {
		if j.seriesKey() != root {
			continue
		}
		if live && !q.isLiveLocked(jid) {
			continue // keep an already-finished window when stopping an ongoing series
		}
		ids = append(ids, jid)
	}
	q.mu.Unlock()
	for _, jid := range ids {
		_ = q.Cancel(jid)
	}
	return nil
}

// isLiveLocked reports whether the job is still queued or running, i.e. not yet
// moved to the finished ring. Caller holds mu.
func (q *Queue) isLiveLocked(id int64) bool {
	if _, ok := q.running[id]; ok {
		return true
	}
	for _, j := range q.pending {
		if j.ID == id {
			return true
		}
	}
	return false
}

// Cancel implements the DELETE contract: a running job is
// signalled to stop (it finalizes as canceled and stays in history); a
// pending or finished job is removed from tracking entirely.
func (q *Queue) Cancel(id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.index[id]
	if j == nil {
		return ErrNotFound
	}
	if cancel, ok := q.cancels[id]; ok {
		cancel()
		return nil
	}
	// Not running: drop it from the pending FIFO or the finished ring.
	q.removeFromPendingLocked(id)
	q.removeFromFinishedLocked(id)
	delete(q.index, id)
	return nil
}

// Wait blocks until the job reaches a terminal state or ctx is done,
// returning the final snapshot. Used by the API's POST /queue?wait=N path.
// A job that is already finished returns immediately.
func (q *Queue) Wait(ctx context.Context, id int64) (*Job, error) {
	q.mu.Lock()
	j := q.index[id]
	q.mu.Unlock()
	if j == nil {
		return nil, ErrNotFound
	}
	select {
	case <-j.doneChan():
		return j.Snapshot(), nil
	case <-ctx.Done():
		return j.Snapshot(), ctx.Err()
	}
}

// Clear drops every finished job from the recent-history ring, freeing the
// items they held. Running and pending jobs are left untouched.
func (q *Queue) Clear() {
	q.mu.Lock()
	for _, j := range q.finished {
		delete(q.index, j.ID)
	}
	q.finished = nil
	q.mu.Unlock()
}

// pushPendingLocked appends bulk jobs to the FIFO tail and inserts priority
// jobs after the last existing priority job, so priority jobs jump ahead of
// bulk work while preserving FIFO order within each class. Caller holds mu.
func (q *Queue) pushPendingLocked(j *Job) {
	if !j.Priority {
		q.pending = append(q.pending, j)
		return
	}
	i := 0
	for i < len(q.pending) && q.pending[i].Priority {
		i++
	}
	q.pending = append(q.pending, nil)
	copy(q.pending[i+1:], q.pending[i:])
	q.pending[i] = j
}

func (q *Queue) removeFromPendingLocked(id int64) {
	for i, j := range q.pending {
		if j.ID == id {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return
		}
	}
}

// lastRunUnimportedLocked reports whether the most recent finished job for url
// did not fully import (failed or partial), so its posts may sit in gallery-dl's
// archive without having reached monbooru. Caller holds mu.
func (q *Queue) lastRunUnimportedLocked(url string) bool {
	unimported := false
	for _, j := range q.finished {
		if j.URL != url {
			continue
		}
		st := j.status()
		unimported = st == JobFailed || st == JobPartial
	}
	return unimported
}

func (q *Queue) removeFromFinishedLocked(id int64) {
	for i, j := range q.finished {
		if j.ID == id {
			q.finished = append(q.finished[:i], q.finished[i+1:]...)
			return
		}
	}
}

// pushFinishedLocked appends a finished job to the ring and evicts the
// oldest entries past the bound, dropping them from the index too. Caller
// holds mu.
func (q *Queue) pushFinishedLocked(j *Job) {
	q.finished = append(q.finished, j)
	for len(q.finished) > q.maxFinished {
		evicted := q.finished[0]
		// Null the slot before reslicing: the evicted job (which can pin up to
		// max_items_per_job items) stays reachable in the backing array
		// otherwise, so the ring's footprint would sawtooth to ~2x the bound.
		q.finished[0] = nil
		q.finished = q.finished[1:]
		delete(q.index, evicted.ID)
	}
}
