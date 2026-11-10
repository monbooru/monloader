package ptr

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func seedGraph(t *testing.T) *Store {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: h32("bb")},
		Tags: map[uint64]string{
			10: "blue eyes",
			11: "blue_eyes",
			12: "character:reimu",
			20: "eyes",
			30: "loop_a",
			31: "loop_b",
		},
	}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{
			{TagID: 10, HashIDs: []uint64{1}},
			{TagID: 12, HashIDs: []uint64{1}},
		}
		c.SiblingsAdd = []pair{{A: 10, B: 11}}
		c.ParentsAdd = []pair{{A: 11, B: 20}}
	})}

	cyc := &update{Content: newContentValue(func(c *contentRows) {
		c.SiblingsAdd = []pair{{A: 30, B: 31}, {A: 31, B: 30}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content, cyc))
	return s
}

func TestTagsForHashResolvesSiblingsAndParents(t *testing.T) {
	s := seedGraph(t)
	tags, ok, err := s.TagsForHash(h32("aa"))
	if err != nil || !ok {
		t.Fatalf("TagsForHash: ok=%v err=%v", ok, err)
	}
	sort.Strings(tags)
	want := []string{"blue_eyes", "character:reimu", "eyes"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

func TestImpliedTagsFollowAParentEdgeOnAnAlias(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{
		Hashes: map[uint64]string{1: h32("aa")},
		Tags:   map[uint64]string{50: "bad spelling", 51: "good", 52: "implied", 53: "implied further"},
	}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 50, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []pair{{A: 50, B: 51}}
		c.ParentsAdd = []pair{{A: 50, B: 52}, {A: 52, B: 53}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content))

	tags, ok, err := s.TagsForHash(h32("aa"))
	if err != nil || !ok {
		t.Fatalf("TagsForHash: ok=%v err=%v", ok, err)
	}
	sort.Strings(tags)
	if want := []string{"good", "implied", "implied further"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
	if has, err := s.HashHasIdeal(h32("aa"), "implied"); err != nil || !has {
		t.Errorf("HashHasIdeal(implied) = %v (err %v), want true, or a redundant mapping reaches the PTR", has, err)
	}
}

func TestTagsForHashUnknownHash(t *testing.T) {
	s := seedGraph(t)
	tags, ok, err := s.TagsForHash(h32("ff"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok || len(tags) != 0 {
		t.Errorf("unknown hash = %v ok=%v, want empty/false", tags, ok)
	}
}

func TestTagsForHashRejectsMalformedHash(t *testing.T) {
	s := seedGraph(t)
	if _, ok, _ := s.TagsForHash("nothex"); ok {
		t.Error("a malformed hash should not resolve")
	}
}

func TestResolveSiblingBreaksCycle(t *testing.T) {
	s := seedGraph(t)
	for _, start := range []uint64{30, 31} {
		got, err := s.resolveSibling(start)
		if err != nil {
			t.Fatalf("resolveSibling: %v", err)
		}
		if got != 30 {
			t.Errorf("resolving from %d = %d, want the loop's lowest id from either end", start, got)
		}
	}
}

func TestResolveSiblingSurfacesQueryErrors(t *testing.T) {
	s := seedGraph(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.resolveSibling(10); err == nil {
		t.Error("resolveSibling on a closed store should error")
	}
	if _, _, err := s.TagsForHash(h32("aa")); err == nil {
		t.Error("TagsForHash on a closed store should error")
	}
}

func seedSearch(t *testing.T) *Store {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{Tags: map[uint64]string{
		50: "character:samus aran",
		51: "character:samus_aran",
		52: "character:samus",
		53: "character:samus aran (metroid)",
		54: "character:samus fox",
		55: "copyright:metroid",
		56: "character:sam_us",
		57: "character:paranormal aran-holder",
	}}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.SiblingsAdd = []pair{{A: 51, B: 50}, {A: 52, B: 50}}
		c.ParentsAdd = []pair{{A: 50, B: 55}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content))
	return s
}

func searchIdeals(matches []TagMatch) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Ideal)
	}
	sort.Strings(out)
	return out
}

func searchTags(t *testing.T, s *Store, r TagRange) []TagMatch {
	t.Helper()
	got, _, err := s.SearchTags(context.Background(), []TagRange{r}, 50)
	if err != nil {
		t.Fatalf("SearchTags: %v", err)
	}
	return got
}

func TestSearchTagsFoldsAPrefixIntoClusters(t *testing.T) {
	s := seedSearch(t)
	got, truncated, err := s.SearchTags(context.Background(), []TagRange{
		{Prefix: "character:samus_aran"}, {Prefix: "character:samus aran"},
	}, 50)
	if err != nil || truncated {
		t.Fatalf("SearchTags: truncated=%v err=%v", truncated, err)
	}

	want := []string{"character:samus aran", "character:samus aran (metroid)"}
	if ideals := searchIdeals(got); !reflect.DeepEqual(ideals, want) {
		t.Fatalf("ideals = %v, want %v", ideals, want)
	}
	for _, m := range got {
		if m.Ideal != "character:samus aran" {
			continue
		}
		sort.Strings(m.Matched)
		if !reflect.DeepEqual(m.Matched, []string{"character:samus aran", "character:samus_aran"}) {
			t.Errorf("matched = %v, want both spellings on the one row", m.Matched)
		}

		if m.Aliases != 2 || m.Implications != 1 || m.ImpliedBy != 0 {
			t.Errorf("counts = %d/%d/%d, want 2/1/0", m.Aliases, m.Implications, m.ImpliedBy)
		}
	}
}

func TestSearchTagsContainsMatchesWordStartsOnly(t *testing.T) {
	s := seedSearch(t)
	got := searchTags(t, s, TagRange{Prefix: "character:", Contains: []string{"aran"}})
	want := []string{
		"character:paranormal aran-holder",
		"character:samus aran",
		"character:samus aran (metroid)",
	}
	if ideals := searchIdeals(got); !reflect.DeepEqual(ideals, want) {
		t.Errorf("ideals = %v, want %v", ideals, want)
	}
	got = searchTags(t, s, TagRange{Prefix: "character:", Contains: []string{"normal"}})
	if len(got) != 0 {
		t.Errorf("mid-word match = %v, want nothing", searchIdeals(got))
	}
}

func TestSearchTagsEscapesLikeWildcards(t *testing.T) {
	s := seedSearch(t)
	got := searchTags(t, s, TagRange{Prefix: "character:", Contains: []string{"sam_us"}})
	if len(got) != 1 || got[0].Ideal != "character:sam_us" {
		t.Errorf("got %+v, want only the literal sam_us", got)
	}
}

func TestSearchTagsReportsTruncation(t *testing.T) {
	s := seedSearch(t)
	got, truncated, err := s.SearchTags(context.Background(), []TagRange{{Prefix: "character:"}}, 2)
	if err != nil {
		t.Fatalf("SearchTags: %v", err)
	}
	if !truncated || len(got) > 2 {
		t.Errorf("got %d clusters truncated=%v, want the cap to bite", len(got), truncated)
	}
}

func TestPrefixEnd(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"character:", "character;"},
		{"a", "b"},
		{"a\xff", "b"},
		{"\xff\xff", ""},
	} {
		if got := prefixEnd(tc.in); got != tc.want {
			t.Errorf("prefixEnd(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTagGraphAliasesAndImplications(t *testing.T) {
	s := seedGraph(t)
	info, err := s.TagGraph(context.Background(), []string{"blue eyes", "unknown_tag"})
	if err != nil {
		t.Fatalf("TagGraph: %v", err)
	}
	be := info["blue eyes"]
	if !be.Known || be.Ideal != "blue_eyes" {
		t.Errorf("blue eyes info = %+v, want ideal blue_eyes", be)
	}
	if !reflect.DeepEqual(be.Implications, []string{"eyes"}) {
		t.Errorf("implications = %v, want [eyes]", be.Implications)
	}
	for _, a := range be.Aliases {
		if a == "blue eyes" {
			t.Error("the queried spelling should not appear in its own aliases")
		}
	}
	if info["unknown_tag"].Known {
		t.Error("unknown_tag should be reported unknown")
	}
}

func TestTagGraphImplicationsAreDeclaredEdges(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{Tags: map[uint64]string{
		40: "futanari", 41: "intersex", 42: "solo",
	}}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.ParentsAdd = []pair{{A: 40, B: 41}, {A: 41, B: 42}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content))

	info, err := s.TagGraph(context.Background(), []string{"futanari", "solo"})
	if err != nil {
		t.Fatalf("TagGraph: %v", err)
	}
	if got := info["futanari"].Implications; !reflect.DeepEqual(got, []string{"intersex"}) {
		t.Errorf("futanari implications = %v, want the declared edge [intersex]", got)
	}
	if got := info["solo"].ImpliedBy; !reflect.DeepEqual(got, []string{"intersex"}) {
		t.Errorf("solo implied_by = %v, want [intersex]", got)
	}
	covered, err := s.ParentEdgeCovered("futanari", "intersex")
	if err != nil || !covered {
		t.Errorf("ParentEdgeCovered(futanari, intersex) = %v (err %v), want true", covered, err)
	}
	if covered, err := s.ParentEdgeCovered("futanari", "solo"); err != nil || !covered {
		t.Errorf("ParentEdgeCovered(futanari, solo) = %v (err %v), want true through intersex", covered, err)
	}
	if covered, _ := s.ParentEdgeCovered("solo", "futanari"); covered {
		t.Error("the closure must not read as covered against the arrow")
	}
}

func TestTagGraphEdgeOnAnAliasedEndpoint(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{Tags: map[uint64]string{
		40: "futanari", 42: "solo", 43: "solo focus",
	}}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.SiblingsAdd = []pair{{A: 43, B: 42}}
		c.ParentsAdd = []pair{{A: 40, B: 43}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content))

	info, err := s.TagGraph(context.Background(), []string{"futanari", "solo"})
	if err != nil {
		t.Fatalf("TagGraph: %v", err)
	}
	if got := info["futanari"].Implications; !reflect.DeepEqual(got, []string{"solo"}) {
		t.Errorf("futanari implications = %v, want [solo]", got)
	}
	if got := info["solo"].ImpliedBy; !reflect.DeepEqual(got, []string{"futanari"}) {
		t.Errorf("solo implied_by = %v, want [futanari] - the edge sits on its alias", got)
	}
	covered, err := s.ParentEdgeCovered("futanari", "solo")
	if err != nil || !covered {
		t.Errorf("ParentEdgeCovered(futanari, solo) = %v (err %v), want true", covered, err)
	}
	if cur, _ := s.ParentCurrent("futanari", "solo focus"); !cur {
		t.Error("ParentCurrent should still find the pair as stored")
	}
}

func TestAChainedSpellingKeepsItsRelations(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{Tags: map[uint64]string{60: "a", 61: "b", 62: "c", 63: "p"}}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.SiblingsAdd = []pair{{A: 60, B: 61}, {A: 61, B: 62}}
		c.ParentsAdd = []pair{{A: 60, B: 63}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content))

	info, err := s.TagGraph(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := info["a"]; !reflect.DeepEqual(got.Implications, []string{"p"}) || !reflect.DeepEqual(got.Aliases, []string{"b"}) {
		t.Errorf("a: implications %v aliases %v, want [p] and [b]", got.Implications, got.Aliases)
	}
	if covered, err := s.ParentEdgeCovered("a", "p"); err != nil || !covered {
		t.Errorf("ParentEdgeCovered(a, p) = %v (err %v), want true", covered, err)
	}
}

func TestTagGraphImpliedBy(t *testing.T) {
	s := seedGraph(t)
	info, err := s.TagGraph(context.Background(), []string{"eyes", "blue eyes"})
	if err != nil {
		t.Fatalf("TagGraph: %v", err)
	}
	if got := info["eyes"].ImpliedBy; !reflect.DeepEqual(got, []string{"blue_eyes"}) {
		t.Errorf("implied_by = %v, want [blue_eyes]", got)
	}
	if got := info["blue eyes"].ImpliedBy; got != nil {
		t.Errorf("blue eyes implied_by = %v, want none", got)
	}
}

func TestGraphQueriesSeek(t *testing.T) {
	s := openTestStore(t)
	for _, q := range []string{
		`SELECT child_tag_id FROM parents WHERE parent_tag_id=? ORDER BY child_tag_id`,
		`SELECT parent_tag_id FROM parents WHERE child_tag_id=? ORDER BY parent_tag_id`,
		`SELECT bad_tag_id FROM siblings WHERE good_tag_id=?`,
	} {
		var id, parent, notused int
		var detail string
		if err := s.db.QueryRow("EXPLAIN QUERY PLAN "+q, 1).Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if !strings.Contains(detail, "SEARCH") {
			t.Errorf("%s\n  plan: %s", q, detail)
		}
	}
}

func TestTagGraphStopsWhenTheCallerGivesUp(t *testing.T) {
	s := seedGraph(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.TagGraph(ctx, []string{"blue eyes", "character:reimu"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the walk abandoned with context.Canceled", err)
	}
}
