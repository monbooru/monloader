package queue

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/shared/logx"
)

const defaultMaxFinished = 100

var ErrNotFound = errors.New("job not found")

var ErrNotCapped = errors.New("job was not capped")

var ErrNoneRetried = errors.New("no window could be re-run")

var ErrNotRunning = errors.New("job is not running")

var errStopping = errors.New("the queue is stopping")

// ItemsAll fetches every window, each queued when the last comes back capped.
const ItemsAll = -1

type Options struct {
	Gallery    string   `json:"gallery"`
	Folder     string   `json:"folder,omitempty"`
	MaxItems   int      `json:"max_items,omitempty"`
	PageURL    string   `json:"page_url,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Commentary string   `json:"commentary,omitempty"`
	Source     string   `json:"source,omitempty"`
	Via        string   `json:"via,omitempty"`
	Offset     int      `json:"-"`
	Root       int64    `json:"root,omitempty"`
	// Site pre-labels a continuation: a window past the last post
	// resolves nothing and never learns its site.
	Site  string `json:"site"`
	Auto  bool   `json:"-"`
	Force bool   `json:"force,omitempty"`
	// ForceTo is how far down the listing a watch check's forced windows chain.
	ForceTo  int  `json:"-"`
	Priority bool `json:"-"`
	// Independent: a bulk lookup the operator starts is Background but
	// not Budgeted.
	Background bool  `json:"-"`
	Budgeted   bool  `json:"-"`
	Watch      int64 `json:"-"`
	// Target is how many posts the whole series takes: past the cap, the
	// windows chain until they reach it.
	Target         int     `json:"-"`
	Kind           JobKind `json:"kind,omitempty"`
	ImageID        int64   `json:"image_id,omitempty"`
	Backend        string  `json:"backend,omitempty"`
	MD5            string  `json:"md5,omitempty"`
	SHA256         string  `json:"sha256,omitempty"`
	ContribIDs     []int64 `json:"-"`
	ContribBacklog bool    `json:"-"`
}

// WithItems sets how many posts a run takes; 0 keeps the configured cap.
func (o Options) WithItems(n int) Options {
	switch {
	case n == ItemsAll:
		o.Auto = true
	case n > 0:
		o.MaxItems, o.Target = n, n
	}
	return o
}

// Process returns nil even when items failed; an error means a job-level
// abort such as a failed resolve.
type Processor interface {
	Process(ctx context.Context, job *Job) error
}

// DropReporter is optional on a Processor: a job dropped before it ran
// sends monbooru no callback of its own.
type DropReporter interface {
	ReportDropped(ctx context.Context, job *Job)
}

// Bounds the whole report, one job or a shutdown's FIFO: shutdown must
// not block on an unreachable monbooru.
var dropReportTimeout = 3 * time.Second

const maxDropReports = 4

// Queue's mu guards the FIFO, the running set and the history ring; a
// Job's own mu may be taken under it.
type Queue struct {
	mu               sync.Mutex
	cond             *sync.Cond
	pending          []*Job
	running          map[int64]*Job
	cancels          map[int64]context.CancelFunc
	finished         []*Job
	index            map[int64]*Job
	series           map[int64]series
	maxFinished      int
	retention        time.Duration
	successRetention time.Duration
	nextID           int64
	closed           bool
	stopped          chan struct{}
	paused           bool
	// The app's own hold, apart from the operator's pause, so neither can undo
	// the other.
	held bool

	// Mirrored to the store: a crash loop must not refill a spent budget.
	budgetLimit int
	budgetDay   string
	budgetSpent int
	// Serializes a take with its store write: the row holds an absolute
	// value, so writes landing out of order would refund a spend.
	budgetWrite sync.Mutex
	// Serializes a row's liveness check and save with a delete, so a
	// Clear landing in between cannot have its delete undone.
	rowWrite sync.Mutex
	// Serializes a watch's update with its store write: the row holds the
	// whole last check, so writes landing out of order would restore an older one.
	watchWrite sync.Mutex

	watches []*Watch

	proc    Processor
	workers int
	wg      sync.WaitGroup
	store   *Store
	now     func() time.Time
}

func New(proc Processor, workers, maxFinished int) *Queue {
	workers = max(workers, 1)
	if maxFinished < 1 {
		maxFinished = defaultMaxFinished
	}
	q := &Queue{
		running:     map[int64]*Job{},
		cancels:     map[int64]context.CancelFunc{},
		index:       map[int64]*Job{},
		series:      map[int64]series{},
		maxFinished: maxFinished,
		proc:        proc,
		workers:     workers,
		now:         time.Now,
		stopped:     make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Workers is fixed at New, so a concurrency change takes effect on restart.
func (q *Queue) Workers() int { return q.workers }

// Call before Start. A job the process died running comes back
// interrupted, not resumed: restarting a large job on every crash would
// surprise the operator.
func (q *Queue) UseStore(st *Store) {
	jobs, err := st.LoadJobs()
	if err != nil {
		logx.Warnf("queue: store reload failed, history lost: %v", err)
	}
	// Unlocked: nothing else can reach these jobs until they are indexed below.
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
	// Loaded here, outside q.mu, so a take never reads the disk under it.
	budgetDay, budgetSpent, err := st.LoadCounter(lookupBudgetCounter)
	if err != nil {
		logx.Warnf("queue: budget counter reload failed: %v", err)
		budgetDay, budgetSpent = "", 0
	}
	watches, err := st.LoadWatches()
	if err != nil {
		logx.Warnf("queue: watch reload failed: %v", err)
	}
	// Past every id ever issued, not just the surviving rows: monbooru still
	// holds old ids, and a reissued one would answer for another job.
	high, err := st.HighestJobID()
	if err != nil {
		logx.Warnf("queue: job id high-water reload failed: %v", err)
	}
	var evicted []int64
	q.mu.Lock()
	q.store = st
	q.budgetDay, q.budgetSpent = budgetDay, budgetSpent
	for i := range watches {
		q.watches = append(q.watches, &watches[i])
	}
	q.nextID = max(q.nextID, high)
	for _, j := range jobs {
		q.nextID = max(q.nextID, j.ID)
		q.indexLocked(j)
		if j.Status == JobQueued {
			q.pushPendingLocked(j)
		} else {
			evicted = append(evicted, q.pushFinishedLocked(j)...)
		}
	}
	q.mu.Unlock()
	q.persistDelete(evicted)
	for _, j := range interrupted {
		q.recordWatch(j)
		q.persist(j)
	}
}

// Caller must not hold q.mu: a slow disk would stall the whole queue.
func (q *Queue) persist(j *Job) {
	if q.store == nil {
		return
	}
	if err := q.store.SaveJob(j.Snapshot()); err != nil {
		logx.Warnf("queue: store write for job %d failed: %v", j.ID, err)
	}
}

// persistKept skips a job a Clear has dropped: saving it would bring it
// back at the next boot. Caller must not hold q.mu.
func (q *Queue) persistKept(j *Job) {
	q.rowWrite.Lock()
	defer q.rowWrite.Unlock()
	q.mu.Lock()
	kept := q.index[j.ID] == j
	q.mu.Unlock()
	if kept {
		q.persist(j)
	}
}

// Caller must not hold q.mu.
func (q *Queue) persistDelete(ids []int64) {
	if q.store == nil || len(ids) == 0 {
		return
	}
	q.rowWrite.Lock()
	defer q.rowWrite.Unlock()
	if err := q.store.DeleteJobs(ids); err != nil {
		logx.Warnf("queue: store delete failed: %v", err)
	}
}

func (q *Queue) SetRetention(d time.Duration) {
	q.mu.Lock()
	q.retention = d
	q.mu.Unlock()
}

func (q *Queue) SetSuccessRetention(d time.Duration) {
	q.mu.Lock()
	q.successRetention = d
	q.mu.Unlock()
}

func (q *Queue) Pause() {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
}

func (q *Queue) Resume() {
	q.mu.Lock()
	q.paused = false
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *Queue) Hold() {
	q.mu.Lock()
	q.held = true
	q.mu.Unlock()
}

func (q *Queue) Release() {
	q.mu.Lock()
	q.held = false
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *Queue) Held() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.held
}

func (q *Queue) Paused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

func (q *Queue) Enqueue(url string, opts Options) int64 {
	q.mu.Lock()
	j := q.enqueueLocked(url, opts)
	q.mu.Unlock()
	q.persistKept(j)
	return j.ID
}

// The caller persists the returned job after unlocking.
func (q *Queue) enqueueLocked(url string, opts Options) *Job {
	q.nextID++
	id := q.nextID
	j := newJob(id, url, opts, q.now())
	if j.Root == 0 {
		j.Root = id
	}
	// A re-add gets the archive bypass a retry would; a URL-less job must
	// match nothing. A watch check stays on the archive: its last failed
	// run keeps its own retry.
	if opts.Root == 0 && opts.Watch == 0 && url != "" {
		if reach, unimported := q.lastRunUnimportedLocked(url); unimported {
			j.Force, j.ForceTo = true, reach
		}
	}
	q.indexLocked(j)
	q.pushPendingLocked(j)
	q.cond.Broadcast()
	return j
}

func (q *Queue) EnqueueMetadata(imageID int64, gallery, url string) int64 {
	return q.Enqueue(url, Options{Kind: KindMetadata, ImageID: imageID, Gallery: gallery, Priority: true})
}

func (q *Queue) EnqueueLookup(imageID int64, gallery, backend, md5, sha256 string, background, budgeted bool) int64 {
	return q.Enqueue("", Options{
		Kind: KindLookup, ImageID: imageID, Gallery: gallery,
		Backend: backend, MD5: md5, SHA256: sha256, Priority: !background,
		Background: background, Budgeted: budgeted,
	})
}

func (q *Queue) EnqueueReplace(imageID int64, gallery, url string) int64 {
	return q.Enqueue(url, Options{Kind: KindReplace, ImageID: imageID, Gallery: gallery, Priority: true})
}

func (q *Queue) EnqueueHashImport(md5 string, opts Options) int64 {
	opts.Kind = KindHashImport
	opts.MD5 = md5
	return q.Enqueue("", opts)
}

// RecordPTRLookup files an in-process batch lookup in the history, so a
// library sweep leaves a trace.
func (q *Queue) RecordPTRLookup(gallery string, scheduled bool, asked int, matched []Item) {
	now := q.now()
	q.mu.Lock()
	if j := q.recentPTRLookupLocked(now, scheduled); j != nil {
		q.mu.Unlock()
		j.recordPTRLookup(gallery, asked, matched, now)
		q.persistKept(j)
		return
	}
	q.nextID++
	j := newJob(q.nextID, "", Options{
		Kind: KindPTRLookup, Gallery: gallery, Background: true, Budgeted: scheduled,
	}, now)
	j.Root = j.ID
	j.recordPTRLookup(gallery, asked, matched, now)
	q.indexLocked(j)
	evicted := q.pushFinishedLocked(j)
	q.mu.Unlock()
	q.persistKept(j)
	q.persistDelete(evicted)
}

// Per day, not per timer: a sweep paced over hours still files one row.
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

func (q *Queue) EnqueueContrib(ids []int64, backlog bool) int64 {
	return q.Enqueue("", Options{Kind: KindContrib, ContribIDs: ids, ContribBacklog: backlog, Priority: true})
}

func (q *Queue) ContribSendLive() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.liveLocked(func(j *Job) bool { return j.Kind == KindContrib })
}

func (q *Queue) Get(id int64) (*Job, error) {
	q.mu.Lock()
	j := q.index[id]
	q.mu.Unlock()
	if j == nil {
		return nil, ErrNotFound
	}
	return j.Snapshot(), nil
}

// LiveHead reports on a job's items copying only the first n, so a poll over
// a large pool costs what it shows.
func (q *Queue) LiveHead(id int64, n int) (head []Item, pending, pushing bool, err error) {
	q.mu.Lock()
	j := q.index[id]
	q.mu.Unlock()
	if j == nil {
		return nil, false, false, ErrNotFound
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	head = slices.Clone(j.Items[:min(n, len(j.Items))])
	for i := range j.Items {
		switch j.Items[i].Status {
		case ItemPending:
			pending = true
		case ItemDownloaded, ItemUploaded:
			pushing = true
		}
	}
	return head, pending, pushing, nil
}

func (q *Queue) InFlight() (running, queued int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.running), len(q.pending)
}

type ListOptions struct {
	Status    JobStatus // "" = every status
	Limit     int       // 0 = no limit
	Page      int       // 1-based; <=1 means the first page
	OmitItems bool
}

func (q *Queue) List(opts ListOptions) ([]*Job, int) {
	q.mu.Lock()
	// Swept here rather than on a timer: every history reader comes
	// through List.
	swept := q.sweepFinishedLocked(q.now())
	all := slices.Collect(maps.Values(q.index))
	q.mu.Unlock()
	q.persistDelete(swept)

	snaps := make([]*Job, 0, len(all))
	for _, j := range all {
		s := j.snapshot(!opts.OmitItems)
		if opts.Status == "" || s.Status == opts.Status {
			snaps = append(snaps, s)
		}
	}
	// By last change, not id: a folded batch-lookup row keeps its id but
	// goes on changing.
	slices.SortFunc(snaps, func(a, b *Job) int {
		return cmp.Or(b.StatusChangedAt.Compare(a.StatusChangedAt), cmp.Compare(b.ID, a.ID))
	})

	total := len(snaps)
	if opts.Limit > 0 {
		// Past this the offset wraps negative.
		start := (min(max(opts.Page, 1), math.MaxInt/opts.Limit) - 1) * opts.Limit
		if start >= len(snaps) {
			return []*Job{}, total
		}
		snaps = snaps[start:min(start+opts.Limit, len(snaps))]
	}
	return snaps, total
}

func (q *Queue) Retry(id int64, force bool) error {
	q.mu.Lock()
	j := q.index[id]
	if j == nil {
		q.mu.Unlock()
		return ErrNotFound
	}
	// A processor settles the status before finish takes the job off
	// the worker.
	if _, ok := q.running[id]; ok {
		q.mu.Unlock()
		return errors.New("cannot retry a running job")
	}
	if err := j.reset(force, q.now()); err != nil {
		q.mu.Unlock()
		return err
	}
	q.removeFromFinishedLocked(id)
	q.pushPendingLocked(j)
	q.cond.Broadcast()
	q.mu.Unlock()
	q.persistKept(j)
	return nil
}

func (q *Queue) RetrySeries(id int64, force bool) error {
	q.mu.Lock()
	src := q.index[id]
	if src == nil {
		q.mu.Unlock()
		return ErrNotFound
	}
	ids := q.series[src.seriesKey()].ids()
	q.mu.Unlock()
	slices.Sort(ids)
	var requeued int
	for _, jid := range ids {
		if q.Retry(jid, force) == nil {
			requeued++
		}
	}
	if requeued == 0 {
		return ErrNoneRetried
	}
	return nil
}

func (q *Queue) Continue(id int64) (int64, error) { return q.continueFrom(id, false, 0) }

func (q *Queue) ContinueAll(id int64) (int64, error) { return q.continueFrom(id, true, 0) }

// A target above 0 ends the chain once the series reaches it, and sizes
// the last window to what is left.
func (q *Queue) continueFrom(id int64, auto bool, target int) (int64, error) {
	// One hold from the high-water read to the enqueue, or two continues
	// fetch the same window.
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
	// From the series' furthest window, not this one's: continuing an
	// earlier window must not re-fetch a later one.
	high, window := q.series[src.seriesKey()].extent()
	if target > 0 {
		if high >= target {
			q.mu.Unlock()
			return 0, ErrNotCapped
		}
		window = min(window, target-high)
	}
	next := q.enqueueLocked(src.URL, Options{
		Gallery:  src.Gallery,
		Folder:   src.Folder,
		MaxItems: window,
		// The submission's metadata belongs to the search, not to one window.
		PageURL:    src.PageURL,
		Tags:       src.Tags,
		Commentary: src.Commentary,
		Source:     src.Source,
		Via:        src.Via,
		Offset:     high,
		Root:       src.seriesKey(),
		Site:       src.Site,
		Auto:       auto,
		Target:     target,
		// A check's next window is still the check: it stops at known
		// posts and keeps the check's lane.
		Watch:      src.Watch,
		Background: src.Background,
		Force:      src.Force && src.ForceTo > high,
		ForceTo:    src.ForceTo,
	})
	q.mu.Unlock()
	q.persistKept(next)
	return next.ID, nil
}

// A canceled or failed window keeps its capped flag, so the status, not
// the flag, ends the chain.
func (q *Queue) autoContinue(j *Job) {
	s := j.Snapshot()
	// A window finishing during a stop ends its chain there; the row
	// keeps get next.
	forced := s.Watch != 0 && s.Force
	if (!s.Auto && s.Target == 0 && !forced) || !s.Capped || q.closing() {
		return
	}
	if s.Status != JobSucceeded && s.Status != JobPartial {
		return
	}
	// A listing that stopped on an error would stop the next window too; the row keeps get next.
	if s.ErrorCode != "" {
		return
	}
	// A retried earlier window has its chain ahead of it already.
	q.mu.Lock()
	high, _ := q.series[s.seriesKey()].extent()
	q.mu.Unlock()
	if j.windowEnd() < high {
		return
	}
	// A smaller items pick since the outage does not forgive what it owes.
	target := s.Target
	if forced && !s.Auto {
		target = max(target, s.ForceTo)
	}
	_, _ = q.continueFrom(s.ID, s.Auto, target)
}

// A retried earlier window comes back capped by its own size, while a later
// window that ran short has already said the source ended there.
func (q *Queue) settleEarlierWindow(j *Job) {
	s := j.Snapshot()
	if !s.Capped {
		return
	}
	end := j.windowEnd()
	q.mu.Lock()
	furthest := q.series[s.seriesKey()].furthestPast(end)
	q.mu.Unlock()
	if furthest == nil {
		return
	}
	if f := furthest.Snapshot(); f.Capped || (f.Status != JobSucceeded && f.Status != JobPartial) {
		return
	}
	if j.clearCapped() {
		q.persistKept(j)
	}
}

// A short continuation means the source ran out; a canceled or failed one
// proves nothing.
func (q *Queue) reconcileSeries(j *Job) {
	s := j.Snapshot()
	if s.Capped || s.Root == 0 || s.Root == s.ID {
		return
	}
	if s.Status != JobSucceeded && s.Status != JobPartial {
		return
	}
	q.mu.Lock()
	cleared := q.series[s.seriesKey()].clearCapped()
	q.mu.Unlock()
	for _, other := range cleared {
		q.persistKept(other)
	}
}

// CancelSeries stops a live series but keeps its finished windows and
// their imports; a finished series is removed whole.
func (q *Queue) CancelSeries(id int64) error {
	ids, live, err := q.seriesToCancel(id)
	for _, jid := range ids {
		q.cancel(jid, live)
	}
	return err
}

// live is whether id was live; then only the windows live with it are
// listed, and each must be re-checked, since one may finish meanwhile.
func (q *Queue) seriesToCancel(id int64) (ids []int64, live bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	src := q.index[id]
	if src == nil {
		return nil, false, ErrNotFound
	}
	live = q.isLiveLocked(id)
	ids = slices.DeleteFunc(q.series[src.seriesKey()].ids(), func(jid int64) bool { return live && !q.isLiveLocked(jid) })
	return ids, live, nil
}

// CancelLive refuses a finished series: a stale cancel button must not
// fall through to a removal. A button can name a window that finished after
// the poll that drew it, so the series decides.
func (q *Queue) CancelLive(id int64) error {
	q.mu.Lock()
	j := q.index[id]
	live := j != nil && slices.ContainsFunc(q.series[j.seriesKey()].ids(), q.isLiveLocked)
	q.mu.Unlock()
	switch {
	case j == nil:
		return ErrNotFound
	case !live:
		return ErrNotRunning
	}
	ids, _, err := q.seriesToCancel(id)
	for _, jid := range ids {
		_ = q.cancelKeepingRow(jid)
	}
	return err
}

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
	if !q.isLiveLocked(id) {
		q.mu.Unlock()
		return ErrNotRunning
	}
	evicted := q.cancelPendingLocked(j)
	q.mu.Unlock()
	q.releaseDropped([]*Job{j}, evicted)
	return nil
}

func (q *Queue) cancelPendingLocked(j *Job) (evicted []int64) {
	q.removeFromPendingLocked(j.ID)
	j.cancel(q.now())
	return q.pushFinishedLocked(j)
}

func (q *Queue) isLiveLocked(id int64) bool {
	if _, ok := q.running[id]; ok {
		return true
	}
	return slices.ContainsFunc(q.pending, func(j *Job) bool { return j.ID == id })
}

func (q *Queue) liveLocked(match func(*Job) bool) bool {
	for _, j := range q.running {
		if match(j) {
			return true
		}
	}
	return slices.ContainsFunc(q.pending, match)
}

// A running job stays in history as canceled, anything else is removed.
func (q *Queue) cancel(id int64, liveOnly bool) {
	q.mu.Lock()
	j := q.index[id]
	if j == nil || (liveOnly && !q.isLiveLocked(id)) {
		q.mu.Unlock()
		return
	}
	if cancel, ok := q.cancels[id]; ok {
		cancel()
		q.mu.Unlock()
		return
	}
	// Only a job still waiting is reported: one in the ring is history, and a
	// report could overwrite monbooru's record of a fetch that happened.
	waiting := j.status() == JobQueued
	q.removeFromPendingLocked(id)
	q.removeFromFinishedLocked(id)
	q.unindexLocked(id)
	q.mu.Unlock()
	q.settleDropped(j)
	if waiting {
		q.stampCanceledCheck(j)
		q.reportDropped(j)
	}
	q.persistDelete([]int64{id})
}

// Both steps no-op on a finished job.
func (q *Queue) settleDropped(j *Job) {
	j.cancel(q.now())
	j.signalDone()
}

// Caller must not hold q.mu.
func (q *Queue) reportDropped(jobs ...*Job) {
	r, ok := q.proc.(DropReporter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dropReportTimeout)
	defer cancel()
	// A canceled batch lookup over a search drops one job per image; one
	// POST each at once would be a burst toward monbooru.
	sem := make(chan struct{}, maxDropReports)
	var wg sync.WaitGroup
	unreported := 0
	for _, j := range jobs {
		snap := j.Snapshot()
		if snap.ImageID == 0 {
			continue
		}
		// Past the deadline each report would only fail and log its own line.
		// Checked first: select picks at random when the slot is free too.
		if ctx.Err() != nil {
			unreported++
			continue
		}
		select {
		case <-ctx.Done():
			unreported++
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			r.ReportDropped(ctx, snap)
		}()
	}
	wg.Wait()
	if unreported > 0 {
		logx.Warnf("queue: %d dropped jobs went unreported to monbooru, past the report deadline", unreported)
	}
}

func (q *Queue) CancelPending() {
	q.mu.Lock()
	dropped, evicted := q.dropPendingLocked()
	q.mu.Unlock()
	q.releaseDropped(dropped, evicted)
}

// The caller hands the result to releaseDropped after unlocking.
func (q *Queue) dropPendingLocked() (dropped []*Job, evicted []int64) {
	dropped = q.pending
	q.pending = nil
	for _, j := range dropped {
		j.cancel(q.now())
		evicted = append(evicted, q.pushFinishedLocked(j)...)
	}
	return dropped, evicted
}

// Caller must not hold q.mu.
func (q *Queue) releaseDropped(dropped []*Job, evicted []int64) {
	for _, j := range dropped {
		j.signalDone()
		q.stampCanceledCheck(j)
		q.persistKept(j)
	}
	q.reportDropped(dropped...)
	q.persistDelete(evicted)
}

func (q *Queue) RenameGallery(from, to string) {
	q.mu.Lock()
	var moved []*Job
	for _, j := range q.index {
		if j.renameGallery(from, to) {
			moved = append(moved, j)
		}
	}
	q.mu.Unlock()
	for _, j := range moved {
		q.persistKept(j)
	}
}

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
	case <-q.stopped:
	}
	// Nothing runs a job accepted during the drain until the next boot; a
	// running job keeps the drain to finish in.
	if j.status() == JobQueued {
		return j.Snapshot(), errStopping
	}
	select {
	case <-j.doneChan():
		return j.Snapshot(), nil
	case <-ctx.Done():
		return j.Snapshot(), ctx.Err()
	}
}

func (q *Queue) Clear() {
	q.mu.Lock()
	ids := make([]int64, 0, len(q.finished))
	for _, j := range q.finished {
		q.unindexLocked(j.ID)
		ids = append(ids, j.ID)
	}
	q.finished = nil
	q.mu.Unlock()
	q.persistDelete(ids)
}

func (q *Queue) ClearSucceeded() {
	q.mu.Lock()
	ids := q.dropFinishedLocked(q.clearableLocked(q.now()))
	q.mu.Unlock()
	q.persistDelete(ids)
}

func (q *Queue) ClearableSucceeded() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.clearableLocked(q.now()))
}

func (q *Queue) pushPendingLocked(j *Job) {
	lane := j.lane()
	i := len(q.pending)
	for i > 0 && q.pending[i-1].lane() > lane {
		i--
	}
	q.pending = slices.Insert(q.pending, i, j)
}

func (q *Queue) removeFromPendingLocked(id int64) {
	q.pending = slices.DeleteFunc(q.pending, func(j *Job) bool { return j.ID == id })
}

func (q *Queue) lastRunLocked(url string) *Job {
	for _, j := range slices.Backward(q.finished) {
		if j.URL == url {
			return j
		}
	}
	return nil
}

func (q *Queue) lastRunUnimportedLocked(url string) (reach int, unimported bool) {
	last := q.lastRunLocked(url)
	if last == nil {
		return 0, false
	}
	return q.series[last.seriesKey()].unimported(q.isLiveLocked)
}

func (q *Queue) removeFromFinishedLocked(id int64) {
	q.finished = slices.DeleteFunc(q.finished, func(j *Job) bool { return j.ID == id })
}

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

func (q *Queue) clearableLocked(cutoff time.Time) []int64 {
	settled := make(map[int64]bool, len(q.series))
	for key, s := range q.series {
		settled[key] = s.settledBefore(cutoff)
	}
	var ids []int64
	for _, j := range q.finished {
		if settled[j.seriesKey()] {
			ids = append(ids, j.ID)
		}
	}
	return ids
}

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
			q.unindexLocked(j.ID)
			taken = append(taken, j.ID)
		}
	}
	q.finished = slices.DeleteFunc(q.finished, func(j *Job) bool { return drop[j.ID] })
	return taken
}

// The job must already be settled: the sweep reads a zero FinishedAt as
// older than any cutoff. The caller drops the evicted ids' store rows
// after unlocking.
func (q *Queue) pushFinishedLocked(j *Job) []int64 {
	var evictedIDs []int64
	q.finished = append(q.finished, j)
	for len(q.finished) > q.maxFinished {
		evicted := q.finished[0]
		// Nil the slot before reslicing, or the evicted job stays
		// reachable from the backing array.
		q.finished[0] = nil
		q.finished = q.finished[1:]
		q.unindexLocked(evicted.ID)
		evictedIDs = append(evictedIDs, evicted.ID)
	}
	return evictedIDs
}
