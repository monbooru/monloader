package ptr

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// h32 builds a 64-hex (32-byte) sha256 string from a short seed.
func h32(seed string) string {
	return seed + strings.Repeat("0", 64-len(seed))
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReplayDefinitionsAndMappings(t *testing.T) {
	s := openTestStore(t)
	defs := &Update{Definitions: &Definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: h32("bb")},
		Tags:   map[uint64]string{10: "blue_sky", 11: "cloud"},
	}}
	content := &Update{Content: newContentValue(func(c *Content) {
		c.MappingsAdd = []Mapping{{TagID: 10, HashIDs: []uint64{1, 2}}, {TagID: 11, HashIDs: []uint64{1}}}
	})}
	if _, err := s.Replay(0, 0, []*Update{defs, content}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	counts, _ := s.Counts()
	if counts.Hashes != 2 || counts.Tags != 2 || counts.Mappings != 3 {
		t.Errorf("counts = %+v, want 2 hashes / 2 tags / 3 mappings", counts)
	}
	cur, ok, _ := s.Cursor()
	if !ok || cur != 0 {
		t.Errorf("cursor = %d ok=%v, want 0/true", cur, ok)
	}
}

func TestReplayDeletesRows(t *testing.T) {
	s := openTestStore(t)
	base := &Update{
		Definitions: &Definitions{Hashes: map[uint64]string{1: h32("aa")}, Tags: map[uint64]string{10: "t", 11: "u"}},
	}
	add := &Update{Content: newContentValue(func(c *Content) {
		c.MappingsAdd = []Mapping{{TagID: 10, HashIDs: []uint64{1}}, {TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []Pair{{A: 11, B: 10}}
		c.ParentsAdd = []Pair{{A: 10, B: 11}}
	})}
	if _, err := s.Replay(0, 0, []*Update{base, add}); err != nil {
		t.Fatalf("Replay add: %v", err)
	}
	del := &Update{Content: newContentValue(func(c *Content) {
		c.MappingsDel = []Mapping{{TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsDel = []Pair{{A: 11, B: 10}}
		c.ParentsDel = []Pair{{A: 10, B: 11}}
	})}
	if _, err := s.Replay(1, 0, []*Update{del}); err != nil {
		t.Fatalf("Replay del: %v", err)
	}
	counts, _ := s.Counts()
	if counts.Mappings != 1 || counts.Siblings != 0 || counts.Parents != 0 {
		t.Errorf("after deletes counts = %+v, want 1 mapping / 0 siblings / 0 parents", counts)
	}
	cur, _, _ := s.Cursor()
	if cur != 1 {
		t.Errorf("cursor = %d, want 1", cur)
	}
}

// Replay persists the coverage timestamp with the cursor; an entry without a
// time window keeps the previous value.
func TestReplayPersistsCoveredThrough(t *testing.T) {
	s := openTestStore(t)
	if got, _ := s.CoveredThrough(); got != 0 {
		t.Errorf("fresh index covered = %d, want 0", got)
	}
	if _, err := s.Replay(0, 1500, nil); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got, _ := s.CoveredThrough(); got != 1500 {
		t.Errorf("covered = %d, want 1500", got)
	}
	if _, err := s.Replay(1, 0, nil); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got, _ := s.CoveredThrough(); got != 1500 {
		t.Errorf("covered after a windowless replay = %d, want 1500 kept", got)
	}
	if _, err := s.Replay(2, 2500, nil); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got, _ := s.CoveredThrough(); got != 2500 {
		t.Errorf("covered = %d, want 2500", got)
	}
}

// A hex hash that is not 32 bytes is skipped, not stored as a bad key.
func TestReplaySkipsMalformedHash(t *testing.T) {
	s := openTestStore(t)
	defs := &Update{Definitions: &Definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: "xyz", 3: "abcd"},
		Tags:   map[uint64]string{},
	}}
	if _, err := s.Replay(0, 0, []*Update{defs}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	counts, _ := s.Counts()
	if counts.Hashes != 1 {
		t.Errorf("hashes = %d, want 1 (the two malformed ones skipped)", counts.Hashes)
	}
}

// A later definition update overwrites an id's value (the service can redefine).
func TestReplayRedefinesID(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Replay(0, 0, []*Update{{Definitions: &Definitions{Tags: map[uint64]string{5: "old"}, Hashes: map[uint64]string{}}}}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := s.Replay(1, 0, []*Update{{Definitions: &Definitions{Tags: map[uint64]string{5: "new"}, Hashes: map[uint64]string{}}}}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var tag string
	if err := s.db.QueryRow(`SELECT tag FROM tags WHERE tag_id=5`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	if tag != "new" {
		t.Errorf("tag = %q, want the redefined value", tag)
	}
}

// The census Replay maintains incrementally must track the real row counts:
// duplicate mapping adds land once, deletes subtract, sibling repoints do not
// inflate, and the persisted copy survives a reopen without a re-scan.
func TestReplayCensusMatchesTables(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defs := &Update{Definitions: &Definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: h32("bb")},
		Tags:   map[uint64]string{10: "blue_sky", 11: "cloud"},
	}}
	add := &Update{Content: newContentValue(func(c *Content) {
		c.MappingsAdd = []Mapping{{TagID: 10, HashIDs: []uint64{1, 2}}, {TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []Pair{{A: 11, B: 10}}
	})}
	if _, err := s.Replay(0, 0, []*Update{defs, add}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	// Re-add an existing mapping (no-op), delete another, repoint the sibling.
	churn := &Update{Content: newContentValue(func(c *Content) {
		c.MappingsAdd = []Mapping{{TagID: 10, HashIDs: []uint64{1}}}
		c.MappingsDel = []Mapping{{TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []Pair{{A: 11, B: 12}}
	})}
	got, err := s.Replay(1, 0, []*Update{churn})
	if err != nil {
		t.Fatalf("Replay churn: %v", err)
	}
	want, _ := s.Counts()
	if got != want {
		t.Errorf("census = %+v, want the scanned truth %+v", got, want)
	}
	if got.Mappings != 2 || got.Siblings != 1 {
		t.Errorf("census = %+v, want 2 mappings / 1 sibling", got)
	}
	s.Close()

	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	stored, err := s2.StoredCounts()
	if err != nil || stored != want {
		t.Errorf("stored census = %+v (%v), want %+v", stored, err, want)
	}
}

// An index built before the census was persisted has rows but no counts key:
// the sync goroutine seeds it with a one-time scan. Opening alone must not -
// the boot path has to stay instant however large the index is.
func TestEnsureCountsSeedsLegacyIndex(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defs := &Update{Definitions: &Definitions{Hashes: map[uint64]string{1: h32("aa")}, Tags: map[uint64]string{10: "t"}}}
	add := &Update{Content: newContentValue(func(c *Content) { c.MappingsAdd = []Mapping{{TagID: 10, HashIDs: []uint64{1}}} })}
	if _, err := s.Replay(3, 0, []*Update{defs, add}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sync WHERE key=?`, countsKey); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if stored, err := s2.StoredCounts(); err != nil || stored != (Counts{}) {
		t.Errorf("census after open alone = %+v (%v), want untouched zeros", stored, err)
	}
	if err := s2.EnsureCounts(context.Background()); err != nil {
		t.Fatalf("EnsureCounts: %v", err)
	}
	stored, err := s2.StoredCounts()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Hashes != 1 || stored.Tags != 1 || stored.Mappings != 1 {
		t.Errorf("seeded census = %+v, want the scanned rows", stored)
	}
}

// A store reopened on the same path resumes from the committed cursor and keeps
// its rows - the crash-resume guarantee (reopening models a restart).
func TestReopenResumesFromCursor(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defs := &Update{Definitions: &Definitions{Hashes: map[uint64]string{1: h32("aa")}, Tags: map[uint64]string{10: "t"}}}
	add := &Update{Content: newContentValue(func(c *Content) { c.MappingsAdd = []Mapping{{TagID: 10, HashIDs: []uint64{1}}} })}
	if _, err := s.Replay(5, 0, []*Update{defs, add}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	s.Close()

	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	cur, ok, _ := s2.Cursor()
	if !ok || cur != 5 {
		t.Errorf("resumed cursor = %d ok=%v, want 5/true", cur, ok)
	}
	counts, _ := s2.Counts()
	if counts.Mappings != 1 {
		t.Errorf("rows lost across reopen: %+v", counts)
	}
}

// A failed Replay rolls back whole: neither its rows nor its cursor land, so the
// cursor never points past data that is not there.
func TestReplayRollsBackOnError(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Replay(0, 0, []*Update{{Definitions: &Definitions{Tags: map[uint64]string{1: "a"}, Hashes: map[uint64]string{}}}}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	// A mapping referencing a hash blob column with a NULL-violating value cannot
	// be forced easily; instead close the DB mid-store to force a commit error.
	s.Close()
	_, err := s.Replay(1, 0, []*Update{{Content: newContentValue(func(c *Content) {
		c.MappingsAdd = []Mapping{{TagID: 1, HashIDs: []uint64{1}}}
	})}})
	if err == nil {
		t.Fatal("want an error replaying into a closed store")
	}
}

func TestSchemaMismatchDetected(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// Corrupt the recorded schema version to model an older index.
	if _, err := s.db.Exec(`UPDATE sync SET value='0' WHERE key='schema'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenStore(dir); err != ErrSchemaMismatch {
		t.Errorf("reopen err = %v, want ErrSchemaMismatch", err)
	}
}

func TestOpenStoreReportsUnwritablePath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permission bits, so a read-only dir cannot refuse the probe")
	}
	dir := filepath.Join(t.TempDir(), "ptr")
	if err := os.Mkdir(dir, 0o555); err != nil { // read-only, like a root-owned mount
		t.Fatal(err)
	}
	_, err := OpenStore(dir)
	if err == nil {
		t.Fatal("OpenStore on a read-only dir should fail")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error = %v, want a clear 'not writable' message, not a raw sqlite error", err)
	}
}

// newContentValue builds a Content via a mutator, so tests read declaratively.
func newContentValue(fn func(*Content)) *Content {
	c := &Content{}
	fn(c)
	return c
}

// BenchmarkReplayMappings measures mapping-insert throughput, the initial
// sync's dominant cost. Run with `go test -run x -bench Replay ./internal/ptr`.
// The result grounds the whole-PTR sync-time estimate (see the spec's sizing).
func BenchmarkReplayMappings(b *testing.B) {
	s, err := OpenStore(b.TempDir())
	if err != nil {
		b.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	// One hash, many tags: b.N mapping rows across one Replay transaction.
	hashes := map[uint64]string{1: h32("aa")}
	tags := map[uint64]string{}
	ms := make([]Mapping, 0, b.N)
	for i := 0; i < b.N; i++ {
		tags[uint64(i+1)] = "tag" + hex.EncodeToString([]byte{byte(i), byte(i >> 8)})
		ms = append(ms, Mapping{TagID: uint64(i + 1), HashIDs: []uint64{1}})
	}
	defs := &Update{Definitions: &Definitions{Hashes: hashes, Tags: tags}}
	content := &Update{Content: &Content{MappingsAdd: ms}}
	b.ResetTimer()
	if _, err := s.Replay(0, 0, []*Update{defs, content}); err != nil {
		b.Fatalf("Replay: %v", err)
	}
	b.StopTimer()
}
