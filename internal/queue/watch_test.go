package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/outcome"
)

func archivedItems(j *Job, created, archived int) {
	items := make([]Item, created+archived)
	j.SetItems(items)
	for i := range created {
		j.UpdateItem(i, func(it *Item) { it.Status = ItemDownloaded })
		j.UpdateItem(i, func(it *Item) { it.Status = ItemUploaded })
		j.UpdateItem(i, func(it *Item) { it.Status = ItemDone; it.Outcome = outcome.Created })
	}
	for i := created; i < created+archived; i++ {
		j.UpdateItem(i, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.SkippedArchive })
	}
}

func blacklistedItems(j *Job, n int) {
	j.SetItems(make([]Item, n))
	for i := range n {
		j.UpdateItem(i, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.SkippedBlacklist })
	}
}

func waitWatchSettled(t *testing.T, q *Queue, id int64) Watch {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, w := range q.Watches() {
			if w.ID == id && !w.Live && w.Last.Status != "" && w.Last.Status != JobRunning {
				return w
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("watch %d never recorded a finished check", id)
	return Watch{}
}

func waitWatchTotal(t *testing.T, q *Queue, id int64, total int) {
	t.Helper()
	if !eventually(2*time.Second, func() bool {
		return slices.ContainsFunc(q.Watches(), func(w Watch) bool { return w.ID == id && w.Last.Total == total })
	}) {
		t.Fatalf("watch %d never counted %d posts", id, total)
	}
}

func watchQueue(t *testing.T, proc Processor) (*Queue, *Store) {
	t.Helper()
	st := openTestQueueStore(t)
	q := New(proc, 1, 100)
	q.UseStore(st)
	q.Start()
	t.Cleanup(q.Close)
	return q, st
}

func TestWatchNeedsTheStore(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	if q.WatchesAvailable() {
		t.Error("a queue without a store reports watches available")
	}
	if _, err := q.AddWatch("https://x/posts?tags=a", 24, 0, nil); !errors.Is(err, ErrNoWatchStore) {
		t.Errorf("AddWatch without a store = %v, want ErrNoWatchStore", err)
	}
}

func TestWatchSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	q := storedQueue(st)
	id, err := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	if err != nil {
		t.Fatalf("AddWatch: %v", err)
	}
	if _, err := q.AddWatch("https://x/posts?tags=a", 6, 0, nil); !errors.Is(err, ErrWatchExists) {
		t.Errorf("a second watch of the same URL = %v, want ErrWatchExists", err)
	}
	gone, _ := q.AddWatch("https://x/posts?tags=b", 24, 0, nil)
	if err := q.SetWatchInterval(id, 168); err != nil {
		t.Fatalf("SetWatchInterval: %v", err)
	}
	if err := q.RemoveWatch(gone); err != nil {
		t.Fatalf("RemoveWatch: %v", err)
	}
	if err := q.RemoveWatch(gone); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing a removed watch = %v, want ErrNotFound", err)
	}
	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	q2 := storedQueue(st2)
	ws := q2.Watches()
	if len(ws) != 1 || ws[0].ID != id || ws[0].IntervalHours != 168 {
		t.Fatalf("reloaded watches = %+v, want only watch %d every 168h", ws, id)
	}
}

func TestDueWatchesQueueOneCheckEach(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q := New(noopProcessor{}, 1, 100)
	q.now = func() time.Time { return now }
	q.UseStore(openTestQueueStore(t))
	due, _ := q.AddWatch("https://x/posts?tags=due", 24, 0, nil)
	manual, _ := q.AddWatch("https://x/posts?tags=manual", 0, 0, nil)
	from := newJob(99, "https://x/posts?tags=fresh", Options{}, now)
	from.StartedAt = now.Add(-time.Hour)
	fresh, _ := q.AddWatch(from.URL, 24, 0, from)

	q.queueDueWatches()
	q.queueDueWatches()
	if len(q.pending) != 1 {
		t.Fatalf("pending = %d jobs, want one check for the due watch", len(q.pending))
	}
	j := q.pending[0]
	if j.Watch != due || !j.Background || j.LookupClass() != "watch" {
		t.Errorf("check = watch %d background %v class %q, want watch %d in the background lane", j.Watch, j.Background, j.LookupClass(), due)
	}

	now = now.Add(23 * time.Hour)
	q.queueDueWatches()
	if len(q.pending) != 2 || q.pending[1].Watch != fresh {
		t.Errorf("pending after a day = %d, want the fresh watch's check added", len(q.pending))
	}
	for _, p := range q.pending {
		if p.Watch == manual {
			t.Error("a manual watch was queued by the schedule")
		}
	}
}

func TestWatchCheckRecordsItsResult(t *testing.T) {
	proc := procFunc(func(ctx context.Context, j *Job) error {
		j.SetSite("danbooru")
		j.SetSubcategory("tag")
		archivedItems(j, 2, 3)
		return nil
	})
	q, _ := watchQueue(t, proc)
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	jobID, err := q.CheckWatch(id, false)
	if err != nil {
		t.Fatalf("CheckWatch: %v", err)
	}
	j := waitFor(t, q, jobID)
	w := waitWatchSettled(t, q, id)
	if w.Last.Created != 2 || w.Last.Total != 5 || w.Last.Site != "danbooru" || w.Last.Subcategory != "tag" || w.Last.JobID != jobID {
		t.Errorf("last check = %+v, want 2 created of 5 from danbooru's tag extractor, job %d", w.Last, jobID)
	}
	if !w.Last.CheckedAt.Equal(j.StartedAt) {
		t.Errorf("checked at %v, want the job's start %v", w.Last.CheckedAt, j.StartedAt)
	}
	if _, err := q.Get(jobID); err != nil {
		t.Errorf("a check that created posts lost its row: %v", err)
	}
}

func TestQuietWatchCheckLeavesNoRow(t *testing.T) {
	q, st := watchQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		archivedItems(j, 0, 4)
		return nil
	}))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	jobID, _ := q.CheckWatch(id, true)
	w := waitWatchSettled(t, q, id)
	if w.Last.Total != 4 || w.Last.Created != 0 {
		t.Errorf("last check = %+v, want 4 seen and none created", w.Last)
	}
	if _, err := q.Get(jobID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a check that found nothing kept its row: %v", err)
	}
	if _, ok := storedStatus(t, st, jobID); ok {
		t.Error("a check that found nothing kept its store row")
	}
}

func TestCappedAllBlacklistedWatchCheckLeavesNoRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxItems int
	}{
		{"cap setting", 0},
		{"its own number", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := watchQueue(t, procFunc(func(ctx context.Context, j *Job) error {
				blacklistedItems(j, 2)
				j.SetCapped(2)
				return nil
			}))
			id, _ := q.AddWatch("https://x/posts?tags=a", 24, tc.maxItems, nil)
			jobID, _ := q.CheckWatch(id, true)
			if w := waitWatchSettled(t, q, id); w.Last.Total != 2 || w.Last.Created != 0 {
				t.Errorf("last check = %+v, want 2 seen and none created", w.Last)
			}
			if _, err := q.Get(jobID); !errors.Is(err, ErrNotFound) {
				t.Errorf("an all-blacklisted check kept its row: %v", err)
			}
		})
	}
}

func TestAWatchCheckCountsItsBlacklistedPosts(t *testing.T) {
	q, _ := watchQueue(t, procFunc(func(_ context.Context, j *Job) error {
		blacklistedItems(j, 3)
		return nil
	}))
	id, err := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CheckWatch(id, false); err != nil {
		t.Fatal(err)
	}
	if w := waitWatchSettled(t, q, id); w.Last.Blacklisted != 3 || w.Last.Total != 3 {
		t.Errorf("last check = %+v, want 3 blacklisted of 3", w.Last)
	}
}

func TestWatchChainWalksPastAnAllBlacklistedWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxItems int
	}{
		{"all", ItemsAll},
		{"past the cap", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := watchQueue(t, procFunc(func(ctx context.Context, j *Job) error {
				if j.Snapshot().Offset > 0 {
					archivedItems(j, 1, 0)
					return nil
				}
				blacklistedItems(j, 2)
				j.SetCapped(2)
				return nil
			}))
			id, _ := q.AddWatch("https://x/posts?tags=a", 24, tc.maxItems, nil)
			jobID, _ := q.CheckWatch(id, true)
			waitWatchTotal(t, q, id, 3)
			if _, err := q.Get(jobID); err != nil {
				t.Errorf("the chain's first window lost its row: %v", err)
			}
		})
	}
}

func TestWatchCheckCappedOnlyWhenTheWholeWindowIsNew(t *testing.T) {
	for _, tc := range []struct {
		name              string
		created, archived int
		wantCapped        bool
	}{
		{"caught up", 1, 1, false},
		{"all new", 2, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := watchQueue(t, procFunc(func(ctx context.Context, j *Job) error {
				archivedItems(j, tc.created, tc.archived)
				j.SetCapped(2)
				return nil
			}))
			id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
			jobID, _ := q.CheckWatch(id, true)
			if j := waitFor(t, q, jobID); j.Capped != tc.wantCapped {
				t.Errorf("capped = %v, want %v", j.Capped, tc.wantCapped)
			}
		})
	}
}

func TestCloseDropsQueuedWatchChecks(t *testing.T) {
	st := openTestQueueStore(t)
	q := storedQueue(st)
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	jobID, _ := q.CheckWatch(id, true)
	other := q.Enqueue("https://x/posts/1", Options{})
	q.Close()
	if _, err := q.Get(jobID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a queued check survived shutdown: %v", err)
	}
	if _, ok := storedStatus(t, st, jobID); ok {
		t.Error("a queued check kept its store row past shutdown")
	}
	if j, _ := q.Get(other); j == nil || j.Status != JobCanceled {
		t.Error("an ordinary queued job was not settled canceled")
	}
	if w := q.Watches()[0]; !w.Last.CheckedAt.IsZero() {
		t.Errorf("the dropped check stamped its watch at %v; it must stay due", w.Last.CheckedAt)
	}
}

func TestWatchCheckStaysOnTheArchive(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	url := "https://x/posts?tags=a"
	parkFinished(q, 50, time.Now(), func(j *Job) {
		j.URL = url
		j.Status = JobFailed
	})
	id, _ := q.AddWatch(url, 24, 0, nil)
	jobID, _ := q.CheckWatch(id, true)
	j, _ := q.Get(jobID)
	if j.Force {
		t.Error("a watch check took the re-add archive bypass")
	}
	if paste, _ := q.Get(q.Enqueue(url, Options{})); !paste.Force {
		t.Error("a pasted re-add lost its archive bypass")
	}
}

func TestRetryKeepsTheWatch(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	j := parkFinished(q, 1, time.Now(), func(j *Job) {
		j.Status = JobFailed
		j.Watch = 7
	})
	mustRetry(t, q, 1)
	if j.Watch != 7 {
		t.Errorf("retry dropped the watch: %d", j.Watch)
	}
}

func TestWatchItemsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	q := storedQueue(st)
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 10, nil)
	if got := q.Watches()[0].MaxItems; got != 10 {
		t.Errorf("items after add = %d, want 10", got)
	}
	if err := q.SetWatchItems(id, ItemsAll); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	q2 := storedQueue(st2)
	if got := q2.Watches()[0].MaxItems; got != ItemsAll {
		t.Errorf("reloaded items = %d, want all", got)
	}
}

func TestWatchTableWithoutItemsMigrates(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", storeDSN(filepath.Join(dir, storeFile)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE watches (id INTEGER PRIMARY KEY, url TEXT NOT NULL UNIQUE,
		interval_hours INTEGER NOT NULL, created_at TEXT NOT NULL, last TEXT NOT NULL DEFAULT '');
		INSERT INTO watches (url, interval_hours, created_at) VALUES ('https://x/posts?tags=a', 24, '2026-10-06T00:00:00Z')`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore on a table without max_items: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws, err := st.LoadWatches()
	if err != nil || len(ws) != 1 || ws[0].MaxItems != 0 {
		t.Errorf("migrated watches = %+v (%v), want one taking the cap", ws, err)
	}
}

type failingChecks struct {
	mu    sync.Mutex
	codes []string
	force []bool
}

func (p *failingChecks) Process(_ context.Context, j *Job) error {
	s := j.Snapshot()
	p.mu.Lock()
	code := ""
	if n := len(p.force); n < len(p.codes) {
		code = p.codes[n]
	}
	p.force = append(p.force, s.Force)
	p.mu.Unlock()
	j.SetItems(make([]Item, 1))
	j.UpdateItem(0, func(it *Item) {
		if code == "" {
			it.Status, it.Outcome = ItemDone, outcome.Created
		} else {
			it.Status, it.Outcome, it.ErrorCode = ItemFailed, outcome.Failed, code
		}
	})
	return nil
}

func TestACheckAfterAFailedPushBypassesTheArchiveOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codes []string
		force []bool
	}{
		{"monbooru back on the next check", []string{outcome.ErrCodeMonbooruUnreachable, ""}, []bool{false, true, false}},
		{"an outage across checks", []string{outcome.ErrCodeMonbooruUnreachable, outcome.ErrCodeMonbooruUnreachable, ""}, []bool{false, true, true, false}},
		{"a refusal that repeats is retried once", []string{outcome.ErrCodeMonbooruRejected, outcome.ErrCodeMonbooruRejected, outcome.ErrCodeMonbooruRejected}, []bool{false, true, false, true}},
		{"a file that failed to download was never archived", []string{outcome.ErrCodeDownloadFailed, ""}, []bool{false, false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := &failingChecks{codes: tc.codes}
			q, _ := watchQueue(t, proc)
			id, err := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			for range tc.force {
				before := q.findWatch(id).Last.JobID
				jobID, err := q.CheckWatch(id, false)
				if err != nil {
					t.Fatal(err)
				}
				if jobID == before {
					t.Fatal("no new check")
				}
				if _, err := q.Wait(context.Background(), jobID); err != nil {
					t.Fatal(err)
				}
				waitWatchSettled(t, q, id)
				q.Clear()
			}
			proc.mu.Lock()
			defer proc.mu.Unlock()
			if !slices.Equal(proc.force, tc.force) {
				t.Errorf("forced checks = %v, want %v", proc.force, tc.force)
			}
		})
	}
}

func TestAnOwedCheckSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	q := New(&failingChecks{codes: []string{outcome.ErrCodeMonbooruUnreachable}}, 1, 100)
	q.UseStore(st)
	q.Start()
	id, err := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := q.CheckWatch(id, false)
	if _, err := q.Wait(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	waitWatchSettled(t, q, id)
	q.Close()
	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	q2 := storedQueue(st2)
	if ws := q2.Watches(); len(ws) != 1 || !ws[0].Last.Owed {
		t.Errorf("reloaded watches = %+v, want the check still owed", ws)
	}
}

func TestCheckWatchPassesItsItems(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	ten, _ := q.AddWatch("https://x/posts?tags=ten", 24, 10, nil)
	all, _ := q.AddWatch("https://x/posts?tags=all", 24, ItemsAll, nil)
	tenJob, _ := q.CheckWatch(ten, true)
	allJob, _ := q.CheckWatch(all, true)
	if j, _ := q.Get(tenJob); j.MaxItems != 10 || j.Auto {
		t.Errorf("check of a 10-item watch = max %d auto %v, want 10 and no chain", j.MaxItems, j.Auto)
	}
	if j, _ := q.Get(allJob); j.MaxItems != 0 || !j.Auto {
		t.Errorf("check of an all watch = max %d auto %v, want the cap and the chain", j.MaxItems, j.Auto)
	}
}

func TestWatchCheckChainsUntilItCatchesUp(t *testing.T) {
	q, _ := watchQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		if j.Snapshot().Offset == 0 {
			archivedItems(j, 2, 0)
		} else {
			archivedItems(j, 0, 2)
		}
		j.SetCapped(2)
		return nil
	}))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, ItemsAll, nil)
	first, _ := q.CheckWatch(id, true)

	var w Watch
	settled := func() bool {
		jobs, _ := q.List(ListOptions{})
		return len(jobs) == 2 && !jobs[0].Capped && !jobs[1].Capped
	}
	eventually(2*time.Second, func() bool {
		w = q.Watches()[0]
		return !w.Live && w.Last.Total == 4 && settled()
	})
	if w.Last.Total != 4 || w.Last.Created != 2 || w.Last.JobID != first {
		t.Fatalf("last check = %+v, want both windows folded into job %d: 2 created of 4", w.Last, first)
	}
	if j, _ := q.Get(first); !w.Last.CheckedAt.Equal(j.StartedAt) {
		t.Errorf("checked at %v, want the first window's start %v", w.Last.CheckedAt, j.StartedAt)
	}
	jobs, _ := q.List(ListOptions{})
	if len(jobs) != 2 {
		t.Fatalf("history holds %d windows, want both: a later window is never dropped as quiet", len(jobs))
	}
	for _, j := range jobs {
		if j.Watch != id || !j.Background || j.Capped {
			t.Errorf("window %d = watch %d background %v capped %v, want the check's watch and lane, caught up", j.ID, j.Watch, j.Background, j.Capped)
		}
	}
}

func TestRemovedWatchIDIsNeverReissued(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	q := storedQueue(st)
	old, _ := q.AddWatch("https://x/posts?tags=typo", 24, 0, nil)
	if err := q.RemoveWatch(old); err != nil {
		t.Fatalf("RemoveWatch: %v", err)
	}
	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	q2 := storedQueue(st2)
	if id, _ := q2.AddWatch("https://x/posts?tags=fixed", 24, 0, nil); id == old {
		t.Errorf("the corrected watch took the removed watch's id %d", old)
	}
}

func TestRemovingAWatchCancelsItsQueuedCheck(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	jobID, _ := q.CheckWatch(id, false)
	if err := q.RemoveWatch(id); err != nil {
		t.Fatalf("RemoveWatch: %v", err)
	}
	if j, _ := q.Get(jobID); j == nil || j.Status != JobCanceled {
		t.Errorf("the removed watch's queued check = %+v, want it canceled", j)
	}
	next, _ := q.AddWatch("https://x/posts?tags=b", 24, 0, nil)
	if _, err := q.CheckWatch(next, false); err != nil {
		t.Errorf("the next watch's first check was refused: %v", err)
	}
}

func TestCanceledQueuedCheckIsNotQueuedAgain(t *testing.T) {
	for name, cancel := range map[string]func(q *Queue, jobID int64){
		"row cancel":     func(q *Queue, jobID int64) { _ = q.CancelLive(jobID) },
		"api delete":     func(q *Queue, jobID int64) { _ = q.CancelSeries(jobID) },
		"cancel pending": func(q *Queue, _ int64) { q.CancelPending() },
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			q := New(noopProcessor{}, 1, 100)
			q.now = func() time.Time { return now }
			q.UseStore(openTestQueueStore(t))
			id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
			q.queueDueWatches()
			if len(q.pending) != 1 {
				t.Fatalf("pending = %d, want the due check", len(q.pending))
			}
			jobID := q.pending[0].ID
			cancel(q, jobID)

			now = now.Add(watchTick)
			q.queueDueWatches()
			if len(q.pending) != 0 {
				t.Errorf("pending after the next tick = %d, want the canceled check to stay canceled", len(q.pending))
			}
			if w := q.Watches()[0]; w.ID != id || w.Last.Status != JobCanceled || w.Last.JobID != jobID {
				t.Errorf("last check = %+v, want the canceled job %d", w.Last, jobID)
			}
		})
	}
}

func finishedWindow(q *Queue, id, root int64, watch int64, created int, status JobStatus) *Job {
	return parkFinished(q, id, time.Now(), func(j *Job) {
		j.Root, j.Watch = root, watch
		archivedItems(j, created, 0)
		j.Status = status
		j.Summary = summarize(j.Items)
		j.StartedAt = time.Now()
	})
}

func TestARetriedLaterWindowReplacesWhatItReported(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	first := finishedWindow(q, 10, 10, id, 2, JobSucceeded)
	q.stampWatch(first)
	q.recordWatch(first)
	later := finishedWindow(q, 11, 10, id, 0, JobFailed)
	q.recordWatch(later)
	if w := q.Watches()[0]; w.Last.Status != JobFailed {
		t.Fatalf("last check = %+v, want the failed window's status", w.Last)
	}

	archivedItems(later, 3, 0)
	later.Status, later.Summary = JobSucceeded, summarize(later.Items)
	q.recordWatch(later)
	w := q.Watches()[0]
	if w.Last.Status != JobSucceeded || w.Last.Created != 5 || w.Last.Total != 5 || w.Last.JobID != 10 {
		t.Errorf("last check = %+v, want the retry to clear the failure and count each window once: 5 of 5 from job 10", w.Last)
	}
}

func TestAnOldWindowRetriedAfterANewerCheckLeavesItAlone(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	old := finishedWindow(q, 10, 10, id, 1, JobSucceeded)
	q.stampWatch(old)
	oldLater := finishedWindow(q, 11, 10, id, 1, JobSucceeded)
	newer := finishedWindow(q, 20, 20, id, 4, JobSucceeded)
	q.stampWatch(newer)
	q.recordWatch(newer)

	q.recordWatch(oldLater)
	if w := q.Watches()[0]; w.Last.JobID != 20 || w.Last.Created != 4 {
		t.Errorf("last check = %+v, want the newer check's 4 untouched", w.Last)
	}
}

func TestAnOldCheckRetriedAfterANewerOneLeavesItsRecordAlone(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	old := finishedWindow(q, 10, 10, id, 1, JobPartial)
	newer := finishedWindow(q, 20, 20, id, 4, JobSucceeded)
	q.stampWatch(newer)
	q.recordWatch(newer)
	q.updateWatchCheck(id, func(c *WatchCheck) { c.Owed, c.OwedTo = true, 4 })
	want := q.Watches()[0].Last

	q.stampWatch(old)
	q.recordWatch(old)
	if got := q.Watches()[0].Last; got != want {
		t.Errorf("last check = %+v, want the newer check's record %+v", got, want)
	}

	old.StartedAt = time.Time{}
	q.stampCanceledCheck(old)
	if got := q.Watches()[0].Last; got != want {
		t.Errorf("after canceling the retry, last check = %+v, want %+v", got, want)
	}
}

func TestStopSettlesARetriedCheckOlderThanTheWatchsLast(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	q.mu.Lock()
	retried := q.enqueueLocked("https://x/posts?tags=a", Options{Watch: id})
	q.mu.Unlock()
	newer := finishedWindow(q, retried.ID+10, retried.ID+10, id, 4, JobSucceeded)
	q.stampWatch(newer)

	q.Close()
	if j, err := q.Get(retried.ID); err != nil || j.Status != JobCanceled {
		t.Errorf("retried old check after a stop = %+v, %v; want a canceled row", j, err)
	}
}

func TestAClearedRootKeepsLaterWindowsRecording(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	root := finishedWindow(q, 10, 10, id, 2, JobSucceeded)
	q.stampWatch(root)
	q.recordWatch(root)
	second := finishedWindow(q, 11, 10, id, 2, JobSucceeded)
	q.mu.Lock()
	delete(q.index, root.ID)
	q.mu.Unlock()
	q.recordWatch(second)
	third := finishedWindow(q, 12, 10, id, 0, JobFailed)
	third.Items = []Item{{Status: ItemFailed, ErrorCode: outcome.ErrCodeMonbooruUnreachable}}
	q.recordWatch(third)

	last := q.Watches()[0].Last
	if last.JobID != root.ID || !last.Owed || last.Status != JobFailed {
		t.Errorf("last check = %+v, want it kept under the root %d, failed and owing", last, root.ID)
	}
}

func TestAClearedWindowsDebtSurvivesTheNextRecord(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	root := finishedWindow(q, 10, 10, id, 0, JobPartial)
	root.Items = []Item{{Status: ItemFailed, ErrorCode: outcome.ErrCodeMonbooruUnreachable}}
	q.stampWatch(root)
	q.recordWatch(root)
	if q.Watches()[0].Last.OwedTo == 0 {
		t.Fatalf("root owes nothing: %+v", q.Watches()[0].Last)
	}
	owed := q.Watches()[0].Last.OwedTo
	q.mu.Lock()
	delete(q.index, root.ID)
	q.mu.Unlock()
	later := finishedWindow(q, 11, 10, id, 2, JobSucceeded)
	q.recordWatch(later)
	if last := q.Watches()[0].Last; !last.Owed || last.OwedTo < owed {
		t.Errorf("last check = %+v, want the cleared window's debt of %d kept", last, owed)
	}
}

func TestWatchCheckWritesLandInOrder(t *testing.T) {
	st := openTestQueueStore(t)
	q := storedQueue(st)
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	for range 100 {
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				q.updateWatchCheck(id, func(c *WatchCheck) { c.Created++ })
			})
		}
		wg.Wait()
		stored, err := st.LoadWatches()
		if err != nil {
			t.Fatal(err)
		}
		if want := q.findWatch(id).Last.Created; stored[0].Last.Created != want {
			t.Fatalf("stored created = %d, want %d", stored[0].Last.Created, want)
		}
	}
}

func TestAWatchRemembersTheListingRunsPastItsPick(t *testing.T) {
	proc := &outageWindows{total: 12}
	q, _ := watchQueue(t, proc)
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 2, nil)
	check := func(want bool) {
		t.Helper()
		before := len(proc.seen())
		if _, err := q.CheckWatch(id, false); err != nil {
			t.Fatal(err)
		}
		if w := waitWindowRuns(t, proc, q, id, before+1); w.Last.More != want {
			t.Errorf("after run %d: more = %v, want %v (%+v)", before+1, w.Last.More, want, w.Last)
		}
	}
	check(true)
	proc.mu.Lock()
	proc.known = true
	proc.mu.Unlock()
	check(true)
	proc.mu.Lock()
	proc.known, proc.total = false, 1
	proc.mu.Unlock()
	check(false)
}

func TestAWatchAddedOverAFailedRunOwesItsPosts(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	url := "https://x/posts?tags=a"
	for _, tc := range []struct {
		name, code string
		wantTo     int
	}{
		{"an unpushed run", outcome.ErrCodeMonbooruUnreachable, 2},
		{"a failed download", outcome.ErrCodeDownloadFailed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q.finished, q.index = nil, map[int64]*Job{}
			parkFinished(q, 10, time.Now(), func(j *Job) {
				j.URL, j.Root, j.Status = url, 10, JobFailed
				j.Items = []Item{{ErrorCode: tc.code}, {ErrorCode: tc.code}}
			})
			id, err := q.AddWatch(url, 24, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = q.RemoveWatch(id) }()
			last := q.findWatch(id).Last
			if last.OwedTo != tc.wantTo || last.Owed != (tc.wantTo > 0) {
				t.Fatalf("last check = %+v, want owed to %d", last, tc.wantTo)
			}
			if !last.CheckedAt.IsZero() {
				t.Errorf("checked at %v, want the watch due at once", last.CheckedAt)
			}
			if tc.wantTo == 0 {
				return
			}
			jobID, err := q.CheckWatch(id, false)
			if err != nil {
				t.Fatal(err)
			}
			j, _ := q.Get(jobID)
			if !j.Force || j.ForceTo != tc.wantTo {
				t.Errorf("check force=%v to %d, want a bypass to %d", j.Force, j.ForceTo, tc.wantTo)
			}
			q.mu.Lock()
			q.pending = nil
			q.mu.Unlock()
		})
	}
}

func TestAWatchFromASeriesRowTakesTheWholeSeries(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	url := "https://x/posts?tags=big"
	finishedWindow(q, 10, 10, 0, 50, JobSucceeded).URL = url
	lead := finishedWindow(q, 11, 10, 0, 50, JobSucceeded)
	lead.URL = url
	id, err := q.AddWatch(url, 24, 0, lead.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if w := q.Watches()[0]; w.ID != id || w.Last.Created != 100 || w.Last.JobID != 10 {
		t.Errorf("last check = %+v, want both windows: 100 created from job 10", w.Last)
	}
}

func TestCloseSettlesAQueuedLaterWindow(t *testing.T) {
	st := openTestQueueStore(t)
	q := storedQueue(st)
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	first := finishedWindow(q, 10, 10, id, 2, JobSucceeded)
	q.stampWatch(first)
	q.mu.Lock()
	later := q.enqueueLocked("https://x/posts?tags=a", Options{Watch: id, Background: true})
	later.Root = 10
	q.mu.Unlock()
	q.Close()
	if j, err := q.Get(later.ID); err != nil || j.Status != JobCanceled {
		t.Errorf("queued later window after shutdown = %+v, %v; want a canceled row", j, err)
	}
}

func TestOneUnreadableWatchDoesNotCostTheOthers(t *testing.T) {
	st := openTestQueueStore(t)
	if _, err := st.InsertWatch("https://x/posts?tags=a", 24, 0, time.Now(), WatchCheck{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO watches (id, url, interval_hours, created_at) VALUES (50, 'https://x/posts?tags=b', 'daily', '')`); err != nil {
		t.Fatal(err)
	}
	ws, err := st.LoadWatches()
	if err != nil || len(ws) != 1 || ws[0].URL != "https://x/posts?tags=a" {
		t.Errorf("watches = %+v (%v), want the readable one kept", ws, err)
	}
}

type outageWindows struct {
	mu    sync.Mutex
	total int
	codes []string
	runs  []windowRun

	known bool
}

type windowRun struct {
	offset int
	force  bool
}

const errNoRun = "no run"

func (p *outageWindows) Process(_ context.Context, j *Job) error {
	s := j.Snapshot()
	p.mu.Lock()
	code := ""
	if n := len(p.runs); n < len(p.codes) {
		code = p.codes[n]
	}
	p.runs = append(p.runs, windowRun{s.Offset, s.Force})
	p.mu.Unlock()
	if code == errNoRun {
		return &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "the site is down"}
	}
	n := min(2, max(p.total-s.Offset, 0))
	j.SetItems(make([]Item, n))
	for i := range n {
		j.UpdateItem(i, func(it *Item) {
			if p.known {
				it.Status, it.Outcome = ItemSkipped, outcome.SkippedArchive
			} else if code == "" {
				it.Status, it.Outcome = ItemDone, outcome.Created
			} else {
				it.Status, it.Outcome, it.ErrorCode = ItemFailed, outcome.Failed, code
			}
		})
	}
	if n == 2 {
		j.SetCapped(2)
	}
	return nil
}

func (p *outageWindows) seen() []windowRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.runs)
}

func waitWindowRuns(t *testing.T, p *outageWindows, q *Queue, id int64, n int) Watch {
	t.Helper()
	var w Watch
	if !eventually(2*time.Second, func() bool {
		w = waitWatchSettled(t, q, id)
		return len(p.seen()) == n
	}) {
		t.Fatalf("runs = %v, want %d", p.seen(), n)
	}
	return w
}

func waitWatchOwed(t *testing.T, q *Queue, id int64, to int) {
	t.Helper()
	var w Watch
	if !eventually(2*time.Second, func() bool {
		w = waitWatchSettled(t, q, id)
		return w.Last.OwedTo == to && w.Last.Owed == (to > 0)
	}) {
		t.Fatalf("last check = %+v, want owed down to post %d", w.Last, to)
	}
}

func TestAForcedCheckReachesAsFarAsTheOutage(t *testing.T) {
	proc := &outageWindows{total: 6, codes: []string{"", outcome.ErrCodeMonbooruUnreachable}}
	q, _ := watchQueue(t, proc)
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, ItemsAll, nil)
	if _, err := q.CheckWatch(id, false); err != nil {
		t.Fatal(err)
	}
	waitWindowRuns(t, proc, q, id, 2)
	waitWatchOwed(t, q, id, 4)
	if _, err := q.CheckWatch(id, false); err != nil {
		t.Fatal(err)
	}

	waitWindowRuns(t, proc, q, id, 6)
	if want := []windowRun{{0, false}, {2, false}, {0, true}, {2, true}, {4, false}, {6, false}}; !slices.Equal(proc.seen(), want) {
		t.Errorf("runs = %v, want %v", proc.seen(), want)
	}
	waitWatchOwed(t, q, id, 0)
}

func TestAForcedCheckChainsWhateverItsItemsPick(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items int
	}{{"the configured cap", 0}, {"one window's worth", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			proc := &outageWindows{total: 100}
			q, st := watchQueue(t, proc)
			id, _ := q.AddWatch("https://x/posts?tags=a", 24, tc.items, nil)
			last := WatchCheck{JobID: 1, Status: JobPartial, Owed: true, OwedTo: 4}
			if err := st.SaveWatchCheck(id, last); err != nil {
				t.Fatal(err)
			}
			q.findWatch(id).Last = last
			if _, err := q.CheckWatch(id, false); err != nil {
				t.Fatal(err)
			}
			waitWindowRuns(t, proc, q, id, 2)
			if want := []windowRun{{0, true}, {2, true}}; !slices.Equal(proc.seen(), want) {
				t.Errorf("runs = %v, want %v", proc.seen(), want)
			}
		})
	}
}

func TestAForcedCheckChainsPastKnownPosts(t *testing.T) {
	proc := &outageWindows{total: 100, known: true}
	q, st := watchQueue(t, proc)
	id, _ := q.AddWatch("https://x/?f_search=a", 24, 2, nil)
	last := WatchCheck{JobID: 1, Status: JobPartial, Owed: true, OwedTo: 4}
	if err := st.SaveWatchCheck(id, last); err != nil {
		t.Fatal(err)
	}
	q.findWatch(id).Last = last
	if _, err := q.CheckWatch(id, false); err != nil {
		t.Fatal(err)
	}
	waitWindowRuns(t, proc, q, id, 2)
	if want := []windowRun{{0, true}, {2, true}}; !slices.Equal(proc.seen(), want) {
		t.Errorf("runs = %v, want %v", proc.seen(), want)
	}
	waitWatchOwed(t, q, id, 0)
}

func TestAForcedCheckCutShortStillOwesTheRest(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		owed bool
	}{
		{"a window that fetched nothing", errNoRun, true},
		{"the outage again", outcome.ErrCodeMonbooruUnreachable, true},
		{"a refusal ends it", outcome.ErrCodeMonbooruRejected, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := &outageWindows{total: 100, codes: []string{tc.code}}
			q, st := watchQueue(t, proc)
			id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
			last := WatchCheck{JobID: 1, Status: JobPartial, Owed: true, OwedTo: 4}
			if err := st.SaveWatchCheck(id, last); err != nil {
				t.Fatal(err)
			}
			q.findWatch(id).Last = last
			if _, err := q.CheckWatch(id, false); err != nil {
				t.Fatal(err)
			}
			w := waitWindowRuns(t, proc, q, id, 1)
			if w.Last.Owed != tc.owed {
				t.Fatalf("last check = %+v, want owed %v", w.Last, tc.owed)
			}
			if tc.owed && w.Last.OwedTo != 4 {
				t.Errorf("owed to %d, want the whole debt", w.Last.OwedTo)
			}
		})
	}
}

func TestACrashInALaterWindowForcesTheNextCheck(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	id, err := st.InsertWatch("https://x/posts?tags=a", 24, 0, time.Now(), WatchCheck{JobID: 1, Status: JobSucceeded, Created: 2})
	if err != nil {
		t.Fatal(err)
	}
	first := newJob(1, "https://x/posts?tags=a", Options{Watch: id}, time.Now())
	archivedItems(first, 2, 0)
	first.Status = JobSucceeded
	later := newJob(2, "https://x/posts?tags=a", Options{Watch: id, Root: 1, Offset: 2}, time.Now())
	later.SetItems(make([]Item, 2))
	later.Status = JobRunning
	for _, j := range []*Job{first, later} {
		if err := st.SaveJob(j.Snapshot()); err != nil {
			t.Fatal(err)
		}
	}
	q := storedQueue(st)
	t.Cleanup(q.Close)
	w := q.Watches()[0]
	if !w.Last.needsBypass() || w.Last.OwedTo != 4 {
		t.Errorf("last check = %+v, want the crashed window owed", w.Last)
	}
}

func TestCancelingAForcedCheckKeepsTheDebt(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	id, _ := q.AddWatch("https://x/posts?tags=a", 24, 0, nil)
	q.findWatch(id).Last = WatchCheck{JobID: 1, Status: JobPartial, Owed: true, OwedTo: 4}
	jobID, err := q.CheckWatch(id, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CancelLive(jobID); err != nil {
		t.Fatal(err)
	}
	if w := q.Watches()[0]; w.Last.Status != JobCanceled || !w.Last.Owed || w.Last.OwedTo != 4 {
		t.Errorf("last check = %+v, want it canceled and still owed", w.Last)
	}
}

func TestAForcedWindowSurvivesARestartWithItsReach(t *testing.T) {
	st := openTestQueueStore(t)
	j := newJob(1, "https://x", Options{Watch: 5, Force: true, ForceTo: 400}, time.Now())
	if err := st.SaveJob(j.Snapshot()); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.LoadJobs()
	if err != nil || len(jobs) != 1 || jobs[0].ForceTo != 400 {
		t.Errorf("reloaded = %+v, %v, want force_to kept", jobs, err)
	}
}

func TestRunWatchesSkipsAHeldTickAndStopsWithItsContext(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	if _, err := q.AddWatch("https://x/posts?tags=due", 24, 0, nil); err != nil {
		t.Fatal(err)
	}
	run := func(held bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		asked := make(chan struct{})
		var once sync.Once
		done := make(chan struct{})
		go func() {
			defer close(done)
			q.RunWatches(ctx, func() bool {
				once.Do(func() { close(asked) })
				return held
			})
		}()
		<-asked
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("RunWatches kept running after its context ended")
		}
	}
	run(true)
	if len(q.pending) != 0 {
		t.Fatalf("a held tick queued %d checks", len(q.pending))
	}
	run(false)
	if len(q.pending) != 1 {
		t.Errorf("a free tick queued %d checks, want the due watch's one", len(q.pending))
	}
}
