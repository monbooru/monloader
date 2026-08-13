package queue

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/logx"
)

// defaultMaxFinished bounds the recent-history ring.
const defaultMaxFinished = 100

// ErrNotFound is returned by Get/Retry/Cancel for an unknown job id.
var ErrNotFound = errors.New("job not found")

// ErrNotCapped is returned by Continue for a job that did not hit the cap, so
// there is no next window to fetch.
var ErrNotCapped = errors.New("job was not capped")

// ErrNotRunning is returned by CancelLive for a job that has already finished,
// so a cancel never falls through to a removal.
var ErrNotRunning = errors.New("job is not running")

// Options carries per-job settings supplied at enqueue time.
// Empty fields fall back to the configured defaults inside the processor.
type Options struct {
	Gallery  string
	Folder   string
	MaxItems int
	// PageURL is the page a direct-file send came from; a directlink item
	// records it as its source link instead of the bare file URL.
	PageURL string
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
	// Background and Budgeted classify a lookup monbooru did not send on
	// behalf of someone watching an image page: Background puts it behind
	// the priority lane, Budgeted also spends a slot of the daily budget.
	// They are independent - a bulk lookup the operator started is
	// background but never refused.
	Background bool
	Budgeted   bool
	// Kind + ImageID mark a metadata-only source refetch (see EnqueueMetadata);
	// a zero-value Kind is a normal download.
	Kind    JobKind
	ImageID int64
	// Backend, MD5, and SHA256 parameterize the hash-keyed kinds (see
	// EnqueueLookup and EnqueueHashImport).
	Backend string
	MD5     string
	SHA256  string
	// ContribIDs / ContribBacklog parameterize a KindContrib send: the
	// staged rows the job claims, or the whole unsent set.
	ContribIDs     []int64
	ContribBacklog bool
}

// Processor runs a job's full pipeline (resolve, download, map, push). It is
// injected so the queue is unit-testable with a fake. Process should return
// nil on normal completion even when some
// items failed (per-item failures live on the items); it returns an error
// only for a job-level abort such as a failed resolve.
type Processor interface {
	Process(ctx context.Context, job *Job) error
}

// DropReporter is the optional half of Processor: a job removed before it
// ran produces no callback of its own, so monbooru would wait on a result
// that is never coming. A Processor implementing this is asked to tell it
// the attempt is over.
type DropReporter interface {
	ReportDropped(ctx context.Context, job *Job)
}

// dropReportTimeout bounds the whole reporting effort, whether it is one
// cancelled job or a shutdown's worth of pending ones. Shutdown must not
// block on an unreachable monbooru.
const dropReportTimeout = 3 * time.Second

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
	// retention drops finished jobs older than this from the ring; zero keeps
	// them until the bound evicts them. successRetention is the shorter window
	// for jobs that left nothing to act on.
	retention        time.Duration
	successRetention time.Duration
	nextID           int64
	closed           bool
	// paused holds a global download pause: workers finish the job in flight
	// but pick up no new one while set, so submissions queue behind a manual
	// resume. It survives no restart (the queue itself is in memory).
	paused bool

	// The daily budget for scheduled lookups: the published cap, and the
	// spend stamped with the local date it belongs to. Mirrored to the
	// store, unlike the pause, so a restart cannot refill it (budget.go).
	budgetLimit int
	budgetDay   string
	budgetSpent int
	// budgetWrite pairs a take with its mirror write. The counter row holds an
	// absolute value, so two takes that left q.mu in one order and reached the
	// store in the other would leave the stored spend one short.
	budgetWrite sync.Mutex

	proc    Processor
	workers int
	wg      sync.WaitGroup

	// store is the durable mirror, nil when /config is unavailable.
	// Written to only outside q.mu so a slow disk never stalls the
	// queue's readers; the in-memory state stays authoritative.
	store *Store

	// now is the clock, overridable in tests.
	now func() time.Time
}

// New builds a queue with the given worker count and recent-history bound.
// A workers value below 1 is snapped to 1; a maxFinished below 1 uses the
// default. Call Start to launch the workers.
func New(proc Processor, workers, maxFinished int) *Queue {
	workers = max(workers, 1)
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

// UseStore attaches the durable mirror and rehydrates from it. Call
// before Start: finished rows reload into the history ring, queued rows
// re-enter the FIFO (processing resumes where it left off), and rows
// that were running when the process died come back as interrupted -
// requeueable with one click, never auto-resumed, because silently
// restarting a large job on every crash would surprise the operator.
func (q *Queue) UseStore(st *Store) {
	jobs, err := st.LoadJobs()
	if err != nil {
		logx.Warnf("queue: store reload failed, history lost: %v", err)
	}
	// The jobs are exclusively owned until placed in the index below, so
	// the terminal mutations need no locks.
	var interrupted []*Job
	now := q.now()
	for _, j := range jobs {
		switch j.Status {
		case JobQueued:
		case JobRunning:
			j.Status = JobInterrupted
			j.FinishedAt = now
			j.StatusChangedAt = now
			j.finalized = true
			interrupted = append(interrupted, j)
		default:
			j.finalized = true
		}
		if j.finalized {
			close(j.done)
		}
	}
	// Read the budget counter here too, where the store is still being read
	// outside the lock: the take path must never reach the disk under q.mu.
	budgetDay, budgetSpent, err := st.LoadCounter(lookupBudgetCounter)
	if err != nil {
		logx.Warnf("queue: budget counter reload failed: %v", err)
		budgetDay, budgetSpent = "", 0
	}
	var evicted []int64
	q.mu.Lock()
	q.store = st
	q.budgetDay, q.budgetSpent = budgetDay, budgetSpent
	for _, j := range jobs {
		if j.ID > q.nextID {
			q.nextID = j.ID
		}
		q.index[j.ID] = j
		if j.Status == JobQueued {
			q.pushPendingLocked(j)
		} else {
			evicted = append(evicted, q.pushFinishedLocked(j)...)
		}
	}
	q.mu.Unlock()
	q.persistDelete(evicted)
	for _, j := range interrupted {
		q.persist(j)
	}
}

// persist mirrors one job's current state to the store, best-effort.
// Never called with q.mu held.
func (q *Queue) persist(j *Job) {
	if q.store == nil {
		return
	}
	if err := q.store.SaveJob(j.Snapshot()); err != nil {
		logx.Warnf("queue: store write for job %d failed: %v", j.ID, err)
	}
}

// persistDelete drops rows from the store, best-effort. Never called
// with q.mu held.
func (q *Queue) persistDelete(ids []int64) {
	if q.store == nil || len(ids) == 0 {
		return
	}
	if err := q.store.DeleteJobs(ids); err != nil {
		logx.Warnf("queue: store delete failed: %v", err)
	}
}

// SetRetention bounds how long a finished job stays in the recent-history
// ring; zero or less keeps every job the bound allows. Settable rather than
// fixed at New so a settings save takes effect without a restart.
func (q *Queue) SetRetention(d time.Duration) {
	q.mu.Lock()
	q.retention = d
	q.mu.Unlock()
}

// SetSuccessRetention bounds how long a finished job that imported everything
// and has no window left stays in the ring; zero leaves it to SetRetention.
func (q *Queue) SetSuccessRetention(d time.Duration) {
	q.mu.Lock()
	q.successRetention = d
	q.mu.Unlock()
}

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
	j := q.enqueueLocked(url, opts)
	q.mu.Unlock()
	q.persist(j)
	return j.ID
}

// enqueueLocked creates and queues the job. Caller holds mu and persists
// the returned job after unlocking.
func (q *Queue) enqueueLocked(url string, opts Options) *Job {
	q.nextID++
	id := q.nextID
	j := newJob(id, url, opts, q.now())
	if j.Root == 0 {
		j.Root = id
	}
	// A fresh add (not a continuation) of a URL a recent job failed to fully
	// import re-downloads past the archive, so a post whose push failed before is
	// re-pushed rather than archive-skipped - the recovery a retry already does.
	// Hash-keyed jobs enqueue with no URL, which must not match one another here.
	if opts.Root == 0 && url != "" && q.lastRunUnimportedLocked(url) {
		j.Force = true
	}
	q.index[id] = j
	q.pushPendingLocked(j)
	q.cond.Broadcast()
	return j
}

// EnqueueMetadata queues a metadata-only source refetch: it re-reads url for
// its tags / commentary / notes and enriches monbooru image imageID (already in
// gallery) instead of downloading a new file. Prioritized so a single refetch
// does not wait behind a bulk download.
func (q *Queue) EnqueueMetadata(imageID int64, gallery, url string) int64 {
	return q.Enqueue(url, Options{Kind: KindMetadata, ImageID: imageID, Gallery: gallery, Priority: true})
}

// EnqueueLookup queues a hash lookup: find tags for the hash on the chosen
// backend and enrich monbooru image imageID. Prioritized like a metadata
// refetch - it answers a person waiting on an image page - unless background
// marks it as bulk or unattended work, which then waits behind anything
// someone is watching.
func (q *Queue) EnqueueLookup(imageID int64, gallery, backend, md5, sha256 string, background, budgeted bool) int64 {
	return q.Enqueue("", Options{
		Kind: KindLookup, ImageID: imageID, Gallery: gallery,
		Backend: backend, MD5: md5, SHA256: sha256, Priority: !background,
		Background: background, Budgeted: budgeted,
	})
}

// EnqueueReplace queues an in-place file replacement: download the post url
// names and push its file into monbooru image imageID, replacing its bytes.
// Prioritized like a metadata refetch - it answers a person waiting on an
// image page.
func (q *Queue) EnqueueReplace(imageID int64, gallery, url string) int64 {
	return q.Enqueue(url, Options{Kind: KindReplace, ImageID: imageID, Gallery: gallery, Priority: true})
}

// EnqueueHashImport queues a hash import: resolve the md5 to a post via the
// lookup walk, then download and push it like a submitted single post.
func (q *Queue) EnqueueHashImport(md5 string, opts Options) int64 {
	opts.Kind = KindHashImport
	opts.MD5 = md5
	return q.Enqueue("", opts)
}

// RecordPTRLookup files a batch PTR hash lookup in the history. That endpoint
// answers in process and never enqueues, so without a row a caller can check
// a whole library against the index and leave no trace of having asked.
// scheduled marks a sweep the nightly run started, which the row names the
// way it names a queued lookup's lane. matched lists the images the index
// answered for, one item each.
func (q *Queue) RecordPTRLookup(gallery string, scheduled bool, asked int, matched []Item) {
	now := q.now()
	q.mu.Lock()
	if j := q.recentPTRLookupLocked(now, scheduled); j != nil {
		q.mu.Unlock()
		j.recordPTRLookup(gallery, asked, matched, now)
		q.persist(j)
		return
	}
	q.nextID++
	j := newJob(q.nextID, "", Options{
		Kind: KindPTRLookup, Gallery: gallery, Background: true, Budgeted: scheduled,
	}, now)
	j.Root = j.ID
	// Settled before anything can see it: the row is history from the moment it
	// exists, and the retention sweep every List runs reads a zero FinishedAt
	// as older than any cutoff.
	j.recordPTRLookup(gallery, asked, matched, now)
	q.index[j.ID] = j
	evicted := q.pushFinishedLocked(j)
	q.mu.Unlock()
	q.persist(j)
	q.persistDelete(evicted)
}

// recentPTRLookupLocked returns today's batch-lookup row for the same class,
// or nil. The day, rather than a timer, is the unit a sweep is measured in: a
// caller that paces its batches over hours still files one row, while a
// nightly sweep and a bulk one keep the separate labels they were asked
// under, and the history can never hold more than one row per class per day.
// Caller holds mu.
func (q *Queue) recentPTRLookupLocked(now time.Time, scheduled bool) *Job {
	day := now.Format(time.DateOnly)
	for i := len(q.finished) - 1; i >= 0; i-- {
		j := q.finished[i]
		if j.Kind == KindPTRLookup && j.Budgeted == scheduled && j.CreatedAt.Format(time.DateOnly) == day {
			return j
		}
	}
	return nil
}

// EnqueueContrib queues a PTR contribution send over the given staged
// row ids, or the whole unsent backlog when backlog is set. Prioritized:
// a monbooru confirm is waiting on it.
func (q *Queue) EnqueueContrib(ids []int64, backlog bool) int64 {
	return q.Enqueue("", Options{Kind: KindContrib, ContribIDs: ids, ContribBacklog: backlog, Priority: true})
}

// ContribSendLive reports whether a contribution send is queued or
// running - the backlog retry's already_running guard (per-confirm
// sends claim disjoint rows and need none).
func (q *Queue) ContribSendLive() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.running {
		if j.Kind == KindContrib {
			return true
		}
	}
	for _, j := range q.pending {
		if j.Kind == KindContrib {
			return true
		}
	}
	return false
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
	// OmitItems leaves each snapshot's item list empty (the summary is still
	// computed), for a caller that groups and pages the rows first and then
	// fetches the items of the page it renders.
	OmitItems bool
}

// List returns job snapshots newest-first, filtered by status and
// paginated, plus the total number of matching jobs (pre-pagination).
func (q *Queue) List(opts ListOptions) ([]*Job, int) {
	q.mu.Lock()
	// Every caller that shows history reads it through here, so aging the ring
	// on the way out keeps expired jobs from surfacing without a sweep timer of
	// its own; the queue view's poll drives it often enough.
	swept := q.sweepFinishedLocked(q.now())
	all := slices.Collect(maps.Values(q.index))
	q.mu.Unlock()
	q.persistDelete(swept)

	// Snapshot outside the queue lock, then filter on the stable copy.
	snaps := make([]*Job, 0, len(all))
	for _, j := range all {
		s := j.snapshot(!opts.OmitItems)
		if opts.Status == "" || s.Status == opts.Status {
			snaps = append(snaps, s)
		}
	}
	// Newest activity first, which is what every row's own timestamp shows: a
	// folded batch-lookup row keeps its id but goes on being written to, so
	// ordering by id alone would bury an active sweep under older entries.
	slices.SortFunc(snaps, func(a, b *Job) int {
		return cmp.Or(b.StatusChangedAt.Compare(a.StatusChangedAt), cmp.Compare(b.ID, a.ID))
	})

	total := len(snaps)
	if opts.Limit > 0 {
		start := (max(opts.Page, 1) - 1) * opts.Limit
		if start >= len(snaps) {
			return []*Job{}, total
		}
		snaps = snaps[start:min(start+opts.Limit, len(snaps))]
	}
	return snaps, total
}

// Retry re-queues a finished job by id, clearing its prior run. force re-runs
// it with the download-archive bypassed so already-fetched posts are fetched
// again.
func (q *Queue) Retry(id int64, force bool) error {
	q.mu.Lock()
	j := q.index[id]
	if j == nil {
		q.mu.Unlock()
		return ErrNotFound
	}
	if err := j.reset(force, q.now()); err != nil {
		q.mu.Unlock()
		return err
	}
	q.removeFromFinishedLocked(id)
	q.pushPendingLocked(j)
	q.cond.Broadcast()
	q.mu.Unlock()
	q.persist(j)
	return nil
}

// RetrySeries re-queues every finished window of id's continue-series, in
// window order so the re-run comes down the way the original did. The collapsed
// queue row sums its windows, so an action offered on those counts has to reach
// all of them; a window still queued or running is left to finish.
func (q *Queue) RetrySeries(id int64, force bool) error {
	q.mu.Lock()
	src := q.index[id]
	if src == nil {
		q.mu.Unlock()
		return ErrNotFound
	}
	root := src.seriesKey()
	var ids []int64
	for jid, j := range q.index {
		if j.seriesKey() == root {
			ids = append(ids, jid)
		}
	}
	q.mu.Unlock()
	slices.Sort(ids)
	for _, jid := range ids {
		_ = q.Retry(jid, force)
	}
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
	// One lock hold from the high-water read to the enqueue: two concurrent
	// continues for a series would otherwise both read the same offset and
	// fetch overlapping windows.
	q.mu.Lock()
	j := q.index[id]
	if j == nil {
		q.mu.Unlock()
		return 0, ErrNotFound
	}
	src := j.Snapshot()
	if !src.Capped {
		q.mu.Unlock()
		return 0, ErrNotCapped
	}
	// Advance past the furthest window the series has fetched, not just the one
	// this call names, so a continue from a non-latest window cannot re-fetch a
	// window an earlier one already took.
	next := q.enqueueLocked(src.URL, Options{
		Gallery:  src.Gallery,
		Folder:   src.Folder,
		MaxItems: src.Cap,
		Offset:   q.seriesHighWaterLocked(src.seriesKey()),
		Root:     src.seriesKey(),
		Site:     src.Site,
		Auto:     auto,
	})
	q.mu.Unlock()
	q.persist(next)
	return next.ID, nil
}

// seriesHighWaterLocked returns the offset just past the furthest window any
// job in the series has fetched. Caller holds mu.
func (q *Queue) seriesHighWaterLocked(root int64) int {
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

// reconcileSeries clears the capped flag across a continue-series once a
// continuation resolves short of the cap (or empty): the source has run out,
// so leaving any window capped would keep advertising "more available" and
// let further continues fetch provably empty windows. A canceled or failed
// window proves nothing about the source and reconciles nothing.
func (q *Queue) reconcileSeries(j *Job) {
	s := j.Snapshot()
	if s.Capped || s.Root == 0 || s.Root == s.ID {
		return
	}
	if s.Status != JobSucceeded && s.Status != JobPartial {
		return
	}
	q.mu.Lock()
	var cleared []*Job
	for _, other := range q.index {
		if other.seriesKey() == s.Root && other.clearCapped() {
			cleared = append(cleared, other)
		}
	}
	q.mu.Unlock()
	for _, other := range cleared {
		q.persist(other)
	}
}

// CancelSeries stops id's continue-series (the originating capped job and its
// continuations), so the collapsed queue row acts on them as one unit. A live
// cancel - the clicked window is still queued or running - stops the in-flight
// and pending windows but leaves the windows that already finished, so their
// imported items stay in the history; once every window has finished, a remove
// drops the whole series.
func (q *Queue) CancelSeries(id int64) error {
	return q.cancelSeries(id, q.Cancel)
}

// cancelSeries walks id's series and stops each window through cancelOne, which
// decides whether a window that never ran leaves a row behind.
func (q *Queue) cancelSeries(id int64, cancelOne func(int64) error) error {
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
		_ = cancelOne(jid)
	}
	return nil
}

// CancelLive stops id's series only while it is still queued or running. The
// queue row's cancel and remove buttons differ only by the state the last poll
// saw, so a cancel clicked on a row that finished in the meantime must refuse
// rather than fall through to the removal CancelSeries would perform.
func (q *Queue) CancelLive(id int64) error {
	q.mu.Lock()
	j := q.index[id]
	live := j != nil && q.isLiveLocked(id)
	q.mu.Unlock()
	switch {
	case j == nil:
		return ErrNotFound
	case !live:
		return ErrNotRunning
	}
	return q.cancelSeries(id, q.cancelKeepingRow)
}

// cancelKeepingRow stops one live job and keeps its row: a running job is
// signalled and finalizes itself, and one that never started settles as
// canceled in the history instead of vanishing, so the URL that was submitted
// stays visible and retriable. Cancel, which the API's DELETE answers with,
// still drops it from tracking.
func (q *Queue) cancelKeepingRow(id int64) error {
	q.mu.Lock()
	j := q.index[id]
	if j == nil {
		q.mu.Unlock()
		return ErrNotFound
	}
	if cancel, ok := q.cancels[id]; ok {
		cancel()
		q.mu.Unlock()
		return nil
	}
	q.removeFromPendingLocked(id)
	// Settle before the ring takes the job: the retention sweep every List runs
	// reads FinishedAt, and a zero one is older than any cutoff, so a poll
	// landing in the gap would drop the row and delete its store entry under
	// the settle that follows. The done signal still goes last, so a woken Wait
	// caller sees the job already in history.
	j.cancel(q.now())
	evicted := q.pushFinishedLocked(j)
	q.mu.Unlock()
	j.signalDone()
	q.reportDropped(j)
	q.persist(j)
	q.persistDelete(evicted)
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
	j := q.index[id]
	if j == nil {
		q.mu.Unlock()
		return ErrNotFound
	}
	if cancel, ok := q.cancels[id]; ok {
		cancel()
		q.mu.Unlock()
		return nil
	}
	// Not running: drop it from the pending FIFO or the finished ring. Only a
	// job still waiting is being dropped before it ran; one already in the ring
	// is being tidied out of the history, and reporting that would overwrite
	// monbooru's record of a fetch that did happen with a cancellation.
	waiting := j.status() == JobQueued
	q.removeFromPendingLocked(id)
	q.removeFromFinishedLocked(id)
	delete(q.index, id)
	q.mu.Unlock()
	q.settleDropped(j)
	if waiting {
		q.reportDropped(j)
	}
	q.persistDelete([]int64{id})
	return nil
}

// settleDropped finalizes a job removed without ever running, so a caller
// parked in Wait returns with its terminal snapshot instead of blocking out
// the whole wait window. Both steps no-op on an already-finished job.
func (q *Queue) settleDropped(j *Job) {
	j.cancel(q.now())
	j.signalDone()
}

// reportDropped hands every dropped job that targets a monbooru image to the
// processor, so monbooru learns the attempt is over instead of waiting on a
// callback that will never come. The whole set shares one deadline and runs
// concurrently, so dropping a full FIFO does not stretch shutdown. Never
// called with q.mu held.
func (q *Queue) reportDropped(jobs ...*Job) {
	r, ok := q.proc.(DropReporter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dropReportTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, j := range jobs {
		snap := j.Snapshot()
		if snap.ImageID == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.ReportDropped(ctx, snap)
		}()
	}
	wg.Wait()
}

// CancelPending drops every queued job that has not started: the FIFO empties
// in one sweep and its rows go with it, mirroring the API's DELETE /queue/{id}.
// The row's own cancel goes through cancelKeepingRow instead, which settles a
// never-started job as canceled in the history rather than dropping it.
// Running jobs and history are untouched.
func (q *Queue) CancelPending() {
	q.mu.Lock()
	ids := make([]int64, 0, len(q.pending))
	for _, j := range q.pending {
		ids = append(ids, j.ID)
		delete(q.index, j.ID)
	}
	dropped := q.pending
	q.pending = nil
	q.mu.Unlock()
	for _, j := range dropped {
		q.settleDropped(j)
	}
	q.reportDropped(dropped...)
	q.persistDelete(ids)
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
	ids := make([]int64, 0, len(q.finished))
	for _, j := range q.finished {
		delete(q.index, j.ID)
		ids = append(ids, j.ID)
	}
	q.finished = nil
	q.mu.Unlock()
	q.persistDelete(ids)
}

// ClearSucceeded drops the finished jobs the success window would take, without
// waiting for it. Running and pending jobs are left untouched.
func (q *Queue) ClearSucceeded() {
	q.mu.Lock()
	ids := q.dropFinishedLocked(q.clearableLocked(q.now()))
	q.mu.Unlock()
	q.persistDelete(ids)
}

// ClearableSucceeded counts what ClearSucceeded would drop, so the queue page
// offers the action only while it has something to take.
func (q *Queue) ClearableSucceeded() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.clearableLocked(q.now()))
}

// pushPendingLocked inserts a job after the last one of its lane or better, so
// each lane jumps ahead of the ones below it while order inside a lane stays
// FIFO. A background lookup therefore waits behind a URL the operator pastes
// after it, which is the whole point of that lane. Caller holds mu.
func (q *Queue) pushPendingLocked(j *Job) {
	lane := j.lane()
	i := len(q.pending)
	for i > 0 && q.pending[i-1].lane() > lane {
		i--
	}
	q.pending = slices.Insert(q.pending, i, j)
}

func (q *Queue) removeFromPendingLocked(id int64) {
	// slices.DeleteFunc also zeroes the vacated tail, so the removed *Job
	// does not stay reachable from the backing array.
	q.pending = slices.DeleteFunc(q.pending, func(j *Job) bool { return j.ID == id })
}

// lastRunUnimportedLocked reports whether the most recent finished job for url
// did not fully import - anything but succeeded - so its posts may sit in
// gallery-dl's archive without having reached monbooru. Caller holds mu.
func (q *Queue) lastRunUnimportedLocked(url string) bool {
	unimported := false
	for _, j := range q.finished {
		if j.URL != url {
			continue
		}
		unimported = j.status() != JobSucceeded
	}
	return unimported
}

func (q *Queue) removeFromFinishedLocked(id int64) {
	q.finished = slices.DeleteFunc(q.finished, func(j *Job) bool { return j.ID == id })
}

// sweepFinishedLocked drops finished jobs past either retention window, so an
// instance left running for weeks does not show history from weeks ago and the
// rows that still need looking at are not buried under the ones that do not.
// Returns the dropped ids for the store delete. Caller holds mu.
func (q *Queue) sweepFinishedLocked(now time.Time) []int64 {
	var stale []int64
	if q.retention > 0 {
		cutoff := now.Add(-q.retention)
		for _, j := range q.finished {
			if j.finishedAt().Before(cutoff) {
				stale = append(stale, j.ID)
			}
		}
	}
	if q.successRetention > 0 {
		stale = append(stale, q.clearableLocked(now.Add(-q.successRetention))...)
	}
	return q.dropFinishedLocked(stale)
}

// clearableLocked lists the finished jobs that left nothing to act on and whose
// series last finished at or before cutoff. A series is judged whole - one
// window still live, failed, or holding more to fetch keeps them all - because
// the queue view collapses it into a single row, and dropping part of one would
// shrink the counts that row shows. Caller holds mu.
func (q *Queue) clearableLocked(cutoff time.Time) []int64 {
	pinned := map[int64]bool{}
	newest := map[int64]time.Time{}
	for _, j := range q.index {
		key := j.seriesKey()
		if !j.noFollowUp() {
			pinned[key] = true
			continue
		}
		if t := j.finishedAt(); t.After(newest[key]) {
			newest[key] = t
		}
	}
	var ids []int64
	for _, j := range q.finished {
		if key := j.seriesKey(); !pinned[key] && !newest[key].After(cutoff) {
			ids = append(ids, j.ID)
		}
	}
	return ids
}

// dropFinishedLocked removes the named jobs from the history ring and the
// index, returning the ids it actually took (an id may be listed twice when
// both windows reach the same job). Caller holds mu.
func (q *Queue) dropFinishedLocked(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	drop := make(map[int64]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	var taken []int64
	for _, j := range q.finished {
		if drop[j.ID] {
			delete(q.index, j.ID)
			taken = append(taken, j.ID)
		}
	}
	q.finished = slices.DeleteFunc(q.finished, func(j *Job) bool { return drop[j.ID] })
	return taken
}

// pushFinishedLocked appends a finished job to the ring and evicts the
// oldest entries past the bound, dropping them from the index too.
// Returns the evicted ids so the caller can drop their store rows
// outside the lock. Caller holds mu.
func (q *Queue) pushFinishedLocked(j *Job) []int64 {
	var evictedIDs []int64
	q.finished = append(q.finished, j)
	for len(q.finished) > q.maxFinished {
		evicted := q.finished[0]
		// Null the slot before reslicing: the evicted job (which can pin up to
		// max_items_per_job items) stays reachable in the backing array
		// otherwise, so the ring's footprint would sawtooth to ~2x the bound.
		q.finished[0] = nil
		q.finished = q.finished[1:]
		delete(q.index, evicted.ID)
		evictedIDs = append(evictedIDs, evicted.ID)
	}
	return evictedIDs
}
