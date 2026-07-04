package queue

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// noopProcessor never does anything; used by tests that exercise queue
// bookkeeping without launching workers.
type noopProcessor struct{}

func (noopProcessor) Process(context.Context, *Job) error { return nil }

func TestJobTransitions(t *testing.T) {
	all := []JobStatus{JobQueued, JobRunning, JobSucceeded, JobPartial, JobFailed, JobCanceled}
	legal := map[JobStatus]map[JobStatus]bool{
		JobQueued:    {JobRunning: true, JobCanceled: true},
		JobRunning:   {JobSucceeded: true, JobPartial: true, JobFailed: true, JobCanceled: true},
		JobSucceeded: {JobQueued: true},
		JobPartial:   {JobQueued: true},
		JobFailed:    {JobQueued: true},
		JobCanceled:  {JobQueued: true},
	}
	for _, from := range all {
		for _, to := range all {
			want := legal[from][to]
			if got := validJobTransition(from, to); got != want {
				t.Errorf("validJobTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestItemTransitions(t *testing.T) {
	all := []ItemStatus{ItemPending, ItemDownloaded, ItemUploaded, ItemDone, ItemSkipped, ItemFailed}
	legal := map[ItemStatus]map[ItemStatus]bool{
		ItemPending:    {ItemDownloaded: true, ItemSkipped: true, ItemFailed: true},
		ItemDownloaded: {ItemUploaded: true, ItemSkipped: true, ItemFailed: true},
		ItemUploaded:   {ItemDone: true, ItemSkipped: true, ItemFailed: true},
	}
	for _, from := range all {
		for _, to := range all {
			want := legal[from][to]
			if got := validItemTransition(from, to); got != want {
				t.Errorf("validItemTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestSummarizeAndDeriveStatus(t *testing.T) {
	cases := []struct {
		name    string
		items   []Item
		summary Summary
		status  JobStatus
	}{
		{"empty", nil, Summary{}, JobSucceeded},
		{
			"all created",
			[]Item{{Outcome: OutcomeCreated}, {Outcome: OutcomeCreated}},
			Summary{Created: 2, Total: 2},
			JobSucceeded,
		},
		{
			"mixed is partial",
			[]Item{{Outcome: OutcomeCreated}, {Outcome: OutcomeDuplicate}, {Outcome: OutcomeSkippedArchive}, {Outcome: OutcomeFailed}},
			Summary{Created: 1, Duplicate: 1, Skipped: 1, Failed: 1, Total: 4},
			JobPartial,
		},
		{
			"all failed",
			[]Item{{Outcome: OutcomeFailed}, {Outcome: OutcomeFailed}},
			Summary{Failed: 2, Total: 2},
			JobFailed,
		},
		{
			"duplicates and skips only succeed",
			[]Item{{Outcome: OutcomeDuplicate}, {Outcome: OutcomeSkippedArchive}},
			Summary{Duplicate: 1, Skipped: 1, Total: 2},
			JobSucceeded,
		},
		{
			"unsupported media skips, not fails",
			[]Item{{Outcome: OutcomeCreated}, {Outcome: OutcomeSkippedUnsupported}},
			Summary{Created: 1, Skipped: 1, Total: 2},
			JobSucceeded,
		},
		{
			"enriched counts on its own, succeeds",
			[]Item{{Outcome: OutcomeEnriched}},
			Summary{Enriched: 1, Total: 1},
			JobSucceeded,
		},
	}
	for _, tc := range cases {
		if got := summarize(tc.items); got != tc.summary {
			t.Errorf("%s: summarize = %+v, want %+v", tc.name, got, tc.summary)
		}
		if got := deriveStatus(tc.items); got != tc.status {
			t.Errorf("%s: deriveStatus = %s, want %s", tc.name, got, tc.status)
		}
	}
}

func TestSummaryAddSumsEveryField(t *testing.T) {
	a := Summary{Created: 1, Duplicate: 2, Enriched: 3, Skipped: 4, Failed: 5, Canceled: 6, Total: 21}
	b := Summary{Created: 10, Duplicate: 20, Enriched: 30, Skipped: 40, Failed: 50, Canceled: 60, Total: 210}
	want := Summary{Created: 11, Duplicate: 22, Enriched: 33, Skipped: 44, Failed: 55, Canceled: 66, Total: 231}
	if got := a.Add(b); got != want {
		t.Errorf("Add = %+v, want %+v", got, want)
	}
}

func TestUpdateItemRejectsIllegalTransition(t *testing.T) {
	j := newJob(1, "u", Options{}, time.Now())
	j.SetItems([]Item{{PostID: "1"}})
	// pending -> done is not a legal item transition.
	if j.UpdateItem(0, func(it *Item) { it.Status = ItemDone }) {
		t.Error("UpdateItem should report an illegal pending->done transition")
	}
	// pending -> downloaded is legal.
	if !j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded }) {
		t.Error("UpdateItem should accept pending->downloaded")
	}
	// out-of-range index.
	if j.UpdateItem(9, func(it *Item) {}) {
		t.Error("UpdateItem should reject an out-of-range index")
	}
}

func TestRetryKeepsBackLinkOnArchiveSkip(t *testing.T) {
	// A plain retry of a created job archive-skips the post on re-run; its
	// monbooru back-link must survive rather than being dropped.
	j := newJob(1, "http://danbooru/posts/489", Options{}, time.Now())
	j.SetItems([]Item{{PostID: "489"}})
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
	j.UpdateItem(0, func(it *Item) {
		it.Status = ItemDone
		it.Outcome = OutcomeCreated
		it.MonbooruID = 77
		it.SHA256 = "abc"
	})
	j.Finalize(time.Now())

	if err := j.reset(false); err != nil {
		t.Fatalf("reset: %v", err)
	}
	j.SetItems([]Item{{PostID: "489"}})
	j.UpdateItem(0, func(it *Item) {
		it.Status = ItemSkipped
		it.Outcome = OutcomeSkippedArchive
	})
	j.Finalize(time.Now())

	got := j.Items[0]
	if got.Outcome != OutcomeSkippedArchive {
		t.Errorf("outcome = %s, want skipped_archive", got.Outcome)
	}
	if got.MonbooruID != 77 || got.SHA256 != "abc" {
		t.Errorf("retry dropped the back-link: monbooru_id=%d sha256=%q, want 77/abc", got.MonbooruID, got.SHA256)
	}
}

func TestEnqueueGetListAndIDs(t *testing.T) {
	q := New(noopProcessor{}, 1, 100) // not started: jobs stay queued
	ids := make([]int64, 5)
	for i := range ids {
		ids[i] = q.Enqueue(fmt.Sprintf("http://x/%d", i), Options{})
	}
	if ids[0] != 1 || ids[4] != 5 {
		t.Errorf("ids should be 1..5, got %v", ids)
	}
	for _, id := range ids {
		if _, err := q.Get(id); err != nil {
			t.Errorf("Get(%d): %v", id, err)
		}
	}
	if _, err := q.Get(999); err != ErrNotFound {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}

	all, total := q.List(ListOptions{})
	if total != 5 || len(all) != 5 {
		t.Fatalf("List all: total=%d len=%d, want 5/5", total, len(all))
	}
	// Newest first.
	if all[0].ID != 5 || all[4].ID != 1 {
		t.Errorf("List order = %d..%d, want 5..1", all[0].ID, all[4].ID)
	}
	queued, n := q.List(ListOptions{Status: JobQueued})
	if n != 5 || len(queued) != 5 {
		t.Errorf("List queued: n=%d len=%d, want 5/5", n, len(queued))
	}
	if _, n := q.List(ListOptions{Status: JobSucceeded}); n != 0 {
		t.Errorf("List succeeded n=%d, want 0", n)
	}

	page2, total := q.List(ListOptions{Limit: 2, Page: 2})
	if total != 5 || len(page2) != 2 || page2[0].ID != 3 || page2[1].ID != 2 {
		t.Errorf("page 2 (limit 2) = %v total=%d, want ids [3 2] total 5", idsOf(page2), total)
	}
	if page, _ := q.List(ListOptions{Limit: 2, Page: 9}); len(page) != 0 {
		t.Errorf("page past the end should be empty, got %d", len(page))
	}
}

func idsOf(jobs []*Job) []int64 {
	out := make([]int64, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func TestRingEvictsPastBound(t *testing.T) {
	// White-box: push five finished jobs through a ring bounded at three and
	// confirm only the newest three remain indexed.
	q := New(noopProcessor{}, 1, 3)
	for i := int64(1); i <= 5; i++ {
		j := newJob(i, "u", Options{}, time.Now())
		q.index[i] = j
		q.pushFinishedLocked(j)
	}
	if len(q.finished) != 3 {
		t.Fatalf("ring holds %d, want 3", len(q.finished))
	}
	for _, gone := range []int64{1, 2} {
		if _, ok := q.index[gone]; ok {
			t.Errorf("job %d should have been evicted from the index", gone)
		}
	}
	for _, kept := range []int64{3, 4, 5} {
		if _, ok := q.index[kept]; !ok {
			t.Errorf("job %d should still be indexed", kept)
		}
	}
}

func TestQueueClear(t *testing.T) {
	// White-box: Clear empties the finished ring and de-indexes those jobs;
	// a pending job is untouched.
	q := New(noopProcessor{}, 1, 100)
	for i := int64(1); i <= 3; i++ {
		j := newJob(i, "u", Options{}, time.Now())
		q.index[i] = j
		q.pushFinishedLocked(j)
	}
	pending := newJob(99, "u", Options{}, time.Now())
	q.index[99] = pending
	q.pending = append(q.pending, pending)

	q.Clear()
	if len(q.finished) != 0 {
		t.Errorf("finished ring = %d, want 0 after clear", len(q.finished))
	}
	for _, gone := range []int64{1, 2, 3} {
		if _, ok := q.index[gone]; ok {
			t.Errorf("finished job %d should be de-indexed after clear", gone)
		}
	}
	if _, ok := q.index[99]; !ok || len(q.pending) != 1 {
		t.Error("a pending job should survive clear")
	}
}

func TestRetryAndCancelUnknownIDs(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	if err := q.Retry(404, false); err != ErrNotFound {
		t.Errorf("Retry(unknown) = %v, want ErrNotFound", err)
	}
	if err := q.Cancel(404); err != ErrNotFound {
		t.Errorf("Cancel(unknown) = %v, want ErrNotFound", err)
	}
}

func TestEnqueueMetadata_KindTargetPriority(t *testing.T) {
	q := New(noopProcessor{}, 1, 100) // not started: the job stays queued
	id := q.EnqueueMetadata(42, "mygallery", "https://danbooru.donmai.us/posts/1")
	job, err := q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != KindMetadata {
		t.Errorf("kind = %q, want metadata", job.Kind)
	}
	if job.ImageID != 42 {
		t.Errorf("image_id = %d, want 42", job.ImageID)
	}
	if job.Gallery != "mygallery" {
		t.Errorf("gallery = %q, want mygallery", job.Gallery)
	}
	if !job.Priority {
		t.Error("a metadata refetch should take the priority lane")
	}
}

func TestContinueEnqueuesNextWindow(t *testing.T) {
	q := New(noopProcessor{}, 1, 100) // not started: jobs stay queued
	id := q.Enqueue("http://x/search", Options{Gallery: "art", MaxItems: 50})
	q.index[id].SetCapped(50) // simulate a capped run

	nid, err := q.Continue(id)
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if nid == id {
		t.Fatal("Continue should create a new job, not reuse the source")
	}
	nj, err := q.Get(nid)
	if err != nil {
		t.Fatal(err)
	}
	if nj.URL != "http://x/search" || nj.Gallery != "art" || nj.MaxItems != 50 || nj.Offset != 50 {
		t.Errorf("continued job = {url:%q gallery:%q max:%d offset:%d}, want the source's next window", nj.URL, nj.Gallery, nj.MaxItems, nj.Offset)
	}

	// Continuing the continuation advances the offset by the cap again.
	q.index[nid].SetCapped(50)
	n2, err := q.Continue(nid)
	if err != nil {
		t.Fatalf("Continue chain: %v", err)
	}
	if j, _ := q.Get(n2); j.Offset != 100 {
		t.Errorf("second continue offset = %d, want 100", j.Offset)
	}

	// A job that was not capped has no next window; an unknown id is not found.
	if _, err := q.Continue(q.Enqueue("http://y", Options{})); err != ErrNotCapped {
		t.Errorf("Continue(non-capped) = %v, want ErrNotCapped", err)
	}
	if _, err := q.Continue(404); err != ErrNotFound {
		t.Errorf("Continue(unknown) = %v, want ErrNotFound", err)
	}
}

func TestContinueFromEarlierWindowAdvancesPastHighWater(t *testing.T) {
	// Continuing a non-latest window must advance past the furthest window the
	// series has fetched, not re-fetch a window an earlier one already took.
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetCapped(8) // window A: posts 1-8
	nid, _ := q.Continue(id)
	q.index[nid].SetCapped(8) // window B: posts 9-16
	if b, _ := q.Get(nid); b.Offset != 8 {
		t.Fatalf("window B offset = %d, want 8", b.Offset)
	}

	// Continue from the earlier window A; the new window starts past B, not at B.
	cid, err := q.Continue(id)
	if err != nil {
		t.Fatalf("Continue(earlier): %v", err)
	}
	if c, _ := q.Get(cid); c.Offset != 16 {
		t.Errorf("continue from an earlier window offset = %d, want 16 (past the high-water)", c.Offset)
	}
}

func TestContinueFromEarlierWindowPastShortFinalWindow(t *testing.T) {
	// A short (below-cap) window still advances the series high-water by the
	// posts it fetched, so a continue from an earlier window starts past it
	// rather than re-fetching the window it already took.
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetCapped(8) // window A: posts 1-8, capped
	nid, _ := q.Continue(id)
	q.index[nid].SetItems(make([]Item, 3)) // window B: posts 9-11, short of the cap

	cid, err := q.Continue(id)
	if err != nil {
		t.Fatalf("Continue(earlier): %v", err)
	}
	if c, _ := q.Get(cid); c.Offset != 11 {
		t.Errorf("continue past a short window offset = %d, want 11 (8 + 3 fetched)", c.Offset)
	}
}

func TestContinueWhilePendingReservesItsWindow(t *testing.T) {
	// A second continue issued while the first continuation has not resolved yet
	// must enqueue the window after its reserved range, not a duplicate of it.
	q := New(noopProcessor{}, 1, 100) // not started: the continuation stays unresolved
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetCapped(8) // window A: posts 1-8
	n1, err := q.Continue(id)
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if b, _ := q.Get(n1); b.Offset != 8 {
		t.Fatalf("window B offset = %d, want 8", b.Offset)
	}
	n2, err := q.Continue(id)
	if err != nil {
		t.Fatalf("Continue(rapid second): %v", err)
	}
	if c, _ := q.Get(n2); c.Offset != 16 {
		t.Errorf("rapid second continue offset = %d, want 16 (past the pending window's reserved range)", c.Offset)
	}
}

func TestSnapshotLiveSummaryWhileRunning(t *testing.T) {
	// A running (not finalized) job's snapshot reflects current item outcomes, so
	// the queue and API show live progress instead of all-zeros until finalize.
	j := newJob(1, "http://x", Options{}, time.Now())
	j.SetItems([]Item{{PostID: "a"}, {PostID: "b"}, {PostID: "c"}})
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = OutcomeCreated })

	if s := j.Snapshot(); s.Summary.Created != 1 || s.Summary.Total != 3 {
		t.Errorf("running snapshot summary = %+v, want 1 created / 3 total", s.Summary)
	}

	// A failed job keeps the zero summary Fail leaves (the row reads "failed - code").
	f := newJob(2, "http://y", Options{}, time.Now())
	f.SetItems([]Item{{PostID: "a"}})
	f.Fail("download_failed", "boom", time.Now())
	if s := f.Snapshot(); s.Summary != (Summary{}) {
		t.Errorf("failed-resolve snapshot summary = %+v, want the zero summary", s.Summary)
	}
}

func TestContinueInheritsSeriesRoot(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	if first, _ := q.Get(id); first.Root != id {
		t.Fatalf("a fresh job should be its own series root: Root=%d id=%d", first.Root, id)
	}
	q.index[id].SetCapped(50)
	nid, _ := q.Continue(id)
	q.index[nid].SetCapped(50)
	n2, _ := q.Continue(nid)
	for _, child := range []int64{nid, n2} {
		if c, _ := q.Get(child); c.Root != id {
			t.Errorf("continuation %d Root = %d, want originating id %d", child, c.Root, id)
		}
	}
}

func TestContinueCarriesSeriesSite(t *testing.T) {
	// A window past the search's last post resolves no items and never learns a
	// site; inheriting the source window's keeps the collapsed row labeled after
	// a fetch to exhaustion.
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetSite("danbooru")
	q.index[id].SetCapped(8)
	nid, err := q.Continue(id)
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if c, _ := q.Get(nid); c.Site != "danbooru" {
		t.Errorf("continuation site = %q, want the source window's %q", c.Site, "danbooru")
	}
}

func TestCancelSeriesRemovesEveryWindow(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	q.index[id].SetCapped(50)
	n1, _ := q.Continue(id)
	q.index[n1].SetCapped(50)
	n2, _ := q.Continue(n1)
	other := q.Enqueue("http://y/search", Options{}) // a separate series must survive

	if err := q.CancelSeries(n1); err != nil {
		t.Fatalf("CancelSeries: %v", err)
	}
	for _, gone := range []int64{id, n1, n2} {
		if _, err := q.Get(gone); err != ErrNotFound {
			t.Errorf("series member %d still present after CancelSeries", gone)
		}
	}
	if _, err := q.Get(other); err != nil {
		t.Errorf("unrelated job %d should survive CancelSeries: %v", other, err)
	}
	if err := q.CancelSeries(404); err != ErrNotFound {
		t.Errorf("CancelSeries(unknown) = %v, want ErrNotFound", err)
	}
}

// haltProc finishes the first window (offset 0) with one created item and caps
// it, then blocks every later window on its context, so a series can hold a
// finished window and a running window at once.
type haltProc struct{ running chan int64 }

func (p haltProc) Process(ctx context.Context, job *Job) error {
	s := job.Snapshot()
	job.SetItems([]Item{{PostID: "p"}})
	if s.Offset == 0 {
		job.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
		job.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
		job.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = OutcomeCreated })
		job.SetCapped(50)
		return nil
	}
	p.running <- s.ID
	<-ctx.Done()
	return ctx.Err()
}

// TestCancelSeriesKeepsFinishedWindows checks that canceling an in-flight
// continuation stops it but keeps the already-finished window's imported items,
// rather than dropping the whole series.
func TestCancelSeriesKeepsFinishedWindows(t *testing.T) {
	running := make(chan int64, 1)
	q := New(haltProc{running: running}, 1, 100)
	q.Start()
	defer q.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	if _, err := q.Wait(ctx, id); err != nil {
		t.Fatalf("wait first window: %v", err)
	}
	nid, err := q.Continue(id)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	<-running // the continuation is now running and blocked on its context

	if err := q.CancelSeries(nid); err != nil {
		t.Fatalf("CancelSeries: %v", err)
	}

	// The finished window survives with its imported item.
	first, err := q.Get(id)
	if err != nil {
		t.Fatalf("the finished window should survive a live cancel: %v", err)
	}
	if first.Summary.Created != 1 {
		t.Errorf("finished window lost its imported item: summary=%+v", first.Summary)
	}
	// The in-flight window is canceled and stays in history.
	if _, err := q.Wait(ctx, nid); err != nil {
		t.Fatalf("wait canceled window: %v", err)
	}
	if cw, _ := q.Get(nid); cw.Status != JobCanceled {
		t.Errorf("the in-flight window should be canceled, got %s", cw.Status)
	}
}

func TestAutoContinueChainsCappedWindows(t *testing.T) {
	q := New(noopProcessor{}, 1, 100) // not started; drive autoContinue directly
	count := func() int { j, _ := q.List(ListOptions{}); return len(j) }

	// A capped auto window enqueues the next auto window, carrying the offset
	// and the series root.
	id := q.Enqueue("http://x/search", Options{MaxItems: 50, Auto: true})
	q.index[id].SetCapped(50)
	q.index[id].Finalize(time.Now()) // a window chains only after finishing normally
	q.autoContinue(q.index[id])
	if count() != 2 {
		t.Fatalf("a capped auto window should chain one window, got %d jobs", count())
	}
	jobs, _ := q.List(ListOptions{})
	var next *Job
	for _, j := range jobs {
		if j.ID != id {
			next = j
		}
	}
	if next.Offset != 50 || next.Root != id || !next.Auto {
		t.Errorf("chained window = {offset:%d root:%d auto:%v}, want {50,%d,true}", next.Offset, next.Root, next.Auto, id)
	}

	// A window short of the cap is not capped, so the chain ends.
	short := q.Enqueue("http://x/search", Options{MaxItems: 50, Auto: true})
	n := count()
	q.autoContinue(q.index[short])
	if count() != n {
		t.Errorf("a non-capped window must not chain: %d -> %d", n, count())
	}

	// A capped but non-auto window does not chain on its own.
	manual := q.Enqueue("http://x/search", Options{MaxItems: 50})
	q.index[manual].SetCapped(50)
	n = count()
	q.autoContinue(q.index[manual])
	if count() != n {
		t.Errorf("a non-auto window must not chain: %d -> %d", n, count())
	}
}

func TestAutoContinueStopsOnCancelOrFailure(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	count := func() int { j, _ := q.List(ListOptions{}); return len(j) }

	// A window keeps the capped flag its resolve set even when the download is
	// then canceled or fails, so the terminal status must stop the chain.
	canceled := q.Enqueue("http://x/search", Options{MaxItems: 50, Auto: true})
	q.index[canceled].SetCapped(50)
	q.index[canceled].cancel(time.Now())
	n := count()
	q.autoContinue(q.index[canceled])
	if count() != n {
		t.Errorf("a canceled auto window must not keep fetching: %d -> %d", n, count())
	}

	failed := q.Enqueue("http://x/search", Options{MaxItems: 50, Auto: true})
	q.index[failed].SetCapped(50)
	q.index[failed].Fail(ErrCodeDownloadFailed, "boom", time.Now())
	n = count()
	q.autoContinue(q.index[failed])
	if count() != n {
		t.Errorf("a failed auto window must not keep fetching: %d -> %d", n, count())
	}
}

// capUntilProc caps every window whose offset is at or below maxOffset, so a
// fetch-all chain runs a known number of windows and then stops.
type capUntilProc struct{ maxOffset int }

func (p capUntilProc) Process(_ context.Context, job *Job) error {
	s := job.Snapshot()
	job.SetItems([]Item{{PostID: "p"}})
	if s.Offset <= p.maxOffset {
		job.SetCapped(50)
	}
	return nil
}

func TestFetchAllRunsChainToExhaustion(t *testing.T) {
	q := New(capUntilProc{maxOffset: 50}, 1, 100) // caps offsets 0 and 50, then runs short
	q.Start()
	defer q.Close()

	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := q.Wait(ctx, id); err != nil {
		t.Fatalf("wait first window: %v", err)
	}
	if _, err := q.ContinueAll(id); err != nil {
		t.Fatalf("ContinueAll: %v", err)
	}

	settled := func() bool {
		jobs, _ := q.List(ListOptions{})
		if len(jobs) != 3 {
			return false
		}
		for _, j := range jobs {
			if j.Status == JobQueued || j.Status == JobRunning {
				return false
			}
		}
		return true
	}
	for deadline := time.Now().Add(2 * time.Second); !settled() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}

	jobs, _ := q.List(ListOptions{})
	if len(jobs) != 3 {
		t.Fatalf("fetch-all should stop after the short window: got %d windows", len(jobs))
	}
	offsets := map[int]bool{}
	for _, j := range jobs {
		offsets[j.Offset] = true
		if j.Root != id {
			t.Errorf("window %d Root = %d, want series %d", j.ID, j.Root, id)
		}
	}
	for _, want := range []int{0, 50, 100} {
		if !offsets[want] {
			t.Errorf("missing window at offset %d; got offsets %v", want, offsets)
		}
	}
}

func TestCancelRemovesPendingJob(t *testing.T) {
	q := New(noopProcessor{}, 1, 100) // not started: the job stays pending
	id := q.Enqueue("http://x", Options{})
	if err := q.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := q.Get(id); err != ErrNotFound {
		t.Errorf("a canceled pending job should be removed, Get = %v", err)
	}
	if len(q.pending) != 0 {
		t.Errorf("pending should be empty after cancel, got %d", len(q.pending))
	}
}

func TestWaitTimesOutOnPendingJob(t *testing.T) {
	q := New(noopProcessor{}, 1, 100) // not started: nothing finishes
	id := q.Enqueue("http://x", Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := q.Wait(ctx, id); err == nil {
		t.Error("Wait on a never-finishing job should return ctx error")
	}
	if _, err := q.Wait(context.Background(), 404); err != ErrNotFound {
		t.Errorf("Wait(unknown) = %v, want ErrNotFound", err)
	}
}
