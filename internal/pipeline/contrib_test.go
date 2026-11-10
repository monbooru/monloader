package pipeline

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
)

type stubSender struct {
	store    *ptr.ContribStore
	noKey    bool
	notReady bool
	banned   bool
	banMsg   string
	accErr   error
	filter   *ptr.TagFilter
	ideals   map[string]bool
	raws     map[string]bool
	siblings map[string]bool
	parents  map[string]bool
	covered  int64
	perms    map[int]int
	onPost   func()

	failFrom int
	failErr  error
	calls    int
	posts    [][]postedAction
}

type postedAction struct {
	Action  int
	Entries []postedEntry
}

type postedEntry struct {
	ContentType int
	Data        any
}

const mappingsContent = 0

func decodePost(body []byte) ([]postedAction, error) {
	zr, err := zlib.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var doc []any
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		return nil, err
	}
	update := doc[2].([]any)[0].([]any)[1].([]any)[1].([]any)
	var out []postedAction
	for _, a := range update[2].([]any) {
		pair := a.([]any)
		action := postedAction{Action: int(pair[0].(float64))}
		for _, e := range pair[1].([]any) {
			content := e.([]any)[0].([]any)[2].([]any)
			action.Entries = append(action.Entries, postedEntry{int(content[0].(float64)), content[1]})
		}
		out = append(out, action)
	}
	return out, nil
}

func (s *stubSender) HasPersonalKey() bool { return !s.noKey }
func (s *stubSender) CaughtUp() bool       { return !s.notReady }

func (s *stubSender) RefreshAccount(context.Context) (*ptr.Account, error) {
	if s.accErr != nil {
		return nil, s.accErr
	}
	perms := s.perms
	if perms == nil {
		perms = map[int]int{0: 1, 1: 0, 2: 0}
	}
	acc := &ptr.Account{Type: ptr.AccountType{Permissions: perms}}
	if s.banned {
		acc.Banned, acc.BanReason = true, s.banMsg
	}
	return acc, nil
}

func (s *stubSender) RefreshTagFilter(context.Context) (*ptr.TagFilter, error) {
	if s.filter != nil {
		return s.filter, nil
	}
	return ptr.NewTagFilter(nil), nil
}

func (s *stubSender) PostUpdate(_ context.Context, u *ptr.C2SUpdate) error {
	if s.onPost != nil {
		s.onPost()
	}
	call := s.calls
	s.calls++
	if s.failFrom >= 0 && call >= s.failFrom {
		return s.failErr
	}
	body, err := u.EncodePOSTBody()
	if err != nil {
		return err
	}
	view, err := decodePost(body)
	if err != nil {
		return err
	}
	s.posts = append(s.posts, view)
	return nil
}

func (s *stubSender) Contrib() *ptr.ContribStore { return s.store }
func (s *stubSender) HashHasIdeal(h, tag string) (bool, error) {
	return s.ideals[h+"|"+tag], nil
}
func (s *stubSender) HashHasRaw(h, tag string) (bool, error) { return s.raws[h+"|"+tag], nil }
func (s *stubSender) SiblingCurrent(bad, good string) (bool, error) {
	return s.siblings[bad+"|"+good], nil
}
func (s *stubSender) ParentCurrent(child, parent string) (bool, error) {
	return s.parents[child+"|"+parent], nil
}

func (s *stubSender) Status() ptr.Status { return ptr.Status{CoveredThrough: s.covered} }

type stubSenderPTR struct{ *stubSender }

func (stubSenderPTR) Enabled() bool                              { return true }
func (stubSenderPTR) TagsForHash(string) ([]string, bool, error) { return nil, false, nil }

func newContribSender(store *ptr.ContribStore) *stubSender {
	return &stubSender{store: store, failFrom: -1}
}

func contribQueue(t *testing.T, sender *stubSender) *queue.Queue {
	return contribQueueSleep(t, sender, 0)
}

func contribQueueSleep(t *testing.T, sender *stubSender, sleep float64) *queue.Queue {
	t.Helper()
	prevBackoff := contribRetryBackoff
	contribRetryBackoff = 0
	t.Cleanup(func() { contribRetryBackoff = prevBackoff })
	cfg := config.Default()
	cfg.PTR.CommitSleep = sleep
	cfg.Monbooru.WebURL = "http://mb.example"
	proc := New(&fakeRunner{}, nil, nil, config.NewProvider(cfg), t.TempDir(), nil, stubSenderPTR{sender}, nil)
	q := queue.New(proc, 1, 10)
	q.Start()
	t.Cleanup(q.Close)
	return q
}

func newContribStore(t *testing.T) *ptr.ContribStore {
	t.Helper()
	store, err := ptr.OpenContribStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func stageBulk(t *testing.T, store *ptr.ContribStore, seed byte, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		h := append(hashN(seed)[:30], byte(i), byte(i>>8))
		id, _, err := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "bulk tag", Hash: h})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func hashN(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestContribSendEndToEnd(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	sender.raws = map[string]bool{strings.Repeat("aa", 32) + "|hat": true}
	sender.siblings = map[string]bool{"a|b": true}
	sender.parents = map[string]bool{"c|d": true}

	var ids []int64
	for _, it := range []ptr.ContribItem{
		{Kind: ptr.ContribMappingAdd, Tag: "blonde hair", Hash: hashN(0xaa)},
		{Kind: ptr.ContribMappingAdd, Tag: "blonde hair", Hash: hashN(0xbb)},
		{Kind: ptr.ContribMappingAdd, Tag: "creator:someone", Hash: hashN(0xaa)},
		{Kind: ptr.ContribMappingPetition, Tag: "hat", Hash: hashN(0xaa), Reason: "not in the image"},
		{Kind: ptr.ContribSibling, Tag: "blond hair", Tag2: "blonde hair", Reason: "misspelling"},
		{Kind: ptr.ContribParent, Tag: "miko", Tag2: "shrine maiden", Reason: "canonical"},
		{Kind: ptr.ContribSiblingPetition, Tag: "a", Tag2: "b", Reason: "not equivalent"},
		{Kind: ptr.ContribParentPetition, Tag: "c", Tag2: "d", Reason: "not implied"},
	} {
		id, dup, err := store.Stage(it)
		if err != nil || dup {
			t.Fatalf("stage %+v: dup=%v err=%v", it, dup, err)
		}
		ids = append(ids, id)
	}

	q := contribQueue(t, sender)
	snap := waitJob(t, q, q.EnqueueContrib(ids, false))
	if snap.Status != queue.JobSucceeded {
		t.Fatalf("job = %s (%s / %s)", snap.Status, snap.ErrorCode, snap.Error)
	}
	if len(sender.posts) != 1 {
		t.Fatalf("posts = %d, want 1 (8 rows fit one chunk)", len(sender.posts))
	}
	upd := sender.posts[0]
	if len(upd) != 2 || upd[0].Action != ptr.ActionPetition || upd[1].Action != ptr.ActionPend {
		t.Fatalf("action order = %+v, want petitions before pends", upd)
	}
	var grouped bool
	for _, e := range upd[1].Entries {
		if e.ContentType == mappingsContent {
			pair := e.Data.([]any)
			if pair[0] == "blonde hair" {
				if hashes := pair[1].([]any); len(hashes) == 2 {
					grouped = true
				}
			}
		}
	}
	if !grouped {
		t.Errorf("blonde hair rows not grouped per tag: %+v", upd[1].Entries)
	}

	unsent, _, _ := store.Counts()
	if unsent != 0 {
		t.Errorf("unsent after send = %d, want 0", unsent)
	}
	logRows, _ := store.LogPage(20, 0)
	if len(logRows) != 8 {
		t.Errorf("ledger rows = %d, want 8", len(logRows))
	}
	if !strings.Contains(snap.Note, "committed 3 tag adds, 3 removal petitions, 1 alias and 1 implication suggestions") {
		t.Errorf("note = %q", snap.Note)
	}
}

func TestContribSendItemsLinked(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	var ids []int64
	for _, it := range []ptr.ContribItem{
		{Kind: ptr.ContribMappingAdd, Tag: "blonde hair", Hash: hashN(0xaa)},
		{Kind: ptr.ContribSibling, Tag: "blond hair", Tag2: "blonde hair", Reason: "misspelling"},
	} {
		id, _, err := store.Stage(it)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	q := contribQueue(t, sender)
	snap := waitJob(t, q, q.EnqueueContrib(ids, false))
	if snap.Status != queue.JobSucceeded {
		t.Fatalf("job = %s (%s)", snap.Status, snap.Error)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want one per contribution", len(snap.Items))
	}
	base := "http://mb.example"
	want := []struct{ postID, url string }{
		{"tag blonde hair", base + "/i/" + strings.Repeat("aa", 32)},
		{"alias blond hair -> blonde hair", base + "/tags?q=blond_hair"},
	}
	for i, w := range want {
		if snap.Items[i].PostID != w.postID {
			t.Errorf("item %d PostID = %q, want %q", i, snap.Items[i].PostID, w.postID)
		}
		if snap.Items[i].URL != w.url {
			t.Errorf("item %d URL = %q, want %q", i, snap.Items[i].URL, w.url)
		}
		if snap.Items[i].Outcome != outcome.Created {
			t.Errorf("item %d outcome = %q, want created", i, snap.Items[i].Outcome)
		}
	}
}

func TestContribRescindReadsRescindedOnceThePetitionIsSent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failErr error
		want    string
	}{
		{"sent", nil, ptr.OutcomeRescinded},
		{"failed", errors.New("connection refused"), ptr.OutcomeApplied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newContribStore(t)
			add, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: hashN(0xaa)})
			claimed, _ := store.ClaimIDs([]int64{add})
			logID, err := store.Complete(claimed[0])
			if err != nil {
				t.Fatal(err)
			}
			sender := newContribSender(store)
			sender.raws = map[string]bool{strings.Repeat("aa", 32) + "|a tag": true}
			if tc.failErr != nil {
				sender.failFrom, sender.failErr = 0, tc.failErr
			}
			petition, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingPetition, Tag: "a tag", Hash: hashN(0xaa), Reason: "r"})
			q := contribQueue(t, sender)

			waitJob(t, q, q.EnqueueContrib([]int64{petition}, false))

			row, err := store.LogRow(logID)
			if err != nil || row.Outcome != tc.want {
				t.Errorf("ledger row = %+v, %v, want %s", row, err, tc.want)
			}
		})
	}
}

func TestContribSendLedgerFailureKeepsTheRowRetryable(t *testing.T) {
	dir := t.TempDir()
	store, err := ptr.OpenContribStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	second, err := sql.Open("sqlite", filepath.Join(dir, "contrib.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	sender := newContribSender(store)
	sender.onPost = func() {
		if _, err := second.Exec(`DROP TABLE contrib_log`); err != nil {
			t.Error(err)
		}
	}
	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: hashN(0xaa)})
	q := contribQueue(t, sender)

	waitJob(t, q, q.EnqueueContrib([]int64{id}, false))

	unsent, err := store.Unsent()
	if err != nil {
		t.Fatal(err)
	}
	if len(unsent) != 1 || unsent[0].Status != "failed" || !strings.Contains(unsent[0].Error, "not recorded") {
		t.Errorf("unsent = %+v, want the row back as failed, not stranded in sending", unsent)
	}
}

func TestContribSendChunking(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	var ids []int64
	for i := 0; i < 150; i++ {
		h := append(hashN(0x01)[:30], byte(i), byte(i>>8))
		id, dup, err := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "bulk tag", Hash: h})
		if err != nil || dup {
			t.Fatalf("stage %d: dup=%v err=%v", i, dup, err)
		}
		ids = append(ids, id)
	}
	for _, pair := range [][2]string{{"a", "b"}, {"c", "d"}, {"e", "f"}} {
		id, _, err := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: pair[0], Tag2: pair[1], Reason: "r"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	q := contribQueue(t, sender)
	snap := waitJob(t, q, q.EnqueueContrib(ids, false))
	if snap.Status != queue.JobSucceeded {
		t.Fatalf("job = %s (%s)", snap.Status, snap.Error)
	}
	if len(sender.posts) != 2 {
		t.Fatalf("posts = %d, want 2 (153 row-equivalents)", len(sender.posts))
	}
}

func TestContribSendPartialFailure(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	ids := stageBulk(t, store, 0x01, 120)
	sender.failFrom = 1
	sender.failErr = &ptr.ServerError{Status: 509, Message: "no bandwidth for you right now"}

	q := contribQueue(t, sender)
	snap := waitJob(t, q, q.EnqueueContrib(ids, false))
	if snap.Status != queue.JobPartial {
		t.Fatalf("job = %s, want partial", snap.Status)
	}
	logRows, _ := store.LogPage(200, 0)
	if len(logRows) != 100 {
		t.Errorf("committed = %d, want the first chunk's 100", len(logRows))
	}
	unsent, failed, _ := store.Counts()
	if unsent != 20 || failed != 20 {
		t.Errorf("counts = %d/%d, want 20 failed leftovers", unsent, failed)
	}
	if !strings.Contains(snap.Note, "20 failed, kept for retry") {
		t.Errorf("note = %q", snap.Note)
	}
	for _, it := range mustUnsent(t, store) {
		if !strings.Contains(it.Error, "no bandwidth") {
			t.Errorf("leftover error = %q, want the server text kept", it.Error)
		}
	}

	sender.failFrom = -1
	snap = waitJob(t, q, q.EnqueueContrib(nil, true))
	if snap.Status != queue.JobSucceeded {
		t.Fatalf("retry job = %s (%s)", snap.Status, snap.Error)
	}
	if unsent, _, _ := store.Counts(); unsent != 0 {
		t.Errorf("unsent after retry = %d, want 0", unsent)
	}
}

func mustUnsent(t *testing.T, store *ptr.ContribStore) []ptr.ContribItem {
	t.Helper()
	items, err := store.Unsent()
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestContribSendRefusals(t *testing.T) {
	store := newContribStore(t)
	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "x", Hash: hashN(0xaa)})

	noKey := newContribSender(store)
	noKey.noKey = true
	q := contribQueue(t, noKey)
	snap := waitJob(t, q, q.EnqueueContrib([]int64{id}, false))
	if snap.Status != queue.JobFailed || snap.ErrorCode != outcome.ErrCodePTRAccountRequired {
		t.Errorf("no key = %s / %s", snap.Status, snap.ErrorCode)
	}
	if _, failed, _ := store.Counts(); failed != 1 {
		t.Errorf("failed = %d, want the claimed row kept", failed)
	}

	banned := newContribSender(store)
	banned.banned, banned.banMsg = true, "junk"
	q2 := contribQueue(t, banned)
	snap = waitJob(t, q2, q2.EnqueueContrib(nil, true))
	if snap.Status != queue.JobFailed || snap.ErrorCode != outcome.ErrCodePTRBanned {
		t.Errorf("banned = %s / %s", snap.Status, snap.ErrorCode)
	}
	if !strings.Contains(snap.Error, "junk") {
		t.Errorf("error = %q, want the janitor text", snap.Error)
	}

	for _, tc := range []struct {
		err  error
		code string
	}{
		{&ptr.ServerError{Status: 503, Message: "down"}, outcome.ErrCodePTRUnavailable},
		{&ptr.ServerError{Status: 403, Message: "unknown key"}, outcome.ErrCodePTRAccountRequired},
	} {
		sender := newContribSender(store)
		sender.accErr = tc.err
		q := contribQueue(t, sender)
		snap = waitJob(t, q, q.EnqueueContrib(nil, true))
		if snap.Status != queue.JobFailed || snap.ErrorCode != tc.code {
			t.Errorf("account read %v = %s / %s, want %s", tc.err, snap.Status, snap.ErrorCode, tc.code)
		}
	}
	if _, failed, _ := store.Counts(); failed != 1 {
		t.Errorf("failed = %d, want the row kept for retry", failed)
	}
}

func TestContribSendFailsAKindTheAccountCannotSend(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	sender.perms = map[int]int{1: 0}
	add, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "denied tag", Hash: hashN(0xaa)})
	sib, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "bad", Tag2: "good", Reason: "r"})
	q := contribQueue(t, sender)

	snap := waitJob(t, q, q.EnqueueContrib([]int64{add, sib}, false))

	if snap.Status != queue.JobPartial {
		t.Errorf("status = %s, want partial", snap.Status)
	}
	if !strings.Contains(snap.Note, "1 failed") {
		t.Errorf("note = %q, want the denied row counted", snap.Note)
	}
	if len(sender.posts) != 1 {
		t.Fatalf("posts = %d, want the sibling posted", len(sender.posts))
	}
	unsent, err := store.Unsent()
	if err != nil || len(unsent) != 1 || unsent[0].ID != add || unsent[0].Status != "failed" {
		t.Errorf("unsent = %+v, %v, want the add kept as failed", unsent, err)
	}
}

func TestContribSendAllDeniedFailsTheJob(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	sender.perms = map[int]int{3: 1}
	add, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "denied tag", Hash: hashN(0xaa)})
	q := contribQueue(t, sender)

	snap := waitJob(t, q, q.EnqueueContrib([]int64{add}, false))

	if snap.Status != queue.JobFailed || snap.ErrorCode != outcome.ErrCodePTRAccountRequired {
		t.Errorf("status = %s code = %s, want failed ptr_account_required", snap.Status, snap.ErrorCode)
	}
	if len(sender.posts) != 0 {
		t.Errorf("posts = %d, want none", len(sender.posts))
	}
	if unsent, err := store.Unsent(); err != nil || len(unsent) != 1 || unsent[0].Status != "failed" {
		t.Errorf("unsent = %+v, %v, want the add kept as failed", unsent, err)
	}
}

func TestContribSendDrops(t *testing.T) {
	store := newContribStore(t)
	knownID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "already there", Hash: hashN(0xaa)})
	filteredID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "booru:junk", Hash: hashN(0xaa)})
	keptID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "fresh tag", Hash: hashN(0xaa)})
	sender := newContribSender(store)
	sender.ideals = map[string]bool{strings.Repeat("aa", 32) + "|already there": true}
	sender.filter = ptr.NewTagFilter(map[string]int{"booru:": 1})

	q := contribQueue(t, sender)
	snap := waitJob(t, q, q.EnqueueContrib([]int64{knownID, filteredID, keptID}, false))
	if snap.Status != queue.JobSucceeded {
		t.Fatalf("job = %s (%s)", snap.Status, snap.Error)
	}
	if !strings.Contains(snap.Note, "dropped 1 (already on the PTR), 1 (server tag filter)") {
		t.Errorf("note = %q", snap.Note)
	}
	logRows, _ := store.LogPage(10, 0)
	if len(logRows) != 1 || logRows[0].Tag != "fresh tag" {
		t.Errorf("ledger = %+v, want only the fresh tag", logRows)
	}
	if unsent, _, _ := store.Counts(); unsent != 0 {
		t.Errorf("dropped rows still unsent: %d", unsent)
	}
}

func TestContribRescindOfAddNotYetSyncedIsSent(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	sender.covered = time.Now().Add(-time.Hour).Unix()
	q := contribQueue(t, sender)

	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "wrong tag", Hash: hashN(0xaa)})
	if snap := waitJob(t, q, q.EnqueueContrib([]int64{addID}, false)); snap.Status != queue.JobSucceeded {
		t.Fatalf("add job = %s (%s)", snap.Status, snap.Error)
	}
	petitionID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingPetition, Tag: "wrong tag", Hash: hashN(0xaa), Reason: "rescinding my own earlier add"})
	snap := waitJob(t, q, q.EnqueueContrib([]int64{petitionID}, false))
	if snap.Status != queue.JobSucceeded {
		t.Fatalf("rescind job = %s (%s)", snap.Status, snap.Error)
	}
	if !strings.Contains(snap.Note, "committed 0 tag adds, 1 removal petitions") {
		t.Errorf("note = %q, want the petition sent", snap.Note)
	}
	if len(sender.posts) != 2 || sender.posts[1][0].Action != ptr.ActionPetition {
		t.Fatalf("posts = %+v, want the add then its petition", sender.posts)
	}
}

func TestContribCrashResetNoDuplicates(t *testing.T) {
	dir := t.TempDir()
	store, err := ptr.OpenContribStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "a", Tag2: "b", Reason: "r"})
	if _, err := store.ClaimIDs([]int64{id}); err != nil {
		t.Fatal(err)
	}

	_ = store.Close()

	reopened, err := ptr.OpenContribStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	sender := newContribSender(reopened)
	q := contribQueue(t, sender)
	snap := waitJob(t, q, q.EnqueueContrib(nil, true))
	if snap.Status != queue.JobSucceeded {
		t.Fatalf("retry after crash = %s (%s)", snap.Status, snap.Error)
	}
	logRows, _ := reopened.LogPage(10, 0)
	if len(logRows) != 1 {
		t.Errorf("ledger rows = %d, want exactly 1", len(logRows))
	}
}

func TestContribSendCancelledMidSleepFailsRemainder(t *testing.T) {
	store := newContribStore(t)
	sender := newContribSender(store)
	ids := stageBulk(t, store, 0x02, 120)

	q := contribQueueSleep(t, sender, 1.0)
	jobID := q.EnqueueContrib(ids, false)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if rows, _ := store.LogPage(200, 0); len(rows) >= 100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first chunk never committed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := q.CancelSeries(jobID); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(3 * time.Second)
	for {
		unsent, failed, _ := store.Counts()
		var sending int
		for _, it := range mustUnsent(t, store) {
			if it.Status == "sending" {
				sending++
			}
		}
		if sending == 0 && unsent == 20 && failed == 20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remainder not failed: unsent=%d failed=%d sending=%d", unsent, failed, sending)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestContribSendRefusedWhileSyncing(t *testing.T) {
	store := newContribStore(t)
	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "x", Hash: hashN(0xaa)})
	sender := newContribSender(store)
	sender.notReady = true
	q := contribQueue(t, sender)
	snap := waitJob(t, q, q.EnqueueContrib([]int64{id}, false))
	if snap.Status != queue.JobFailed || snap.ErrorCode != outcome.ErrCodePTRSyncing {
		t.Errorf("job = %s / %s, want failed ptr_syncing", snap.Status, snap.ErrorCode)
	}
	if len(sender.posts) != 0 {
		t.Errorf("posted %d chunks while syncing, want 0", len(sender.posts))
	}
	if _, failed, _ := store.Counts(); failed != 1 {
		t.Errorf("failed = %d, want the claimed row kept", failed)
	}
}
