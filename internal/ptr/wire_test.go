package ptr

import (
	"bytes"
	"compress/zlib"
	"reflect"
	"testing"
)

func TestParseMetadataRoundTrip(t *testing.T) {
	entries := []UpdateEntry{
		{Index: 0, Hashes: []string{"aa", "bb"}, Begin: 100, End: 200},
		{Index: 1, Hashes: []string{"cc"}, Begin: 200, End: 300},
	}
	raw := buildMetadata(t, 1234567890, entries)
	got, err := ParseMetadata(raw)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
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
	got, err := ParseMetadata(raw)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}
	if len(got.Updates) != 0 || got.NextUpdateDue != 42 {
		t.Errorf("empty slice = %+v, want no updates and next_update_due 42", got)
	}
}

func TestParseDefinitionsRoundTrip(t *testing.T) {
	hashes := map[uint64]string{1: "abcd", 9000000000: "ef01"} // an id past 2^32
	tags := map[uint64]string{2: "blue_sky", 3: "character:reimu"}
	raw := buildDefinitions(t, hashes, tags)
	up, err := ParseUpdate(raw)
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
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
	up, err := ParseUpdate(raw)
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	c := up.Content
	if c == nil {
		t.Fatal("want a content update")
	}
	wantAdd := []Mapping{{TagID: 2, HashIDs: []uint64{1, 5, 9}}, {TagID: 3, HashIDs: []uint64{1}}}
	if !sameMappings(c.MappingsAdd, wantAdd) {
		t.Errorf("mappings add = %+v, want %+v", c.MappingsAdd, wantAdd)
	}
	if !sameMappings(c.MappingsDel, []Mapping{{TagID: 2, HashIDs: []uint64{5}}}) {
		t.Errorf("mappings del = %+v", c.MappingsDel)
	}
	if !reflect.DeepEqual(c.SiblingsAdd, []Pair{{A: 10, B: 11}}) {
		t.Errorf("siblings add = %+v", c.SiblingsAdd)
	}
	if !reflect.DeepEqual(c.ParentsAdd, []Pair{{A: 3, B: 20}}) {
		t.Errorf("parents add = %+v", c.ParentsAdd)
	}
}

// A big mappings row (up to the 25k-hash server chunk) decodes intact.
func TestParseContentLargeMappingRow(t *testing.T) {
	ids := make([]uint64, 25000)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	raw := newContent().mappingAdd(7, ids...).build(t)
	up, err := ParseUpdate(raw)
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	if len(up.Content.MappingsAdd) != 1 || len(up.Content.MappingsAdd[0].HashIDs) != 25000 {
		t.Fatalf("large row lost ids: %d rows", len(up.Content.MappingsAdd))
	}
}

// An unknown content type (a files block) and an unknown definition kind are
// skipped rather than failing the parse.
func TestParseSkipsUnknownKinds(t *testing.T) {
	content := deflate(t, []any{typeContentUpdate, 1, []any{
		[]any{contentTypeFiles, []any{[]any{actionAdd, []any{[]any{1, 2, 3}}}}},
		[]any{contentTypeMappings, []any{[]any{actionAdd, []any{[]any{2, []uint64{1}}}}}},
	}})
	up, err := ParseUpdate(content)
	if err != nil {
		t.Fatalf("ParseUpdate with a files block: %v", err)
	}
	if len(up.Content.MappingsAdd) != 1 {
		t.Errorf("mappings should survive an unknown-type block, got %+v", up.Content.MappingsAdd)
	}

	defs := deflate(t, []any{typeDefinitionsUpdate, 1, []any{
		[]any{99, []any{[]any{1, "x"}}}, // unknown definition kind
		[]any{defTypeTags, []any{[]any{2, "sky"}}},
	}})
	up, err = ParseUpdate(defs)
	if err != nil {
		t.Fatalf("ParseUpdate with an unknown definition kind: %v", err)
	}
	if up.Definitions.Tags[2] != "sky" || len(up.Definitions.Hashes) != 0 {
		t.Errorf("unknown definition kind mishandled: %+v", up.Definitions)
	}
}

func TestParseRejectsFutureVersion(t *testing.T) {
	raw := deflate(t, []any{typeDefinitionsUpdate, 2, []any{}}) // version 2, we know 1
	if _, err := ParseUpdate(raw); err == nil {
		t.Fatal("want an error for a future object version")
	}
}

func TestParseRejectsUnknownType(t *testing.T) {
	raw := deflate(t, []any{99, 1, []any{}})
	if _, err := ParseUpdate(raw); err == nil {
		t.Fatal("want an error for an update that is neither definitions nor content")
	}
}

func TestParseRejectsCorruptZlib(t *testing.T) {
	if _, err := ParseUpdate([]byte("not zlib at all")); err == nil {
		t.Fatal("want an error inflating a non-zlib blob")
	}
}

func TestParseRejectsOversizedInflation(t *testing.T) {
	// A blob that inflates past the cap must be refused, not buffered whole.
	big := bytes.Repeat([]byte("a"), maxUpdateBytes+1024)
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(big)
	_ = zw.Close()
	if _, err := ParseUpdate(buf.Bytes()); err == nil {
		t.Fatal("want an error for an over-cap inflation")
	}
}

func TestParseMetadataRejectsWrongEnvelope(t *testing.T) {
	// A metadata body that is not a dictionary envelope is rejected.
	raw := deflate(t, []any{typeMetadata, 1, []any{[]any{}, 0}})
	if _, err := ParseMetadata(raw); err == nil {
		t.Fatal("want an error when the metadata envelope is not a dictionary")
	}
}

func sameMappings(a, b []Mapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TagID != b[i].TagID || !reflect.DeepEqual(a[i].HashIDs, b[i].HashIDs) {
			return false
		}
	}
	return true
}

// TestParseLargeIDsExact guards the id decode path: a hash id past 2^53 must
// not be mangled by a float round-trip.
func TestParseLargeIDsExact(t *testing.T) {
	const big = uint64(9007199254740993) // 2^53 + 1, not representable as float64
	raw := buildDefinitions(t, map[uint64]string{big: "ff"}, nil)
	up, err := ParseUpdate(raw)
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	if _, ok := up.Definitions.Hashes[big]; !ok {
		var got []uint64
		for id := range up.Definitions.Hashes {
			got = append(got, id)
		}
		t.Errorf("large id lost precision: got %v, want %d", got, big)
	}
}
