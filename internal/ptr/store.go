package ptr

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"github.com/monbooru/monloader/internal/logx"
)

// schemaVersion gates the on-disk index. A database written by an older schema
// is reported as needing a rebuild rather than being read wrong.
const schemaVersion = 1

// dbFile is the index filename under the data path.
const dbFile = "ptr.sqlite"

// schema is the local index: the service's id definitions, the mappings keyed
// hash-first (the lookup direction; there is no reverse index, which roughly
// halves the disk cost), and the sibling / parent graph. It is a cache of
// public data, rebuildable from the repository, so it is not the durable app
// state the no-database rule protects.
const schema = `
CREATE TABLE IF NOT EXISTS hashes (
  hash_id INTEGER PRIMARY KEY,
  hash    BLOB NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS tags (
  tag_id INTEGER PRIMARY KEY,
  tag    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mappings (
  hash_id INTEGER NOT NULL,
  tag_id  INTEGER NOT NULL,
  PRIMARY KEY (hash_id, tag_id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS siblings (
  bad_tag_id  INTEGER PRIMARY KEY,
  good_tag_id INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS parents (
  child_tag_id  INTEGER NOT NULL,
  parent_tag_id INTEGER NOT NULL,
  PRIMARY KEY (child_tag_id, parent_tag_id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS sync (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tags_by_name ON tags(tag);
CREATE INDEX IF NOT EXISTS siblings_by_good ON siblings(good_tag_id);
`

// Store is the local PTR index over SQLite. The replay gets its own handle:
// the page cache is per connection, and only the replay's randomly-landing
// inserts are worth a large one (see replayCacheKB).
type Store struct {
	db     *sql.DB
	replay *sql.DB
}

// Counts is the row census the status endpoint and the ptr page show.
type Counts struct {
	Hashes   int64 `json:"hashes"`
	Tags     int64 `json:"tags"`
	Mappings int64 `json:"mappings"`
	Siblings int64 `json:"siblings"`
	Parents  int64 `json:"parents"`
}

// ErrSchemaMismatch means the on-disk index was written by a different schema
// version and must be rebuilt (delete and re-sync).
var ErrSchemaMismatch = fmt.Errorf("ptr index schema mismatch; rebuild required")

// Page-cache budgets, in KB (sqlite's negative cache_size form), one per
// handle. Only the replay earns a large cache: its inserts land randomly in
// the mappings and hashes B-trees, which end far past any cache, so keeping
// the hot upper levels resident is what keeps the insert rate from sagging as
// the index grows. Nothing on the shared handle benefits - a lookup is a
// B-tree descent touching a handful of pages, and the census is a sequential
// scan that streams the whole index past the cache reusing nothing, so a large
// budget there is pollution the per-connection, never-shrinking cache then
// holds onto. A small one also bounds what the pool can hold at once.
const (
	replayCacheKB = 256 * 1024
	queryCacheKB  = 16 * 1024
)

// dsn builds a connection string for one handle. WAL with normal synchronous
// fsyncs only at each slice-transaction commit, so a crash loses at most the
// slice in flight, which the cursor makes resumable.
func dsn(path string, cacheKB int) string {
	return "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=cache_size(-" + strconv.Itoa(cacheKB) + ")" +
		"&_pragma=temp_store(memory)"
}

// idleConnWindow bounds how long the pool parks an idle connection. The page
// cache is per connection and only ever grows, so a connection that filled it
// during a sync would pin its cache for the life of the process - the pool
// keeps idle connections forever otherwise. Closing one hands the pages back
// to the OS; a GC cannot, since the memory is sqlite's rather than the Go
// heap's. The window outlasts the gap between two slice transactions
// (fetch_sleep, seconds) so a running sync keeps its warm cache, and undercuts
// the engine's caught-up poll so an idle index is not held open by the poll
// refreshing the timer. A lookup after the pool drains reopens cold, which
// costs it well under a millisecond of B-tree descent. A var so tests can
// shorten it.
var idleConnWindow = 2 * time.Minute

// OpenStore opens (creating if absent) the index under dataPath. The pragmas
// favor a large sequential build: WAL with normal synchronous fsyncs only at
// each slice-transaction commit, so a crash loses at most the slice in flight,
// which the cursor makes resumable. A schema-version mismatch is reported, not
// silently migrated.
func OpenStore(dataPath string) (*Store, error) {
	if err := os.MkdirAll(dataPath, 0o755); err != nil {
		return nil, fmt.Errorf("creating ptr data dir: %w", err)
	}
	// MkdirAll is a no-op (no error) on an existing but non-writable directory,
	// after which sqlite fails to create its file with a cryptic "unable to open
	// database file". Probe writability first so the error names the real cause.
	if err := checkWritable(dataPath); err != nil {
		return nil, err
	}
	path := filepath.Join(dataPath, dbFile)
	db, err := sql.Open("sqlite", dsn(path, queryCacheKB))
	if err != nil {
		return nil, fmt.Errorf("opening ptr index: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(idleConnWindow)
	// A single writer serializes the sync replay, on its own handle so its
	// large page cache cannot spread to the pool the lookups share.
	replay, err := sql.Open("sqlite", dsn(path, replayCacheKB))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening ptr index: %w", err)
	}
	replay.SetMaxOpenConns(1)
	replay.SetConnMaxIdleTime(idleConnWindow)
	s := &Store{db: db, replay: replay}
	if _, err := db.Exec(schema); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("creating ptr schema: %w", err)
	}
	if err := s.checkSchemaVersion(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// EnsureCounts seeds the persisted row census when it is missing - an index
// built before the census was kept incrementally. The one-time full scan can
// take minutes on a synced index, so it must run on the sync goroutine, never
// on the boot path (OpenStore is called before the HTTP listener binds, and a
// healthcheck that cannot reach /health kills the app mid-scan). ctx aborts
// the scan on shutdown. From then on Replay maintains the census and no table
// is ever re-counted.
func (s *Store) EnsureCounts(ctx context.Context) error {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM sync WHERE key=?`, countsKey).Scan(&v)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, ok, _ := s.Cursor(); ok {
		logx.Infof("ptr: counting existing index rows once to seed the census")
	}
	c, err := s.counts(ctx)
	if err != nil {
		return err
	}
	buf, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return upsertSync(s.db, countsKey, string(buf))
}

// execer is the single statement shape upsertSync needs; *sql.DB and
// *sql.Tx both satisfy it.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// upsertSync writes one sync KV pair, inserting or replacing.
func upsertSync(e execer, key, value string) error {
	_, err := e.Exec(`INSERT INTO sync(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// checkWritable reports whether the process can create files under dir, so a
// read-only mount (a root-owned volume under a non-root user) fails with a clear
// message instead of a sqlite open error.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe")
	if err != nil {
		return fmt.Errorf("ptr data path %q is not writable: %w", dir, err)
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return nil
}

// checkSchemaVersion records the schema version on a fresh index and refuses an
// index written by a different one.
func (s *Store) checkSchemaVersion() error {
	var v string
	err := s.db.QueryRow(`SELECT value FROM sync WHERE key='schema'`).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		_, err := s.db.Exec(`INSERT INTO sync(key,value) VALUES('schema',?)`, strconv.Itoa(schemaVersion))
		return err
	case err != nil:
		return err
	case v != strconv.Itoa(schemaVersion):
		return ErrSchemaMismatch
	}
	return nil
}

// Close closes the index.
func (s *Store) Close() error {
	err := s.db.Close()
	if rerr := s.replay.Close(); err == nil {
		err = rerr
	}
	return err
}

// Cursor returns the highest fully processed update index, and whether any has
// been processed yet.
func (s *Store) Cursor() (uint64, bool, error) {
	v, ok, err := syncValue(s.db, cursorKey)
	if err != nil || !ok {
		return 0, false, err
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return n, true, err
}

// cursorKey persists the highest fully processed update index.
const cursorKey = "cursor"

// countsKey persists the row census in the sync KV. Replay maintains it
// incrementally in the same transaction as the data, so the status counts
// never require re-scanning the multi-billion-row tables.
const countsKey = "counts"

// coveredKey persists the end of the last applied update's server-side time
// window, so the page can say how far in time the synced data reaches.
const coveredKey = "covered"

// CoveredThrough returns the persisted coverage timestamp, or 0 when no update
// carrying one has been applied yet.
func (s *Store) CoveredThrough() (int64, error) {
	v, ok, err := syncValue(s.db, coveredKey)
	if err != nil || !ok {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// SetCoveredThrough persists the coverage timestamp outside a replay - the
// backfill for an index synced before the timestamp was recorded, whose
// caught-up passes never replay an update.
func (s *Store) SetCoveredThrough(ts int64) error {
	return upsertSync(s.db, coveredKey, strconv.FormatInt(ts, 10))
}

// blobsKey persists how many update blobs have been replayed. Blob count
// tracks the repository's real volume (the server splits a window's rows into
// row-capped blobs) where the update count does not - most of the row volume
// sits in the manifest's later, many-blob entries - so it is what the
// progress fraction weights by.
const blobsKey = "blobs"

// BlobsApplied returns the persisted applied-blob census, and whether it has
// been recorded; an index synced before it existed reports false until the
// engine backfills it.
func (s *Store) BlobsApplied() (uint64, bool, error) {
	v, ok, err := syncValue(s.db, blobsKey)
	if err != nil || !ok {
		return 0, false, err
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return n, true, err
}

// SeedBlobsApplied persists the applied-blob census outside a replay - the
// backfill for an index synced before the census was recorded.
func (s *Store) SeedBlobsApplied(n uint64) error {
	return upsertSync(s.db, blobsKey, strconv.FormatUint(n, 10))
}

// Replay applies one update index's blobs in a single transaction and advances
// the cursor, the persisted census, and the coverage timestamp to that index
// atomically, so a crash or restart resumes at cursor+1 having re-fetched
// nothing. Blobs arrive compressed and each is inflated and parsed only when
// its pass wants it - definitions first, then content, per the repository's
// ordering rule - so an entry's memory is its compressed blobs plus one
// parsed update, and a hundred-blob entry stays affordable. The hashes and
// mappings counts advance by insert/delete deltas (re-counting those tables
// is what used to stall large syncs); the small tables are re-counted
// in-transaction when touched, which keeps the sibling upsert exact. It
// returns the new census and how many rows the entry carried. applied, when
// non-nil, is called after each blob's rows land, so the caller can show
// progress inside a long transaction.
func (s *Store) Replay(index uint64, coveredThrough int64, blobs [][]byte, applied func()) (c Counts, rows int64, err error) {
	tx, err := s.replay.Begin()
	if err != nil {
		return c, rows, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if c, err = scanCounts(tx); err != nil {
		return c, rows, err
	}
	var defs, content replayPass
	if defs, err = replayDefinitions(tx, blobs, applied); err != nil {
		return c, rows, err
	}
	if content, err = replayContent(tx, blobs, applied); err != nil {
		return c, rows, err
	}
	c.Hashes += defs.hashDelta
	c.Mappings += content.mapDelta
	rows = defs.rows + content.rows
	if err = recountTouched(tx, &c, defs, content); err != nil {
		return c, rows, err
	}
	var buf []byte
	if buf, err = json.Marshal(c); err != nil {
		return c, rows, err
	}
	if err = upsertSync(tx, countsKey, string(buf)); err != nil {
		return c, rows, err
	}
	if err = upsertSync(tx, cursorKey, strconv.FormatUint(index, 10)); err != nil {
		return c, rows, err
	}
	var blobsApplied uint64
	v, ok, err := syncValue(tx, blobsKey)
	if err != nil {
		return c, rows, err
	}
	if ok {
		if blobsApplied, err = strconv.ParseUint(v, 10, 64); err != nil {
			return c, rows, err
		}
	}
	if err = upsertSync(tx, blobsKey, strconv.FormatUint(blobsApplied+uint64(len(blobs)), 10)); err != nil {
		return c, rows, err
	}
	if coveredThrough > 0 {
		if err = upsertSync(tx, coveredKey, strconv.FormatInt(coveredThrough, 10)); err != nil {
			return c, rows, err
		}
	}
	return c, rows, tx.Commit()
}

// replayPass is what one pass over an entry's blobs changed: the row deltas it
// applied and which small tables it touched, so only those are re-counted.
type replayPass struct {
	hashDelta, mapDelta, rows int64
	touchedTags               bool
	touchedSiblings           bool
	touchedParents            bool
}

// replayDefinitions applies the entry's definition blobs, which must land
// before any content references their ids. It is also where an unknown blob
// kind fails the entry, so the content pass can simply skip what is not its
// own.
func replayDefinitions(tx *sql.Tx, blobs [][]byte, applied func()) (p replayPass, err error) {
	for _, raw := range blobs {
		kind, err := updateKind(raw)
		if err != nil {
			return p, err
		}
		if kind != typeDefinitionsUpdate {
			if kind != typeContentUpdate {
				return p, fmt.Errorf("update type %d is neither definitions (36) nor content (34)", kind)
			}
			continue
		}
		up, err := ParseUpdate(raw)
		if err != nil {
			return p, err
		}
		delta, err := applyDefinitions(tx, up.Definitions)
		if err != nil {
			return p, err
		}
		p.hashDelta += delta
		p.rows += up.Rows()
		p.touchedTags = p.touchedTags || len(up.Definitions.Tags) > 0
		if applied != nil {
			applied()
		}
	}
	return p, nil
}

// replayContent applies the entry's content blobs, whose ids the definitions
// pass has already defined.
func replayContent(tx *sql.Tx, blobs [][]byte, applied func()) (p replayPass, err error) {
	for _, raw := range blobs {
		kind, err := updateKind(raw)
		if err != nil {
			return p, err
		}
		if kind != typeContentUpdate {
			continue
		}
		up, err := ParseUpdate(raw)
		if err != nil {
			return p, err
		}
		delta, err := applyContent(tx, up.Content)
		if err != nil {
			return p, err
		}
		p.mapDelta += delta
		p.rows += up.Rows()
		p.touchedSiblings = p.touchedSiblings || len(up.Content.SiblingsAdd)+len(up.Content.SiblingsDel) > 0
		p.touchedParents = p.touchedParents || len(up.Content.ParentsAdd)+len(up.Content.ParentsDel) > 0
		if applied != nil {
			applied()
		}
	}
	return p, nil
}

// recountTouched re-counts only the small tables this entry changed; the
// multi-billion-row ones advance by the passes' deltas instead.
func recountTouched(tx *sql.Tx, c *Counts, defs, content replayPass) error {
	for _, rc := range []struct {
		touched bool
		table   string
		dst     *int64
	}{
		{defs.touchedTags, "tags", &c.Tags},
		{content.touchedSiblings, "siblings", &c.Siblings},
		{content.touchedParents, "parents", &c.Parents},
	} {
		if !rc.touched {
			continue
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + rc.table).Scan(rc.dst); err != nil {
			return err
		}
	}
	return nil
}

// rowQuerier is the single query shape the sync-KV reads need; *sql.Tx and
// *sql.DB both satisfy it, so a read inside a replay transaction and one
// outside share the same helper.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// syncValue reads one sync-KV scalar; ok=false means the key is absent, which
// on this table only ever means a brand-new index. The write side goes through
// upsertSync.
func syncValue(q rowQuerier, key string) (value string, ok bool, err error) {
	err = q.QueryRow(`SELECT value FROM sync WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// scanCounts reads the persisted census. A missing key only means a brand-new
// index.
func scanCounts(q rowQuerier) (Counts, error) {
	var c Counts
	v, ok, err := syncValue(q, countsKey)
	if err != nil || !ok {
		return c, err
	}
	return c, json.Unmarshal([]byte(v), &c)
}

// StoredCounts returns the persisted census without touching the data tables.
func (s *Store) StoredCounts() (Counts, error) {
	return scanCounts(s.db)
}

// applyDefinitions returns the hashes-count delta. INSERT OR REPLACE reports
// one affected row whether the id was new or redefined, so a redefinition
// overcounts by one - the repository's definitions are append-only, so in
// practice the delta is the insert count.
func applyDefinitions(tx *sql.Tx, defs *Definitions) (int64, error) {
	hst, err := tx.Prepare(`INSERT OR REPLACE INTO hashes(hash_id,hash) VALUES(?,?)`)
	if err != nil {
		return 0, err
	}
	defer hst.Close()
	var delta int64
	for id, hexHash := range defs.Hashes {
		raw, derr := hex.DecodeString(hexHash)
		if derr != nil || len(raw) != 32 {
			continue // not a sha256; skip rather than store a bad key
		}
		res, err := hst.Exec(int64(id), raw)
		if err != nil {
			return delta, err
		}
		if n, err := res.RowsAffected(); err == nil {
			delta += n
		}
	}
	tst, err := tx.Prepare(`INSERT OR REPLACE INTO tags(tag_id,tag) VALUES(?,?)`)
	if err != nil {
		return delta, err
	}
	defer tst.Close()
	for id, tag := range defs.Tags {
		if _, err := tst.Exec(int64(id), tag); err != nil {
			return delta, err
		}
	}
	return delta, nil
}

// applyContent returns the mappings-count delta; the sibling / parent counts
// are re-counted by the caller instead (tiny tables, and the sibling upsert's
// affected-rows tell insert and repoint apart no better than REPLACE does).
func applyContent(tx *sql.Tx, c *Content) (int64, error) {
	added, err := applyMappings(tx, c.MappingsAdd, true)
	if err != nil {
		return 0, err
	}
	removed, err := applyMappings(tx, c.MappingsDel, false)
	if err != nil {
		return added, err
	}
	if err := applyPairs(tx, `INSERT OR REPLACE INTO siblings(bad_tag_id,good_tag_id) VALUES(?,?)`, c.SiblingsAdd); err != nil {
		return added - removed, err
	}
	if err := applyPairs(tx, `DELETE FROM siblings WHERE bad_tag_id=? AND good_tag_id=?`, c.SiblingsDel); err != nil {
		return added - removed, err
	}
	if err := applyPairs(tx, `INSERT OR REPLACE INTO parents(child_tag_id,parent_tag_id) VALUES(?,?)`, c.ParentsAdd); err != nil {
		return added - removed, err
	}
	return added - removed, applyPairs(tx, `DELETE FROM parents WHERE child_tag_id=? AND parent_tag_id=?`, c.ParentsDel)
}

// applyMappings returns how many rows actually landed or left: OR IGNORE and
// DELETE both report affected rows exactly, so the mappings census stays
// exact without ever re-counting the table.
func applyMappings(tx *sql.Tx, ms []Mapping, add bool) (int64, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	q := `INSERT OR IGNORE INTO mappings(hash_id,tag_id) VALUES(?,?)`
	if !add {
		q = `DELETE FROM mappings WHERE hash_id=? AND tag_id=?`
	}
	st, err := tx.Prepare(q)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	var delta int64
	for _, m := range ms {
		for _, hid := range m.HashIDs {
			res, err := st.Exec(int64(hid), int64(m.TagID))
			if err != nil {
				return delta, err
			}
			if n, err := res.RowsAffected(); err == nil {
				delta += n
			}
		}
	}
	return delta, nil
}

func applyPairs(tx *sql.Tx, query string, pairs []Pair) error {
	if len(pairs) == 0 {
		return nil
	}
	st, err := tx.Prepare(query)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, p := range pairs {
		if _, err := st.Exec(int64(p.A), int64(p.B)); err != nil {
			return err
		}
	}
	return nil
}

// Counts re-counts every table - full scans, minutes on a synced index.
// Production reads the persisted census through StoredCounts (EnsureCounts
// seeds it via the unexported counts); this export exists for tests to
// verify a replayed index against a real count.
func (s *Store) Counts() (Counts, error) {
	return s.counts(context.Background())
}

func (s *Store) counts(ctx context.Context) (Counts, error) {
	var c Counts
	for _, q := range []struct {
		table string
		dst   *int64
	}{
		{"hashes", &c.Hashes}, {"tags", &c.Tags}, {"mappings", &c.Mappings},
		{"siblings", &c.Siblings}, {"parents", &c.Parents},
	} {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+q.table).Scan(q.dst); err != nil {
			return c, err
		}
	}
	return c, nil
}
