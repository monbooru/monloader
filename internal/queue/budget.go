package queue

import (
	"time"

	"github.com/monbooru/monloader/internal/logx"
)

const lookupBudgetCounter = "scheduled_lookup"

// A limit of 0 refuses every budgeted lookup.
func (q *Queue) SetLookupBudget(limit int) {
	q.mu.Lock()
	q.budgetLimit = max(limit, 0)
	q.mu.Unlock()
}

// TakeLookupBudget spends at accept time, not run time: a lookup that
// fails has still walked the chain the budget rations.
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

func (q *Queue) LookupBudget() (limit, left int, resetsAt time.Time) {
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollBudgetLocked(now.Format(time.DateOnly))
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	return q.budgetLimit, max(q.budgetLimit-q.budgetSpent, 0), midnight
}

func (q *Queue) rollBudgetLocked(day string) {
	if q.budgetDay != day {
		q.budgetDay, q.budgetSpent = day, 0
	}
}

// Caller must not hold q.mu.
func (q *Queue) persistBudget(day string, spent int) {
	if q.store == nil {
		return
	}
	if err := q.store.SaveCounter(lookupBudgetCounter, day, spent); err != nil {
		logx.Warnf("queue: budget counter write failed: %v", err)
	}
}
