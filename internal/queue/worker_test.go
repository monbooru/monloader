package queue

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// procFunc adapts a closure to the Processor interface.
type procFunc func(ctx context.Context, j *Job) error

func (f procFunc) Process(ctx context.Context, j *Job) error { return f(ctx, j) }

// waitFor blocks for a job to finish with a generous timeout, failing the
// test on timeout so a stuck worker surfaces as a clear failure.
func waitFor(t *testing.T, q *Queue, id int64) *Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	j, err := q.Wait(ctx, id)
	if err != nil {
		t.Fatalf("waiting for job %d: %v", id, err)
	}
	return j
}

// createOne walks one item through the full created path.
func createOne(j *Job, monbooruID int64) {
	j.SetItems([]Item{{PostID: "1"}})
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
	j.UpdateItem(0, func(it *Item) {
		it.Status = ItemDone
		it.Outcome = OutcomeCreated
		it.MonbooruID = monbooruID
		it.SHA256 = "abc"
	})
}

func TestProcessProducesOutcomes(t *testing.T) {
	proc := procFunc(func(ctx context.Context, j *Job) error {
		j.SetSite("danbooru")
		j.SetItems([]Item{{PostID: "a"}, {PostID: "b"}, {PostID: "c"}, {PostID: "d"}})
		// created
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
		j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = OutcomeCreated; it.MonbooruID = 7 })
		// duplicate
		j.UpdateItem(1, func(it *Item) { it.Status = ItemDownloaded })
		j.UpdateItem(1, func(it *Item) { it.Status = ItemUploaded })
		j.UpdateItem(1, func(it *Item) { it.Status = ItemSkipped; it.Outcome = OutcomeDuplicate })
		// skipped_archive
		j.UpdateItem(2, func(it *Item) { it.Status = ItemSkipped; it.Outcome = OutcomeSkippedArchive })
		// failed
		j.UpdateItem(3, func(it *Item) {
			it.Status = ItemFailed
			it.Outcome = OutcomeFailed
			it.ErrorCode = ErrCodeDownloadFailed
			it.Error = "boom"
		})
		return nil
	})
	q := New(proc, 1, 100)
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://danbooru/pool", Options{})
	job := waitFor(t, q, id)

	if job.Status != JobPartial {
		t.Errorf("status = %s, want partial", job.Status)
	}
	want := Summary{Created: 1, Duplicate: 1, Skipped: 1, Failed: 1, Total: 4}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	if job.Site != "danbooru" {
		t.Errorf("site = %q, want danbooru", job.Site)
	}
	if job.Items[0].MonbooruID != 7 {
		t.Errorf("item 0 monbooru id = %d, want 7", job.Items[0].MonbooruID)
	}
}

func TestFinishCancelsJobContext(t *testing.T) {
	var jobCtx context.Context
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		jobCtx = ctx
		createOne(j, 1)
		return nil
	}), 1, 100)
	q.Start()
	defer q.Close()

	waitFor(t, q, q.Enqueue("http://x", Options{}))
	if jobCtx.Err() == nil {
		t.Error("job context should be canceled once the job finishes")
	}
}

func TestProcessSucceededAndStartedAt(t *testing.T) {
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		createOne(j, 99)
		return nil
	}), 1, 100)
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://x", Options{})
	job := waitFor(t, q, id)
	if job.Status != JobSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
	if job.StartedAt.IsZero() || job.FinishedAt.IsZero() {
		t.Error("StartedAt/FinishedAt should be set")
	}
}

func TestProcessorErrorFailsJob(t *testing.T) {
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		j.Fail(ErrCodeUnsupportedURL, "no extractor", time.Now())
		return context.Canceled // any error; the specific Fail code should win
	}), 1, 100)
	q.Start()
	defer q.Close()

	// The processor here both Fails and returns an error; because it returned
	// before ctx was actually canceled, the worker treats it as a failure.
	id := q.Enqueue("http://unknown", Options{})
	job := waitFor(t, q, id)
	if job.Status != JobFailed {
		t.Errorf("status = %s, want failed", job.Status)
	}
	if job.ErrorCode != ErrCodeUnsupportedURL {
		t.Errorf("error_code = %q, want %q", job.ErrorCode, ErrCodeUnsupportedURL)
	}
}

func TestProcessorPanicContained(t *testing.T) {
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		panic("kaboom")
	}), 1, 100)
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://x", Options{})
	job := waitFor(t, q, id)
	if job.Status != JobFailed {
		t.Errorf("a panicking job should end failed, got %s", job.Status)
	}
	// The worker must survive: a second job still processes.
	id2 := q.Enqueue("http://y", Options{})
	if _, err := q.Wait(mustCtx(t), id2); err != nil {
		t.Errorf("worker did not survive the panic: %v", err)
	}
}

func mustCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRetryRerunsJob(t *testing.T) {
	var calls int32
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		n := atomic.AddInt32(&calls, 1)
		j.SetItems([]Item{{PostID: "1"}})
		if n == 1 {
			j.UpdateItem(0, func(it *Item) {
				it.Status = ItemFailed
				it.Outcome = OutcomeFailed
				it.ErrorCode = ErrCodeDownloadFailed
			})
		} else {
			j.UpdateItem(0, func(it *Item) { it.Status = ItemSkipped; it.Outcome = OutcomeDuplicate })
		}
		return nil
	}), 1, 100)
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://x", Options{})
	if job := waitFor(t, q, id); job.Status != JobFailed {
		t.Fatalf("first run status = %s, want failed", job.Status)
	}
	if err := q.Retry(id, false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	job := waitFor(t, q, id)
	if job.Status != JobSucceeded {
		t.Errorf("after retry status = %s, want succeeded", job.Status)
	}
	if job.Summary.Duplicate != 1 {
		t.Errorf("after retry duplicate = %d, want 1", job.Summary.Duplicate)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("processor called %d times, want 2", calls)
	}
}

// TestRetryForcePropagates checks that a forced retry sets Job.Force, the
// processor sees it on the re-run, and a subsequent normal retry clears it.
func TestRetryForcePropagates(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		mu.Lock()
		seen = append(seen, j.Snapshot().Force)
		mu.Unlock()
		j.SetItems([]Item{{PostID: "1"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemSkipped; it.Outcome = OutcomeSkippedArchive })
		return nil
	}), 1, 100)
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://x", Options{})
	waitFor(t, q, id)

	if err := q.Retry(id, true); err != nil {
		t.Fatalf("forced Retry: %v", err)
	}
	if job := waitFor(t, q, id); !job.Force {
		t.Error("after forced retry job.Force = false, want true")
	}

	if err := q.Retry(id, false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if job := waitFor(t, q, id); job.Force {
		t.Error("after normal retry job.Force = true, want false")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []bool{false, true, false}
	if len(seen) != len(want) {
		t.Fatalf("processor saw runs %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("run %d force = %v, want %v", i, seen[i], want[i])
		}
	}
}

// failOnceProc fails its first run's item and succeeds after, recording each
// run's Force flag into seen, for the archive-bypass tests.
func failOnceProc(mu *sync.Mutex, seen *[]bool, monbooruID int64) procFunc {
	var calls int32
	return func(ctx context.Context, j *Job) error {
		mu.Lock()
		*seen = append(*seen, j.Snapshot().Force)
		mu.Unlock()
		j.SetItems([]Item{{PostID: "1"}})
		if atomic.AddInt32(&calls, 1) == 1 {
			j.UpdateItem(0, func(it *Item) {
				it.Status = ItemFailed
				it.Outcome = OutcomeFailed
				it.ErrorCode = ErrCodeMonbooruRejected
			})
			return nil
		}
		createOne(j, monbooruID)
		return nil
	}
}

// TestRetryFailedJobForcesArchiveBypass checks that a plain retry of a job that
// did not fully import re-runs past the download-archive (so a failed push is
// re-downloaded rather than archive-skipped), while a retry of a fully
// succeeded job runs against the archive as before.
func TestRetryFailedJobForcesArchiveBypass(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	q := New(failOnceProc(&mu, &seen, 5), 1, 100)
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://x", Options{})
	if job := waitFor(t, q, id); job.Status != JobFailed {
		t.Fatalf("first run status = %s, want failed", job.Status)
	}

	if err := q.Retry(id, false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if job := waitFor(t, q, id); job.Status != JobSucceeded || !job.Force {
		t.Errorf("retry of failed job: status=%s force=%v, want succeeded/true", job.Status, job.Force)
	}

	if err := q.Retry(id, false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if job := waitFor(t, q, id); job.Force {
		t.Error("retry of succeeded job force = true, want false")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []bool{false, true, false}
	if len(seen) != len(want) {
		t.Fatalf("processor saw runs %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("run %d force = %v, want %v", i, seen[i], want[i])
		}
	}
}

// TestReaddOfFailedURLForcesArchiveBypass checks that a fresh add of a URL a
// recent job failed to import re-runs past the archive, while a re-add after a
// successful run, or of a URL with no failed history, does not.
func TestReaddOfFailedURLForcesArchiveBypass(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	q := New(failOnceProc(&mu, &seen, 9), 1, 100)
	q.Start()
	defer q.Close()

	if job := waitFor(t, q, q.Enqueue("http://x", Options{})); job.Status != JobFailed {
		t.Fatalf("first run status = %s, want failed", job.Status)
	}
	if job := waitFor(t, q, q.Enqueue("http://x", Options{})); job.Status != JobSucceeded || !job.Force {
		t.Errorf("re-add of failed url: status=%s force=%v, want succeeded/true", job.Status, job.Force)
	}
	if job := waitFor(t, q, q.Enqueue("http://x", Options{})); job.Force {
		t.Error("re-add after a successful run forced, want not forced")
	}
	if job := waitFor(t, q, q.Enqueue("http://y", Options{})); job.Force {
		t.Error("re-add of a url with no failed history forced, want not forced")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []bool{false, true, false, false}
	if len(seen) != len(want) {
		t.Fatalf("processor saw runs %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("run %d force = %v, want %v", i, seen[i], want[i])
		}
	}
}

func TestCancelStopsRunningJob(t *testing.T) {
	started := make(chan struct{})
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		j.SetItems([]Item{{PostID: "1"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
		close(started)
		<-ctx.Done() // block until canceled
		return ctx.Err()
	}), 1, 100)
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://x", Options{})
	<-started // the job is now running and blocked
	if err := q.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	job := waitFor(t, q, id)
	if job.Status != JobCanceled {
		t.Errorf("status = %s, want canceled", job.Status)
	}
	// The in-flight item must be recorded failed with the canceled code.
	if job.Items[0].Outcome != OutcomeFailed || job.Items[0].ErrorCode != ErrCodeCanceled {
		t.Errorf("in-flight item = %+v, want failed/canceled", job.Items[0])
	}
}

func TestPriorityJobJumpsAhead(t *testing.T) {
	var mu sync.Mutex
	var order []int64
	var gated int32
	started := make(chan struct{})
	gate := make(chan struct{})

	q := New(procFunc(func(ctx context.Context, j *Job) error {
		// The first job to start blocks on the gate so the rest queue behind
		// it; this makes the FIFO ordering deterministic.
		if atomic.AddInt32(&gated, 1) == 1 {
			close(started)
			<-gate
		}
		mu.Lock()
		order = append(order, j.ID)
		mu.Unlock()
		return nil
	}), 1, 100)
	q.Start()
	defer q.Close()

	a := q.Enqueue("http://a", Options{})               // bulk; gets picked up first
	<-started                                           // a is running and blocked
	b := q.Enqueue("http://b", Options{})               // bulk, queued behind a
	c := q.Enqueue("http://c", Options{Priority: true}) // priority, jumps ahead of b
	close(gate)

	for _, id := range []int64{a, b, c} {
		waitFor(t, q, id)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []int64{a, c, b}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("processing order = %v, want %v (priority c ahead of bulk b)", order, want)
	}
}

func TestPauseHoldsPendingJobs(t *testing.T) {
	ran := make(chan int64, 4)
	q := New(procFunc(func(ctx context.Context, j *Job) error {
		createOne(j, 1)
		ran <- j.ID
		return nil
	}), 1, 100)
	q.Start()
	defer q.Close()

	q.Pause()
	if !q.Paused() {
		t.Fatal("Paused() = false after Pause()")
	}
	id := q.Enqueue("http://x", Options{})

	// A paused queue must not start the job even though it is pending.
	select {
	case got := <-ran:
		t.Fatalf("job %d ran while paused", got)
	case <-time.After(200 * time.Millisecond):
	}
	if j, _ := q.Get(id); j.Status != JobQueued {
		t.Errorf("status = %s while paused, want queued", j.Status)
	}

	q.Resume()
	if q.Paused() {
		t.Fatal("Paused() = true after Resume()")
	}
	select {
	case got := <-ran:
		if got != id {
			t.Errorf("ran job %d, want %d", got, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job did not run after resume")
	}
	if j := waitFor(t, q, id); j.Status != JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	q := New(noopProcessor{}, 2, 100)
	q.Start()
	q.Close()
	q.Close() // must not panic or hang
}
