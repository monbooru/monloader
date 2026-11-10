package queue

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/outcome"
)

func storedStatus(t *testing.T, st *Store, id int64) (string, bool) {
	t.Helper()
	var status string
	err := st.db.QueryRow(`SELECT status FROM jobs WHERE id = ?`, id).Scan(&status)
	if err != nil {
		return "", false
	}
	return status, true
}

func storedCount(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitStoredStatus(t *testing.T, st *Store, id int64, want string) {
	t.Helper()
	if eventually(3*time.Second, func() bool {
		got, ok := storedStatus(t, st, id)
		return ok && got == want
	}) {
		return
	}
	got, ok := storedStatus(t, st, id)
	t.Fatalf("store row %d = %q (present=%v), want %q", id, got, ok, want)
}

func openTestQueueStore(t *testing.T) *Store {
	t.Helper()
	return openQueueStoreAt(t, t.TempDir())
}

func eventually(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for !cond() {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

func storedQueue(st *Store) *Queue {
	q := New(noopProcessor{}, 1, 100)
	q.UseStore(st)
	return q
}

func openQueueStoreAt(t *testing.T, dir string) *Store {
	t.Helper()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestLandedBookSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	const url = "https://e-hentai.org/g/1/aaaaaaaaaa/"
	if landed, err := st.BookLanded(url); landed || err != nil {
		t.Fatalf("before landing = %v, %v", landed, err)
	}
	for range 2 {
		if err := st.MarkBookLanded(url); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	if landed, err := openQueueStoreAt(t, dir).BookLanded(url); !landed || err != nil {
		t.Errorf("after a restart = %v, %v, want landed", landed, err)
	}
}

func TestBlacklistedBookKeepsItsMetadata(t *testing.T) {
	st := openTestQueueStore(t)
	const url = "https://e-hentai.org/g/1/aaaaaaaaaa/"
	if meta, err := st.BlacklistedBook(url); meta != nil || err != nil {
		t.Fatalf("before recording = %v, %v", meta, err)
	}
	for _, tag := range []string{"old", "new"} {
		if err := st.MarkBookBlacklisted(url, map[string]any{"tags": []any{tag}}); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := st.BlacklistedBook(url)
	if err != nil || !reflect.DeepEqual(meta["tags"], []any{"new"}) {
		t.Errorf("after recording = %v, %v, want the latest metadata", meta, err)
	}
}

func TestStoreKeepsAJobsSubcategory(t *testing.T) {
	st := openTestQueueStore(t)
	j := newJob(1, "https://x", Options{}, time.Now())
	j.SetSubcategory("search")
	if err := st.SaveJob(j.Snapshot()); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.LoadJobs()
	if err != nil || len(jobs) != 1 || jobs[0].Subcategory != "search" {
		t.Errorf("reloaded = %+v, %v, want the subcategory kept", jobs, err)
	}
}

func TestStoreMirrorsTransitions(t *testing.T) {
	st := openTestQueueStore(t)
	release := make(chan struct{})
	running := make(chan int64, 1)
	q := New(haltAllProc{running: running, release: release}, 1, 10)
	q.UseStore(st)

	id := q.Enqueue("https://example.com/a", Options{})
	if got, _ := storedStatus(t, st, id); got != "queued" {
		t.Errorf("after enqueue: store = %q, want queued", got)
	}

	q.Start()
	defer q.Close()
	<-running
	waitStoredStatus(t, st, id, "running")
	close(release)
	waitStoredStatus(t, st, id, "succeeded")
}

type haltAllProc struct {
	running chan int64
	release chan struct{}
}

func (p haltAllProc) Process(ctx context.Context, job *Job) error {
	p.running <- job.ID
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestStoreReload(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)

	release := make(chan struct{})
	running := make(chan int64, 1)
	q := New(haltAllProc{running: running, release: release}, 1, 10)
	q.UseStore(st)
	q.Start()
	doneID := q.Enqueue("https://example.com/done", Options{})
	<-running
	close(release)
	if _, err := q.Wait(context.Background(), doneID); err != nil {
		t.Fatal(err)
	}
	waitStoredStatus(t, st, doneID, "succeeded")

	release2 := make(chan struct{})
	q2 := New(haltAllProc{running: running, release: release2}, 1, 10)
	q2.UseStore(st)
	runID := q2.Enqueue("https://example.com/interrupted", Options{})
	queuedID := q2.Enqueue("https://example.com/queued", Options{})
	q2.Start()
	<-running
	waitStoredStatus(t, st, runID, "running")

	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	release3 := make(chan struct{})
	close(release3)
	q3 := New(haltAllProc{running: make(chan int64, 8), release: release3}, 1, 10)
	q3.UseStore(st2)

	if got, _ := storedStatus(t, st2, runID); got != "interrupted" {
		t.Errorf("running row reloaded as %q, want interrupted", got)
	}
	snaps, _ := q3.List(ListOptions{})
	byID := map[int64]JobStatus{}
	for _, s := range snaps {
		byID[s.ID] = s.Status
	}
	if byID[doneID] != JobSucceeded {
		t.Errorf("finished job reloaded as %s, want succeeded", byID[doneID])
	}
	if byID[runID] != JobInterrupted {
		t.Errorf("running job reloaded as %s, want interrupted", byID[runID])
	}

	q3.Start()
	defer q3.Close()
	if snap, err := q3.Wait(context.Background(), queuedID); err != nil || snap.Status != JobSucceeded {
		t.Errorf("reloaded queued job = %v (err %v), want succeeded", snap.Status, err)
	}

	if err := q3.Retry(runID, false); err != nil {
		t.Fatalf("requeue interrupted: %v", err)
	}
	snap, err := q3.Wait(context.Background(), runID)
	if err != nil || snap.Status != JobSucceeded {
		t.Errorf("requeued job = %v (err %v), want succeeded", snap.Status, err)
	}
	if !snap.Force {
		t.Error("requeued interrupted job runs against the archive; it must bypass it")
	}
}

func TestStoreClearControls(t *testing.T) {
	st := openTestQueueStore(t)
	q := New(noopProcessor{}, 1, 10)
	q.UseStore(st)

	a := q.Enqueue("https://example.com/a", Options{})
	b := q.Enqueue("https://example.com/b", Options{})
	if n := storedCount(t, st); n != 2 {
		t.Fatalf("stored rows = %d, want 2", n)
	}
	if err := q.CancelSeries(a); err != nil {
		t.Fatal(err)
	}
	if _, ok := storedStatus(t, st, a); ok {
		t.Error("canceled pending job still has a store row")
	}
	q.CancelPending()
	if status, ok := storedStatus(t, st, b); !ok || status != string(JobCanceled) {
		t.Errorf("cancel-pending stored status = %q ok=%v, want canceled", status, ok)
	}
	snaps, _ := q.List(ListOptions{})
	if len(snaps) != 1 || snaps[0].Status != JobCanceled || snaps[0].URL != "https://example.com/b" {
		t.Errorf("after cancel-pending = %+v, want one canceled row keeping its url", snaps)
	}
	q.Clear()
	if _, ok := storedStatus(t, st, b); ok {
		t.Error("Clear left the canceled row's store entry")
	}

	q2 := New(noopProcessor{}, 1, 10)
	q2.store = st
	q2.Start()
	defer q2.Close()
	id := q2.Enqueue("https://example.com/c", Options{})
	if _, err := q2.Wait(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitStoredStatus(t, st, id, "succeeded")
	q2.Clear()
	if _, ok := storedStatus(t, st, id); ok {
		t.Error("Clear left a finished store row")
	}
}

func TestStoreJobIDsAreNeverReissued(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	q := New(noopProcessor{}, 1, 10)
	q.UseStore(st)
	q.Enqueue("https://example.com/a", Options{})
	last := q.Enqueue("https://example.com/b", Options{})
	q.Clear()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2 := openQueueStoreAt(t, dir)
	q2 := New(noopProcessor{}, 1, 10)
	q2.UseStore(st2)
	if id := q2.Enqueue("https://example.com/c", Options{}); id <= last {
		t.Errorf("first id after a cleared restart = %d, want past the %d already issued", id, last)
	}
}

func TestStoreRingBoundEvictsRows(t *testing.T) {
	st := openTestQueueStore(t)
	q := New(noopProcessor{}, 1, 3)
	q.UseStore(st)
	q.Start()
	defer q.Close()
	var last int64
	for i := 0; i < 6; i++ {
		last = q.Enqueue("https://example.com/x", Options{})
		if _, err := q.Wait(context.Background(), last); err != nil {
			t.Fatal(err)
		}
	}
	waitStoredStatus(t, st, last, "succeeded")
	if !eventually(3*time.Second, func() bool { return storedCount(t, st) <= 3 }) {
		t.Errorf("stored rows = %d, want <= ring bound 3", storedCount(t, st))
	}
}

func TestStoreDegradesWithoutStore(t *testing.T) {
	q := startQueue(t, noopProcessor{}, 1, 10)
	id := q.Enqueue("https://example.com/a", Options{})
	if snap, err := q.Wait(context.Background(), id); err != nil || snap.Status != JobSucceeded {
		t.Errorf("job without store = %v (err %v), want succeeded", snap.Status, err)
	}
}

func TestOpenStoreUnwritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits do not decide writability here")
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores permission bits, so a read-only dir cannot refuse the probe")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir); err == nil {
		t.Error("OpenStore on an unwritable dir returned nil error")
	}
}

func TestOpenStoreInAFolderNamedWithURISyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses ? in a file name")
	}
	dir := filepath.Join(t.TempDir(), "a#b?c%41")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	openQueueStoreAt(t, dir)
	if _, err := os.Stat(filepath.Join(dir, storeFile)); err != nil {
		t.Errorf("store not written inside %q: %v", dir, err)
	}
}

func TestStoreStaleWriteDoesNotRegressRow(t *testing.T) {
	st := openTestQueueStore(t)
	q := New(noopProcessor{}, 1, 10)
	q.store = st
	id := q.Enqueue("https://example.com/a", Options{})
	j := q.index[id]
	stale := j.Snapshot()

	if err := j.start(time.Now()); err != nil {
		t.Fatal(err)
	}
	j.Finalize(time.Now())
	q.persist(j)
	if got, _ := storedStatus(t, st, id); got != "succeeded" {
		t.Fatalf("finished row = %q, want succeeded", got)
	}

	if err := st.SaveJob(stale); err != nil {
		t.Fatalf("stale SaveJob: %v", err)
	}
	if got, _ := storedStatus(t, st, id); got != "succeeded" {
		t.Errorf("stale snapshot regressed the row to %q", got)
	}
}

func TestOpenStoreMigratesOldSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", storeDSN(filepath.Join(dir, storeFile)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT NOT NULL, data TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO jobs (id, status, data) VALUES (1, 'succeeded', '{"job":{"id":1,"status":"succeeded","url":"https://example.com/a"}}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore over the old schema: %v", err)
	}
	defer func() { _ = st.Close() }()
	jobs, err := st.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != JobSucceeded {
		t.Fatalf("LoadJobs over migrated store = %v", jobs)
	}
}

func TestStoreReloadSkipsCorruptRow(t *testing.T) {
	st := openTestQueueStore(t)
	q := New(noopProcessor{}, 1, 10)
	q.store = st
	a := q.Enqueue("https://example.com/a", Options{})
	b := q.Enqueue("https://example.com/b", Options{})
	if _, err := st.db.Exec(`UPDATE jobs SET data = 'not json' WHERE id = ?`, a); err != nil {
		t.Fatal(err)
	}

	jobs, err := st.LoadJobs()
	if err != nil {
		t.Fatalf("LoadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != b {
		t.Fatalf("LoadJobs = %d jobs, want only the intact job %d", len(jobs), b)
	}
}

func TestStorePersistsContinuationFields(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	q := New(noopProcessor{}, 1, 10)
	q.store = st
	id := q.Enqueue("https://example.com/s", Options{
		Offset: 40, Root: 7, Auto: true, Priority: true, MaxItems: 20,
	})
	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	jobs, err := st2.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != id {
		t.Fatalf("LoadJobs = %v, want the one enqueued job", jobs)
	}
	j := jobs[0]
	if j.Offset != 40 || j.Root != 7 || !j.Auto || !j.Priority || j.MaxItems != 20 {
		t.Errorf("reloaded fields = offset %d root %d auto %v priority %v max %d",
			j.Offset, j.Root, j.Auto, j.Priority, j.MaxItems)
	}
}

func TestCloseSettlesPendingRowsInTheStore(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)

	release := make(chan struct{})
	running := make(chan int64, 1)
	q := New(haltAllProc{running: running, release: release}, 1, 10)
	q.UseStore(st)
	q.Start()
	q.Enqueue("https://example.com/running", Options{})
	<-running
	waiting := q.Enqueue("https://example.com/waiting", Options{})
	q.Close()
	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	if got, _ := storedStatus(t, st2, waiting); got != "canceled" {
		t.Fatalf("pending row stored as %q after a graceful stop, want canceled", got)
	}

	q2 := New(noopProcessor{}, 1, 10)
	q2.UseStore(st2)
	defer q2.Close()
	snap, err := q2.Get(waiting)
	if err != nil {
		t.Fatalf("the row was lost across the restart: %v", err)
	}
	if snap.Status != JobCanceled {
		t.Errorf("reloaded as %s, want canceled rather than back in the FIFO", snap.Status)
	}
}

func TestDeleteJobsPastSQLitesVariableLimit(t *testing.T) {
	st := openTestQueueStore(t)
	j := newJob(33000, "http://x", Options{}, time.Now())
	if err := st.SaveJob(j.Snapshot()); err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 33001)
	for i := range ids {
		ids[i] = int64(i)
	}
	if err := st.DeleteJobs(ids); err != nil {
		t.Fatalf("DeleteJobs(%d ids): %v", len(ids), err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows left (err %v), want none", n, err)
	}
}

type stalledReporter struct {
	noopProcessor
	reached chan struct{}
	release chan struct{}
}

func (p stalledReporter) ReportDropped(context.Context, *Job) {
	close(p.reached)
	<-p.release
}

func TestClearDuringACancelsReportKeepsTheRowGone(t *testing.T) {
	dir := t.TempDir()
	st := openQueueStoreAt(t, dir)
	proc := stalledReporter{reached: make(chan struct{}), release: make(chan struct{})}
	q := New(proc, 1, 100)
	q.UseStore(st)
	id := q.EnqueueLookup(1, "", BackendBooru, "abc", "", false, false)

	done := make(chan error, 1)
	go func() { done <- q.CancelLive(id) }()
	<-proc.reached
	q.Clear()
	close(proc.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2 := openQueueStoreAt(t, dir)
	q2 := storedQueue(st2)
	if _, err := q2.Get(id); err == nil {
		t.Error("a cleared row came back at the next boot")
	}
}

func TestEnqueueRetryAndContinueSaveUnderTheRowGuard(t *testing.T) {
	q := storedQueue(openTestQueueStore(t))
	failed := parkFinished(q, 10, time.Now(), func(j *Job) { j.Fail(outcome.ErrCodeDownloadFailed, "boom", time.Now()) })
	capped := parkFinished(q, 11, time.Now(), func(j *Job) {
		j.Capped, j.Cap = true, 2
		j.Finalize(time.Now())
	})
	for name, call := range map[string]func(){
		"enqueue":  func() { q.Enqueue("http://x", Options{}) },
		"retry":    func() { _ = q.Retry(failed.ID, false) },
		"continue": func() { _, _ = q.Continue(capped.ID) },
	} {
		t.Run(name, func(t *testing.T) {
			q.rowWrite.Lock()
			done := make(chan struct{})
			go func() {
				call()
				close(done)
			}()
			select {
			case <-done:
				q.rowWrite.Unlock()
				t.Fatal("saved past a held row guard")
			case <-time.After(50 * time.Millisecond):
			}
			q.rowWrite.Unlock()
			<-done
		})
	}
}

func fillEvery(t *testing.T, v reflect.Value) {
	t.Helper()
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			fillEvery(t, v.Field(i))
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillEvery(t, v.Index(0))
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Bool:
		v.SetBool(true)
	default:
		t.Fatalf("no filler for %s", v.Type())
	}
}

func TestStoreKeepsEveryJobField(t *testing.T) {
	st := openQueueStoreAt(t, t.TempDir())
	var want jobState
	fillEvery(t, reflect.ValueOf(&want).Elem())
	if err := st.SaveJob(&Job{jobState: want}); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.LoadJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("LoadJobs = %v, %v", jobs, err)
	}
	if !reflect.DeepEqual(jobs[0].jobState, want) {
		t.Errorf("reloaded = %+v\nwant %+v", jobs[0].jobState, want)
	}
}
