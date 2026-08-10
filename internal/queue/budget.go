package queue

import (
	"time"

	"github.com/monbooru/monloader/internal/logx"
)

// lookupBudgetCounter names the budget's row in the store's counter table.
const lookupBudgetCounter = "scheduled_lookup"

// SetLookupBudget publishes how many budgeted lookups a day monloader
// accepts; 0 refuses them all. Settable rather than fixed at New so a
// settings save takes effect without a restart, like SetRetention.
func (q *Queue) SetLookupBudget(limit int) {
	q.mu.Lock()
	q.budgetLimit = max(limit, 0)
	q.mu.Unlock()
}

// TakeLookupBudget reserves one image's worth of budget, reporting whether
// there was any left. The reservation happens at accept time, not at run
// time: a job that fails still walked the chain and spent the politeness the
// budget exists to ration.
func (q *Queue) TakeLookupBudget() bool {
	day := q.now().Format(time.DateOnly)
	q.budgetWrite.Lock()
	defer q.budgetWrite.Unlock()
	q.mu.Lock()
	q.rollBudgetLocked(day)
	if q.budgetSpent >= q.budgetLimit {
		q.mu.Unlock()
		return false
	}
	q.budgetSpent++
	spent := q.budgetSpent
	q.mu.Unlock()
	q.persistBudget(day, spent)
	return true
}

// LookupBudget reports the cap, what is left of it today, and when the count
// rolls over, for the settings readout and monbooru's status probe.
func (q *Queue) LookupBudget() (limit, left int, resetsAt time.Time) {
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollBudgetLocked(now.Format(time.DateOnly))
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	return q.budgetLimit, max(q.budgetLimit-q.budgetSpent, 0), midnight
}

// rollBudgetLocked resets the count when the local date has turned over. The
// day it starts from is hydrated by UseStore, so a restart cannot refill a
// budget the day already spent and no take reaches the disk under mu. Caller
// holds mu.
func (q *Queue) rollBudgetLocked(day string) {
	if q.budgetDay != day {
		q.budgetDay, q.budgetSpent = day, 0
	}
}

// persistBudget mirrors the spend, best-effort like every other store write.
// Never called with q.mu held.
func (q *Queue) persistBudget(day string, spent int) {
	if q.store == nil {
		return
	}
	if err := q.store.SaveCounter(lookupBudgetCounter, day, spent); err != nil {
		logx.Warnf("queue: budget counter write failed: %v", err)
	}
}
