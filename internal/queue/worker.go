package queue

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/leqwin/monloader/internal/logx"
)

// Start launches the worker goroutines. Call once after New.
func (q *Queue) Start() {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
}

// Close stops accepting work, cancels every in-flight job, and waits for the
// workers to exit. Pending jobs are dropped; losing them on a restart is
// acceptable. Idempotent.
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	for _, cancel := range q.cancels {
		cancel()
	}
	q.cond.Broadcast()
	q.mu.Unlock()
	q.wg.Wait()
}

func (q *Queue) worker() {
	defer q.wg.Done()
	for {
		j, ctx, ok := q.nextJob()
		if !ok {
			return
		}
		q.runJob(j, ctx)
		// A job briefly holds the whole download (the -j output, each file's
		// bytes) in the heap; hand the freed pages back to the OS so memory
		// returns to baseline between downloads rather than climbing and staying
		// there.
		debug.FreeOSMemory()
	}
}

// nextJob blocks until a pending job is available or the queue is closed.
// On success it moves the job into the running set with a cancelable
// context. Caller must eventually call finish.
func (q *Queue) nextJob() (*Job, context.Context, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// A pause holds even with pending work; Close still wins so shutdown never
	// blocks on it.
	for (len(q.pending) == 0 || q.paused) && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return nil, nil, false
	}
	j := q.pending[0]
	q.pending = q.pending[1:]
	ctx, cancel := context.WithCancel(context.Background())
	q.running[j.ID] = j
	q.cancels[j.ID] = cancel
	return j, ctx, true
}

// runJob drives one job through the processor and finalizes it. A processor
// panic is contained so a single bad job never takes down the worker.
func (q *Queue) runJob(j *Job, ctx context.Context) {
	defer q.finish(j)
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("queue: job %d panicked: %v", j.ID, r)
			j.Fail(ErrCodeDownloadFailed, fmt.Sprintf("internal error: %v", r), q.now())
		}
	}()

	if err := j.start(q.now()); err != nil {
		logx.Warnf("queue: job %d not startable: %v", j.ID, err)
		j.cancel(q.now())
		return
	}

	err := q.proc.Process(ctx, j)
	switch {
	case ctx.Err() != nil:
		j.cancel(q.now())
	case err != nil:
		// A job-level abort (a failed resolve) leaves no item rows to carry the
		// reason, so log it here. The processor already classified the failure
		// and set the job's code; Fail no-ops once finalized, so that code wins
		// over this generic fallback.
		logx.Warnf("queue: job %d failed: %s", j.ID, err.Error())
		j.Fail(ErrCodeDownloadFailed, err.Error(), q.now())
	default:
		j.Finalize(q.now())
	}
}

// finish moves a job out of the running set and into the recent-history
// ring, then wakes any Wait callers. Signalling done last guarantees a
// woken Wait sees the job already settled in history, so an immediate Retry
// can't race the move.
func (q *Queue) finish(j *Job) {
	q.mu.Lock()
	delete(q.running, j.ID)
	if cancel, ok := q.cancels[j.ID]; ok {
		cancel() // release the context on the normal path too, not only on Cancel
		delete(q.cancels, j.ID)
	}
	q.pushFinishedLocked(j)
	q.mu.Unlock()
	j.signalDone()
	q.autoContinue(j)
}
