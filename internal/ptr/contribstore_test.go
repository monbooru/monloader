package ptr

import (
	"bytes"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func openTestContribStore(t *testing.T) (*ContribStore, string) {
	t.Helper()
	dir := t.TempDir()
	return openContribStoreAt(t, dir), dir
}

func openContribStoreAt(t *testing.T, dir string) *ContribStore {
	t.Helper()
	s, err := OpenContribStore(dir)
	if err != nil {
		t.Fatalf("OpenContribStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func commitStaged(t *testing.T, s *ContribStore, id int64) int64 {
	t.Helper()
	items, err := s.ClaimIDs([]int64{id})
	if err != nil || len(items) != 1 {
		t.Fatalf("claiming %d = %v, %v", id, items, err)
	}
	logID, err := s.Complete(items[0])
	if err != nil {
		t.Fatal(err)
	}
	return logID
}

func commitItem(t *testing.T, s *ContribStore, it ContribItem) int64 {
	t.Helper()
	id, _, err := s.Stage(it)
	if err != nil {
		t.Fatal(err)
	}
	return commitStaged(t, s, id)
}

func TestContribStageDedup(t *testing.T) {
	s, _ := openTestContribStore(t)
	h := bytes.Repeat([]byte{0xaa}, 32)
	id, dup, err := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "blonde hair", Hash: h})
	if err != nil || dup || id == 0 {
		t.Fatalf("first stage = %d dup=%v err=%v", id, dup, err)
	}
	_, dup, err = s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "blonde hair", Hash: h})
	if err != nil || !dup {
		t.Errorf("re-stage dup=%v err=%v, want duplicate", dup, err)
	}
	if _, dup, _ := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "blonde hair", Hash: bytes.Repeat([]byte{0xbb}, 32)}); dup {
		t.Error("distinct hash reported duplicate")
	}

	if _, dup, _ := s.Stage(ContribItem{Kind: ContribSiblingPetition, Tag: "bad tag", Tag2: "good tag"}); dup {
		t.Error("first pair petition reported duplicate")
	}
	if _, dup, _ := s.Stage(ContribItem{Kind: ContribSiblingPetition, Tag: "bad tag", Tag2: "good tag"}); !dup {
		t.Error("re-staged identical pair petition not deduped")
	}
	if _, dup, _ := s.Stage(ContribItem{Kind: ContribSiblingPetition, Tag: "bad tag", Tag2: "other good"}); dup {
		t.Error("distinct pair reported duplicate")
	}
}

func TestContribStagedID(t *testing.T) {
	s, _ := openTestContribStore(t)
	h := bytes.Repeat([]byte{0xcc}, 32)
	addID, _, _ := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "blonde hair", Hash: h})
	pairID, _, _ := s.Stage(ContribItem{Kind: ContribSiblingPetition, Tag: "bad tag", Tag2: "good tag"})

	if got, ok, err := s.StagedID(ContribMappingAdd, "blonde hair", "", h); err != nil || !ok || got != addID {
		t.Errorf("StagedID(mapping add) = %d ok=%v err=%v, want %d", got, ok, err, addID)
	}
	if got, ok, err := s.StagedID(ContribSiblingPetition, "bad tag", "good tag", nil); err != nil || !ok || got != pairID {
		t.Errorf("StagedID(pair petition) = %d ok=%v err=%v, want %d", got, ok, err, pairID)
	}
	if _, ok, err := s.StagedID(ContribSiblingPetition, "bad tag", "absent", nil); err != nil || ok {
		t.Errorf("StagedID(unstaged pair) ok=%v err=%v, want not found", ok, err)
	}
}

func TestContribClaimIsolation(t *testing.T) {
	s, _ := openTestContribStore(t)
	a, _, _ := s.Stage(ContribItem{Kind: ContribSibling, Tag: "a", Tag2: "b", Reason: "misspelling"})
	b, _, _ := s.Stage(ContribItem{Kind: ContribParent, Tag: "c", Tag2: "d", Reason: "canonical"})
	first, err := s.ClaimIDs([]int64{a})
	if err != nil || len(first) != 1 || first[0].ID != a {
		t.Fatalf("claim a = %v err=%v", first, err)
	}

	again, _ := s.ClaimIDs([]int64{a})
	if len(again) != 0 {
		t.Errorf("re-claim = %v, want empty", again)
	}
	backlog, _ := s.ClaimBacklog()
	if len(backlog) != 1 || backlog[0].ID != b {
		t.Errorf("backlog claim = %v, want only b", backlog)
	}
}

func TestContribCompleteAndBootReset(t *testing.T) {
	s, dir := openTestContribStore(t)
	h := bytes.Repeat([]byte{0xaa}, 32)
	addID, _, _ := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "blonde hair", Hash: h})
	sibID, _, _ := s.Stage(ContribItem{Kind: ContribSibling, Tag: "blond hair", Tag2: "blonde hair", Reason: "misspelling"})
	items, _ := s.ClaimIDs([]int64{addID, sibID})
	if len(items) != 2 {
		t.Fatalf("claimed %d, want 2", len(items))
	}

	if _, err := s.Complete(items[0]); err != nil {
		t.Fatal(err)
	}
	if mid, _ := s.Unsent(); len(mid) != 0 {
		t.Errorf("unsent during a send = %+v, want none", mid)
	}
	logRows, _ := s.LogPage(10, 0)
	if len(logRows) != 1 || logRows[0].Kind != ContribMappingAdd || logRows[0].Outcome != OutcomeApplied {
		t.Fatalf("log = %+v, want one applied mapping add", logRows)
	}
	_ = s.Close()

	reopened := openContribStoreAt(t, dir)
	unsent, _ := reopened.Unsent()
	if len(unsent) != 1 || unsent[0].ID != sibID || unsent[0].Status != "staged" {
		t.Errorf("after reopen = %+v, want the sibling reset to staged", unsent)
	}
}

func TestContribFailRetryRescind(t *testing.T) {
	s, _ := openTestContribStore(t)
	id, _, _ := s.Stage(ContribItem{Kind: ContribParentPetition, Tag: "a", Tag2: "b", Reason: "not implied"})
	items, _ := s.ClaimIDs([]int64{id})
	if err := s.FailItem(items[0].ID, "bandwidth refusal"); err != nil {
		t.Fatal(err)
	}
	unsent, failed, _ := s.Counts()
	if unsent != 1 || failed != 1 {
		t.Errorf("counts = %d/%d, want 1/1", unsent, failed)
	}
	back, _ := s.ClaimBacklog()
	if len(back) != 1 {
		t.Fatalf("backlog = %v", back)
	}
	_ = s.FailItem(id, "again")
	if ok, _ := s.Rescind(id); !ok {
		t.Error("rescind of a failed row refused")
	}
	if unsent, _, _ := s.Counts(); unsent != 0 {
		t.Errorf("unsent after rescind = %d, want 0", unsent)
	}
}

func TestContribLogPrune(t *testing.T) {
	s, dir := openTestContribStore(t)
	complete := func(tag string) int64 {
		t.Helper()
		return commitItem(t, s, ContribItem{Kind: ContribSibling, Tag: tag, Tag2: "g", Reason: "r"})
	}

	oldSettled := complete("old settled")
	if err := s.SetOutcome(oldSettled, OutcomeApplied); err != nil {
		t.Fatal(err)
	}
	oldPending := complete("old pending")
	recent := complete("recent settled")
	if err := s.SetOutcome(recent, OutcomeApplied); err != nil {
		t.Fatal(err)
	}
	beyond := time.Now().AddDate(0, 0, -contribLogKeepDays-1).Unix()
	for _, id := range []int64{oldSettled, oldPending} {
		if _, err := s.db.Exec(`UPDATE contrib_log SET committed_at = ? WHERE id = ?`, beyond, id); err != nil {
			t.Fatal(err)
		}
	}

	if row, err := s.LogRow(oldSettled); err != nil || row == nil {
		t.Fatalf("a commit pruned the ledger; only a send or an open should (row=%v err=%v)", row, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openContribStoreAt(t, dir)
	if row, err := s.LogRow(oldSettled); err != nil || row != nil {
		t.Errorf("aged settled row survived the prune (row=%v err=%v)", row, err)
	}
	if row, err := s.LogRow(oldPending); err != nil || row == nil {
		t.Errorf("aged pending row pruned (row=%v err=%v); the dedup guard is gone", row, err)
	}
	if row, err := s.LogRow(recent); err != nil || row == nil {
		t.Errorf("in-window row %d pruned (row=%v err=%v)", recent, row, err)
	}
}

func TestContribLogQueriesUseAnIndex(t *testing.T) {
	s, _ := openTestContribStore(t)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND hash = ? AND committed_at > ?`, []any{ContribMappingAdd, "t", []byte{1}, 0}},
		{`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND hash = ? AND outcome = ''`, []any{ContribMappingPetition, "t", []byte{1}}},
		{`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND tag2 = ? AND outcome = ''`, []any{ContribSibling, "t", "u"}},
		{`DELETE FROM contrib_log WHERE outcome != '' AND committed_at < ?`, []any{0}},
	} {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		joined := strings.Join(plan, "; ")
		if !strings.Contains(joined, "USING INDEX") && !strings.Contains(joined, "USING COVERING INDEX") {
			t.Errorf("%s scans the table: %s", q.sql, joined)
		}
	}
}

func TestContribLogDaily(t *testing.T) {
	s, _ := openTestContribStore(t)
	complete := func(tag string) int64 {
		t.Helper()
		return commitItem(t, s, ContribItem{Kind: ContribSibling, Tag: tag, Tag2: "g", Reason: "r"})
	}
	day := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	backdate := map[int64]time.Time{
		complete("a"): day,
		complete("b"): day.Add(2 * time.Hour),
		complete("c"): day.AddDate(0, 0, 1),
	}
	for id, at := range backdate {
		if _, err := s.db.Exec(`UPDATE contrib_log SET committed_at = ? WHERE id = ?`, at.Unix(), id); err != nil {
			t.Fatal(err)
		}
	}
	daily, err := s.LogDaily(0)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"2026-03-10": 2, "2026-03-11": 1}; !reflect.DeepEqual(daily, want) {
		t.Errorf("LogDaily = %v, want %v", daily, want)
	}
	daily, _ = s.LogDaily(day.AddDate(0, 0, 1).Unix())
	if want := map[string]int{"2026-03-11": 1}; !reflect.DeepEqual(daily, want) {
		t.Errorf("bounded LogDaily = %v, want %v", daily, want)
	}
}

func TestContribPendingLog(t *testing.T) {
	s, _ := openTestContribStore(t)
	id, _, _ := s.Stage(ContribItem{Kind: ContribSibling, Tag: "blond hair", Tag2: "blonde hair", Reason: "misspelling"})
	logID := commitStaged(t, s, id)
	if ok, _ := s.PendingLog(ContribSibling, "blond hair", "blonde hair"); !ok {
		t.Error("committed suggestion not pending in the log")
	}
	_ = s.SetOutcome(logID, OutcomeApproved)
	if ok, _ := s.PendingLog(ContribSibling, "blond hair", "blonde hair"); ok {
		t.Error("approved suggestion still reads pending")
	}
}

func TestContribPendingMappingPetition(t *testing.T) {
	s, _ := openTestContribStore(t)
	h := bytes.Repeat([]byte{0xcc}, 32)
	if ok, _ := s.PendingMappingPetition("hat", h); ok {
		t.Error("fresh store reports a pending petition")
	}
	id, _, _ := s.Stage(ContribItem{Kind: ContribMappingPetition, Tag: "hat", Hash: h, Reason: "not in the image"})
	if ok, _ := s.PendingMappingPetition("hat", h); !ok {
		t.Error("staged petition not reported")
	}
	commitStaged(t, s, id)
	if ok, _ := s.PendingMappingPetition("hat", h); !ok {
		t.Error("committed petition awaiting janitors not reported")
	}
}

func TestContribMappingAddCommittedSince(t *testing.T) {
	s, _ := openTestContribStore(t)
	h := bytes.Repeat([]byte{0xdd}, 32)
	if ok, _ := s.MappingAddCommittedSince("hat", h, 0); ok {
		t.Error("fresh store reports a committed add")
	}
	id, _, _ := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "hat", Hash: h})
	if ok, _ := s.MappingAddCommittedSince("hat", h, 0); ok {
		t.Error("unsent add reported as committed")
	}
	commitStaged(t, s, id)
	if ok, _ := s.MappingAddCommittedSince("hat", h, 0); !ok {
		t.Error("committed add not reported")
	}
	if ok, _ := s.MappingAddCommittedSince("hat", h, time.Now().Unix()+10); ok {
		t.Error("add older than the coverage timestamp still reported")
	}
}

func seedDiffIndex(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{
		Hashes: map[uint64]string{1: h32("aa")},
		Tags: map[uint64]string{
			1: "blond hair", 2: "blonde hair", 3: "hat",
			4: "series:touhou", 5: "miko", 6: "shrine maiden",
			7: "headwear",
		},
	}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 1, HashIDs: []uint64{1}}, {TagID: 3, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []pair{{A: 1, B: 2}}
		c.ParentsAdd = []pair{{A: 5, B: 6}, {A: 3, B: 7}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content))
	return s
}

func TestDiffHashHasIdeal(t *testing.T) {
	s := seedDiffIndex(t)
	cases := []struct {
		tag  string
		want bool
	}{
		{"blond hair", true},

		{"blonde hair", true},
		{"hat", true},

		{"headwear", true},
		{"series:touhou", false},
		{"shrine maiden", false},
		{"unknown tag", false},
	}
	for _, tc := range cases {
		got, err := s.HashHasIdeal(h32("aa"), tc.tag)
		if err != nil || got != tc.want {
			t.Errorf("HashHasIdeal(%q) = %v err=%v, want %v", tc.tag, got, err, tc.want)
		}
	}
	if got, _ := s.HashHasIdeal(h32("ff"), "hat"); got {
		t.Error("unknown hash reported a mapping")
	}
}

func TestDiffHashHasIdeals(t *testing.T) {
	s := seedDiffIndex(t)
	tags := []string{"blond hair", "blonde hair", "hat", "headwear",
		"series:touhou", "shrine maiden", "unknown tag"}
	has, err := s.HashHasIdeals(h32("aa"), tags)
	if err != nil {
		t.Fatalf("HashHasIdeals: %v", err)
	}
	for _, tag := range tags {
		want, werr := s.HashHasIdeal(h32("aa"), tag)
		if werr != nil || has[tag] != want {
			t.Errorf("HashHasIdeals[%q] = %v, want %v (err=%v)", tag, has[tag], want, werr)
		}
	}
	if _, ok := has["unknown tag"]; ok {
		t.Error("a spelling the index does not know should be absent from the answer")
	}
	if got, err := s.HashHasIdeals(h32("ff"), tags); err != nil || len(got) != 0 {
		t.Errorf("unknown hash = %v err=%v, want no answers", got, err)
	}
}

func TestDiffIdealTag(t *testing.T) {
	s := seedDiffIndex(t)
	if got, ok, err := s.IdealTag("blond hair"); err != nil || !ok || got != "blonde hair" {
		t.Errorf("IdealTag(blond hair) = %q ok=%v err=%v, want blonde hair", got, ok, err)
	}
	if got, ok, _ := s.IdealTag("hat"); !ok || got != "hat" {
		t.Errorf("IdealTag(hat) = %q ok=%v, want hat", got, ok)
	}
	if _, ok, _ := s.IdealTag("unknown tag"); ok {
		t.Error("unknown spelling reported an ideal")
	}
}

func TestDiffHashHasRaw(t *testing.T) {
	s := seedDiffIndex(t)
	if got, _ := s.HashHasRaw(h32("aa"), "blond hair"); !got {
		t.Error("raw mapping not reported current")
	}
	if got, _ := s.HashHasRaw(h32("aa"), "blonde hair"); got {
		t.Error("ideal spelling reported as a raw mapping")
	}
}

func TestDiffPairChecks(t *testing.T) {
	s := seedDiffIndex(t)
	if ok, _ := s.SiblingCurrent("blond hair", "blonde hair"); !ok {
		t.Error("current sibling not reported")
	}
	if ok, _ := s.SiblingCurrent("blonde hair", "blond hair"); ok {
		t.Error("reverse sibling reported current")
	}
	if ok, _ := s.ParentCurrent("miko", "shrine maiden"); !ok {
		t.Error("current parent not reported")
	}
	if ok, _ := s.ParentCurrent("shrine maiden", "miko"); ok {
		t.Error("reverse parent reported current")
	}
}

func TestDiffRawTagsForHash(t *testing.T) {
	s := seedDiffIndex(t)
	got, err := s.RawTagsForHash(h32("aa"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"blond hair", "hat"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RawTagsForHash = %v, want %v (raw rows only, no resolution)", got, want)
	}
}

func TestRemoveContribStore(t *testing.T) {
	s, dir := openTestContribStore(t)
	_, _, _ = s.Stage(ContribItem{Kind: ContribSibling, Tag: "a", Tag2: "b", Reason: "r"})
	_ = s.Close()
	if err := removeContribStore(dir); err != nil {
		t.Fatal(err)
	}
	reopened := openContribStoreAt(t, dir)
	if unsent, _, _ := reopened.Counts(); unsent != 0 {
		t.Errorf("store not removed: %d rows survive", unsent)
	}
}
