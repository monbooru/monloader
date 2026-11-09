package queue

import (
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/contract"
)

type JobStatus string

const (
	JobQueued      JobStatus = contract.JobQueued
	JobRunning     JobStatus = contract.JobRunning
	JobSucceeded   JobStatus = contract.JobSucceeded
	JobPartial     JobStatus = contract.JobPartial
	JobFailed      JobStatus = contract.JobFailed
	JobCanceled    JobStatus = contract.JobCanceled
	JobInterrupted JobStatus = contract.JobInterrupted
)

func ValidJobStatus(s JobStatus) bool {
	switch s {
	case JobQueued, JobRunning, JobSucceeded, JobPartial, JobFailed, JobCanceled, JobInterrupted:
		return true
	}
	return false
}

type ItemStatus string

const (
	ItemPending    ItemStatus = "pending"
	ItemDownloaded ItemStatus = "downloaded"
	ItemUploaded   ItemStatus = "uploaded"
	ItemDone       ItemStatus = "done"
	ItemSkipped    ItemStatus = "skipped"
	ItemFailed     ItemStatus = "failed"
)

type Summary struct {
	Created   int `json:"created"`
	Duplicate int `json:"duplicate"`
	Enriched  int `json:"enriched,omitempty"`
	Replaced  int `json:"replaced,omitempty"`
	Skipped   int `json:"skipped"`
	// The archive half of Skipped, so a row can offer a force download
	// without its items.
	Archived int `json:"archived,omitempty"`
	// The rule half of Skipped: a row offers no force download for these,
	// since the archive never held them.
	Blacklisted int `json:"blacklisted,omitempty"`
	Failed      int `json:"failed"`
	Matched     int `json:"matched,omitempty"`
	Canceled    int `json:"canceled,omitempty"`
	Total       int `json:"total"`
}

type Item struct {
	PostID      string          `json:"post_id"`
	Num         int             `json:"num"`
	URL         string          `json:"url,omitempty"`
	Status      ItemStatus      `json:"status"`
	Outcome     outcome.Outcome `json:"outcome,omitempty"`
	MonbooruID  int64           `json:"monbooru_id,omitempty"`
	SHA256      string          `json:"sha256,omitempty"`
	TagWarnings []string        `json:"tag_warnings,omitempty"`
	MergeNote   string          `json:"merge_note,omitempty"`
	TagsSent    int             `json:"tags_sent,omitempty"`
	// Pre-phrased: what never reached the image, and why.
	Held      []string `json:"held_tags,omitempty"`
	ErrorCode string   `json:"error_code,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// A download job carries the zero JobKind.
type JobKind string

const (
	KindMetadata   JobKind = "metadata"
	KindLookup     JobKind = contract.KindLookup
	KindHashImport JobKind = "hash_import"
	KindReplace    JobKind = "replace"
	KindContrib    JobKind = "contrib"
	KindPTRLookup  JobKind = "ptr_lookup"
)

const (
	BackendBooru = contract.LookupBooru
	BackendPTR   = contract.LookupPTR
	BackendAll   = contract.LookupAll
)

// Split from the lock so Snapshot copies it whole; reset carries Options and
// zeroes the rest.
type jobState struct {
	Options
	ID          int64     `json:"id"`
	URL         string    `json:"url"`
	Status      JobStatus `json:"status"`
	Subcategory string    `json:"-"`
	Summary     Summary   `json:"summary"`
	Note        string    `json:"note,omitempty"`
	Capped      bool      `json:"capped,omitempty"`
	Cap         int       `json:"cap,omitempty"`
	// The next window of one its listing's error cut short starts here, not
	// past its range.
	ListedTo   int       `json:"-"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	Items      []Item    `json:"items"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// Set by every Status write, and not derived from the stamps above: a
	// retry re-queues a job but keeps its CreatedAt.
	StatusChangedAt time.Time `json:"-"`
}

// Job's mu guards its mutable fields: read them through Snapshot, change
// them through the methods.
type Job struct {
	mu sync.Mutex
	jobState

	finalized  bool
	done       chan struct{}
	itemCount  int
	seq        uint64
	priorItems map[string]Item
}

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
			Options:         opts,
			ID:              id,
			URL:             url,
			Status:          JobQueued,
			CreatedAt:       now,
			StatusChangedAt: now,
		},
		done: make(chan struct{}),
	}
}

func (j *Job) start(now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status != JobQueued {
		return fmt.Errorf("illegal job transition %s -> running", j.Status)
	}
	j.Status = JobRunning
	j.StartedAt = now
	j.StatusChangedAt = now
	return nil
}

func (j *Job) SetSite(site string) {
	j.mu.Lock()
	j.Site = site
	j.mu.Unlock()
}

func (j *Job) SetSubcategory(sub string) {
	j.mu.Lock()
	j.Subcategory = sub
	j.mu.Unlock()
}

func (j *Job) SetURL(url string) {
	j.mu.Lock()
	j.URL = url
	j.mu.Unlock()
}

func (j *Job) Subject() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.Kind == KindContrib && j.ContribBacklog:
		return "ptr contributions (retry)"
	case j.Kind == KindContrib:
		return "ptr contributions"
	case j.Kind == KindPTRLookup:
		return "ptr hash lookup"
	case j.URL != "":
		return j.URL
	case j.MD5 != "":
		return "md5:" + j.MD5
	case j.SHA256 != "":
		return "sha256:" + j.SHA256
	}
	return ""
}

func (j *Job) LookupClass() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.Watch != 0:
		return "watch"
	case j.Budgeted:
		return "scheduled"
	case j.Background:
		return "bulk"
	}
	return ""
}

// No lock: Priority and Background are fixed at enqueue and kept by a retry.
func (j *Job) lane() int {
	switch {
	case j.Priority:
		return 0
	case j.Background:
		return 2
	}
	return 1
}

const maxPTRLookupItems = 200

// The counts are per ask on both sides, so a re-asked hash raises matched
// with total; the items are folded per image, since a sweep re-asks what it
// has not resolved yet.
func (j *Job) recordPTRLookup(gallery string, asked int, matched []Item, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Gallery = gallery
	j.Summary.Total += asked
	j.Summary.Matched += len(matched)
	at := make(map[int64]int, len(j.Items))
	for i, it := range j.Items {
		at[it.MonbooruID] = i
	}
	for _, it := range matched {
		if i, seen := at[it.MonbooruID]; seen {
			j.Items[i] = it
			continue
		}
		if len(j.Items) < maxPTRLookupItems {
			at[it.MonbooruID] = len(j.Items)
			j.Items = append(j.Items, it)
		}
	}
	j.Status = JobSucceeded
	j.FinishedAt, j.StatusChangedAt = now, now
	if !j.finalized {
		j.finalized = true
		close(j.done)
	}
}

func (j *Job) SetGallery(name string) {
	j.mu.Lock()
	j.Gallery = name
	j.mu.Unlock()
}

func (j *Job) SetNote(note string) {
	j.mu.Lock()
	j.Note = note
	j.mu.Unlock()
}

func (j *Job) SetCapped(cap int) {
	j.mu.Lock()
	j.Capped = true
	j.Cap = cap
	j.mu.Unlock()
}

// CutShort names why a listing ended early, which no item of it can say.
func (j *Job) CutShort(code, msg string) {
	j.mu.Lock()
	j.ErrorCode, j.Error = code, msg
	j.mu.Unlock()
}

func (j *Job) SetListedTo(cap, listedTo int) {
	j.mu.Lock()
	j.Capped, j.Cap, j.ListedTo = true, cap, listedTo
	j.mu.Unlock()
}

func (j *Job) capEndLocked() int {
	if j.ListedTo > 0 {
		return j.ListedTo
	}
	return j.Offset + j.Cap
}

func (j *Job) capOf() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Cap
}

func (j *Job) clearCapped() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.Capped {
		return false
	}
	j.Capped = false
	return true
}

func (j *Job) SetItems(items []Item) {
	j.mu.Lock()
	cp := slices.Clone(items)
	for i := range cp {
		if cp[i].Status == "" {
			cp[i].Status = ItemPending
		}
	}
	j.Items = cp
	j.mu.Unlock()
}

func (j *Job) AddItems(items []Item) {
	j.mu.Lock()
	for _, it := range items {
		if it.Status == "" {
			it.Status = ItemPending
		}
		j.Items = append(j.Items, it)
	}
	j.mu.Unlock()
}

// mutate runs under j.mu and must not call Job methods; an illegal status
// change discards the whole mutation.
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

func (j *Job) status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status
}

func (j *Job) finishedAt() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.FinishedAt
}

// A capped success still has follow-up: its row carries the offset a
// continue needs.
func (j *Job) noFollowUp() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status == JobSucceeded && !j.Capped
}

// No lock: Root and ID are fixed before the job is indexed.
func (j *Job) seriesKey() int64 {
	if j.Root != 0 {
		return j.Root
	}
	return j.ID
}

// An unresolved window reserves its whole range, so a second continue
// skips past it.
func (j *Job) windowEnd() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Capped {
		return j.capEndLocked()
	}
	if !j.finalized && len(j.Items) == 0 && j.MaxItems > 0 {
		return j.Offset + j.MaxItems
	}
	return j.Offset + len(j.Items)
}

func (j *Job) renameGallery(from, to string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Gallery != from {
		return false
	}
	j.Gallery = to
	return true
}

func (j *Job) Fail(code, msg string, now time.Time) {
	j.settle(now, func() {
		j.ErrorCode = code
		j.Error = msg
		j.failPendingItems(code, msg)
		j.Summary = summarize(j.Items)
		j.Status = JobFailed
	})
}

func (j *Job) Finalize(now time.Time) {
	j.settle(now, func() {
		j.restorePriorLinksLocked()
		j.Summary = summarize(j.Items)
		j.Status = deriveStatus(j.Items)
		if j.Status == JobSucceeded && j.ErrorCode != "" {
			j.Status = JobPartial
		}
	})
}

func (j *Job) cancel(now time.Time) {
	j.settle(now, func() {
		j.failPendingItems(outcome.ErrCodeCanceled, "job canceled")
		j.Summary = summarize(j.Items)
		j.Status = JobCanceled
	})
}

// The items keep what they reached and no error code applies: nothing
// aborted them on purpose.
func (j *Job) interrupt(now time.Time) {
	j.settle(now, func() {
		j.Summary = summarize(j.Items)
		j.Status = JobInterrupted
	})
}

// apply runs under j.mu, and only on a job not settled yet.
func (j *Job) settle(now time.Time, apply func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finalized {
		return
	}
	apply()
	j.finalized = true
	j.FinishedAt = now
	j.StatusChangedAt = now
}

func (j *Job) reset(force bool, now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status == JobQueued || j.Status == JobRunning {
		return fmt.Errorf("cannot retry a %s job", j.Status)
	}
	if j.Kind == KindPTRLookup {
		// The endpoint answered in process; a worker would take the row
		// for a URL-less download and wipe the sweep.
		return fmt.Errorf("a batch hash lookup has nothing to re-run")
	}
	j.priorItems = priorImports(j.Items)
	url := j.URL
	if j.Kind == KindHashImport {
		// Re-resolve the hash rather than trust a post that may have moved.
		url = ""
	}
	opts := j.Options
	// Only a succeeded run may lean on the archive: a failure, cancel or
	// crash can land between a post's download and its push.
	opts.Force = force || j.Status != JobSucceeded
	j.jobState = jobState{
		Options:         opts,
		ID:              j.ID,
		URL:             url,
		Status:          JobQueued,
		CreatedAt:       j.CreatedAt,
		StatusChangedAt: now,
	}
	j.finalized = false
	// A retry inside finish's window would strand a waiter on the old channel.
	select {
	case <-j.done:
	default:
		close(j.done)
	}
	j.done = make(chan struct{})
	return nil
}

// Caller must hold j.mu.
func (j *Job) failPendingItems(code, msg string) {
	for i := range j.Items {
		switch j.Items[i].Status {
		case ItemDone, ItemSkipped, ItemFailed:
			continue
		}
		j.Items[i].Status = ItemFailed
		j.Items[i].Outcome = outcome.Failed
		if j.Items[i].ErrorCode == "" {
			j.Items[i].ErrorCode = code
			j.Items[i].Error = msg
		}
	}
}

// Call only after the queue has moved the job: a woken Wait may Retry it
// at once.
func (j *Job) signalDone() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.finalized {
		return
	}
	select {
	case <-j.done:
	default:
		close(j.done)
	}
}

func (j *Job) doneChan() chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

func (j *Job) Snapshot() *Job { return j.snapshot(true) }

func (j *Job) snapshot(withItems bool) *Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	state := j.jobState
	if withItems {
		// Not slices.Clone: a nil list must still marshal as [] for the
		// extension.
		state.Items = make([]Item, len(j.Items))
		copy(state.Items, j.Items)
	} else {
		state.Items = nil
	}
	// Only while unfinished: a finished summary is final, and a batch
	// lookup's counts run past the items it keeps.
	if !j.finalized {
		state.Summary = summarize(j.Items)
	}
	j.seq++
	return &Job{jobState: state, finalized: j.finalized, seq: j.seq, itemCount: len(j.Items)}
}

// ItemCount is set only on a snapshot, including one taken without items.
func (j *Job) ItemCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.itemCount
}

// A new Summary counter must be summed here too.
func (s Summary) Add(b Summary) Summary {
	return Summary{
		Created:   s.Created + b.Created,
		Duplicate: s.Duplicate + b.Duplicate,
		Enriched:  s.Enriched + b.Enriched,
		Replaced:  s.Replaced + b.Replaced,
		Skipped:   s.Skipped + b.Skipped,
		Archived:  s.Archived + b.Archived,
		Failed:    s.Failed + b.Failed,
		Matched:   s.Matched + b.Matched,
		Canceled:  s.Canceled + b.Canceled,
		Total:     s.Total + b.Total,

		Blacklisted: s.Blacklisted + b.Blacklisted,
	}
}

func summarize(items []Item) Summary {
	s := Summary{Total: len(items)}
	for _, it := range items {
		switch it.Outcome {
		case outcome.Created:
			s.Created++
		case outcome.Duplicate:
			s.Duplicate++
		case outcome.Enriched:
			s.Enriched++
		case outcome.Matched:
			s.Matched++
		case outcome.Replaced:
			s.Replaced++
		case outcome.SkippedArchive:
			s.Skipped++
			s.Archived++
		case outcome.SkippedUnsupported:
			s.Skipped++
		case outcome.SkippedBlacklist:
			s.Skipped++
			s.Blacklisted++
		case outcome.Failed:
			if it.ErrorCode == outcome.ErrCodeCanceled {
				s.Canceled++
			} else {
				s.Failed++
			}
		}
	}
	return s
}

func deriveStatus(items []Item) JobStatus {
	if len(items) == 0 {
		return JobSucceeded
	}
	failed := 0
	for _, it := range items {
		if it.Outcome == outcome.Failed {
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

func priorImports(items []Item) map[string]Item {
	m := map[string]Item{}
	for _, it := range items {
		if it.MonbooruID != 0 {
			m[itemKey(it.PostID, it.Num)] = it
		}
	}
	return m
}

func (j *Job) restorePriorLinksLocked() {
	for i := range j.Items {
		it := &j.Items[i]
		if it.Outcome != outcome.SkippedArchive || it.MonbooruID != 0 {
			continue
		}
		if prior, ok := j.priorItems[itemKey(it.PostID, it.Num)]; ok {
			it.MonbooruID = prior.MonbooruID
			it.SHA256 = prior.SHA256
		}
	}
}

func itemKey(postID string, num int) string {
	return postID + "#" + strconv.Itoa(num)
}
