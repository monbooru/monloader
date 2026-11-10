package ptr

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func contribClient(t *testing.T, f *fakeRepo) *client {
	t.Helper()
	key := strings.Repeat("ab", 32)
	f.grantKey(key)
	return newClient(config.PTRConfig{Address: f.URL(), AccessKey: key})
}

func TestContribClientTransport(t *testing.T) {
	f := newFakeRepo(t)
	c := contribClient(t, f)

	f.locked(func() { f.tagFilterRules = []any{[]any{"booru:", 1}} })
	filter, err := c.FetchTagFilter(context.Background())
	if err != nil {
		t.Fatalf("FetchTagFilter: %v", err)
	}
	if blocked, rule := filter.Blocks("booru:danbooru"); !blocked || rule == "" {
		t.Errorf("Blocks = %v %q, want the namespace rule to block", blocked, rule)
	}

	f.locked(func() { f.fail509 = 1 })
	err = c.PostUpdate(context.Background(), specTestUpdate(t))
	var se *ServerError
	if !errors.As(err, &se) || !se.Retryable() {
		t.Fatalf("509 = %v, want a retryable ServerError", err)
	}
	if err := c.PostUpdate(context.Background(), specTestUpdate(t)); err != nil {
		t.Fatalf("PostUpdate after the 509: %v", err)
	}

	f.locked(func() { f.banned, f.banReason = true, "sent junk" })
	err = c.PostUpdate(context.Background(), specTestUpdate(t))
	if !errors.As(err, &se) || se.Retryable() || !strings.Contains(se.Message, "sent junk") {
		t.Fatalf("banned = %v, want a permanent refusal naming the ban", err)
	}
	if msg := se.Error(); !strings.Contains(msg, "403") || !strings.Contains(msg, "sent junk") {
		t.Errorf("ServerError.Error() = %q, want the status and the reason", msg)
	}

	other := newClient(config.PTRConfig{Address: f.URL(), AccessKey: strings.Repeat("cd", 32)})
	if _, err := other.FetchTagFilter(context.Background()); err == nil {
		t.Error("an unknown key should be refused")
	}
}

func TestEngineContribQueriesRefuseWhileDisabled(t *testing.T) {
	e := NewEngine(config.PTRConfig{Address: "http://x"})
	hash := h32("aa")
	if _, err := e.HashHasIdeal(hash, "t"); err == nil {
		t.Error("HashHasIdeal should refuse")
	}
	if _, err := e.HashHasIdeals(hash, []string{"t"}); err == nil {
		t.Error("HashHasIdeals should refuse")
	}
	if _, _, err := e.IdealTag("t"); err == nil {
		t.Error("IdealTag should refuse")
	}
	if _, err := e.HashHasRaw(hash, "t"); err == nil {
		t.Error("HashHasRaw should refuse")
	}
	if _, err := e.RawTagsForHash(hash); err == nil {
		t.Error("RawTagsForHash should refuse")
	}
	if _, err := e.SiblingCurrent("a", "b"); err == nil {
		t.Error("SiblingCurrent should refuse")
	}
	if _, err := e.ParentCurrent("a", "b"); err == nil {
		t.Error("ParentCurrent should refuse")
	}
	if _, err := e.ParentEdgeCovered("a", "b"); err == nil {
		t.Error("ParentEdgeCovered should refuse")
	}
	if e.Contrib() != nil {
		t.Error("a disabled engine holds no contribution store")
	}
	if e.Status().State == StateSyncing || e.CaughtUp() || !e.Provisional() {
		t.Error("a disabled engine is neither syncing nor caught up, and answers provisionally")
	}
}

func TestEngineContribQueriesReadTheIndex(t *testing.T) {
	fr := newFixtureRepo(t)
	seedFixtureStream(t, fr)
	e := startedEngine(t, fr, t.TempDir())
	waitState(t, e, StateReady)

	hash := h32("aa")
	if !e.CaughtUp() || e.Status().State == StateSyncing || e.Provisional() {
		t.Error("a caught-up engine should be ready and no longer provisional")
	}
	if ok, err := e.HashHasRaw(hash, "blue eyes"); err != nil || !ok {
		t.Errorf("HashHasRaw = %v %v, want the mapped tag", ok, err)
	}
	if ok, err := e.HashHasRaw(hash, "green eyes"); err != nil || ok {
		t.Errorf("HashHasRaw(unmapped) = %v %v, want false", ok, err)
	}
	raws, err := e.RawTagsForHash(hash)
	if err != nil || len(raws) == 0 {
		t.Errorf("RawTagsForHash = %v %v, want the hash's mappings", raws, err)
	}
	ideal, ok, err := e.IdealTag("blue_eyes")
	if err != nil {
		t.Fatalf("IdealTag: %v", err)
	}
	if ok && ideal == "" {
		t.Error("a resolved ideal must not be empty")
	}
	if _, err := e.HashHasIdeal(hash, "blue eyes"); err != nil {
		t.Errorf("HashHasIdeal: %v", err)
	}
	got, err := e.HashHasIdeals(hash, []string{"blue eyes", "green eyes"})
	if err != nil {
		t.Fatalf("HashHasIdeals: %v", err)
	}
	if !got["blue eyes"] {
		t.Errorf("HashHasIdeals = %v, want the mapped tag displayed", got)
	}
	if _, known := got["green eyes"]; known {
		t.Errorf("HashHasIdeals = %v, want an unknown tag left out", got)
	}
	if _, err := e.SiblingCurrent("blue_eyes", "blue eyes"); err != nil {
		t.Errorf("SiblingCurrent: %v", err)
	}
	if _, err := e.ParentCurrent("blue eyes", "eyes"); err != nil {
		t.Errorf("ParentCurrent: %v", err)
	}
	if _, err := e.ParentEdgeCovered("blue eyes", "eyes"); err != nil {
		t.Errorf("ParentEdgeCovered: %v", err)
	}
	if e.Contrib() == nil {
		t.Error("an enabled engine should hold a contribution store")
	}
}

func TestEngineTagFilterCache(t *testing.T) {
	f := newFakeRepo(t)
	key := strings.Repeat("ab", 32)
	f.grantKey(key)
	e := NewEngine(config.PTRConfig{Address: f.URL(), AccessKey: key})

	if e.TagFilterCached() != nil {
		t.Error("nothing is cached before a fetch")
	}
	f.locked(func() { f.tagFilterRules = []any{[]any{"booru:", 1}} })
	got, err := e.RefreshTagFilter(context.Background())
	if err != nil {
		t.Fatalf("RefreshTagFilter: %v", err)
	}
	if blocked, _ := got.Blocks("booru:danbooru"); !blocked {
		t.Error("the refreshed filter should carry the rule")
	}
	if e.TagFilterCached() != got {
		t.Error("a refresh should cache what it fetched")
	}
	e.SetTagFilterCache(nil)
	if e.TagFilterCached() != nil {
		t.Error("the cache should be replaceable")
	}
	if err := e.PostUpdate(context.Background(), specTestUpdate(t)); err != nil {
		t.Errorf("PostUpdate: %v", err)
	}
	acc, err := e.RefreshAccount(context.Background())
	if err != nil || acc == nil {
		t.Fatalf("RefreshAccount = %v, %v", acc, err)
	}
	f.locked(func() { f.banned, f.banReason = true, "sent junk" })
	if acc, err := e.RefreshAccount(context.Background()); err != nil || !acc.Banned {
		t.Errorf("RefreshAccount after a ban = %+v, %v; want banned", acc, err)
	}
}

func TestContribLabelsAndLinks(t *testing.T) {
	hash := bytes.Repeat([]byte{0xab}, 32)
	for _, tc := range []struct {
		kind, tag, tag2 string
		hash            []byte
		label, queue    string
	}{
		{ContribMappingAdd, "blue eyes", "", hash, "blue eyes @ abababab..", "tag blue eyes"},
		{ContribMappingPetition, "blue eyes", "", hash, "blue eyes @ abababab..", "remove tag blue eyes"},
		{ContribMappingAdd, "blue eyes", "", nil, "blue eyes", "tag blue eyes"},
		{ContribSibling, "blue_eyes", "blue eyes", nil, "blue_eyes -> blue eyes", "alias blue_eyes -> blue eyes"},
		{ContribSiblingPetition, "a", "b", nil, "a -> b", "remove alias a -> b"},
		{ContribParent, "a", "b", nil, "a => b", "implies a => b"},
		{ContribParentPetition, "a", "b", nil, "a => b", "remove implies a => b"},
	} {
		if got := ContribItemLabel(tc.kind, tc.tag, tc.tag2, tc.hash); got != tc.label {
			t.Errorf("ContribItemLabel(%s) = %q, want %q", tc.kind, got, tc.label)
		}
		if got := ContribQueueLabel(tc.kind, tc.tag, tc.tag2); got != tc.queue {
			t.Errorf("ContribQueueLabel(%s) = %q, want %q", tc.kind, got, tc.queue)
		}
	}
	if got := ContribQueueLabel("unknown", "t", ""); got != "t" {
		t.Errorf("an unknown kind should fall back to the tag, got %q", got)
	}

	base := "http://booru.example"
	if got := ContribItemLink(base, ContribMappingAdd, "t", hash); got != base+"/i/"+strings.Repeat("ab", 32) {
		t.Errorf("mapping link = %q", got)
	}
	if got := ContribItemLink(base, ContribMappingAdd, "t", nil); got != "" {
		t.Errorf("a hashless mapping has no link, got %q", got)
	}
	if got := ContribItemLink(base, ContribSibling, "creator:some one", nil); got != base+"/tags?q=some_one" {
		t.Errorf("pair link = %q, want the namespace stripped and spaces folded", got)
	}
	if got := ContribItemLink(base, ContribSibling, "", nil); got != "" {
		t.Errorf("an empty tag has no link, got %q", got)
	}
	if got := ContribItemLink("", ContribMappingAdd, "t", hash); got != "" {
		t.Errorf("no monbooru base means no link, got %q", got)
	}
}

func TestContribStoreQueriesAndDrops(t *testing.T) {
	s, _ := openTestContribStore(t)
	hash := bytes.Repeat([]byte{0xaa}, 32)

	if n, err := s.LogCount(); err != nil || n != 0 {
		t.Fatalf("LogCount on a fresh store = %d, %v", n, err)
	}
	id, _, err := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "blonde hair", Hash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if waiting, err := s.UnsentByKindTag(ContribMappingAdd, "blonde hair", hash); err != nil || !waiting {
		t.Errorf("UnsentByKindTag = %v, %v; want the staged row", waiting, err)
	}
	if waiting, err := s.UnsentByKindTag(ContribMappingAdd, "blue eyes", hash); err != nil || waiting {
		t.Errorf("UnsentByKindTag(other tag) = %v, %v; want false", waiting, err)
	}
	if err := s.Drop(id); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if waiting, _ := s.UnsentByKindTag(ContribMappingAdd, "blonde hair", hash); waiting {
		t.Error("a dropped row should stop counting as unsent")
	}

	a, _, _ := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "a", Hash: hash})
	if _, _, err := s.Stage(ContribItem{Kind: ContribMappingAdd, Tag: "b", Hash: hash}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailItem(a, "boom"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RescindAll()
	if err != nil || n != 2 {
		t.Errorf("RescindAll = %d, %v; want both rows", n, err)
	}
	if unsent, failed, _ := s.Counts(); unsent != 0 || failed != 0 {
		t.Errorf("counts after RescindAll = %d unsent / %d failed, want none", unsent, failed)
	}
}

func TestNewTagFilterBlocksLikeTheDecodedOne(t *testing.T) {
	f := NewTagFilter(map[string]int{"booru:": 1, "creator:allowed one": 0})
	if blocked, rule := f.Blocks("booru:danbooru"); !blocked || rule == "" {
		t.Errorf("Blocks(namespaced) = %v %q, want the rule to block", blocked, rule)
	}
	if blocked, _ := f.Blocks("creator:allowed one"); blocked {
		t.Error("an allowed tag must not be blocked")
	}
	if blocked, _ := f.Blocks("blonde hair"); blocked {
		t.Error("an unlisted tag must not be blocked")
	}
}

func TestC2SUpdateSize(t *testing.T) {
	u := &C2SUpdate{}
	if !u.Empty() || u.Len() != 0 {
		t.Fatalf("a fresh update = empty %v len %d", u.Empty(), u.Len())
	}
	u.AddMappings(ActionPend, "t", [][]byte{bytes.Repeat([]byte{1}, 32)}, "")
	u.AddSibling(ActionPend, "a", "b", "")
	if u.Empty() || u.Len() != 2 {
		t.Errorf("after two adds = empty %v len %d, want 2 entries", u.Empty(), u.Len())
	}
}

func (u *C2SUpdate) Len() int { return len(u.entries) }
