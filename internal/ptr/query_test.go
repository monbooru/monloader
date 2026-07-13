package ptr

import (
	"reflect"
	"sort"
	"testing"
)

// seedGraph loads a small fixture: two files, a sibling chain, and a parent.
func seedGraph(t *testing.T) *Store {
	s := openTestStore(t)
	defs := &Update{Definitions: &Definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: h32("bb")},
		Tags: map[uint64]string{
			10: "blue eyes", // an alias...
			11: "blue_eyes", // ...of this ideal
			12: "character:reimu",
			20: "eyes", // parent of blue_eyes
			30: "loop_a",
			31: "loop_b",
		},
	}}
	content := &Update{Content: newContentValue(func(c *Content) {
		c.MappingsAdd = []Mapping{
			{TagID: 10, HashIDs: []uint64{1}}, // file 1 tagged with the alias "blue eyes"
			{TagID: 12, HashIDs: []uint64{1}},
		}
		c.SiblingsAdd = []Pair{{A: 10, B: 11}} // blue eyes -> blue_eyes
		c.ParentsAdd = []Pair{{A: 11, B: 20}}  // blue_eyes implies eyes
	})}
	// a sibling cycle: loop_a <-> loop_b, to exercise the cycle guard
	cyc := &Update{Content: newContentValue(func(c *Content) {
		c.SiblingsAdd = []Pair{{A: 30, B: 31}, {A: 31, B: 30}}
	})}
	if _, err := s.Replay(0, 0, []*Update{defs, content, cyc}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return s
}

func TestTagsForHashResolvesSiblingsAndParents(t *testing.T) {
	s := seedGraph(t)
	tags, ok, err := s.TagsForHash(h32("aa"))
	if err != nil || !ok {
		t.Fatalf("TagsForHash: ok=%v err=%v", ok, err)
	}
	sort.Strings(tags)
	// "blue eyes" resolves to the ideal "blue_eyes", which implies "eyes";
	// "character:reimu" rides through unchanged.
	want := []string{"blue_eyes", "character:reimu", "eyes"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
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
	// loop_a <-> loop_b: resolution must terminate, not spin.
	got, err := s.resolveSibling(30)
	if err != nil {
		t.Fatalf("resolveSibling: %v", err)
	}
	if got != 30 && got != 31 {
		t.Errorf("cycle resolution = %d, want one of the loop members", got)
	}
}

// A query failure must surface, not silently answer with the unresolved alias.
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

func TestTagGraphAliasesAndImplications(t *testing.T) {
	s := seedGraph(t)
	info, err := s.TagGraph([]string{"blue eyes", "unknown_tag"})
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
	// The queried spelling is excluded from its own alias list.
	for _, a := range be.Aliases {
		if a == "blue eyes" {
			t.Error("the queried spelling should not appear in its own aliases")
		}
	}
	if info["unknown_tag"].Known {
		t.Error("unknown_tag should be reported unknown")
	}
}
