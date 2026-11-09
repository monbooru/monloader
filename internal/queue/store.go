package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/monbooru/monloader/internal/shared/logx"
)

// Store is the queue's disk mirror, read only at boot; the in-memory
// queue stays authoritative.
type Store struct {
	db *sql.DB
}

const storeFile = "queue.sqlite"

const storeSchema = `
CREATE TABLE IF NOT EXISTS jobs (
    id     INTEGER PRIMARY KEY,
    status TEXT NOT NULL,
    data   TEXT NOT NULL,
    seq    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS counters (
    name TEXT PRIMARY KEY,
    day  TEXT NOT NULL,
    n    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS watches (
    id             INTEGER PRIMARY KEY,
    url            TEXT NOT NULL UNIQUE,
    interval_hours INTEGER NOT NULL,
    max_items      INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    last           TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS landed_books (
    url TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS blacklisted_books (
    url  TEXT PRIMARY KEY,
    meta TEXT NOT NULL
);
`

// Only these are URI syntax in a path; a URL encoding breaks relative and drive-letter paths.
var uriPathEscaper = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

func storeDSN(path string) string {
	return "file:" + uriPathEscaper.Replace(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)"
}

func OpenStore(dir string) (*Store, error) {
	db, err := sql.Open("sqlite", storeDSN(filepath.Join(dir, storeFile)))
	if err != nil {
		return nil, fmt.Errorf("opening queue store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(storeSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating queue store schema: %w", err)
	}
	// A database created before the write-order guard or the watch cap lacks the column.
	for _, c := range []struct{ table, column string }{{"jobs", "seq"}, {"watches", "max_items"}} {
		if err := addColumn(db, c.table, c.column, "INTEGER NOT NULL DEFAULT 0"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrating queue store schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func addColumn(db *sql.DB, table, column, def string) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_xinfo(?) WHERE name = ?`, table, column).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + def)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// storedJob carries the jobState fields tagged json:"-": the API drops
// them but a reloaded job needs them.
type storedJob struct {
	Job             jobState  `json:"job"`
	Subcategory     string    `json:"subcategory,omitempty"`
	Offset          int       `json:"offset,omitempty"`
	Auto            bool      `json:"auto,omitempty"`
	Priority        bool      `json:"priority,omitempty"`
	Background      bool      `json:"background,omitempty"`
	Budgeted        bool      `json:"budgeted,omitempty"`
	Watch           int64     `json:"watch,omitempty"`
	Target          int       `json:"target,omitempty"`
	ForceTo         int       `json:"force_to,omitempty"`
	ListedTo        int       `json:"listed_to,omitempty"`
	ContribIDs      []int64   `json:"contrib_ids,omitempty"`
	ContribBacklog  bool      `json:"contrib_backlog,omitempty"`
	StatusChangedAt time.Time `json:"status_changed_at"`
}

const jobIDHighWater = "job_id_high_water"

const bumpHighWater = `INSERT INTO counters (name, day, n) VALUES (?, '', ?)
	 ON CONFLICT(name) DO UPDATE SET n = max(n, excluded.n)`

func (s *Store) HighestJobID() (int64, error) {
	_, n, err := s.LoadCounter(jobIDHighWater)
	return int64(n), err
}

func (s *Store) SaveJob(j *Job) error {
	rec := storedJob{
		Job:             j.jobState,
		Subcategory:     j.Subcategory,
		Offset:          j.Offset,
		Auto:            j.Auto,
		Priority:        j.Priority,
		Background:      j.Background,
		Budgeted:        j.Budgeted,
		Watch:           j.Watch,
		Target:          j.Target,
		ForceTo:         j.ForceTo,
		ListedTo:        j.ListedTo,
		ContribIDs:      j.ContribIDs,
		ContribBacklog:  j.ContribBacklog,
		StatusChangedAt: j.StatusChangedAt,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// The workers and the API goroutines snapshot in one order but can Exec
	// in another; the seq guard keeps a stale snapshot from regressing the row.
	_, err = s.db.Exec(
		`INSERT INTO jobs (id, status, data, seq) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET status = excluded.status, data = excluded.data, seq = excluded.seq
		 WHERE excluded.seq >= jobs.seq`,
		j.ID, string(j.Status), string(data), j.seq,
	)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(bumpHighWater, jobIDHighWater, j.ID)
	return err
}

// SQLite refuses a statement with more than 32766 variables.
const deleteChunk = 5000

func (s *Store) DeleteJobs(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for chunk := range slices.Chunk(ids, deleteChunk) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		if _, err := tx.Exec(`DELETE FROM jobs WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadCounter(name string) (day string, n int, err error) {
	err = s.db.QueryRow(`SELECT day, n FROM counters WHERE name = ?`, name).Scan(&day, &n)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return day, n, err
}

func (s *Store) SaveCounter(name, day string, n int) error {
	_, err := s.db.Exec(
		`INSERT INTO counters (name, day, n) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET day = excluded.day, n = excluded.n`,
		name, day, n)
	return err
}

func (s *Store) LoadJobs() ([]*Job, error) {
	rows, err := s.db.Query(`SELECT data, seq FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	for rows.Next() {
		var data string
		var seq uint64
		if err := rows.Scan(&data, &seq); err != nil {
			logx.Warnf("queue: skipping an unreadable stored job: %v", err)
			continue
		}
		var rec storedJob
		if err := json.Unmarshal([]byte(data), &rec); err != nil {
			// One corrupt row must not cost the rest of the history.
			logx.Warnf("queue: skipping an undecodable stored job: %v", err)
			continue
		}
		st := rec.Job
		st.Subcategory = rec.Subcategory
		st.Offset = rec.Offset
		st.Auto = rec.Auto
		st.Priority = rec.Priority
		st.Background = rec.Background
		st.Budgeted = rec.Budgeted
		st.Watch = rec.Watch
		st.Target = rec.Target
		st.ForceTo = rec.ForceTo
		st.ListedTo = rec.ListedTo
		st.ContribIDs = rec.ContribIDs
		st.ContribBacklog = rec.ContribBacklog
		st.StatusChangedAt = rec.StatusChangedAt
		// Without the stored seq the guard would refuse the reloaded
		// job's writes.
		out = append(out, &Job{jobState: st, done: make(chan struct{}), seq: seq})
	}
	return out, rows.Err()
}

func (s *Store) BookLanded(url string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM landed_books WHERE url = ?`, url).Scan(&n)
	return n > 0, err
}

func (s *Store) MarkBookLanded(url string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO landed_books (url) VALUES (?)`, url)
	return err
}

// BlacklistedBook answers a nil meta for a book never recorded.
func (s *Store) BlacklistedBook(url string) (map[string]any, error) {
	var data string
	err := s.db.QueryRow(`SELECT meta FROM blacklisted_books WHERE url = ?`, url).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta map[string]any
	return meta, json.Unmarshal([]byte(data), &meta)
}

func (s *Store) MarkBookBlacklisted(url string, meta map[string]any) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO blacklisted_books (url, meta) VALUES (?, ?)`, url, string(data))
	return err
}

func (s *Store) InsertWatch(url string, intervalHours, maxItems int, createdAt time.Time, last WatchCheck) (int64, error) {
	data, err := json.Marshal(last)
	if err != nil {
		return 0, err
	}
	// Past every id ever issued, not only the live ones: a removed watch's
	// check may still be queued under its id.
	res, err := s.db.Exec(
		`INSERT INTO watches (id, url, interval_hours, max_items, created_at, last)
		 VALUES (max(coalesce((SELECT max(id) FROM watches), 0),
		             coalesce((SELECT n FROM counters WHERE name = ?), 0)) + 1, ?, ?, ?, ?, ?)`,
		watchIDHighWater, url, intervalHours, maxItems, createdAt.UTC().Format(time.RFC3339Nano), string(data))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrWatchExists
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetWatchInterval(id int64, intervalHours int) error {
	_, err := s.db.Exec(`UPDATE watches SET interval_hours = ? WHERE id = ?`, intervalHours, id)
	return err
}

func (s *Store) SetWatchItems(id int64, maxItems int) error {
	_, err := s.db.Exec(`UPDATE watches SET max_items = ? WHERE id = ?`, maxItems, id)
	return err
}

func (s *Store) SaveWatchCheck(id int64, last WatchCheck) error {
	data, err := json.Marshal(last)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE watches SET last = ? WHERE id = ?`, string(data), id)
	return err
}

const watchIDHighWater = "watch_id_high_water"

func (s *Store) DeleteWatch(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM watches WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(bumpHighWater, watchIDHighWater, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadWatches() ([]Watch, error) {
	rows, err := s.db.Query(`SELECT id, url, interval_hours, max_items, created_at, last FROM watches ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Watch
	for rows.Next() {
		var w Watch
		var created, last string
		if err := rows.Scan(&w.ID, &w.URL, &w.IntervalHours, &w.MaxItems, &created, &last); err != nil {
			// The table is a watch's only copy: one bad row must not cost
			// the others.
			logx.Warnf("queue: skipping an unreadable watch: %v", err)
			continue
		}
		w.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if last != "" {
			if err := json.Unmarshal([]byte(last), &w.Last); err != nil {
				logx.Warnf("queue: watch %d keeps no last check, its record is undecodable: %v", w.ID, err)
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
