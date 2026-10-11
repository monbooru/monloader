package queue

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/monbooru/monloader/internal/logx"
)

var (
	ErrWatchExists  = errors.New("that URL is already watched")
	ErrNoWatchStore = errors.New("watches need the queue store, which is not writable")
	ErrWatchLive    = errors.New("a check of this watch is already queued")
)

// WatchIntervals are the hours a watch waits between checks; 0 is manual.
var WatchIntervals = []int{0, 6, 12, 24, 72, 168}

const watchTick = time.Minute

type WatchCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	JobID     int64     `json:"job_id,omitempty"`
	Site      string    `json:"site,omitempty"`
	Status    JobStatus `json:"status,omitempty"`
	Created   int       `json:"created,omitempty"`
	Duplicate int       `json:"duplicate,omitempty"`
	Total     int       `json:"total,omitempty"`
	ErrorCode string    `json:"error_code,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Watch struct {
	ID            int64
	URL           string
	IntervalHours int
	// 0 takes the configured cap; ItemsAll chains windows until the check
	// reaches posts it already has.
	MaxItems  int
	CreatedAt time.Time
	Last      WatchCheck
	// Set only on the copies Watches returns.
	Live bool
}

func (w Watch) Next() time.Time {
	if w.IntervalHours == 0 {
		return time.Time{}
	}
	return w.Last.CheckedAt.Add(time.Duration(w.IntervalHours) * time.Hour)
}

func (q *Queue) WatchesAvailable() bool { return q.store != nil }

func (q *Queue) Watches() []Watch {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Watch, len(q.watches))
	for i, w := range q.watches {
		out[i] = *w
		out[i].Live = q.watchLiveLocked(w.ID)
	}
	return out
}

// from is the finished job the watch was started from, which counts as its
// first check; nil leaves the watch due at once.
func (q *Queue) AddWatch(url string, intervalHours, maxItems int, from *Job) (int64, error) {
	if q.store == nil {
		return 0, ErrNoWatchStore
	}
	var last WatchCheck
	if from != nil {
		// The row stands for its whole series, not the window it was
		// clicked on.
		q.mu.Lock()
		last = checkOfSeries(q.seriesSnapshotsLocked(from.seriesKey(), from))
		q.mu.Unlock()
	}
	now := q.now()
	id, err := q.store.InsertWatch(url, intervalHours, maxItems, now, last)
	if err != nil {
		return 0, err
	}
	q.mu.Lock()
	q.watches = append(q.watches, &Watch{ID: id, URL: url, IntervalHours: intervalHours, MaxItems: maxItems, CreatedAt: now, Last: last})
	q.mu.Unlock()
	return id, nil
}

func (q *Queue) SetWatchInterval(id int64, intervalHours int) error {
	return q.setWatch(id, func() error { return q.store.SetWatchInterval(id, intervalHours) },
		func(w *Watch) { w.IntervalHours = intervalHours })
}

func (q *Queue) SetWatchItems(id int64, maxItems int) error {
	return q.setWatch(id, func() error { return q.store.SetWatchItems(id, maxItems) },
		func(w *Watch) { w.MaxItems = maxItems })
}

// The store is written first: it holds the only copy of a watch.
func (q *Queue) setWatch(id int64, save func() error, apply func(*Watch)) error {
	if q.store == nil {
		return ErrNoWatchStore
	}
	if q.findWatch(id) == nil {
		return ErrNotFound
	}
	if err := save(); err != nil {
		return err
	}
	q.mu.Lock()
	if w := q.findWatchLocked(id); w != nil {
		apply(w)
	}
	q.mu.Unlock()
	return nil
}

// A running check finishes, since its downloads stay; one still queued
// would only fetch for a search nobody watches.
func (q *Queue) RemoveWatch(id int64) error {
	var queued []int64
	err := q.setWatch(id, func() error { return q.store.DeleteWatch(id) }, func(*Watch) {
		q.watches = slices.DeleteFunc(q.watches, func(w *Watch) bool { return w.ID == id })
		for _, j := range q.pending {
			if j.Watch == id {
				queued = append(queued, j.ID)
			}
		}
	})
	for _, jid := range queued {
		_ = q.cancelKeepingRow(jid)
	}
	return err
}

// CheckWatch queues a check now; background puts it behind the operator's
// own work, as the schedule does.
func (q *Queue) CheckWatch(id int64, background bool) (int64, error) {
	q.mu.Lock()
	w := q.findWatchLocked(id)
	if w == nil {
		q.mu.Unlock()
		return 0, ErrNotFound
	}
	if q.watchLiveLocked(id) {
		q.mu.Unlock()
		return 0, ErrWatchLive
	}
	j := q.enqueueLocked(w.URL, Options{Watch: id, Background: background}.WithItems(w.MaxItems))
	q.mu.Unlock()
	q.persist(j)
	return j.ID, nil
}

// A tick where held returns true is skipped, leaving its watches due for
// the next one.
func (q *Queue) RunWatches(ctx context.Context, held func() bool) {
	t := time.NewTicker(watchTick)
	defer t.Stop()
	for {
		if !held() {
			q.queueDueWatches()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (q *Queue) queueDueWatches() {
	now := q.now()
	q.mu.Lock()
	var ids []int64
	for _, w := range q.watches {
		if w.IntervalHours > 0 && !now.Before(w.Next()) && !q.watchLiveLocked(w.ID) {
			ids = append(ids, w.ID)
		}
	}
	q.mu.Unlock()
	for _, id := range ids {
		_, _ = q.CheckWatch(id, true)
	}
}

func (q *Queue) findWatch(id int64) *Watch {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.findWatchLocked(id)
}

func (q *Queue) findWatchLocked(id int64) *Watch {
	for _, w := range q.watches {
		if w.ID == id {
			return w
		}
	}
	return nil
}

func (q *Queue) watchLiveLocked(id int64) bool {
	return q.liveLocked(func(j *Job) bool { return j.Watch == id })
}

// Stamped at start, not at enqueue: a check still waiting at shutdown
// leaves its watch due.
func (q *Queue) stampWatch(j *Job) {
	s := j.snapshot(false)
	if s.Watch == 0 || s.Root != s.ID {
		return
	}
	q.updateWatchCheck(s.Watch, func(c *WatchCheck) {
		*c = WatchCheck{CheckedAt: s.StartedAt, JobID: s.ID, Site: c.Site, Status: s.Status}
	})
}

// An operator's cancel of a check that never started counts as the check:
// left unstamped, the watch is still due and the next tick queues it again.
// The shutdown drop does not come here, so a stop leaves the watch due.
func (q *Queue) stampCanceledCheck(j *Job) {
	s := j.snapshot(false)
	if s.Watch == 0 || s.Root != s.ID || !s.StartedAt.IsZero() {
		return
	}
	q.updateWatchCheck(s.Watch, func(c *WatchCheck) {
		*c = WatchCheck{CheckedAt: s.FinishedAt, JobID: s.ID, Site: c.Site, Status: JobCanceled}
	})
}

// Rebuilt from the series on every window, so a retried window replaces
// what it reported instead of adding to it.
func (q *Queue) recordWatch(j *Job) {
	s := j.snapshot(false)
	if s.Watch == 0 {
		return
	}
	q.mu.Lock()
	windows := q.seriesSnapshotsLocked(s.seriesKey(), s)
	q.mu.Unlock()
	q.updateWatchCheck(s.Watch, func(c *WatchCheck) {
		// A retry made after a newer check is not that check's.
		if c.JobID != s.seriesKey() {
			return
		}
		checkedAt := c.CheckedAt
		*c = checkOfSeries(windows)
		c.CheckedAt = checkedAt
	})
}

// self stands in for its own entry: a quiet check has already left the
// index when it is recorded.
func (q *Queue) seriesSnapshotsLocked(root int64, self *Job) []*Job {
	out := []*Job{self}
	for id, j := range q.index {
		if id != self.ID && j.seriesKey() == root {
			out = append(out, j.snapshot(false))
		}
	}
	return out
}

// Only finished windows count; the first that did not succeed names the
// check's status.
func checkOfSeries(windows []*Job) WatchCheck {
	slices.SortFunc(windows, func(a, b *Job) int { return cmp.Compare(a.ID, b.ID) })
	c := checkOf(windows[0])
	c.Created, c.Duplicate, c.Total = 0, 0, 0
	c.Status, c.ErrorCode, c.Error = JobSucceeded, "", ""
	for _, w := range windows {
		if w.Status == JobQueued || w.Status == JobRunning {
			continue
		}
		c.Created += w.Summary.Created
		c.Duplicate += w.Summary.Duplicate
		c.Total += w.Summary.Total
		if c.Status == JobSucceeded && w.Status != JobSucceeded {
			c.Status, c.ErrorCode, c.Error = w.Status, w.ErrorCode, w.Error
		}
	}
	return c
}

func (q *Queue) updateWatchCheck(id int64, update func(*WatchCheck)) {
	q.mu.Lock()
	w := q.findWatchLocked(id)
	if w == nil {
		q.mu.Unlock()
		return
	}
	update(&w.Last)
	last := w.Last
	q.mu.Unlock()
	if q.store == nil {
		return
	}
	if err := q.store.SaveWatchCheck(id, last); err != nil {
		logx.Warnf("queue: watch %d check write failed: %v", id, err)
	}
}

func checkOf(s *Job) WatchCheck {
	return WatchCheck{
		CheckedAt: cmp.Or(s.StartedAt, s.FinishedAt),
		JobID:     s.ID,
		Site:      s.Site,
		Status:    s.Status,
		Created:   s.Summary.Created,
		Duplicate: s.Summary.Duplicate,
		Total:     s.Summary.Total,
		ErrorCode: s.ErrorCode,
		Error:     s.Error,
	}
}

// A check that reached a post the archive holds has caught up with the
// last one; only an all-new window may have more past it.
func (j *Job) settleWatchCap() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Watch != 0 && j.Capped && j.Summary.Archived > 0 {
		j.Capped = false
	}
}

// A check that found nothing leaves no row: its result is on the watch. A
// later window stays, since its series row already shows.
func (j *Job) quietWatchCheck() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Watch != 0 && j.Root == j.ID && j.Status == JobSucceeded && !j.Capped && j.Summary.Created == 0
}
