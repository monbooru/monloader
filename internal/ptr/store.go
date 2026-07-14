package ptr

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"github.com/leqwin/monloader/internal/logx"
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
	path   string
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
	s := &Store{db: db, replay: replay, path: path}
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO sync(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, countsKey, string(buf))
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

// Path is the index file path.
func (s *Store) Path() string { return s.path }

// Cursor returns the highest fully processed update index, and whether any has
// been processed yet.
func (s *Store) Cursor() (uint64, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM sync WHERE key='cursor'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return n, true, err
}

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
	var v string
	err := s.db.QueryRow(`SELECT value FROM sync WHERE key=?`, coveredKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// SetCoveredThrough persists the coverage timestamp outside a replay - the
// backfill for an index synced before the timestamp was recorded, whose
// caught-up passes never replay an update.
func (s *Store) SetCoveredThrough(ts int64) error {
	_, err := s.db.Exec(`INSERT INTO sync(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		coveredKey, strconv.FormatInt(ts, 10))
	return err
}

// Replay applies one update index's blobs in a single transaction and advances
// the cursor, the persisted census, and the coverage timestamp to that index
// atomically, so a crash or restart resumes at cursor+1 having re-fetched
// nothing. Definitions in the slice are applied before content, per the
// repository's ordering rule. The hashes and mappings counts advance by
// insert/delete deltas (re-counting those tables is what used to stall large
// syncs); the small tables are re-counted in-transaction when touched, which
// keeps the sibling upsert exact. It returns the new census.
func (s *Store) Replay(index uint64, coveredThrough int64, updates []*Update) (c Counts, err error) {
	tx, err := s.replay.Begin()
	if err != nil {
		return c, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if c, err = scanCounts(tx); err != nil {
		return c, err
	}
	var touchedTags, touchedSiblings, touchedParents bool
	for _, up := range updates {
		if up.Definitions != nil {
			var hashDelta int64
			if hashDelta, err = applyDefinitions(tx, up.Definitions); err != nil {
				return c, err
			}
			c.Hashes += hashDelta
			touchedTags = touchedTags || len(up.Definitions.Tags) > 0
		}
	}
	for _, up := range updates {
		if up.Content != nil {
			var mapDelta int64
			if mapDelta, err = applyContent(tx, up.Content); err != nil {
				return c, err
			}
			c.Mappings += mapDelta
			touchedSiblings = touchedSiblings || len(up.Content.SiblingsAdd)+len(up.Content.SiblingsDel) > 0
			touchedParents = touchedParents || len(up.Content.ParentsAdd)+len(up.Content.ParentsDel) > 0
		}
	}
	for _, rc := range []struct {
		touched bool
		table   string
		dst     *int64
	}{
		{touchedTags, "tags", &c.Tags},
		{touchedSiblings, "siblings", &c.Siblings},
		{touchedParents, "parents", &c.Parents},
	} {
		if !rc.touched {
			continue
		}
		if err = tx.QueryRow(`SELECT COUNT(*) FROM ` + rc.table).Scan(rc.dst); err != nil {
			return c, err
		}
	}
	var buf []byte
	if buf, err = json.Marshal(c); err != nil {
		return c, err
	}
	if _, err = tx.Exec(
		`INSERT INTO sync(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		countsKey, string(buf),
	); err != nil {
		return c, err
	}
	if _, err = tx.Exec(
		`INSERT INTO sync(key,value) VALUES('cursor',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.FormatUint(index, 10),
	); err != nil {
		return c, err
	}
	if coveredThrough > 0 {
		if _, err = tx.Exec(
			`INSERT INTO sync(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			coveredKey, strconv.FormatInt(coveredThrough, 10),
		); err != nil {
			return c, err
		}
	}
	return c, tx.Commit()
}

// rowQuerier is the single query shape scanCounts needs; *sql.Tx and *sql.DB
// both satisfy it.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// scanCounts reads the persisted census. A missing key only means a brand-new
// index.
func scanCounts(q rowQuerier) (Counts, error) {
	var c Counts
	var v string
	err := q.QueryRow(`SELECT value FROM sync WHERE key=?`, countsKey).Scan(&v)
	if err == sql.ErrNoRows {
		return c, nil
	}
	if err != nil {
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

// Counts re-counts every table - full scans, minutes on a synced index. Only
// EnsureCounts uses it, once, to seed the persisted census; everything else
// reads StoredCounts.
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
