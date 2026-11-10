package ptr

import (
	"bytes"
	"compress/zlib"
	"reflect"
	"slices"
	"testing"
)

func TestParseMetadataRoundTrip(t *testing.T) {
	entries := []updateEntry{
		{Index: 0, Hashes: []string{"aa", "bb"}, Begin: 100, End: 200},
		{Index: 1, Hashes: []string{"cc"}, Begin: 200, End: 300},
	}
	raw := buildMetadata(t, 1234567890, entries)
	got, err := parseMetadata(raw)
	if err != nil {
		t.Fatalf("parseMetadata: %v", err)
	}
	if got.NextUpdateDue != 1234567890 {
		t.Errorf("next_update_due = %d, want 1234567890", got.NextUpdateDue)
	}
	if !reflect.DeepEqual(got.Updates, entries) {
		t.Errorf("updates = %+v, want %+v", got.Updates, entries)
	}
}

func TestParseMetadataEmptySlice(t *testing.T) {
	raw := buildMetadata(t, 42, nil)
	got, err := parseMetadata(raw)
	if err != nil {
		t.Fatalf("parseMetadata: %v", err)
	}
	if len(got.Updates) != 0 || got.NextUpdateDue != 42 {
		t.Errorf("empty slice = %+v, want no updates and next_update_due 42", got)
	}
}

func TestParseDefinitionsRoundTrip(t *testing.T) {
	hashes := map[uint64]string{1: "abcd", 9000000000: "ef01"}
	tags := map[uint64]string{2: "blue_sky", 3: "character:reimu"}
	raw := buildDefinitions(t, hashes, tags)
	up, err := parseUpdate(raw)
	if err != nil {
		t.Fatalf("parseUpdate: %v", err)
	}
	if up.Definitions == nil {
		t.Fatal("want a definitions update")
	}
	if !reflect.DeepEqual(up.Definitions.Hashes, hashes) {
		t.Errorf("hashes = %v, want %v", up.Definitions.Hashes, hashes)
	}
	if !reflect.DeepEqual(up.Definitions.Tags, tags) {
		t.Errorf("tags = %v, want %v", up.Definitions.Tags, tags)
	}
}

func TestParseContentRoundTrip(t *testing.T) {
	raw := newContent().
		mappingAdd(2, 1, 5, 9).
		mappingAdd(3, 1).
		mappingDel(2, 5).
		siblingAdd(10, 11).
		parentAdd(3, 20).
		build(t)
	up, err := parseUpdate(raw)
	if err != nil {
		t.Fatalf("parseUpdate: %v", err)
	}
	c := up.Content
	if c == nil {
		t.Fatal("want a content update")
	}
	wantAdd := []mapping{{TagID: 2, HashIDs: []uint64{1, 5, 9}}, {TagID: 3, HashIDs: []uint64{1}}}
	if !sameMappings(c.MappingsAdd, wantAdd) {
		t.Errorf("mappings add = %+v, want %+v", c.MappingsAdd, wantAdd)
	}
	if !sameMappings(c.MappingsDel, []mapping{{TagID: 2, HashIDs: []uint64{5}}}) {
		t.Errorf("mappings del = %+v", c.MappingsDel)
	}
	if !reflect.DeepEqual(c.SiblingsAdd, []pair{{A: 10, B: 11}}) {
		t.Errorf("siblings add = %+v", c.SiblingsAdd)
	}
	if !reflect.DeepEqual(c.ParentsAdd, []pair{{A: 3, B: 20}}) {
		t.Errorf("parents add = %+v", c.ParentsAdd)
	}
}

func TestParseContentLargeMappingRow(t *testing.T) {
	ids := make([]uint64, 25000)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	raw := newContent().mappingAdd(7, ids...).build(t)
	up, err := parseUpdate(raw)
	if err != nil {
		t.Fatalf("parseUpdate: %v", err)
	}
	if len(up.Content.MappingsAdd) != 1 || len(up.Content.MappingsAdd[0].HashIDs) != 25000 {
		t.Fatalf("large row lost ids: %d rows", len(up.Content.MappingsAdd))
	}
}

func TestParseSkipsUnknownKinds(t *testing.T) {
	content := deflate(t, []any{typeContentUpdate, 1, []any{
		[]any{contentTypeFiles, []any{[]any{actionAdd, []any{[]any{1, 2, 3}}}}},
		[]any{contentTypeMappings, []any{[]any{actionAdd, []any{[]any{2, []uint64{1}}}}}},
	}})
	up, err := parseUpdate(content)
	if err != nil {
		t.Fatalf("parseUpdate with a files block: %v", err)
	}
	if len(up.Content.MappingsAdd) != 1 {
		t.Errorf("mappings should survive an unknown-type block, got %+v", up.Content.MappingsAdd)
	}

	defs := deflate(t, []any{typeDefinitionsUpdate, 1, []any{
		[]any{99, []any{[]any{1, "x"}}},
		[]any{defTypeTags, []any{[]any{2, "sky"}}},
	}})
	up, err = parseUpdate(defs)
	if err != nil {
		t.Fatalf("parseUpdate with an unknown definition kind: %v", err)
	}
	if up.Definitions.Tags[2] != "sky" || len(up.Definitions.Hashes) != 0 {
		t.Errorf("unknown definition kind mishandled: %+v", up.Definitions)
	}
}

func TestParseRejectsFutureVersion(t *testing.T) {
	raw := deflate(t, []any{typeDefinitionsUpdate, 2, []any{}})
	if _, err := parseUpdate(raw); err == nil {
		t.Fatal("want an error for a future object version")
	}
}

func TestParseRejectsUnknownType(t *testing.T) {
	raw := deflate(t, []any{99, 1, []any{}})
	if _, err := parseUpdate(raw); err == nil {
		t.Fatal("want an error for an update that is neither definitions nor content")
	}
}

func TestParseRejectsCorruptZlib(t *testing.T) {
	if _, err := parseUpdate([]byte("not zlib at all")); err == nil {
		t.Fatal("want an error inflating a non-zlib blob")
	}
}

func TestParseRejectsOversizedInflation(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxUpdateBytes+1024)
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(big)
	_ = zw.Close()
	if _, err := parseUpdate(buf.Bytes()); err == nil {
		t.Fatal("want an error for an over-cap inflation")
	}
}

func TestParseMetadataRejectsWrongEnvelope(t *testing.T) {
	raw := deflate(t, []any{typeMetadata, 1, []any{[]any{}, 0}})
	if _, err := parseMetadata(raw); err == nil {
		t.Fatal("want an error when the metadata envelope is not a dictionary")
	}
}

func sameMappings(a, b []mapping) bool {
	return slices.EqualFunc(a, b, func(x, y mapping) bool {
		return x.TagID == y.TagID && slices.Equal(x.HashIDs, y.HashIDs)
	})
}

func TestParseLargeIDsExact(t *testing.T) {
	const big = uint64(9007199254740993)
	raw := buildDefinitions(t, map[uint64]string{big: "ff"}, nil)
	up, err := parseUpdate(raw)
	if err != nil {
		t.Fatalf("parseUpdate: %v", err)
	}
	if _, ok := up.Definitions.Hashes[big]; !ok {
		var got []uint64
		for id := range up.Definitions.Hashes {
			got = append(got, id)
		}
		t.Errorf("large id lost precision: got %v, want %d", got, big)
	}
}

func TestUpdateRows(t *testing.T) {
	u := &update{
		Definitions: &definitions{Hashes: map[uint64]string{1: "aa"}, Tags: map[uint64]string{10: "a", 11: "b"}},
		Content: &contentRows{
			MappingsAdd: []mapping{{TagID: 10, HashIDs: []uint64{1, 2, 3}}},
			MappingsDel: []mapping{{TagID: 11, HashIDs: []uint64{4}}},
			SiblingsAdd: []pair{{A: 10, B: 11}},
			ParentsDel:  []pair{{A: 11, B: 10}},
		},
	}

	if got := u.Rows(); got != 9 {
		t.Errorf("Rows() = %d, want 9", got)
	}
	if got := (&update{}).Rows(); got != 0 {
		t.Errorf("empty Rows() = %d, want 0", got)
	}
}

func TestUpdateKind(t *testing.T) {
	defs := buildDefinitions(t, map[uint64]string{1: "aa"}, nil)
	if kind, err := updateKind(defs); err != nil || kind != typeDefinitionsUpdate {
		t.Errorf("kind = %d (%v), want %d", kind, err, typeDefinitionsUpdate)
	}
	content := newContent().mappingAdd(1, 2).build(t)
	if kind, err := updateKind(content); err != nil || kind != typeContentUpdate {
		t.Errorf("kind = %d (%v), want %d", kind, err, typeContentUpdate)
	}
	if _, err := updateKind([]byte("not zlib")); err == nil {
		t.Error("garbage should not yield a kind")
	}
	if _, err := updateKind(deflate(t, map[string]int{"a": 1})); err == nil {
		t.Error("a non-array object should not yield a kind")
	}
}
