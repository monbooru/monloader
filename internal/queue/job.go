package queue

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// JobStatus is the lifecycle state of a queued download job.
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobPartial   JobStatus = "partial"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
)

// ValidJobStatus reports whether s is a known job status, for validating a
// status filter at the API boundary.
func ValidJobStatus(s JobStatus) bool {
	switch s {
	case JobQueued, JobRunning, JobSucceeded, JobPartial, JobFailed, JobCanceled:
		return true
	}
	return false
}

// ItemStatus is the per-item progress state.
type ItemStatus string

const (
	ItemPending    ItemStatus = "pending"
	ItemDownloaded ItemStatus = "downloaded"
	ItemUploaded   ItemStatus = "uploaded"
	ItemDone       ItemStatus = "done"
	ItemSkipped    ItemStatus = "skipped"
	ItemFailed     ItemStatus = "failed"
)

// Outcome is the terminal result the UI and API branch on.
type Outcome string

const (
	OutcomeCreated            Outcome = "created"
	OutcomeDuplicate          Outcome = "duplicate"
	OutcomeEnriched           Outcome = "enriched"
	OutcomeSkippedArchive     Outcome = "skipped_archive"
	OutcomeSkippedUnsupported Outcome = "skipped_unsupported"
	OutcomeFailed             Outcome = "failed"
)

// Error-code vocabulary. Stable strings the future browser
// extension can branch on; the gallery-dl wrapper, the monbooru client, and
// the pipeline all classify failures into these.
const (
	ErrCodeDownloadFailed      = "download_failed"
	ErrCodeUnsupportedURL      = "unsupported_url"
	ErrCodeAuthRequired        = "auth_required"
	ErrCodeBlocked             = "blocked"
	ErrCodeRateLimited         = "rate_limited"
	ErrCodeNetworkUnreachable  = "network_unreachable"
	ErrCodeFileTooLarge        = "file_too_large"
	ErrCodeMonbooruUnreachable = "monbooru_unreachable"
	ErrCodeMonbooruRejected    = "monbooru_rejected"
	ErrCodeHashMismatch        = "hash_mismatch"
	ErrCodeMappingFailed       = "mapping_failed"
	ErrCodeCanceled            = "canceled"
	ErrCodeHashNotFound        = "hash_not_found"
	ErrCodePTRUnavailable      = "ptr_unavailable"
)

// Summary aggregates per-item outcomes for the queue view and the API.
// Skipped counts skipped_archive items; duplicate is
// tracked separately so the extension can say "already in your library".
type Summary struct {
	Created   int `json:"created"`
	Duplicate int `json:"duplicate"`
	// Enriched counts metadata-only source refetches that merged tags into an
	// image monbooru already holds; like Duplicate, but no file was pushed.
	Enriched int `json:"enriched,omitempty"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	// Canceled counts items aborted by a job cancel (failed with the canceled
	// code), kept out of Failed so a deliberate cancel does not read as errors.
	Canceled int `json:"canceled,omitempty"`
	Total    int `json:"total"`
}

// Item is one downloadable post within a job. PostID + Num key it back to
// the gallery-dl resolve pass.
type Item struct {
	PostID string `json:"post_id"`
	Num    int    `json:"num"`
	// URL is the canonical source post page (the same url pushed to monbooru),
	// shown as a link on the item in the queue. Empty for a cbz bundle and for
	// sources without a post-url template.
	URL        string     `json:"url,omitempty"`
	Status     ItemStatus `json:"status"`
	Outcome    Outcome    `json:"outcome,omitempty"`
	MonbooruID int64      `json:"monbooru_id,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	// TagWarnings are tags monbooru rejected on the push; recorded, not fatal.
	TagWarnings []string `json:"tag_warnings,omitempty"`
	// MergeNote summarises what a duplicate merge folded in (e.g. "+7 tags").
	MergeNote string `json:"merge_note,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// JobKind distinguishes a normal download from a metadata-only source refetch
// and the two hash-keyed jobs.
type JobKind string

const (
	KindDownload JobKind = "download"
	KindMetadata JobKind = "metadata"
	// KindLookup finds tags for a hash (on the booru walk or the PTR index)
	// and enriches an existing monbooru image; nothing is downloaded.
	KindLookup JobKind = "lookup"
	// KindHashImport resolves an md5 to a post via the booru walk, then
	// downloads and pushes it like a submitted single post.
	KindHashImport JobKind = "hash_import"
)

// Lookup backends a KindLookup job can query. BackendAll runs every backend
// that is available: the PTR when enabled, then the booru walk.
const (
	BackendBooru = "booru"
	BackendPTR   = "ptr"
	BackendAll   = "all"
)

// jobState is the job's data, split from the lock so Snapshot can copy it
// wholesale and reset can rebuild it, instead of each maintaining its own
// field list: a new field is snapshotted automatically and zeroed on reset
// unless reset explicitly keeps it. The JSON tags shape the API payload
// (embedding promotes them onto Job).
type jobState struct {
	ID     int64     `json:"id"`
	URL    string    `json:"url"`
	Status JobStatus `json:"status"`
	// Kind is "download" (the default) or "metadata": a source refetch that
	// re-reads URL for its tags/commentary/notes and enriches ImageID (an image
	// monbooru already holds) instead of downloading and pushing a new file.
	Kind    JobKind `json:"kind,omitempty"`
	ImageID int64   `json:"image_id,omitempty"`
	// Backend picks the lookup source ("booru", "ptr", or "all") for a lookup
	// job; MD5 / SHA256 carry the hash a lookup or hash import is keyed on. A
	// hash-keyed job starts with no URL, so the row shows the hash instead
	// (Subject) until a hash import's walk resolves one.
	Backend  string `json:"backend,omitempty"`
	MD5      string `json:"md5,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Site     string `json:"site"`
	Gallery  string `json:"gallery"`
	Folder   string `json:"folder,omitempty"`
	MaxItems int    `json:"max_items,omitempty"`
	// Offset skips this many leading posts before the job's window, so a
	// continue on a capped search fetches the next batch via --range.
	Offset int `json:"-"`
	// Root is the originating job of a continue-series (a capped search and its
	// continuations); the queue view and the extension group the windows that
	// share it. A standalone job is its own root.
	Root int64 `json:"root,omitempty"`
	// Auto marks a window of a "fetch all" chain: when it comes back capped the
	// queue enqueues the next window on its own, until a window returns short.
	Auto bool `json:"-"`
	// Force bypasses gallery-dl's download-archive on the next run so
	// already-fetched posts are downloaded again. Set by a forced retry,
	// e.g. to re-fetch an image deleted in monbooru.
	Force bool `json:"force,omitempty"`
	// Priority single-post jobs jump ahead of bulk jobs in the FIFO so a
	// `?wait=N` request behind a long job still resolves quickly.
	Priority bool    `json:"-"`
	Summary  Summary `json:"summary"`
	// Capped is set when the resolve returned the full per-job item cap, so
	// more posts may remain; Cap is the applied limit. Surfaced in the UI and
	// the API so a truncated import is not mistaken for a complete one.
	Capped     bool      `json:"capped,omitempty"`
	Cap        int       `json:"cap,omitempty"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	Items      []Item    `json:"items"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// StatusChangedAt is stamped by every Status write, for the queue row's
	// "how long in this state" note. Kept rather than derived from the three
	// timestamps above because a retry re-queues a job while keeping its
	// original CreatedAt, which would then read as the change time. The API
	// carries the raw timestamps, so it is not on the wire.
	StatusChangedAt time.Time `json:"-"`
}

// Job is a queued URL and its resolved items. The mutex guards every mutable
// field; callers read through Snapshot (which returns an independent copy) and
// the worker/processor mutate through the methods below.
type Job struct {
	mu sync.Mutex
	jobState

	finalized bool
	done      chan struct{}
	// priorItems remembers a finished run's imported items by post key, so a
	// plain retry that archive-skips a post can restore its monbooru back-link.
	priorItems map[string]Item
}

// validJobTransition is the centralized job state machine.
// queued runs; a running job ends succeeded/partial/failed/canceled; a
// queued job can be canceled before it runs; a finished job can be
// re-queued by Retry.
func validJobTransition(from, to JobStatus) bool {
	switch from {
	case JobQueued:
		return to == JobRunning || to == JobCanceled
	case JobRunning:
		return to == JobSucceeded || to == JobPartial || to == JobFailed || to == JobCanceled
	case JobSucceeded, JobPartial, JobFailed, JobCanceled:
		return to == JobQueued // Retry
	}
	return false
}

// validItemTransition is the centralized item state machine.
// pending downloads (or is archive-skipped); downloaded uploads, or is skipped
// when monbooru cannot ingest its type; uploaded lands as done (created) or
// skipped (duplicate); any state can fail.
func validItemTransition(from, to ItemStatus) bool {
	if to == ItemFailed {
		return from == ItemPending || from == ItemDownloaded || from == ItemUploaded
	}
	switch from {
	case ItemPending:
		return to == ItemDownloaded || to == ItemSkipped
	case ItemDownloaded:
		return to == ItemUploaded || to == ItemSkipped
	case ItemUploaded:
		return to == ItemDone || to == ItemSkipped
	}
	return false
}

func newJob(id int64, url string, opts Options, now time.Time) *Job {
	return &Job{
		jobState: jobState{
			ID:              id,
			URL:             url,
			Status:          JobQueued,
			Kind:            opts.Kind,
			ImageID:         opts.ImageID,
			Backend:         opts.Backend,
			MD5:             opts.MD5,
			SHA256:          opts.SHA256,
			Site:            opts.Site,
			Gallery:         opts.Gallery,
			Folder:          opts.Folder,
			MaxItems:        opts.MaxItems,
			Offset:          opts.Offset,
			Root:            opts.Root,
			Auto:            opts.Auto,
			Priority:        opts.Priority,
			CreatedAt:       now,
			StatusChangedAt: now,
		},
		done: make(chan struct{}),
	}
}

// start moves a queued job to running. Returns an error if the transition
// is illegal (e.g. the job was canceled while pending).
func (j *Job) start(now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !validJobTransition(j.Status, JobRunning) {
		return fmt.Errorf("illegal job transition %s -> running", j.Status)
	}
	j.Status = JobRunning
	j.StartedAt = now
	j.StatusChangedAt = now
	return nil
}

// SetSite records the gallery-dl category the resolve pass discovered.
func (j *Job) SetSite(site string) {
	j.mu.Lock()
	j.Site = site
	j.mu.Unlock()
}

// SetURL records the post URL a hash import's walk resolved, so the row reads
// like a normal download's from then on.
func (j *Job) SetURL(url string) {
	j.mu.Lock()
	j.URL = url
	j.mu.Unlock()
}

// Subject is the row label: the URL, or the hash a lookup / hash import was
// keyed on while no URL is known yet.
func (j *Job) Subject() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.URL != "":
		return j.URL
	case j.MD5 != "":
		return "md5:" + j.MD5
	case j.SHA256 != "":
		return "sha256:" + j.SHA256
	}
	return ""
}

// SetGallery records the effective monbooru gallery the processor resolved
// for this job (per-site setting or default).
func (j *Job) SetGallery(name string) {
	j.mu.Lock()
	j.Gallery = name
	j.mu.Unlock()
}

// SetCapped records that the resolve hit the per-job item cap (so more posts
// may remain) and the limit that was applied.
func (j *Job) SetCapped(cap int) {
	j.mu.Lock()
	j.Capped = true
	j.Cap = cap
	j.mu.Unlock()
}

// SetItems installs the resolved item list (all pending). Called once after
// the resolve pass.
func (j *Job) SetItems(items []Item) {
	j.mu.Lock()
	cp := make([]Item, len(items))
	copy(cp, items)
	for i := range cp {
		if cp[i].Status == "" {
			cp[i].Status = ItemPending
		}
	}
	j.Items = cp
	j.mu.Unlock()
}

// UpdateItem applies mutate to item i under the job lock and commits it only
// when the resulting status is a legal transition from the prior one; an
// illegal transition is rejected without taking effect. The closure must not
// call back into Job methods (it already holds the lock). Returns false for
// an out-of-range index or a rejected transition.
func (j *Job) UpdateItem(i int, mutate func(*Item)) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i < 0 || i >= len(j.Items) {
		return false
	}
	prev := j.Items[i]
	cand := prev
	mutate(&cand)
	if cand.Status != prev.Status && !validItemTransition(prev.Status, cand.Status) {
		return false
	}
	j.Items[i] = cand
	return true
}

// status reads the job's lifecycle state under the lock, for a queue scan that
// needs a finished job's terminal status while holding the queue lock.
func (j *Job) status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status
}

// finishedAt reads the job's terminal timestamp under the lock, for the
// history sweep scanning the ring while holding the queue lock.
func (j *Job) finishedAt() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.FinishedAt
}

// seriesKey identifies the continue-series a job belongs to. Root is set for
// every job routed through Enqueue; the fallback covers a job built directly.
// Both fields are immutable after creation, so no lock is taken.
func (j *Job) seriesKey() int64 {
	if j.Root != 0 {
		return j.Root
	}
	return j.ID
}

// windowEnd is the offset just past this window's fetched range: its start plus
// the posts it took - the full cap when capped, else the items it resolved. A
// short window must still count what it fetched, or a continue from an earlier
// window in the series would re-fetch it. A window that has not resolved yet
// holds its whole reserved range, so a second continue issued while it is still
// pending enqueues the following window instead of a duplicate of it.
func (j *Job) windowEnd() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Capped {
		return j.Offset + j.Cap
	}
	if !j.finalized && len(j.Items) == 0 && j.MaxItems > 0 {
		return j.Offset + j.MaxItems
	}
	return j.Offset + len(j.Items)
}

// Fail records a job-level failure (e.g. the resolve pass errored) and
// transitions to failed. Any non-terminal items are marked failed with the
// same code so the summary reflects the abort.
func (j *Job) Fail(code, msg string, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finalized {
		return
	}
	j.ErrorCode = code
	j.Error = msg
	j.failPendingItems(code, msg)
	j.Status = JobFailed
	j.markFinishedLocked(now)
}

// Finalize computes the summary and derives the terminal status from the
// item outcomes. A no-op if already finalized.
func (j *Job) Finalize(now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finalized {
		return
	}
	j.restorePriorLinksLocked()
	j.Summary = summarize(j.Items)
	j.Status = deriveStatus(j.Items)
	j.markFinishedLocked(now)
}

// cancel marks the job canceled, recording any in-flight items as failed
// with the canceled error code.
func (j *Job) cancel(now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finalized {
		return
	}
	j.failPendingItems(ErrCodeCanceled, "job canceled")
	j.Summary = summarize(j.Items)
	j.Status = JobCanceled
	j.markFinishedLocked(now)
}

// reset returns a finished job to the queued state for Retry: the state is
// rebuilt from the identity fields the re-run keeps, so the prior run's items,
// summary, error, site, and timestamps zero out, and done is re-armed. The
// re-run bypasses the download-archive when force is set or the prior run did
// not fully import (failed or partial).
func (j *Job) reset(force bool, now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !validJobTransition(j.Status, JobQueued) {
		return fmt.Errorf("cannot retry a %s job", j.Status)
	}
	j.priorItems = priorImports(j.Items)
	url := j.URL
	if j.Kind == KindHashImport {
		// The URL was derived by the walk; clear it so the retry re-resolves
		// the hash rather than trusting a post that may have moved.
		url = ""
	}
	j.jobState = jobState{
		ID:      j.ID,
		URL:     url,
		Status:  JobQueued,
		Kind:    j.Kind,
		ImageID: j.ImageID,
		Backend: j.Backend,
		MD5:     j.MD5,
		SHA256:  j.SHA256,
		Gallery: j.Gallery,
		Folder:  j.Folder,
		// A job that did not fully import has items whose download landed in the
		// archive but whose push never reached monbooru; retrying past the archive
		// re-downloads and re-pushes them instead of archive-skipping them.
		Force:           force || j.Status == JobFailed || j.Status == JobPartial,
		MaxItems:        j.MaxItems,
		Offset:          j.Offset,
		Root:            j.Root,
		Auto:            j.Auto,
		Priority:        j.Priority,
		CreatedAt:       j.CreatedAt,
		StatusChangedAt: now,
	}
	j.finalized = false
	j.done = make(chan struct{})
	return nil
}

// failPendingItems marks every non-terminal item failed. Caller holds j.mu.
func (j *Job) failPendingItems(code, msg string) {
	for i := range j.Items {
		switch j.Items[i].Status {
		case ItemDone, ItemSkipped, ItemFailed:
			continue
		}
		j.Items[i].Status = ItemFailed
		j.Items[i].Outcome = OutcomeFailed
		if j.Items[i].ErrorCode == "" {
			j.Items[i].ErrorCode = code
			j.Items[i].Error = msg
		}
	}
}

// markFinishedLocked stamps the terminal state. The done channel is closed
// later by signalDone, after the queue has moved the job into the finished
// ring, so a Wait caller that wakes can safely Retry the job. Caller holds
// j.mu.
func (j *Job) markFinishedLocked(now time.Time) {
	j.finalized = true
	j.FinishedAt = now
	j.StatusChangedAt = now
}

// signalDone closes the done channel exactly once. Called by the queue once
// the job has fully left the running set.
func (j *Job) signalDone() {
	j.mu.Lock()
	defer j.mu.Unlock()
	select {
	case <-j.done:
	default:
		close(j.done)
	}
}

// doneChan returns the channel closed when the job reaches a terminal state.
func (j *Job) doneChan() chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

// Snapshot returns an independent copy of the job safe to read without
// further locking. The returned *Job carries a fresh (unused) mutex.
func (j *Job) Snapshot() *Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	state := j.jobState
	state.Items = make([]Item, len(j.Items))
	copy(state.Items, j.Items)
	// A running job's summary is only stamped at finalize; compute it live so the
	// queue and API track item progress instead of reading all-zeros until then.
	if !j.finalized {
		state.Summary = summarize(j.Items)
	}
	return &Job{jobState: state, finalized: j.finalized}
}

// Add returns the field-wise sum of two summaries, for merging a
// continue-series' windows into one queue row. Kept next to summarize so a new
// outcome counter cannot be tallied there but dropped here.
func (s Summary) Add(b Summary) Summary {
	return Summary{
		Created:   s.Created + b.Created,
		Duplicate: s.Duplicate + b.Duplicate,
		Enriched:  s.Enriched + b.Enriched,
		Skipped:   s.Skipped + b.Skipped,
		Failed:    s.Failed + b.Failed,
		Canceled:  s.Canceled + b.Canceled,
		Total:     s.Total + b.Total,
	}
}

// summarize tallies item outcomes into the job summary.
func summarize(items []Item) Summary {
	s := Summary{Total: len(items)}
	for _, it := range items {
		switch it.Outcome {
		case OutcomeCreated:
			s.Created++
		case OutcomeDuplicate:
			s.Duplicate++
		case OutcomeEnriched:
			s.Enriched++
		case OutcomeSkippedArchive, OutcomeSkippedUnsupported:
			s.Skipped++
		case OutcomeFailed:
			if it.ErrorCode == ErrCodeCanceled {
				s.Canceled++
			} else {
				s.Failed++
			}
		}
	}
	return s
}

// deriveStatus picks the terminal status from item outcomes.
// No items means the resolve pass found nothing to fetch, which is a
// success (a re-run where the archive already had everything lands here
// too, as those items are skipped, not failed).
func deriveStatus(items []Item) JobStatus {
	if len(items) == 0 {
		return JobSucceeded
	}
	failed := 0
	for _, it := range items {
		if it.Outcome == OutcomeFailed {
			failed++
		}
	}
	switch {
	case failed == 0:
		return JobSucceeded
	case failed == len(items):
		return JobFailed
	default:
		return JobPartial
	}
}

// priorImports indexes the items that landed in monbooru by post key, captured
// before a retry clears them.
func priorImports(items []Item) map[string]Item {
	var m map[string]Item
	for _, it := range items {
		if it.MonbooruID != 0 {
			if m == nil {
				m = map[string]Item{}
			}
			m[itemKey(it.PostID, it.Num)] = it
		}
	}
	return m
}

// restorePriorLinksLocked re-attaches the monbooru id and sha256 a post earned
// on a prior run to its skipped_archive item, so a plain retry keeps the
// back-link to the image it already imported. Caller holds j.mu.
func (j *Job) restorePriorLinksLocked() {
	if j.priorItems == nil {
		return
	}
	for i := range j.Items {
		it := &j.Items[i]
		if it.Outcome != OutcomeSkippedArchive || it.MonbooruID != 0 {
			continue
		}
		if prior, ok := j.priorItems[itemKey(it.PostID, it.Num)]; ok {
			it.MonbooruID = prior.MonbooruID
			it.SHA256 = prior.SHA256
		}
	}
}

// itemKey keys an item by its post (id plus pool page) for cross-run lookup.
func itemKey(postID string, num int) string {
	return postID + "#" + strconv.Itoa(num)
}
