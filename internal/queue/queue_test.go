package queue

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/outcome"
)

type noopProcessor struct{}

func (noopProcessor) Process(context.Context, *Job) error { return nil }

type reportingProcessor struct {
	noopProcessor
	mu      sync.Mutex
	dropped []int64
}

func (p *reportingProcessor) ReportDropped(_ context.Context, snap *Job) {
	p.mu.Lock()
	p.dropped = append(p.dropped, snap.ID)
	p.mu.Unlock()
}

func (p *reportingProcessor) reported() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.dropped...)
}

func TestOnlyAQueuedJobStartsAndOnlyAFinishedOneRetries(t *testing.T) {
	now := time.Now()
	for _, status := range []JobStatus{JobRunning, JobSucceeded, JobPartial, JobFailed, JobCanceled, JobInterrupted} {
		j := newJob(1, "http://x", Options{}, now)
		j.Status = status
		if err := j.start(now); err == nil {
			t.Errorf("a %s job started", status)
		}
	}
	for _, status := range []JobStatus{JobQueued, JobRunning} {
		j := newJob(1, "http://x", Options{}, now)
		j.Status = status
		if err := j.reset(false, now); err == nil {
			t.Errorf("a %s job retried", status)
		}
	}
	for _, status := range []JobStatus{JobSucceeded, JobPartial, JobFailed, JobCanceled, JobInterrupted} {
		j := newJob(1, "http://x", Options{}, now)
		j.Status = status
		if err := j.reset(false, now); err != nil {
			t.Errorf("a %s job refused its retry: %v", status, err)
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

func startQueue(t *testing.T, proc Processor, workers, maxFinished int) *Queue {
	t.Helper()
	q := New(proc, workers, maxFinished)
	q.Start()
	t.Cleanup(q.Close)
	return q
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
			[]Item{{Outcome: outcome.Created}, {Outcome: outcome.Created}},
			Summary{Created: 2, Total: 2},
			JobSucceeded,
		},
		{
			"mixed is partial",
			[]Item{{Outcome: outcome.Created}, {Outcome: outcome.Duplicate}, {Outcome: outcome.SkippedArchive}, {Outcome: outcome.Failed}},
			Summary{Created: 1, Duplicate: 1, Skipped: 1, Archived: 1, Failed: 1, Total: 4},
			JobPartial,
		},
		{
			"all failed",
			[]Item{{Outcome: outcome.Failed}, {Outcome: outcome.Failed}},
			Summary{Failed: 2, Total: 2},
			JobFailed,
		},
		{
			"duplicates and skips only succeed",
			[]Item{{Outcome: outcome.Duplicate}, {Outcome: outcome.SkippedArchive}},
			Summary{Duplicate: 1, Skipped: 1, Archived: 1, Total: 2},
			JobSucceeded,
		},
		{
			"unsupported media skips, not fails",
			[]Item{{Outcome: outcome.Created}, {Outcome: outcome.SkippedUnsupported}},
			Summary{Created: 1, Skipped: 1, Total: 2},
			JobSucceeded,
		},
		{
			"a blacklist skip counts under skipped, apart from the archive",
			[]Item{{Outcome: outcome.Created}, {Outcome: outcome.SkippedBlacklist}, {Outcome: outcome.SkippedArchive}},
			Summary{Created: 1, Skipped: 2, Archived: 1, Blacklisted: 1, Total: 3},
			JobSucceeded,
		},
		{
			"enriched counts on its own, succeeds",
			[]Item{{Outcome: outcome.Enriched}},
			Summary{Enriched: 1, Total: 1},
			JobSucceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarize(tc.items); got != tc.summary {
				t.Errorf("summarize = %+v, want %+v", got, tc.summary)
			}
			if got := deriveStatus(tc.items); got != tc.status {
				t.Errorf("deriveStatus = %s, want %s", got, tc.status)
			}
		})
	}
}

func TestSummaryAddSumsEveryField(t *testing.T) {
	a := Summary{Created: 1, Duplicate: 2, Enriched: 3, Replaced: 4, Skipped: 5, Archived: 9, Failed: 6, Matched: 7, Canceled: 8, Total: 36}
	b := Summary{Created: 10, Duplicate: 20, Enriched: 30, Replaced: 40, Skipped: 50, Archived: 90, Failed: 60, Matched: 70, Canceled: 80, Total: 360}
	want := Summary{Created: 11, Duplicate: 22, Enriched: 33, Replaced: 44, Skipped: 55, Archived: 99, Failed: 66, Matched: 77, Canceled: 88, Total: 396}
	if got := a.Add(b); got != want {
		t.Errorf("Add = %+v, want %+v", got, want)
	}
}

func TestUpdateItemRejectsIllegalTransition(t *testing.T) {
	j := newJob(1, "u", Options{}, time.Now())
	j.SetItems([]Item{{PostID: "1"}})
	if j.UpdateItem(0, func(it *Item) { it.Status = ItemDone }) {
		t.Error("UpdateItem should report an illegal pending->done transition")
	}
	if !j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded }) {
		t.Error("UpdateItem should accept pending->downloaded")
	}
	if j.UpdateItem(9, func(it *Item) {}) {
		t.Error("UpdateItem should reject an out-of-range index")
	}
}

func TestRetryKeepsBackLinkOnArchiveSkip(t *testing.T) {
	j := newJob(1, "http://danbooru/posts/489", Options{}, time.Now())
	j.SetItems([]Item{{PostID: "489"}})
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
	j.UpdateItem(0, func(it *Item) {
		it.Status = ItemDone
		it.Outcome = outcome.Created
		it.MonbooruID = 77
		it.SHA256 = "abc"
	})
	j.Finalize(time.Now())

	if err := j.reset(false, time.Now()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	j.SetItems([]Item{{PostID: "489"}})
	j.UpdateItem(0, func(it *Item) {
		it.Status = ItemSkipped
		it.Outcome = outcome.SkippedArchive
	})
	j.Finalize(time.Now())

	got := j.Items[0]
	if got.Outcome != outcome.SkippedArchive {
		t.Errorf("outcome = %s, want skipped_archive", got.Outcome)
	}
	if got.MonbooruID != 77 || got.SHA256 != "abc" {
		t.Errorf("retry dropped the back-link: monbooru_id=%d sha256=%q, want 77/abc", got.MonbooruID, got.SHA256)
	}
}

func TestEnqueueGetListAndIDs(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
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
	q := New(noopProcessor{}, 1, 3)
	for i := int64(1); i <= 5; i++ {
		parkFinished(q, i, time.Now(), func(*Job) {})
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

func TestListOmitItems(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error {
		j.SetItems([]Item{{PostID: "1"}, {PostID: "2"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.Duplicate })
		if j.ID == 2 {
			close(started)
			<-release
		}
		return nil
	}), 1, 100)

	done := q.Enqueue("http://x", Options{})
	waitFor(t, q, done)
	running := q.Enqueue("http://y", Options{})
	<-started
	defer close(release)

	full, _ := q.List(ListOptions{})
	meta, _ := q.List(ListOptions{OmitItems: true})
	if len(full) != 2 || len(meta) != 2 {
		t.Fatalf("listed %d full / %d meta jobs, want 2 each", len(full), len(meta))
	}
	byID := func(js []*Job, id int64) *Job {
		for _, j := range js {
			if j.ID == id {
				return j
			}
		}
		t.Fatalf("job %d not listed", id)
		return nil
	}
	for _, id := range []int64{done, running} {
		f, m := byID(full, id), byID(meta, id)
		if len(f.Items) != 2 {
			t.Errorf("job %d full list carries %d items, want 2", id, len(f.Items))
		}
		if len(m.Items) != 0 {
			t.Errorf("job %d meta list carries items: %+v", id, m.Items)
		}
		if m.Summary != f.Summary {
			t.Errorf("job %d meta summary = %+v, want %+v", id, m.Summary, f.Summary)
		}
	}
}

func TestListSweepsFinishedPastRetention(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	q.SetRetention(7 * 24 * time.Hour)

	ages := map[int64]time.Duration{
		1: 8 * 24 * time.Hour,
		2: 7*24*time.Hour + 1,
		3: 6 * 24 * time.Hour,
	}
	for _, id := range []int64{1, 2, 3} {
		j := newJob(id, "u", Options{}, now.Add(-ages[id]))
		j.Finalize(now.Add(-ages[id]))
		q.index[id] = j
		q.pushFinishedLocked(j)
	}
	pending := newJob(99, "u", Options{}, now)
	q.index[99] = pending
	q.pending = append(q.pending, pending)

	got, total := q.List(ListOptions{})
	if total != 2 || len(got) != 2 {
		t.Fatalf("List = %v (total %d), want the two jobs inside the window", idsOf(got), total)
	}
	for _, gone := range []int64{1, 2} {
		if _, ok := q.index[gone]; ok {
			t.Errorf("job %d is past the retention window and should be de-indexed", gone)
		}
	}
	if len(q.finished) != 1 || q.finished[0].ID != 3 {
		t.Errorf("finished ring = %v, want only job 3", idsOf(q.finished))
	}
	if _, ok := q.index[99]; !ok || len(q.pending) != 1 {
		t.Error("a pending job has no terminal time and must survive the sweep")
	}

	q2 := New(noopProcessor{}, 1, 100)
	q2.now = func() time.Time { return now }
	old := newJob(1, "u", Options{}, now.Add(-30*24*time.Hour))
	old.Finalize(now.Add(-30 * 24 * time.Hour))
	q2.index[1] = old
	q2.pushFinishedLocked(old)
	if _, total := q2.List(ListOptions{}); total != 1 {
		t.Errorf("with retention unset a 30-day-old job should remain, got total %d", total)
	}
}

func parkFinished(q *Queue, id int64, at time.Time, shape func(*Job)) *Job {
	j := newJob(id, "u", Options{}, at)
	shape(j)
	q.index[id] = j
	q.pushFinishedLocked(j)
	return j
}

func TestListSweepsSucceededPastSuccessRetention(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	q.SetSuccessRetention(3 * 24 * time.Hour)

	old := now.Add(-5 * 24 * time.Hour)
	parkFinished(q, 1, old, func(j *Job) { j.Finalize(old) })
	parkFinished(q, 2, old, func(j *Job) { j.Fail(outcome.ErrCodeDownloadFailed, "boom", old) })
	parkFinished(q, 3, old, func(j *Job) {
		j.SetItems([]Item{{PostID: "a"}, {PostID: "b"}})
		j.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = outcome.Created })
		j.UpdateItem(1, func(it *Item) { it.Status = ItemFailed; it.Outcome = outcome.Failed })
		j.Finalize(old)
	})
	parkFinished(q, 4, old, func(j *Job) { j.cancel(old) })
	parkFinished(q, 5, old, func(j *Job) { j.interrupt(old) })
	parkFinished(q, 6, old, func(j *Job) { j.SetCapped(50); j.Finalize(old) })

	fresh := now.Add(-time.Hour)
	parkFinished(q, 7, fresh, func(j *Job) { j.Finalize(fresh) })

	got, total := q.List(ListOptions{})
	want := []int64{7, 6, 5, 4, 3, 2}
	if total != len(want) || !slices.Equal(idsOf(got), want) {
		t.Fatalf("List = %v (total %d), want %v", idsOf(got), total, want)
	}
	if _, ok := q.index[1]; ok {
		t.Error("a succeeded job past the success window should be de-indexed")
	}
}

func TestClearSucceeded(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	parkFinished(q, 1, now, func(j *Job) { j.Finalize(now) })
	parkFinished(q, 2, now, func(j *Job) { j.Fail(outcome.ErrCodeDownloadFailed, "boom", now) })
	parkFinished(q, 3, now, func(j *Job) { j.SetCapped(50); j.Finalize(now) })

	if got := q.ClearableSucceeded(); got != 1 {
		t.Fatalf("ClearableSucceeded = %d, want 1", got)
	}
	q.ClearSucceeded()
	got, total := q.List(ListOptions{})
	want := []int64{3, 2}
	if total != len(want) || !slices.Equal(idsOf(got), want) {
		t.Fatalf("List = %v (total %d), want %v", idsOf(got), total, want)
	}
	if got := q.ClearableSucceeded(); got != 0 {
		t.Errorf("ClearableSucceeded = %d after the clear, want 0", got)
	}
}

func TestSuccessSweepJudgesASeriesWhole(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	q.SetSuccessRetention(3 * 24 * time.Hour)

	old := now.Add(-5 * 24 * time.Hour)

	parkFinished(q, 1, old, func(j *Job) { j.Root = 1; j.Finalize(old) })
	parkFinished(q, 2, old, func(j *Job) { j.Root = 1; j.Fail(outcome.ErrCodeDownloadFailed, "boom", old) })

	parkFinished(q, 3, old, func(j *Job) { j.Root = 3; j.Finalize(old) })
	parkFinished(q, 4, now.Add(-time.Hour), func(j *Job) { j.Root = 3; j.Finalize(now.Add(-time.Hour)) })

	if _, total := q.List(ListOptions{}); total != 4 {
		t.Fatalf("total = %d, want all four windows kept", total)
	}

	q.now = func() time.Time { return now.Add(4 * 24 * time.Hour) }
	got, total := q.List(ListOptions{})
	want := []int64{2, 1}
	if total != len(want) || !slices.Equal(idsOf(got), want) {
		t.Fatalf("List = %v (total %d), want the pinned series %v", idsOf(got), total, want)
	}
}

func TestStatusChangedAtTracksEveryTransition(t *testing.T) {
	created := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	j := newJob(1, "u", Options{}, created)
	if !j.StatusChangedAt.Equal(created) {
		t.Errorf("queued StatusChangedAt = %v, want the creation time %v", j.StatusChangedAt, created)
	}

	started := created.Add(time.Hour)
	if err := j.start(started); err != nil {
		t.Fatal(err)
	}
	if !j.StatusChangedAt.Equal(started) {
		t.Errorf("running StatusChangedAt = %v, want the start time %v", j.StatusChangedAt, started)
	}

	finished := started.Add(time.Minute)
	j.Finalize(finished)
	if !j.StatusChangedAt.Equal(finished) {
		t.Errorf("succeeded StatusChangedAt = %v, want the finish time %v", j.StatusChangedAt, finished)
	}

	requeued := finished.Add(72 * time.Hour)
	if err := j.reset(false, requeued); err != nil {
		t.Fatal(err)
	}
	if !j.StatusChangedAt.Equal(requeued) {
		t.Errorf("re-queued StatusChangedAt = %v, want the retry time %v", j.StatusChangedAt, requeued)
	}
	if !j.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want the original %v", j.CreatedAt, created)
	}
}

func TestQueueClear(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	for i := int64(1); i <= 3; i++ {
		parkFinished(q, i, time.Now(), func(*Job) {})
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
	if err := q.CancelSeries(404); err != ErrNotFound {
		t.Errorf("Cancel(unknown) = %v, want ErrNotFound", err)
	}
}

func TestEnqueueMetadata_KindTargetPriority(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
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
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{Gallery: "art", MaxItems: 50})
	q.index[id].SetCapped(50)

	nid := mustContinue(t, q, id)
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

	q.index[nid].SetCapped(50)
	n2 := mustContinue(t, q, nid)
	if j, _ := q.Get(n2); j.Offset != 100 {
		t.Errorf("second continue offset = %d, want 100", j.Offset)
	}

	if _, err := q.Continue(q.Enqueue("http://y", Options{})); err != ErrNotCapped {
		t.Errorf("Continue(non-capped) = %v, want ErrNotCapped", err)
	}
	if _, err := q.Continue(404); err != ErrNotFound {
		t.Errorf("Continue(unknown) = %v, want ErrNotFound", err)
	}
}

func TestRetryKeepsThePreLabelledSite(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{Site: "danbooru", Gallery: "art"})
	q.index[id].Fail(outcome.ErrCodeDownloadFailed, "boom", q.now())

	mustRetry(t, q, id)
	j, _ := q.Get(id)
	if j.Site != "danbooru" {
		t.Errorf("site after retry = %q, want it carried like the gallery (%q)", j.Site, j.Gallery)
	}
}

func TestRetryKeepsTheClientMetadata(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/1", Options{Tags: []string{"from_script"}, Commentary: "c", Source: "s", Via: "v", PageURL: "http://x/page"})
	j := q.index[id]
	j.Finalize(time.Now())
	q.removeFromPendingLocked(id)
	q.pushFinishedLocked(j)

	mustRetry(t, q, id)
	got, _ := q.Get(id)
	if !slices.Equal(got.Tags, []string{"from_script"}) || got.Commentary != "c" || got.Source != "s" || got.Via != "v" || got.PageURL != "http://x/page" {
		t.Errorf("retried job lost the client's metadata: %+v", got.jobState)
	}
}

func TestRenameGalleryMovesWaitingAndFinishedJobs(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	waiting := q.Enqueue("http://x/1", Options{Gallery: "art"})
	done := q.Enqueue("http://x/2", Options{Gallery: "art"})
	other := q.Enqueue("http://x/3", Options{Gallery: "other"})
	j := q.index[done]
	j.Finalize(time.Now())
	q.removeFromPendingLocked(done)
	q.pushFinishedLocked(j)

	q.RenameGallery("art", "fanart")

	for id, want := range map[int64]string{waiting: "fanart", done: "fanart", other: "other"} {
		if j, _ := q.Get(id); j.Gallery != want {
			t.Errorf("job %d gallery = %q, want %q", id, j.Gallery, want)
		}
	}
	mustRetry(t, q, done)
	if j, _ := q.Get(done); j.Gallery != "fanart" {
		t.Errorf("a retry went back to %q", j.Gallery)
	}
}

func TestAFailedJobsSummaryCountsItsPublishedItems(t *testing.T) {
	j := newJob(1, "http://x", Options{}, time.Now())
	j.SetItems(make([]Item, 2))
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = outcome.Created })
	j.Fail(outcome.ErrCodeDownloadFailed, "panic", time.Now())
	if s := j.Snapshot().Summary; s.Created != 1 || s.Failed != 1 || s.Total != 2 {
		t.Errorf("summary = %+v, want 1 created, 1 failed, 2 total", s)
	}
}

type countingReporter struct {
	noopProcessor
	calls atomic.Int32
}

func (p *countingReporter) ReportDropped(ctx context.Context, _ *Job) {
	p.calls.Add(1)
	<-ctx.Done()
}

func TestReportDroppedStopsAtItsDeadline(t *testing.T) {
	old := dropReportTimeout
	dropReportTimeout = 30 * time.Millisecond
	t.Cleanup(func() { dropReportTimeout = old })
	proc := &countingReporter{}
	q := New(proc, 1, 100)
	jobs := make([]*Job, 200)
	for i := range jobs {
		jobs[i] = newJob(int64(i+1), "", Options{ImageID: 1, Kind: KindLookup}, time.Now())
	}
	q.reportDropped(jobs...)
	if n := proc.calls.Load(); n > maxDropReports {
		t.Errorf("%d reports started, want no more than the %d in flight when the deadline passed", n, maxDropReports)
	}
}

func TestRetryKeepsTheLookupLane(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.EnqueueLookup(4, "art", BackendPTR, "", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true, true)
	q.index[id].Fail(outcome.ErrCodePTRSyncing, "boom", q.now())

	mustRetry(t, q, id)
	if got := q.index[id].LookupClass(); got != "scheduled" {
		t.Errorf("lookup class after retry = %q, want it carried like the priority lane", got)
	}
}

func TestContinueFromEarlierWindowAdvancesPastHighWater(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetCapped(8)
	nid, _ := q.Continue(id)
	q.index[nid].SetCapped(8)
	if b, _ := q.Get(nid); b.Offset != 8 {
		t.Fatalf("window B offset = %d, want 8", b.Offset)
	}

	cid := mustContinue(t, q, id)
	if c, _ := q.Get(cid); c.Offset != 16 {
		t.Errorf("continue from an earlier window offset = %d, want 16 (past the high-water)", c.Offset)
	}
}

func TestContinueFromEarlierWindowPastShortFinalWindow(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetCapped(8)
	nid, _ := q.Continue(id)
	q.index[nid].SetItems(make([]Item, 3))

	cid := mustContinue(t, q, id)
	if c, _ := q.Get(cid); c.Offset != 11 {
		t.Errorf("continue past a short window offset = %d, want 11 (8 + 3 fetched)", c.Offset)
	}
}

func TestContinueWhilePendingReservesItsWindow(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetCapped(8)
	n1 := mustContinue(t, q, id)
	if b, _ := q.Get(n1); b.Offset != 8 {
		t.Fatalf("window B offset = %d, want 8", b.Offset)
	}
	n2 := mustContinue(t, q, id)
	if c, _ := q.Get(n2); c.Offset != 16 {
		t.Errorf("rapid second continue offset = %d, want 16 (past the pending window's reserved range)", c.Offset)
	}
}

func TestConcurrentContinuesTakeDistinctWindows(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 10})
	q.index[id].SetCapped(10)

	const n = 8
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cid, err := q.Continue(id)
			if err != nil {
				t.Errorf("Continue: %v", err)
				return
			}
			ids[i] = cid
		}(i)
	}
	wg.Wait()

	offsets := map[int]bool{}
	for _, cid := range ids {
		c, err := q.Get(cid)
		if err != nil {
			t.Fatal(err)
		}
		offsets[c.Offset] = true
	}
	for want := 10; want <= n*10; want += 10 {
		if !offsets[want] {
			t.Errorf("missing window at offset %d; got %v", want, offsets)
		}
	}
}

func TestSnapshotLiveSummaryWhileRunning(t *testing.T) {
	j := newJob(1, "http://x", Options{}, time.Now())
	j.SetItems([]Item{{PostID: "a"}, {PostID: "b"}, {PostID: "c"}})
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
	j.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = outcome.Created })

	if s := j.Snapshot(); s.Summary.Created != 1 || s.Summary.Total != 3 {
		t.Errorf("running snapshot summary = %+v, want 1 created / 3 total", s.Summary)
	}

	f := newJob(2, "http://y", Options{}, time.Now())
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
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 8})
	q.index[id].SetSite("danbooru")
	q.index[id].SetCapped(8)
	nid := mustContinue(t, q, id)
	if c, _ := q.Get(nid); c.Site != "danbooru" {
		t.Errorf("continuation site = %q, want the source window's %q", c.Site, "danbooru")
	}
}

func TestContinueCarriesTheSubmissionsOptions(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{
		MaxItems:   8,
		PageURL:    "http://x/thread/1",
		Tags:       []string{"meta:from_alice"},
		Commentary: "shared by alice",
		Source:     "discord",
		Via:        "my-sender",
	})
	q.index[id].SetCapped(8)
	nid := mustContinue(t, q, id)
	c, _ := q.Get(nid)
	if !slices.Equal(c.Tags, []string{"meta:from_alice"}) || c.Commentary != "shared by alice" ||
		c.Source != "discord" || c.Via != "my-sender" || c.PageURL != "http://x/thread/1" {
		t.Errorf("continuation = {page_url:%q tags:%v commentary:%q source:%q via:%q}, want the source window's",
			c.PageURL, c.Tags, c.Commentary, c.Source, c.Via)
	}
}

func TestCancelSeriesRemovesEveryWindow(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	q.index[id].SetCapped(50)
	n1, _ := q.Continue(id)
	q.index[n1].SetCapped(50)
	n2, _ := q.Continue(n1)
	other := q.Enqueue("http://y/search", Options{})

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

func TestRetrySeriesRequeuesEveryWindow(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	q.index[id].SetCapped(50)
	n1, _ := q.Continue(id)
	other := q.Enqueue("http://y/search", Options{})
	for _, jid := range []int64{id, n1, other} {
		j := q.index[jid]
		j.Finalize(time.Now())
		q.removeFromPendingLocked(jid)
		q.pushFinishedLocked(j)
	}

	if err := q.RetrySeries(n1, true); err != nil {
		t.Fatalf("RetrySeries: %v", err)
	}
	for _, jid := range []int64{id, n1} {
		j, err := q.Get(jid)
		if err != nil {
			t.Fatalf("get window %d: %v", jid, err)
		}
		if j.Status != JobQueued || !j.Force {
			t.Errorf("window %d = %s force=%v, want a forced re-queue", jid, j.Status, j.Force)
		}
	}
	if j, _ := q.Get(other); j.Status == JobQueued {
		t.Error("an unrelated series should not be re-queued")
	}
	if err := q.RetrySeries(404, true); err != ErrNotFound {
		t.Errorf("RetrySeries(unknown) = %v, want ErrNotFound", err)
	}
	if err := q.RetrySeries(n1, true); err != ErrNoneRetried {
		t.Errorf("RetrySeries(all live) = %v, want ErrNoneRetried", err)
	}
}

type haltProc struct{ running chan int64 }

func (p haltProc) Process(ctx context.Context, job *Job) error {
	s := job.Snapshot()
	job.SetItems([]Item{{PostID: "p"}})
	if s.Offset == 0 {
		job.UpdateItem(0, func(it *Item) { it.Status = ItemDownloaded })
		job.UpdateItem(0, func(it *Item) { it.Status = ItemUploaded })
		job.UpdateItem(0, func(it *Item) { it.Status = ItemDone; it.Outcome = outcome.Created })
		job.SetCapped(50)
		return nil
	}
	p.running <- s.ID
	<-ctx.Done()
	return ctx.Err()
}

func TestCancelSeriesKeepsFinishedWindows(t *testing.T) {
	running := make(chan int64, 1)
	q := startQueue(t, haltProc{running: running}, 1, 100)

	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	waitFor(t, q, id)
	nid := mustContinue(t, q, id)
	<-running

	if err := q.CancelSeries(nid); err != nil {
		t.Fatalf("CancelSeries: %v", err)
	}

	first, err := q.Get(id)
	if err != nil {
		t.Fatalf("the finished window should survive a live cancel: %v", err)
	}
	if first.Summary.Created != 1 {
		t.Errorf("finished window lost its imported item: summary=%+v", first.Summary)
	}
	waitFor(t, q, nid)
	if cw, _ := q.Get(nid); cw.Status != JobCanceled {
		t.Errorf("the in-flight window should be canceled, got %s", cw.Status)
	}
}

func TestAutoContinueChainsCappedWindows(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	count := func() int { j, _ := q.List(ListOptions{}); return len(j) }

	id := q.Enqueue("http://x/search", Options{MaxItems: 50, Auto: true})
	q.index[id].SetCapped(50)
	q.index[id].Finalize(time.Now())
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

	short := q.Enqueue("http://x/search", Options{MaxItems: 50, Auto: true})
	n := count()
	q.autoContinue(q.index[short])
	if count() != n {
		t.Errorf("a non-capped window must not chain: %d -> %d", n, count())
	}

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
	q.index[failed].Fail(outcome.ErrCodeDownloadFailed, "boom", time.Now())
	n = count()
	q.autoContinue(q.index[failed])
	if count() != n {
		t.Errorf("a failed auto window must not keep fetching: %d -> %d", n, count())
	}
}

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
	q := startQueue(t, capUntilProc{maxOffset: 50}, 1, 100)

	id := q.Enqueue("http://x/search", Options{MaxItems: 50})
	waitFor(t, q, id)
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
	eventually(2*time.Second, settled)

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

func TestPriorityJobsKeepFIFOWithinClass(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	bulk := q.Enqueue("http://x/bulk", Options{})
	p1 := q.Enqueue("http://x/p1", Options{Priority: true})
	p2 := q.Enqueue("http://x/p2", Options{Priority: true})

	want := []int64{p1, p2, bulk}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != len(want) {
		t.Fatalf("pending = %d jobs, want %d", len(q.pending), len(want))
	}
	for i, id := range want {
		if q.pending[i].ID != id {
			t.Errorf("pending[%d] = job %d, want %d", i, q.pending[i].ID, id)
		}
	}
}

type exhaustedSearchProc struct{}

func (exhaustedSearchProc) Process(_ context.Context, job *Job) error {
	s := job.Snapshot()
	if s.Offset == 0 {
		job.SetItems([]Item{{PostID: "1"}, {PostID: "2"}, {PostID: "3"}})
		job.SetCapped(3)
	}
	return nil
}

func TestContinuePastExhaustionClearsSeriesCapped(t *testing.T) {
	q := startQueue(t, exhaustedSearchProc{}, 1, 100)

	id := q.Enqueue("http://x/search", Options{MaxItems: 3})
	waitFor(t, q, id)
	cid := mustContinue(t, q, id)
	waitFor(t, q, cid)

	settled := func() bool {
		src, err := q.Get(id)
		return err == nil && !src.Capped
	}
	eventually(2*time.Second, settled)
	if src, _ := q.Get(id); src.Capped {
		t.Error("origin still capped after the series ran out")
	}
	if _, err := q.Continue(id); err != ErrNotCapped {
		t.Errorf("continue past exhaustion = %v, want ErrNotCapped", err)
	}
}

func TestCancelRemovesPendingJob(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x", Options{})
	if err := q.CancelSeries(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := q.Get(id); err != ErrNotFound {
		t.Errorf("a canceled pending job should be removed, Get = %v", err)
	}
	if len(q.pending) != 0 {
		t.Errorf("pending should be empty after cancel, got %d", len(q.pending))
	}
}

func TestCancelLiveKeepsARowForAPendingJob(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.Enqueue("http://x", Options{})
	if err := q.CancelLive(id); err != nil {
		t.Fatalf("CancelLive: %v", err)
	}
	j, err := q.Get(id)
	if err != nil {
		t.Fatalf("a canceled pending job should keep its row, Get = %v", err)
	}
	if j.Status != JobCanceled {
		t.Errorf("status = %s, want canceled", j.Status)
	}
	if len(q.pending) != 0 {
		t.Errorf("pending should be empty after cancel, got %d", len(q.pending))
	}
	if err := q.Retry(id, false); err != nil {
		t.Errorf("the kept row should be retriable: %v", err)
	}
}

func TestCancelLiveOnAFinishedWindowStopsItsSeries(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	done := parkFinished(q, 10, time.Now(), func(j *Job) { j.Finalize(time.Now()) })
	done.Root = 10
	q.mu.Lock()
	next := q.enqueueLocked("u", Options{Root: 10})
	q.mu.Unlock()

	if err := q.CancelLive(done.ID); err != nil {
		t.Fatalf("CancelLive: %v", err)
	}
	if j, _ := q.Get(next.ID); j.Status != JobCanceled {
		t.Errorf("next window = %s, want canceled", j.Status)
	}
	if j, err := q.Get(done.ID); err != nil || j.Status != JobSucceeded {
		t.Errorf("finished window = %+v, %v; want its row kept", j, err)
	}
	if err := q.CancelLive(done.ID); !errors.Is(err, ErrNotRunning) {
		t.Errorf("CancelLive on a wholly finished series = %v, want ErrNotRunning", err)
	}
}

func TestARetryInsideFinishsWindowLeavesNoWaiterParked(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	j := parkFinished(q, 10, time.Now(), func(j *Job) { j.Finalize(time.Now()) })
	j.mu.Lock()
	j.done = make(chan struct{})
	j.mu.Unlock()
	first := make(chan struct{})
	go func() {
		_, _ = q.Wait(context.Background(), j.ID)
		close(first)
	}()
	time.Sleep(10 * time.Millisecond)

	if err := q.Retry(j.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("the first waiter stayed parked on the old channel")
	}

	j.signalDone()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := q.Wait(ctx, j.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter on the retried job = %v, want it parked until the retry ends", err)
	}
}

func TestCancelWakesPendingWaiter(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(q *Queue, id int64)
	}{
		{"cancel", func(q *Queue, id int64) { _ = q.CancelSeries(id) }},
		{"cancel pending", func(q *Queue, _ int64) { q.CancelPending() }},
		{"close", func(q *Queue, _ int64) { q.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := New(noopProcessor{}, 1, 100)
			id := q.Enqueue("http://x", Options{})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			type result struct {
				job *Job
				err error
			}
			done := make(chan result, 1)
			go func() {
				job, err := q.Wait(ctx, id)
				done <- result{job, err}
			}()
			time.Sleep(20 * time.Millisecond)
			tc.drop(q, id)

			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("Wait = %v, want the dropped job's snapshot", got.err)
				}
				if got.job.Status != JobCanceled {
					t.Errorf("status = %s, want canceled", got.job.Status)
				}
			case <-ctx.Done():
				t.Fatal("the parked waiter was never released")
			}
		})
	}
}

func TestWaitTimesOutOnPendingJob(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
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

func TestEnqueueLookupCarriesHashFields(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	id := q.EnqueueLookup(42, "art", BackendBooru, "0123456789abcdef0123456789abcdef", "", false, false)
	j, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Kind != KindLookup || j.Backend != BackendBooru || j.ImageID != 42 || j.Gallery != "art" {
		t.Errorf("lookup job fields wrong: %+v", j.jobState)
	}
	if j.URL != "" || j.Subject() != "md5:0123456789abcdef0123456789abcdef" {
		t.Errorf("subject = %q url = %q, want the md5 form and no url", j.Subject(), j.URL)
	}
}

func TestEnqueueLookupLaneAndBudgetAreIndependent(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	const md5 = "0123456789abcdef0123456789abcdef"
	watched := q.EnqueueLookup(1, "", BackendBooru, md5, "", false, false)
	bulk := q.EnqueueLookup(2, "", BackendBooru, md5, "", true, false)
	scheduled := q.EnqueueLookup(3, "", BackendBooru, md5, "", true, true)

	for _, tc := range []struct {
		id       int64
		priority bool
		class    string
	}{
		{watched, true, ""},
		{bulk, false, "bulk"},
		{scheduled, false, "scheduled"},
	} {
		j, err := q.Get(tc.id)
		if err != nil {
			t.Fatalf("Get(%d): %v", tc.id, err)
		}
		if j.Priority != tc.priority {
			t.Errorf("job %d priority = %v, want %v", tc.id, j.Priority, tc.priority)
		}
		if got := j.LookupClass(); got != tc.class {
			t.Errorf("job %d class = %q, want %q", tc.id, got, tc.class)
		}
	}

	paste := q.Enqueue("http://x/posts/1", Options{})
	want := []int64{watched, paste, bulk, scheduled}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, id := range want {
		if q.pending[i].ID != id {
			t.Errorf("pending[%d] = job %d, want %d", i, q.pending[i].ID, id)
		}
	}
}

func TestDroppedJobsAreReportedOnce(t *testing.T) {
	const md5 = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		drop func(q *Queue, id int64)
	}{
		{"operator cancel", func(q *Queue, id int64) { _ = q.CancelLive(id) }},
		{"api delete", func(q *Queue, id int64) { _ = q.CancelSeries(id) }},
		{"drain the fifo", func(q *Queue, _ int64) { q.CancelPending() }},
		{"shutdown", func(q *Queue, _ int64) { q.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := &reportingProcessor{}
			q := New(proc, 1, 100)
			download := q.Enqueue("http://x/bulk", Options{})
			lookup := q.EnqueueLookup(7, "art", BackendBooru, md5, "", true, true)

			tc.drop(q, lookup)
			if got := proc.reported(); len(got) != 1 || got[0] != lookup {
				t.Errorf("reported = %v, want just the lookup job %d", got, lookup)
			}
			_ = download
		})
	}
}

func TestRemovingAFinishedJobReportsNothing(t *testing.T) {
	proc := &reportingProcessor{}
	q := startQueue(t, proc, 1, 100)

	id := q.EnqueueLookup(7, "art", BackendBooru, "0123456789abcdef0123456789abcdef", "", false, false)
	waitFor(t, q, id)
	if err := q.CancelSeries(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := proc.reported(); len(got) != 0 {
		t.Errorf("reported = %v, want nothing for a job that already ran", got)
	}
}

func TestSubjectPrefersURLThenHashes(t *testing.T) {
	j := newJob(1, "", Options{MD5: "aa", SHA256: "bb"}, time.Now())
	if got := j.Subject(); got != "md5:aa" {
		t.Errorf("Subject = %q, want md5:aa", got)
	}
	j.SetURL("http://x/posts/1")
	if got := j.Subject(); got != "http://x/posts/1" {
		t.Errorf("Subject after SetURL = %q, want the url", got)
	}
	sha := newJob(2, "", Options{SHA256: "bb"}, time.Now())
	if got := sha.Subject(); got != "sha256:bb" {
		t.Errorf("Subject = %q, want sha256:bb", got)
	}
}

func TestHashImportRetryClearsDerivedURL(t *testing.T) {
	j := newJob(1, "", Options{Kind: KindHashImport, MD5: "aa"}, time.Now())
	j.SetURL("http://booru/posts/9")
	j.Fail(outcome.ErrCodeDownloadFailed, "boom", time.Now())
	if err := j.reset(false, time.Now()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if j.URL != "" || j.MD5 != "aa" || j.Kind != KindHashImport {
		t.Errorf("retry state: url=%q md5=%q kind=%s, want re-walk from the hash", j.URL, j.MD5, j.Kind)
	}
}

func TestLookupRetryKeepsKindAndHash(t *testing.T) {
	j := newJob(1, "", Options{Kind: KindLookup, Backend: BackendPTR, SHA256: "cc", ImageID: 7}, time.Now())
	j.Fail(outcome.ErrCodeHashNotFound, "nobody has it", time.Now())
	if err := j.reset(false, time.Now()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if j.Kind != KindLookup || j.Backend != BackendPTR || j.SHA256 != "cc" || j.ImageID != 7 {
		t.Errorf("retry dropped lookup identity: %+v", j.jobState)
	}
}

func TestRetryRefusesABatchLookupRow(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	q.RecordPTRLookup("default", true, 10, ptrMatches(1, 2))
	jobs, _ := q.List(ListOptions{})
	if err := q.Retry(jobs[0].ID, false); err == nil {
		t.Error("Retry on a ptr_lookup row should refuse")
	}
	if _, total := q.List(ListOptions{Status: JobQueued}); total != 0 {
		t.Error("the refused retry must leave nothing in the FIFO")
	}
}

func TestRecordPTRLookupFoldsASweep(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	q.RecordPTRLookup("default", true, 100, ptrMatches(1, 40))
	q.RecordPTRLookup("default", true, 100, ptrMatches(41, 12))
	jobs, total := q.List(ListOptions{})
	if total != 1 {
		t.Fatalf("history holds %d jobs, want the sweep folded into one", total)
	}
	j := jobs[0]
	if j.Kind != KindPTRLookup || j.Status != JobSucceeded || j.Gallery != "default" {
		t.Errorf("job = %+v, want a succeeded ptr_lookup on the default gallery", j.jobState)
	}
	if j.Summary.Total != 200 || j.Summary.Matched != 52 {
		t.Errorf("summary = %+v, want 200 asked and 52 matched", j.Summary)
	}
	if len(j.Items) != 52 || j.Items[51].MonbooruID != 52 {
		t.Errorf("items = %d, want one per matched image across both calls", len(j.Items))
	}
	if j.LookupClass() != "scheduled" {
		t.Errorf("class = %q, want the nightly run's lane", j.LookupClass())
	}
	if j.Subject() == "" {
		t.Error("the row needs a subject; the url cell is otherwise blank")
	}

	q.RecordPTRLookup("default", false, 20, ptrMatches(200, 3))
	if _, total := q.List(ListOptions{}); total != 2 {
		t.Errorf("history holds %d jobs, want the bulk sweep filed apart", total)
	}

	now = now.Add(3 * time.Hour)
	q.RecordPTRLookup("default", true, 20, ptrMatches(300, 3))
	if _, total := q.List(ListOptions{}); total != 2 {
		t.Errorf("history holds %d jobs, want the paced sweep folded into its own lane", total)
	}

	now = now.Add(24 * time.Hour)
	q.RecordPTRLookup("default", true, 20, ptrMatches(400, 3))
	if _, total := q.List(ListOptions{}); total != 3 {
		t.Errorf("history holds %d jobs, want the next day's sweep filed apart", total)
	}
}

func TestListOrdersByLastChange(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	q.RecordPTRLookup("default", true, 10, ptrMatches(1, 2))
	now = now.Add(time.Minute)
	download := q.Enqueue("http://x/post/1", Options{})
	now = now.Add(time.Minute)
	q.RecordPTRLookup("default", true, 10, ptrMatches(3, 2))

	jobs, _ := q.List(ListOptions{})
	if len(jobs) != 2 || jobs[0].Kind != KindPTRLookup || jobs[1].ID != download {
		t.Errorf("order = %v, want the freshly folded sweep above the older download", idsOf(jobs))
	}
}

func TestRecordPTRLookupCapsItems(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	q.RecordPTRLookup("default", false, maxPTRLookupItems+50, ptrMatches(1, maxPTRLookupItems+50))
	jobs, _ := q.List(ListOptions{})
	j := jobs[0]
	if len(j.Items) != maxPTRLookupItems {
		t.Errorf("items = %d, want the row capped at %d", len(j.Items), maxPTRLookupItems)
	}
	if j.Summary.Matched != maxPTRLookupItems+50 {
		t.Errorf("matched = %d, want the true count past the cap", j.Summary.Matched)
	}
}

func TestRecordPTRLookupFoldsItemsPerImageAndCountsPerAsk(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	for i := 0; i < 3; i++ {
		q.RecordPTRLookup("default", true, 6, ptrMatches(1, 5))
	}
	jobs, _ := q.List(ListOptions{})
	j := jobs[0]
	if len(j.Items) != 5 {
		t.Errorf("items = %d, want the 5 images however often they were asked", len(j.Items))
	}
	if j.Summary.Matched != 15 || j.Summary.Total != 18 {
		t.Errorf("matched/total = %d/%d, want both counted per ask: 15 of 18", j.Summary.Matched, j.Summary.Total)
	}
}

func ptrMatches(first, n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{Status: ItemDone, Outcome: outcome.Enriched, MonbooruID: int64(first + i)}
	}
	return items
}

func TestInFlightCountsQueuedAndRunning(t *testing.T) {
	q := New(noopProcessor{}, 1, 100)
	if running, queued := q.InFlight(); running != 0 || queued != 0 {
		t.Fatalf("empty queue: running=%d queued=%d", running, queued)
	}
	q.Enqueue("https://example.com/a", Options{})
	q.Enqueue("https://example.com/b", Options{})
	running, queued := q.InFlight()
	if running != 0 || queued != 2 {
		t.Errorf("running=%d queued=%d, want 0 and 2", running, queued)
	}
}

type capProc struct{}

func (capProc) Process(_ context.Context, j *Job) error {
	n := 4
	if m := j.Snapshot().MaxItems; m > 0 && m < n {
		n = m
	}
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{PostID: strconv.Itoa(i)}
	}
	j.SetItems(items)
	for i := range items {
		j.UpdateItem(i, func(it *Item) { it.Status = ItemSkipped; it.Outcome = outcome.SkippedArchive })
	}
	j.SetCapped(n)
	return nil
}

func TestTargetChainsWindowsUpToIt(t *testing.T) {
	for _, tc := range []struct {
		target  int
		windows []int
	}{
		{10, []int{4, 4, 2}},
		{3, []int{3}},
	} {
		q := startQueue(t, capProc{}, 1, 100)
		root := q.Enqueue("https://x/posts?tags=a", Options{}.WithItems(tc.target))
		var got []int
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			jobs, _ := q.List(ListOptions{})
			got = got[:0]
			done := true
			for _, j := range slices.Backward(jobs) {
				if j.Root != root {
					continue
				}
				got = append(got, len(j.Items))
				done = done && j.Status == JobSucceeded
			}
			if done && slices.Equal(got, tc.windows) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		time.Sleep(50 * time.Millisecond)
		jobs, _ := q.List(ListOptions{})
		if len(jobs) != len(tc.windows) || !slices.Equal(got, tc.windows) {
			t.Errorf("target %d ran windows %v (%d jobs), want %v", tc.target, got, len(jobs), tc.windows)
		}
	}
}

func TestOperatorContinueDropsTheTarget(t *testing.T) {
	q := startQueue(t, capProc{}, 1, 100)
	root := q.Enqueue("https://x/posts?tags=a", Options{}.WithItems(2))
	waitFor(t, q, root)
	next := mustContinue(t, q, root)
	if j := waitFor(t, q, next); j.Target != 0 || len(j.Items) != 2 {
		t.Errorf("operator continue = target %d with %d items, want no target and the series' window", j.Target, len(j.Items))
	}
}

func TestContinueAfterATargetTakesTheWidestWindow(t *testing.T) {
	q := startQueue(t, capProc{}, 1, 100)
	root := q.Enqueue("https://x/posts?tags=a", Options{}.WithItems(6))
	var last int64
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && last == 0 {
		for _, j := range q.listSeries(root) {
			if j.Status == JobSucceeded && len(j.Items) == 2 {
				last = j.ID
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if last == 0 {
		t.Fatal("the target never reached its cut last window")
	}
	next := mustContinue(t, q, last)
	if j := waitFor(t, q, next); len(j.Items) != 4 {
		t.Errorf("continue after a 4+2 target fetched %d, want the widest window of 4", len(j.Items))
	}
}

func (q *Queue) listSeries(root int64) []*Job {
	jobs, _ := q.List(ListOptions{})
	return slices.DeleteFunc(jobs, func(j *Job) bool { return j.Root != root })
}

type slowReporter struct {
	noopProcessor
	live, peak atomic.Int32
}

func (p *slowReporter) ReportDropped(_ context.Context, _ *Job) {
	n := p.live.Add(1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	p.live.Add(-1)
}

func TestDroppedReportsAreBounded(t *testing.T) {
	proc := &slowReporter{}
	q := New(proc, 1, 100)
	for i := range 20 {
		q.EnqueueLookup(int64(i+1), "", "", "", "", true, false)
	}
	q.CancelPending()
	if peak := proc.peak.Load(); peak > maxDropReports {
		t.Errorf("%d reports ran at once, want at most %d", peak, maxDropReports)
	}
}

func TestListPastTheLastPageIsEmpty(t *testing.T) {
	q := startQueue(t, procFunc(func(ctx context.Context, j *Job) error { return nil }), 1, 100)
	waitFor(t, q, q.Enqueue("http://x", Options{}))
	for _, page := range []int{2, math.MaxInt / 2, math.MaxInt} {
		if got, total := q.List(ListOptions{Limit: 2, Page: page}); len(got) != 0 || total != 1 {
			t.Errorf("page %d = %d jobs of %d, want none of 1", page, len(got), total)
		}
	}
}

func TestCancelKeepingRowLeavesAFinishedJobAlone(t *testing.T) {
	proc := &reportingProcessor{}
	q := startQueue(t, proc, 1, 100)
	id := q.EnqueueLookup(7, "art", BackendBooru, "0123456789abcdef0123456789abcdef", "", false, false)
	waitFor(t, q, id)

	if err := q.cancelKeepingRow(id); err != ErrNotRunning {
		t.Errorf("cancelKeepingRow(finished) = %v, want ErrNotRunning", err)
	}
	q.cancel(id, true)
	q.mu.Lock()
	rows := len(slices.DeleteFunc(slices.Clone(q.finished), func(j *Job) bool { return j.ID != id }))
	q.mu.Unlock()
	if rows != 1 {
		t.Errorf("history holds the job %d times, want once", rows)
	}
	if j, err := q.Get(id); err != nil || j.Status != JobSucceeded {
		t.Errorf("the finished job should stay as it was, got %v %v", j, err)
	}
	if got := proc.reported(); len(got) != 0 {
		t.Errorf("reported = %v, want nothing for a job that already ran", got)
	}
}

type settledThenHeldProc struct{ settled, release chan struct{} }

func (p settledThenHeldProc) Process(_ context.Context, job *Job) error {
	job.Fail(outcome.ErrCodeDownloadFailed, "boom", time.Now())
	close(p.settled)
	<-p.release
	return nil
}

func TestRetryRefusesAJobStillOnTheWorker(t *testing.T) {
	proc := settledThenHeldProc{settled: make(chan struct{}), release: make(chan struct{})}
	q := startQueue(t, proc, 1, 100)
	id := q.Enqueue("http://x", Options{})
	<-proc.settled

	err := q.Retry(id, false)
	close(proc.release)
	if err == nil {
		t.Fatal("Retry took a job its worker had not finished")
	}
	waitFor(t, q, id)
	if err := q.Retry(id, false); err != nil {
		t.Errorf("Retry once finished: %v", err)
	}
}

func storeHoldsOnlyIndexedRows(t *testing.T, q *Queue, st *Store) {
	t.Helper()
	rows, err := st.db.Query(`SELECT id FROM jobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	q.mu.Lock()
	defer q.mu.Unlock()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if q.index[id] == nil {
			t.Fatalf("job %d has a store row after a Clear dropped it; it would come back at boot", id)
		}
	}
}

func TestFinishRacingAClearLeavesNoStoreRow(t *testing.T) {
	st := openTestQueueStore(t)
	q := storedQueue(st)
	q.Start()
	defer q.Close()
	for range 200 {
		id := q.Enqueue("https://example.com/a", Options{})
		q.mu.Lock()
		j := q.index[id]
		q.mu.Unlock()
		for done := false; !done; {
			q.Clear()
			select {
			case <-j.doneChan():
				done = true
			default:
			}
		}
		storeHoldsOnlyIndexedRows(t, q, st)
	}
}

func TestRecordPTRLookupRacingAClearLeavesNoStoreRow(t *testing.T) {
	st := openTestQueueStore(t)
	q := storedQueue(st)
	for range 200 {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			q.RecordPTRLookup("default", true, 10, ptrMatches(1, 2))
		}()
		go func() {
			defer wg.Done()
			q.Clear()
		}()
		wg.Wait()
		storeHoldsOnlyIndexedRows(t, q, st)
	}
}
