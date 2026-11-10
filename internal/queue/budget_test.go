package queue

import (
	"sync"
	"testing"
	"time"
)

func TestLookupBudgetSpendsAndRefuses(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	if q.TakeLookupBudget() {
		t.Error("an unconfigured budget should refuse")
	}
	q.SetLookupBudget(2)
	for i := range 2 {
		if !q.TakeLookupBudget() {
			t.Fatalf("take %d refused, want the budget to allow it", i+1)
		}
	}
	if q.TakeLookupBudget() {
		t.Error("the third take passed, want it refused past the cap")
	}
	if limit, left, _ := q.LookupBudget(); limit != 2 || left != 0 {
		t.Errorf("budget = %d left = %d, want 2 and 0", limit, left)
	}

	q.SetLookupBudget(0)
	if q.TakeLookupBudget() {
		t.Error("a zero budget should refuse")
	}
}

func TestLookupBudgetRollsOverAtMidnight(t *testing.T) {
	day := time.Date(2026, 8, 5, 23, 59, 0, 0, time.Local)
	q := New(noopProcessor{}, 1, 100)
	q.now = func() time.Time { return day }
	q.SetLookupBudget(1)
	if !q.TakeLookupBudget() {
		t.Fatal("the first take was refused")
	}
	if q.TakeLookupBudget() {
		t.Fatal("the second take passed inside the same day")
	}
	day = day.Add(2 * time.Minute)
	if !q.TakeLookupBudget() {
		t.Error("the take after the date turned over was refused")
	}
	if _, left, _ := q.LookupBudget(); left != 0 {
		t.Errorf("left = %d after spending the rolled-over budget, want 0", left)
	}
}

func TestLookupBudgetSurvivesRestart(t *testing.T) {
	st := openTestQueueStore(t)
	q := storedQueue(st)
	q.SetLookupBudget(1)
	if !q.TakeLookupBudget() {
		t.Fatal("the first take was refused")
	}

	next := storedQueue(st)
	next.SetLookupBudget(1)
	if next.TakeLookupBudget() {
		t.Error("the take after a restart passed, want the spend to carry over")
	}
}

func TestLookupBudgetConcurrentTakesPersistInOrder(t *testing.T) {
	st := openTestQueueStore(t)
	q := storedQueue(st)
	q.SetLookupBudget(50)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.TakeLookupBudget()
		}()
	}
	wg.Wait()

	next := storedQueue(st)
	next.SetLookupBudget(50)
	if _, left, _ := next.LookupBudget(); left != 0 {
		t.Errorf("left after a restart = %d, want the whole spend to have been mirrored", left)
	}
}
