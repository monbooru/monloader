package queue

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/outcome"
)

type procFunc func(ctx context.Context, j *Job) error

func (f procFunc) Process(ctx context.Context, j *Job) error { return f(ctx, j) }

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

func createOne(j *Job, monbooruID int64) {
	j.SetItems([]Item{{PostID: "1"}})
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
	j.UpdateItem(0, func(it *Item) {
		it.Status = ItemDone
		it.Outcome = outcome.Created
		it.MonbooruID = monbooruID
		it.SHA256 = "abc"
	})
}

func TestProcessProducesOutcomes(t *testing.T) {
	proc := procFunc(func(ctx context.Context, j *Job) error {
		j.SetSite("danbooru")
		j.SetItems([]Item{{PostID: "a"}, {PostID: "b"}, {PostID: "c"}, {PostID: "d"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
		j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = outcome.Created; it.MonbooruID = 7 })
		j.UpdateItem(1, func(it *Item) { it.Status = ItemDownloaded })
		j.UpdateItem(1, func(it *Item) { it.Status = ItemUploaded })
		j.UpdateItem(1, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.Duplicate })
		j.UpdateItem(2, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.SkippedArchive })
		j.UpdateItem(3, func(it *Item) {
			it.Status = ItemFailed
			it.Outcome = outcome.Failed
			it.ErrorCode = outcome.ErrCodeDownloadFailed
			it.Error = "boom"
		})
		return nil
	})
	q := startQueue(t, proc, 1, 100)

	id := q.Enqueue("http://danbooru/pool", Options{})
	job := waitFor(t, q, id)

	if job.Status != JobPartial {
		t.Errorf("status = %s, want partial", job.Status)
	}
	want := Summary{Created: 1, Duplicate: 1, Skipped: 1, Archived: 1, Failed: 1, Total: 4}
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
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		jobCtx = ctx
		createOne(j, 1)
		return nil
	}), 1, 100)

	waitFor(t, q, q.Enqueue("http://x", Options{}))
	if jobCtx.Err() == nil {
		t.Error("job context should be canceled once the job finishes")
	}
}

func TestProcessSucceededAndStartedAt(t *testing.T) {
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		createOne(j, 99)
		return nil
	}), 1, 100)

	job := waitFor(t, q, q.Enqueue("http://x", Options{}))
	if job.Status != JobSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
	if job.StartedAt.IsZero() || job.FinishedAt.IsZero() {
		t.Error("StartedAt/FinishedAt should be set")
	}
}

func TestProcessorErrorFailsJob(t *testing.T) {
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		j.Fail(outcome.ErrCodeUnsupportedURL, "no extractor", time.Now())
		return context.Canceled
	}), 1, 100)

	job := waitFor(t, q, q.Enqueue("http://unknown", Options{}))
	if job.Status != JobFailed {
		t.Errorf("status = %s, want failed", job.Status)
	}
	if job.ErrorCode != outcome.ErrCodeUnsupportedURL {
		t.Errorf("error_code = %q, want %q", job.ErrorCode, outcome.ErrCodeUnsupportedURL)
	}
}

func TestProcessorPanicContained(t *testing.T) {
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		panic("kaboom")
	}), 1, 100)

	job := waitFor(t, q, q.Enqueue("http://x", Options{}))
	if job.Status != JobFailed {
		t.Errorf("a panicking job should end failed, got %s", job.Status)
	}
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
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		n := atomic.AddInt32(&calls, 1)
		j.SetItems([]Item{{PostID: "1"}})
		if n == 1 {
			j.UpdateItem(0, func(it *Item) {
				it.Status = ItemFailed
				it.Outcome = outcome.Failed
				it.ErrorCode = outcome.ErrCodeDownloadFailed
			})
		} else {
			j.UpdateItem(0, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.Duplicate })
		}
		return nil
	}), 1, 100)

	id := q.Enqueue("http://x", Options{})
	if job := waitFor(t, q, id); job.Status != JobFailed {
		t.Fatalf("first run status = %s, want failed", job.Status)
	}
	mustRetry(t, q, id)
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

func TestRetryForcePropagates(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		mu.Lock()
		seen = append(seen, j.Snapshot().Force)
		mu.Unlock()
		j.SetItems([]Item{{PostID: "1"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.SkippedArchive })
		return nil
	}), 1, 100)

	id := q.Enqueue("http://x", Options{})
	waitFor(t, q, id)

	if err := q.Retry(id, true); err != nil {
		t.Fatalf("forced Retry: %v", err)
	}
	if job := waitFor(t, q, id); !job.Force {
		t.Error("after forced retry job.Force = false, want true")
	}

	mustRetry(t, q, id)
	if job := waitFor(t, q, id); job.Force {
		t.Error("after normal retry job.Force = true, want false")
	}

	wantForceRuns(t, &mu, &seen, false, true, false)
}

func mustRetry(t *testing.T, q *Queue, id int64) {
	t.Helper()
	if err := q.Retry(id, false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
}

func mustContinue(t *testing.T, q *Queue, id int64) int64 {
	t.Helper()
	next, err := q.Continue(id)
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	return next
}

func wantForceRuns(t *testing.T, mu *sync.Mutex, seen *[]bool, want ...bool) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(*seen, want) {
		t.Errorf("processor saw force runs %v, want %v", *seen, want)
	}
}

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
				it.Outcome = outcome.Failed
				it.ErrorCode = outcome.ErrCodeMonbooruRejected
			})
			return nil
		}
		createOne(j, monbooruID)
		return nil
	}
}

func TestRetryFailedJobForcesArchiveBypass(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	q := startQueue(t, failOnceProc(&mu, &seen, 5), 1, 100)

	id := q.Enqueue("http://x", Options{})
	if job := waitFor(t, q, id); job.Status != JobFailed {
		t.Fatalf("first run status = %s, want failed", job.Status)
	}

	mustRetry(t, q, id)
	if job := waitFor(t, q, id); job.Status != JobSucceeded || !job.Force {
		t.Errorf("retry of failed job: status=%s force=%v, want succeeded/true", job.Status, job.Force)
	}

	mustRetry(t, q, id)
	if job := waitFor(t, q, id); job.Force {
		t.Error("retry of succeeded job force = true, want false")
	}

	wantForceRuns(t, &mu, &seen, false, true, false)
}

func waitWindows(t *testing.T, p *outageWindows, n int) {
	t.Helper()
	if !eventually(2*time.Second, func() bool { return len(p.seen()) >= n }) {
		t.Fatalf("runs = %v, want %d", p.seen(), n)
	}
}

func waitSeriesSettled(t *testing.T, q *Queue, root int64) {
	t.Helper()
	if !eventually(2*time.Second, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return !q.liveLocked(func(j *Job) bool { return j.seriesKey() == root })
	}) {
		t.Fatal("series still live")
	}
}

func TestReaddOfFailedURLForcesArchiveBypass(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	q := startQueue(t, failOnceProc(&mu, &seen, 9), 1, 100)

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

	wantForceRuns(t, &mu, &seen, false, true, false, false)
}

func TestReaddBypassReachesAsFarAsTheFailedSeries(t *testing.T) {
	proc := &outageWindows{total: 6, codes: []string{"", outcome.ErrCodeMonbooruUnreachable}}
	q, _ := watchQueue(t, proc)
	url := "https://x/posts?tags=a"
	first := q.Enqueue(url, Options{MaxItems: 2, Auto: true})
	waitWindows(t, proc, 2)
	waitSeriesSettled(t, q, first)

	q.Enqueue(url, Options{MaxItems: 2, Auto: true})
	waitWindows(t, proc, 6)
	want := []windowRun{{0, false}, {2, false}, {0, true}, {2, true}, {4, false}, {6, false}}
	if got := proc.seen(); !slices.Equal(got, want) {
		t.Errorf("runs = %v, want %v", got, want)
	}
}

func TestReaddJudgesTheLastRunAcrossItsSeries(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	url := "https://x/posts?tags=a"
	parkFinished(q, 10, time.Now(), func(j *Job) {
		j.URL, j.Root, j.Offset, j.Status = url, 10, 0, JobPartial
		j.Items = make([]Item, 2)
	})
	parkFinished(q, 11, time.Now(), func(j *Job) {
		j.URL, j.Root, j.Offset, j.Status = url, 10, 2, JobSucceeded
		j.Items = make([]Item, 2)
	})
	q.mu.Lock()
	j := q.enqueueLocked(url, Options{})
	q.mu.Unlock()
	if !j.Force || j.ForceTo != 2 {
		t.Errorf("re-add force=%v to %d, want the partial window's reach of 2", j.Force, j.ForceTo)
	}
}

func TestReaddCountsOnlyTheFinishedWindowsOfItsSeries(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	url := "https://x/posts?tags=a"
	parkFinished(q, 10, time.Now(), func(j *Job) {
		j.URL, j.Root, j.Offset, j.Status = url, 10, 0, JobPartial
		j.Items = make([]Item, 2)
	})
	parkFinished(q, 20, time.Now(), func(j *Job) {
		j.URL, j.Root, j.Offset, j.Status = "https://x/posts?tags=b", 20, 0, JobSucceeded
		j.Items = make([]Item, 2)
	})
	for _, w := range []struct{ id, root int64 }{{11, 10}, {21, 20}} {
		later := newJob(w.id, "", Options{Root: w.root, Offset: 2, MaxItems: 2}, time.Now())
		q.index[w.id] = later
		q.pending = append(q.pending, later)
	}
	q.mu.Lock()
	a := q.enqueueLocked(url, Options{})
	b := q.enqueueLocked("https://x/posts?tags=b", Options{})
	q.mu.Unlock()
	if !a.Force || a.ForceTo != 2 {
		t.Errorf("re-add force=%v to %d, want the finished window's reach of 2, not the queued one's", a.Force, a.ForceTo)
	}
	if b.Force {
		t.Error("re-add of a clean run forced by a window still queued, want not forced")
	}
}

func TestRetryCanceledJobForcesArchiveBypass(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	started := make(chan struct{}, 1)
	var runs int32
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		mu.Lock()
		seen = append(seen, j.Snapshot().Force)
		mu.Unlock()
		j.SetItems([]Item{{PostID: "1"}})
		if atomic.AddInt32(&runs, 1) == 1 {
			j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		createOne(j, 3)
		return nil
	}), 1, 100)

	id := q.Enqueue("http://x", Options{})
	<-started
	if err := q.CancelSeries(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if job := waitFor(t, q, id); job.Status != JobCanceled {
		t.Fatalf("first run status = %s, want canceled", job.Status)
	}

	mustRetry(t, q, id)
	if job := waitFor(t, q, id); job.Status != JobSucceeded || !job.Force {
		t.Errorf("retry of canceled job: status=%s force=%v, want succeeded/true", job.Status, job.Force)
	}

	wantForceRuns(t, &mu, &seen, false, true)
}

func TestCancelStopsRunningJob(t *testing.T) {
	started := make(chan struct{})
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		j.SetItems([]Item{{PostID: "1"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}), 1, 100)

	id := q.Enqueue("http://x", Options{})
	<-started
	if err := q.CancelSeries(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	job := waitFor(t, q, id)
	if job.Status != JobCanceled {
		t.Errorf("status = %s, want canceled", job.Status)
	}
	if job.Items[0].Outcome != outcome.Failed || job.Items[0].ErrorCode != outcome.ErrCodeCanceled {
		t.Errorf("in-flight item = %+v, want failed/canceled", job.Items[0])
	}
}

func TestPriorityJobJumpsAhead(t *testing.T) {
	var mu sync.Mutex
	var order []int64
	var gated int32
	started := make(chan struct{})
	gate := make(chan struct{})

	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {

		if atomic.AddInt32(&gated, 1) == 1 {
			close(started)
			<-gate
		}
		mu.Lock()
		order = append(order, j.ID)
		mu.Unlock()
		return nil
	}), 1, 100)

	a := q.Enqueue("http://a", Options{})
	<-started
	b := q.Enqueue("http://b", Options{})
	c := q.Enqueue("http://c", Options{Priority: true})
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
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		createOne(j, 1)
		ran <- j.ID
		return nil
	}), 1, 100)

	q.Pause()
	if !q.Paused() {
		t.Fatal("Paused() = false after Pause()")
	}
	id := q.Enqueue("http://x", Options{})

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

func TestCloseInterruptsTheRunningJob(t *testing.T) {
	started := make(chan struct{})
	proc := procFunc(func(ctx context.Context, j *Job) error {
		j.SetItems([]Item{{PostID: "a"}, {PostID: "b"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	q := New(proc, 1, 100)
	q.Start()
	id := q.Enqueue("http://danbooru/posts/1", Options{})
	<-started
	q.Close()

	j, err := q.Get(id)
	if err != nil {
		t.Fatalf("getting the drained job: %v", err)
	}
	if j.Status != JobInterrupted {
		t.Errorf("status = %s, want interrupted", j.Status)
	}
	if j.ErrorCode != "" {
		t.Errorf("interrupted carries no error code, got %q", j.ErrorCode)
	}
	if j.Summary.Canceled != 0 {
		t.Errorf("a drained job must not report canceled items, got %+v", j.Summary)
	}
	if err := q.Retry(id, false); err != nil {
		t.Errorf("an interrupted job should be requeueable: %v", err)
	}
	if j, _ := q.Get(id); !j.Force {
		t.Error("a requeued interrupted job should bypass the archive")
	}
}

func TestCloseTakesTheJobsItCancelsOutOfTheFIFO(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x", Options{})
	q.Close()

	if _, queued := q.InFlight(); queued != 0 {
		t.Errorf("queued = %d after Close, want 0: the waiting job was canceled", queued)
	}
	if err := q.Retry(id, false); err != nil {
		t.Fatalf("Retry after Close: %v", err)
	}
	if _, queued := q.InFlight(); queued != 1 {
		t.Errorf("queued = %d after one Retry, want the job queued once", queued)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	q := New(noopProcessor{}, 2, 100)
	q.Start()
	q.Close()
	q.Close()
}

func TestStopFreesAWaiterWhileTheRunningJobFinishes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	proc := procFunc(func(ctx context.Context, j *Job) error {
		close(started)
		<-release
		return nil
	})
	q := New(proc, 1, 100)
	q.Start()
	running := q.Enqueue("http://x/posts/1", Options{})
	queued := q.Enqueue("http://x/posts/2", Options{})
	<-started

	q.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if j, err := q.Wait(ctx, queued); err != nil || j.Status != JobCanceled {
		t.Fatalf("waiter on the queued job = %+v, %v; want it freed as canceled", j, err)
	}
	close(release)
	if j := waitFor(t, q, running); j.Status != JobSucceeded {
		t.Errorf("running job = %s, want it left to finish until Close", j.Status)
	}
	q.Close()
}

func TestAJobAcceptedAfterStopFreesItsWaiterAndWaitsForTheNextBoot(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	q := storedQueue(st)
	q.Start()
	q.Stop()

	id := q.Enqueue("http://x/posts/1", Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if j, err := q.Wait(ctx, id); !errors.Is(err, errStopping) || j.Status != JobQueued {
		t.Fatalf("waiter after stop = %+v, %v; want it freed with the job still queued", j, err)
	}
	q.Close()
	if got, _ := storedStatus(t, st, id); got != "queued" {
		t.Errorf("stored status = %q, want it kept queued for the next boot", got)
	}
}

func TestAWaiterOnARunningJobOutlastsStop(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	q := New(procFunc(func(context.Context, *Job) error {
		close(started)
		<-release
		return nil
	}), 1, 100)
	q.Start()
	defer q.Close()
	id := q.Enqueue("http://x/posts/1", Options{})
	<-started

	q.Stop()
	freed := make(chan *Job, 1)
	go func() {
		j, _ := q.Wait(context.Background(), id)
		freed <- j
	}()
	select {
	case j := <-freed:
		t.Fatalf("waiter freed while the job runs: %+v", j)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if j := <-freed; j.Status != JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
}

type windowedSearch struct {
	mu      sync.Mutex
	total   int
	offsets []int
}

func (p *windowedSearch) Process(_ context.Context, j *Job) error {
	s := j.Snapshot()
	p.mu.Lock()
	p.offsets = append(p.offsets, s.Offset)
	p.mu.Unlock()
	n := min(s.MaxItems, max(p.total-s.Offset, 0))
	j.SetItems(make([]Item, n))
	for i := range n {
		j.UpdateItem(i, func(it *Item) { it.Status = ItemDone; it.Outcome = outcome.Created })
	}
	if n == s.MaxItems {
		j.SetCapped(n)
	}
	return nil
}

func (p *windowedSearch) seen() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.offsets)
}

func runChain(t *testing.T, q *Queue, p *windowedSearch, want []int) {
	t.Helper()
	eventually(3*time.Second, func() bool {
		running, queued := q.InFlight()
		return running == 0 && queued == 0 && len(p.seen()) >= len(want)
	})
	if got := p.seen(); !slices.Equal(got, want) {
		t.Errorf("windows ran at offsets %v, want %v", got, want)
	}
}

func TestSeriesRetryOfAChainQueuesNoExtraWindows(t *testing.T) {
	p := &windowedSearch{total: 450}
	q := New(p, 1, 100)
	q.Start()
	defer q.Close()
	id := q.Enqueue("http://x/search", Options{MaxItems: 200, Auto: true})
	runChain(t, q, p, []int{0, 200, 400})

	if err := q.RetrySeries(id, true); err != nil {
		t.Fatal(err)
	}
	runChain(t, q, p, []int{0, 200, 400, 0, 200, 400})
	time.Sleep(50 * time.Millisecond)
	if got := p.seen(); len(got) != 6 {
		t.Errorf("windows ran at offsets %v, want no window past the end", got)
	}
}

func TestForcingAFirstWindowOfAFinishedChainClaimsNoMore(t *testing.T) {
	p := &windowedSearch{total: 12}
	q := New(p, 1, 100)
	q.Start()
	defer q.Close()
	id := q.Enqueue("http://x/search", Options{MaxItems: 5, Auto: true})
	runChain(t, q, p, []int{0, 5, 10})

	if err := q.Retry(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Wait(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !eventually(time.Second, func() bool {
		j, _ := q.Get(id)
		return !j.Capped
	}) {
		t.Error("the forced first window still claims more is available")
	}
}
