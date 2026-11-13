package queue

import (
	"slices"
	"time"
)

// A series is a search's windows: every indexed job under one root. Its
// answers are read from the windows each time, never kept, since a window
// changes under its own lock.
type series []*Job

// The only writes to q.index, so q.series always holds the same jobs.
func (q *Queue) indexLocked(j *Job) {
	q.index[j.ID] = j
	key := j.seriesKey()
	q.series[key] = append(q.series[key], j)
}

func (q *Queue) unindexLocked(id int64) {
	j := q.index[id]
	if j == nil {
		return
	}
	delete(q.index, id)
	key := j.seriesKey()
	rest := slices.DeleteFunc(q.series[key], func(o *Job) bool { return o.ID == id })
	if len(rest) == 0 {
		delete(q.series, key)
		return
	}
	q.series[key] = rest
}

func (s series) ids() []int64 {
	ids := make([]int64, 0, len(s))
	for _, j := range s {
		ids = append(ids, j.ID)
	}
	return ids
}

// widest is the widest window, not the last: a target cuts its last window
// to what was left, which is no size anyone asked for.
func (s series) extent() (high, widest int) {
	for _, j := range s {
		high = max(high, j.windowEnd())
		widest = max(widest, j.capOf())
	}
	return high, widest
}

func (s series) furthestPast(end int) *Job {
	var furthest *Job
	for _, j := range s {
		if j.windowEnd() > end && (furthest == nil || j.windowEnd() > furthest.windowEnd()) {
			furthest = j
		}
	}
	return furthest
}

func (s series) clearCapped() []*Job {
	var cleared []*Job
	for _, j := range s {
		if j.clearCapped() {
			cleared = append(cleared, j)
		}
	}
	return cleared
}

// A clean later window does not settle an earlier one, and a re-add owes as
// far as the furthest window that did not finish clean. A window still live
// has not finished at all.
func (s series) unimported(live func(int64) bool) (reach int, unimported bool) {
	for _, j := range s {
		if !live(j.ID) && j.status() != JobSucceeded {
			unimported = true
			reach = max(reach, j.windowEnd())
		}
	}
	return reach, unimported
}

// A series goes whole or not at all: the queue view sums it into one row.
func (s series) settledBefore(cutoff time.Time) bool {
	var newest time.Time
	for _, j := range s {
		if !j.noFollowUp() {
			return false
		}
		if t := j.finishedAt(); t.After(newest) {
			newest = t
		}
	}
	return !newest.After(cutoff)
}

// self stands in for its own entry: a quiet check has already left the
// index when it is recorded.
func (s series) snapshots(self *Job) []*Job {
	out := []*Job{self}
	for _, j := range s {
		if j.ID != self.ID {
			out = append(out, j.snapshot(false))
		}
	}
	return out
}

// self stands in for its own entry.
func (s series) owedTo(self *Job) int {
	var to int
	if self.owesArchivedPost() {
		to = self.windowEnd()
	}
	for _, j := range s {
		if j.ID != self.ID && j.owesArchivedPost() {
			to = max(to, j.windowEnd())
		}
	}
	return to
}
