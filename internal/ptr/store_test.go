package ptr

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func h32(seed string) string {
	return seed + strings.Repeat("0", 64-len(seed))
}

func mustReplay(t *testing.T, s *Store, index uint64, covered int64, blobs [][]byte) {
	t.Helper()
	if _, _, err := s.Replay(context.Background(), index, covered, blobs, nil); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

func openTestStore(t *testing.T) *Store { return openStoreAt(t, t.TempDir()) }

func openStoreAt(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReplayStopsAndRollsBackOnCancel(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{
		Hashes: map[uint64]string{1: h32("aa")},
		Tags:   map[uint64]string{10: "blue_sky"},
	}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1}}}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	applied := 0
	_, _, err := s.Replay(ctx, 0, 0, wireBlobs(t, defs, content), func() { applied++; cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Replay = %v, want context.Canceled", err)
	}
	if applied != 1 {
		t.Errorf("applied ticks = %d, want the replay stopped after the first blob", applied)
	}
	if _, ok, _ := s.Cursor(); ok {
		t.Error("a canceled replay advanced the cursor")
	}
	if counts, _ := s.Counts(); counts.Hashes != 0 {
		t.Errorf("counts = %+v, want the canceled entry rolled back", counts)
	}
}

func TestReplayDefinitionsAndMappings(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: h32("bb")},
		Tags:   map[uint64]string{10: "blue_sky", 11: "cloud"},
	}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1, 2}}, {TagID: 11, HashIDs: []uint64{1}}}
	})}
	applied := 0
	_, rows, err := s.Replay(context.Background(), 0, 0, wireBlobs(t, defs, content), func() { applied++ })
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if rows != 7 {
		t.Errorf("rows = %d, want 7", rows)
	}
	if applied != 2 {
		t.Errorf("applied ticks = %d, want one per blob (2)", applied)
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
	base := &update{
		Definitions: &definitions{Hashes: map[uint64]string{1: h32("aa")}, Tags: map[uint64]string{10: "t", 11: "u"}},
	}
	add := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1}}, {TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []pair{{A: 11, B: 10}}
		c.ParentsAdd = []pair{{A: 10, B: 11}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, base, add))
	del := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsDel = []mapping{{TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsDel = []pair{{A: 11, B: 10}}
		c.ParentsDel = []pair{{A: 10, B: 11}}
	})}
	mustReplay(t, s, 1, 0, wireBlobs(t, del))
	counts, _ := s.Counts()
	if counts.Mappings != 1 || counts.Siblings != 0 || counts.Parents != 0 {
		t.Errorf("after deletes counts = %+v, want 1 mapping / 0 siblings / 0 parents", counts)
	}
	cur, _, _ := s.Cursor()
	if cur != 1 {
		t.Errorf("cursor = %d, want 1", cur)
	}
}

func TestReplayPersistsCoveredThrough(t *testing.T) {
	s := openTestStore(t)
	if got, _ := s.CoveredThrough(); got != 0 {
		t.Errorf("fresh index covered = %d, want 0", got)
	}
	mustReplay(t, s, 0, 1500, nil)
	if got, _ := s.CoveredThrough(); got != 1500 {
		t.Errorf("covered = %d, want 1500", got)
	}
	mustReplay(t, s, 1, 0, nil)
	if got, _ := s.CoveredThrough(); got != 1500 {
		t.Errorf("covered after a windowless replay = %d, want 1500 kept", got)
	}
	mustReplay(t, s, 2, 2500, nil)
	if got, _ := s.CoveredThrough(); got != 2500 {
		t.Errorf("covered = %d, want 2500", got)
	}
}

func TestReplayCountsBlobs(t *testing.T) {
	s := openTestStore(t)
	if _, ok, _ := s.BlobsApplied(); ok {
		t.Error("fresh index should have no blob census")
	}
	defs := &update{Definitions: &definitions{Hashes: map[uint64]string{1: h32("aa")}, Tags: map[uint64]string{10: "a"}}}
	content := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1}}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, content))
	if _, ok, _ := s.BlobsApplied(); ok {
		t.Error("a replay with no seed should leave the census absent, for the next boot to seed")
	}
	if err := s.SeedBlobsApplied(0); err != nil {
		t.Fatalf("SeedBlobsApplied: %v", err)
	}
	mustReplay(t, s, 1, 0, wireBlobs(t, defs, content))
	mustReplay(t, s, 2, 0, wireBlobs(t, defs))
	if got, ok, _ := s.BlobsApplied(); !ok || got != 3 {
		t.Errorf("blobs = %d ok=%v, want 3/true", got, ok)
	}
	if err := s.SeedBlobsApplied(41); err != nil {
		t.Fatalf("SeedBlobsApplied: %v", err)
	}
	if got, ok, _ := s.BlobsApplied(); !ok || got != 41 {
		t.Errorf("seeded blobs = %d ok=%v, want 41/true", got, ok)
	}
}

func TestReplaySkipsMalformedHash(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: "xyz", 3: "abcd"},
		Tags:   map[uint64]string{},
	}}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs))
	counts, _ := s.Counts()
	if counts.Hashes != 1 {
		t.Errorf("hashes = %d, want 1 (the two malformed ones skipped)", counts.Hashes)
	}
}

func TestReplayRedefinesID(t *testing.T) {
	s := openTestStore(t)
	mustReplay(t, s, 0, 0, wireBlobs(t, &update{Definitions: &definitions{Tags: map[uint64]string{5: "old"}, Hashes: map[uint64]string{}}}))
	mustReplay(t, s, 1, 0, wireBlobs(t, &update{Definitions: &definitions{Tags: map[uint64]string{5: "new"}, Hashes: map[uint64]string{}}}))
	var tag string
	if err := s.db.QueryRow(`SELECT tag FROM tags WHERE tag_id=5`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	if tag != "new" {
		t.Errorf("tag = %q, want the redefined value", tag)
	}
}

func TestReplayCensusMatchesTables(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	defs := &update{Definitions: &definitions{
		Hashes: map[uint64]string{1: h32("aa"), 2: h32("bb")},
		Tags:   map[uint64]string{10: "blue_sky", 11: "cloud"},
	}}
	add := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1, 2}}, {TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []pair{{A: 11, B: 10}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, add))

	churn := &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1}}}
		c.MappingsDel = []mapping{{TagID: 11, HashIDs: []uint64{1}}}
		c.SiblingsAdd = []pair{{A: 11, B: 12}}
	})}
	got, _, err := s.Replay(context.Background(), 1, 0, wireBlobs(t, churn), nil)
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

	s2 := openStoreAt(t, dir)
	stored, err := s2.StoredCounts()
	if err != nil || stored != want {
		t.Errorf("stored census = %+v (%v), want %+v", stored, err, want)
	}
}

func TestEnsureCountsSeedsLegacyIndex(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	defs := &update{Definitions: &definitions{Hashes: map[uint64]string{1: h32("aa")}, Tags: map[uint64]string{10: "t"}}}
	add := &update{Content: newContentValue(func(c *contentRows) { c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1}}} })}
	mustReplay(t, s, 3, 0, wireBlobs(t, defs, add))
	if _, err := s.db.Exec(`DELETE FROM sync WHERE key=?`, countsKey); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := openStoreAt(t, dir)
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

func TestReopenResumesFromCursor(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	defs := &update{Definitions: &definitions{Hashes: map[uint64]string{1: h32("aa")}, Tags: map[uint64]string{10: "t"}}}
	add := &update{Content: newContentValue(func(c *contentRows) { c.MappingsAdd = []mapping{{TagID: 10, HashIDs: []uint64{1}}} })}
	mustReplay(t, s, 5, 0, wireBlobs(t, defs, add))
	s.Close()

	s2 := openStoreAt(t, dir)
	cur, ok, _ := s2.Cursor()
	if !ok || cur != 5 {
		t.Errorf("resumed cursor = %d ok=%v, want 5/true", cur, ok)
	}
	counts, _ := s2.Counts()
	if counts.Mappings != 1 {
		t.Errorf("rows lost across reopen: %+v", counts)
	}
}

func TestReplayRollsBackOnError(t *testing.T) {
	s := openTestStore(t)
	mustReplay(t, s, 0, 0, wireBlobs(t, &update{Definitions: &definitions{Tags: map[uint64]string{1: "a"}, Hashes: map[uint64]string{}}}))

	s.Close()
	_, _, err := s.Replay(context.Background(), 1, 0, wireBlobs(t, &update{Content: newContentValue(func(c *contentRows) {
		c.MappingsAdd = []mapping{{TagID: 1, HashIDs: []uint64{1}}}
	})}), nil)
	if err == nil {
		t.Fatal("want an error replaying into a closed store")
	}
}

func TestSchemaMismatchDetected(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	if _, err := s.db.Exec(`UPDATE sync SET value='0' WHERE key='schema'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenStore(dir); err != errSchemaMismatch {
		t.Errorf("reopen err = %v, want errSchemaMismatch", err)
	}
}

func TestOpenStoreReportsUnwritablePath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permission bits, so a read-only dir cannot refuse the probe")
	}
	dir := filepath.Join(t.TempDir(), "ptr")
	if err := os.Mkdir(dir, 0o555); err != nil {
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

func newContentValue(fn func(*contentRows)) *contentRows {
	c := &contentRows{}
	fn(c)
	return c
}

func BenchmarkReplayMappings(b *testing.B) {
	s, err := OpenStore(b.TempDir())
	if err != nil {
		b.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	hashes := map[uint64]string{1: h32("aa")}
	tags := map[uint64]string{}
	ms := make([]mapping, 0, b.N)
	for i := 0; i < b.N; i++ {
		tags[uint64(i+1)] = "tag" + hex.EncodeToString([]byte{byte(i), byte(i >> 8)})
		ms = append(ms, mapping{TagID: uint64(i + 1), HashIDs: []uint64{1}})
	}
	defs := &update{Definitions: &definitions{Hashes: hashes, Tags: tags}}
	content := &update{Content: &contentRows{MappingsAdd: ms}}
	blobs := wireBlobs(b, defs, content)
	b.ResetTimer()
	if _, _, err := s.Replay(context.Background(), 0, 0, blobs, nil); err != nil {
		b.Fatalf("Replay: %v", err)
	}
	b.StopTimer()
}

func TestStoresOpenInAFolderNamedWithURISyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses ? in a file name")
	}
	dir := filepath.Join(t.TempDir(), "a#b?c%41")
	openStoreAt(t, dir)
	openContribStoreAt(t, dir)
	for _, name := range []string{dbFile, contribFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not written inside %q: %v", name, dir, err)
		}
	}
}

func (s *Store) Counts() (Counts, error) {
	return s.counts(context.Background())
}

func TestReplayCountsTagsAsATableCountWould(t *testing.T) {
	s := openTestStore(t)
	mustReplay(t, s, 0, 0, wireBlobs(t, &update{Definitions: &definitions{Tags: map[uint64]string{1: "a", 2: "b"}}}))
	mustReplay(t, s, 1, 1, wireBlobs(t, &update{Definitions: &definitions{Tags: map[uint64]string{3: "c"}}}))
	counts, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if counts.Tags != n || n != 3 {
		t.Errorf("counts.Tags = %d, table holds %d, want 3 both", counts.Tags, n)
	}
}

func TestReplayRepointsASiblingAddedAndDeletedInOneUpdate(t *testing.T) {
	s := openTestStore(t)
	defs := &update{Definitions: &definitions{Tags: map[uint64]string{10: "old", 11: "bad", 12: "new"}}}
	first := &update{Content: newContentValue(func(c *contentRows) {
		c.SiblingsAdd = []pair{{A: 11, B: 10}}
	})}
	mustReplay(t, s, 0, 0, wireBlobs(t, defs, first))
	repoint := &update{Content: newContentValue(func(c *contentRows) {
		c.SiblingsAdd = []pair{{A: 11, B: 12}}
		c.SiblingsDel = []pair{{A: 11, B: 10}}
	})}
	mustReplay(t, s, 1, 0, wireBlobs(t, repoint))
	got, err := s.resolveSibling(11)
	if err != nil {
		t.Fatal(err)
	}
	if got != 12 {
		t.Errorf("bad resolves to %d, want the new target 12", got)
	}
	counts, err := s.Counts()
	if err != nil || counts.Siblings != 1 {
		t.Errorf("counts = %+v, %v, want the one current sibling", counts, err)
	}
}
