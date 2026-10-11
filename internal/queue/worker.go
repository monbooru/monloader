package queue

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"

	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/outcome"
)

func (q *Queue) Start() {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
}

// Stop takes no more work and settles what was still pending, so a parked
// Wait frees the HTTP drain; the running jobs carry on until Close.
func (q *Queue) Stop() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	checks := q.dropWatchChecksLocked()
	dropped, evicted := q.dropPendingLocked()
	q.cond.Broadcast()
	q.mu.Unlock()
	// Persisted so boot recovery does not rerun a job monbooru was told is
	// canceled.
	q.releaseDropped(dropped, evicted)
	ids := make([]int64, len(checks))
	for i, j := range checks {
		q.settleDropped(j)
		ids[i] = j.ID
	}
	q.persistDelete(ids)
}

func (q *Queue) Close() {
	q.Stop()
	q.mu.Lock()
	for _, cancel := range q.cancels {
		cancel()
	}
	q.mu.Unlock()
	q.wg.Wait()
}

func (q *Queue) closing() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

func (q *Queue) worker() {
	defer q.wg.Done()
	for {
		j, ctx, ok := q.nextJob()
		if !ok {
			return
		}
		q.runJob(j, ctx)
		// A job holds whole downloads in the heap; without this, memory
		// stays at its peak between jobs.
		debug.FreeOSMemory()
	}
}

// The caller must finish the job it returns.
func (q *Queue) nextJob() (*Job, context.Context, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for (len(q.pending) == 0 || q.paused) && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return nil, nil, false
	}
	j := q.pending[0]
	// Nil the slot: head slots are never reused, so the backing array
	// would pin the jobs already run.
	q.pending[0] = nil
	q.pending = q.pending[1:]
	ctx, cancel := context.WithCancel(context.Background())
	q.running[j.ID] = j
	q.cancels[j.ID] = cancel
	return j, ctx, true
}

func (q *Queue) runJob(j *Job, ctx context.Context) {
	defer q.finish(j)
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("queue: job %d panicked: %v", j.ID, r)
			j.Fail(outcome.ErrCodeDownloadFailed, fmt.Sprintf("internal error: %v", r), q.now())
		}
	}()

	if err := j.start(q.now()); err != nil {
		logx.Warnf("queue: job %d not startable: %v", j.ID, err)
		j.cancel(q.now())
		return
	}
	q.persist(j)
	q.stampWatch(j)

	err := q.proc.Process(ctx, j)
	switch {
	case ctx.Err() != nil && q.closing():
		// A shutdown is not an operator cancel: settle it like a crash,
		// so a restart keeps the requeue.
		j.interrupt(q.now())
	case ctx.Err() != nil:
		j.cancel(q.now())
	case err != nil:
		// Fail no-ops once finalized, so a code the processor already set
		// wins over this fallback.
		logx.Warnf("queue: job %d failed: %s", j.ID, err.Error())
		j.Fail(outcome.ErrCodeDownloadFailed, err.Error(), q.now())
	default:
		j.Finalize(q.now())
	}
}

func (q *Queue) finish(j *Job) {
	j.settleWatchCap()
	quiet := j.quietWatchCheck()
	q.mu.Lock()
	delete(q.running, j.ID)
	if cancel, ok := q.cancels[j.ID]; ok {
		cancel()
		delete(q.cancels, j.ID)
	}
	var evicted []int64
	if quiet {
		delete(q.index, j.ID)
	} else {
		evicted = q.pushFinishedLocked(j)
	}
	q.mu.Unlock()
	if quiet {
		q.persistDelete([]int64{j.ID})
	} else {
		q.persist(j)
	}
	q.persistDelete(evicted)
	j.signalDone()
	q.recordWatch(j)
	q.autoContinue(j)
	q.reconcileSeries(j)
}

// A check that never started is dropped rather than settled canceled: its
// watch is still due, so the next start queues it again. A later window or
// a retried check has stamped its watch, so it settles like any other job.
func (q *Queue) dropWatchChecksLocked() []*Job {
	var checks []*Job
	q.pending = slices.DeleteFunc(q.pending, func(j *Job) bool {
		if j.Watch == 0 || j.seriesKey() != j.ID {
			return false
		}
		if w := q.findWatchLocked(j.Watch); w != nil && w.Last.JobID == j.ID {
			return false
		}
		checks = append(checks, j)
		delete(q.index, j.ID)
		return true
	})
	return checks
}
