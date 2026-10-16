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

	"github.com/monbooru/monloader/internal/shared/logx"
)

// Bump it on any schema change: a mismatched index is refused for a
// rebuild, never read wrong.
const schemaVersion = 1

const dbFile = "ptr.sqlite"

// Mappings are keyed hash-first with no reverse index, which roughly
// halves the disk cost.
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
CREATE INDEX IF NOT EXISTS parents_by_parent ON parents(parent_tag_id);
`

type Store struct {
	db     *sql.DB
	replay *sql.DB
}

type Counts struct {
	Hashes   int64 `json:"hashes"`
	Tags     int64 `json:"tags"`
	Mappings int64 `json:"mappings"`
	Siblings int64 `json:"siblings"`
	Parents  int64 `json:"parents"`
}

var errSchemaMismatch = fmt.Errorf("ptr index schema mismatch; rebuild required")

// Per-handle page caches in KiB. Only the replay's random inserts gain
// from a large one; lookups touch a few pages and the census scan reuses
// none, so on the pool it would only pin memory.
const (
	replayCacheKB = 256 * 1024
	queryCacheKB  = 16 * 1024
)

// Under WAL, synchronous(normal) may lose recent commits on power loss;
// each carries its cursor, so the sync resumes cleanly.
func dsn(path string, cacheKB int) string {
	return "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=cache_size(-" + strconv.Itoa(cacheKB) + ")" +
		"&_pragma=temp_store(memory)"
}

// Closing an idle connection is the only way to hand back its page cache.
// The window spans the gap between two replays but not the caught-up
// poll, which would keep it alive; a var so tests can shorten it.
var idleConnWindow = 2 * time.Minute

func OpenStore(dataPath string) (*Store, error) {
	if err := os.MkdirAll(dataPath, 0o755); err != nil {
		return nil, fmt.Errorf("creating ptr data dir: %w", err)
	}
	// MkdirAll succeeds on a read-only directory, and sqlite then fails
	// with a cryptic open error.
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
	// The replay gets its own one-connection handle, so its large cache
	// stays off the lookup pool.
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

// EnsureCounts seeds the census on an index built before it was kept
// incrementally.
func (s *Store) EnsureCounts(ctx context.Context) error {
	if _, ok, err := syncValue(s.db, countsKey); err != nil || ok {
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

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertSync(e execer, key, value string) error {
	_, err := e.Exec(`INSERT INTO sync(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

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
		return errSchemaMismatch
	}
	return nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	if rerr := s.replay.Close(); err == nil {
		err = rerr
	}
	return err
}

func syncUint(q rowQuerier, key string) (uint64, bool, error) {
	v, ok, err := syncValue(q, key)
	if err != nil || !ok {
		return 0, false, err
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return n, true, err
}

func (s *Store) Cursor() (uint64, bool, error) { return syncUint(s.db, cursorKey) }

const cursorKey = "cursor"

const countsKey = "counts"

const coveredKey = "covered"

const readyKey = "ready"

const addressKey = "address"

// BuiltFrom names the repository the index holds; an index older than the
// record claims the first address it is opened with.
func (s *Store) BuiltFrom(addr string) (string, error) {
	v, ok, err := syncValue(s.db, addressKey)
	if err != nil || ok {
		return v, err
	}
	return addr, upsertSync(s.db, addressKey, addr)
}

func (s *Store) EverReady() (bool, error) {
	_, ok, err := syncValue(s.db, readyKey)
	return ok, err
}

func (s *Store) MarkReady() error {
	return upsertSync(s.db, readyKey, "1")
}

func (s *Store) CoveredThrough() (int64, error) {
	v, ok, err := syncValue(s.db, coveredKey)
	if err != nil || !ok {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func (s *Store) SetCoveredThrough(ts int64) error {
	return upsertSync(s.db, coveredKey, strconv.FormatInt(ts, 10))
}

const blobsKey = "blobs"

func (s *Store) BlobsApplied() (uint64, bool, error) { return syncUint(s.db, blobsKey) }

func (s *Store) SeedBlobsApplied(n uint64) error {
	return upsertSync(s.db, blobsKey, strconv.FormatUint(n, 10))
}

// Replay commits an entry's rows with the cursor, census and coverage in one
// transaction, so a restart resumes at cursor+1 with nothing re-fetched. A
// cancel rolls the entry back: a large one replays for up to an hour, which
// a disable or a shutdown must not wait out.
func (s *Store) Replay(ctx context.Context, index uint64, coveredThrough int64, blobs [][]byte, applied func()) (c Counts, rows int64, err error) {
	tx, err := s.replay.BeginTx(ctx, nil)
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
	if defs, err = replayKind(ctx, tx, blobs, typeDefinitionsUpdate, applied); err != nil {
		return c, rows, err
	}
	if content, err = replayKind(ctx, tx, blobs, typeContentUpdate, applied); err != nil {
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
	blobsApplied, _, err := syncUint(tx, blobsKey)
	if err != nil {
		return c, rows, err
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

type replayPass struct {
	hashDelta, mapDelta, rows int64
	touchedTags               bool
	touchedSiblings           bool
	touchedParents            bool
}

// Run once per kind: definitions must land before content references their
// ids. An unknown blob kind fails the definitions pass, which lets the
// content pass skip what is not its own.
func replayKind(ctx context.Context, tx *sql.Tx, blobs [][]byte, want int, applied func()) (p replayPass, err error) {
	for _, raw := range blobs {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		kind, err := updateKind(raw)
		if err != nil {
			return p, err
		}
		if kind != want {
			if want == typeDefinitionsUpdate && kind != typeContentUpdate {
				return p, fmt.Errorf("update type %d is neither definitions (36) nor content (34)", kind)
			}
			continue
		}
		up, err := parseUpdate(raw)
		if err != nil {
			return p, err
		}
		if want == typeDefinitionsUpdate {
			delta, err := applyDefinitions(tx, up.Definitions)
			if err != nil {
				return p, err
			}
			p.hashDelta += delta
			p.touchedTags = p.touchedTags || len(up.Definitions.Tags) > 0
		} else {
			delta, err := applyContent(tx, up.Content)
			if err != nil {
				return p, err
			}
			p.mapDelta += delta
			p.touchedSiblings = p.touchedSiblings || len(up.Content.SiblingsAdd)+len(up.Content.SiblingsDel) > 0
			p.touchedParents = p.touchedParents || len(up.Content.ParentsAdd)+len(up.Content.ParentsDel) > 0
		}
		p.rows += up.Rows()
		if applied != nil {
			applied()
		}
	}
	return p, nil
}

// The small tables are re-counted: a sibling upsert's affected rows
// cannot tell an insert from a repoint. Hashes and mappings are too large
// and move by deltas.
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

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func syncValue(q rowQuerier, key string) (value string, ok bool, err error) {
	return scanOne[string](q.QueryRow(`SELECT value FROM sync WHERE key=?`, key))
}

// A missing row is an answer, ok false, not an error.
func scanOne[T any](row *sql.Row) (v T, ok bool, err error) {
	err = row.Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

func scanCounts(q rowQuerier) (Counts, error) {
	var c Counts
	v, ok, err := syncValue(q, countsKey)
	if err != nil || !ok {
		return c, err
	}
	return c, json.Unmarshal([]byte(v), &c)
}

func (s *Store) StoredCounts() (Counts, error) {
	return scanCounts(s.db)
}

// INSERT OR REPLACE counts a redefined id as new; the delta holds because
// the repository's definitions are append-only.
func applyDefinitions(tx *sql.Tx, defs *definitions) (int64, error) {
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

func applyContent(tx *sql.Tx, c *contentRows) (int64, error) {
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

// OR IGNORE, unlike OR REPLACE, reports affected rows exactly, which
// keeps the census exact.
func applyMappings(tx *sql.Tx, ms []mapping, add bool) (int64, error) {
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

func applyPairs(tx *sql.Tx, query string, pairs []pair) error {
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

// Scans every table, so production reads the census through StoredCounts.
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
