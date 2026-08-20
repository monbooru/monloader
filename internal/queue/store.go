package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/monbooru/monloader/internal/logx"
)

// Store is the queue's durable mirror: one row per tracked job on the
// /config volume, written through on every state transition and read
// only at boot. The in-memory queue stays authoritative while the app
// runs; a write failure logs and never fails the job, so a read-only
// or full /config degrades to the old volatile behavior.
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
`

// storeDSN mirrors the ptr index's pragmas: WAL with normal synchronous
// fsyncs at commit; the traffic is one small row per job transition, so
// no cache tuning is needed.
func storeDSN(path string) string {
	return "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)"
}

// OpenStore opens (creating if absent) the queue mirror inside dir, the
// directory holding monloader.toml.
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
	// A database created before the write-order guard lacks the column.
	if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("migrating queue store schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the store handle.
func (s *Store) Close() error { return s.db.Close() }

// storedJob wraps jobState with the fields its JSON tags drop from the
// API payload but a rehydrated job still needs (a continuation's window
// offset, the fetch-all flag, FIFO priority, the state-change stamp).
type storedJob struct {
	Job             jobState  `json:"job"`
	Offset          int       `json:"offset,omitempty"`
	Auto            bool      `json:"auto,omitempty"`
	Priority        bool      `json:"priority,omitempty"`
	Background      bool      `json:"background,omitempty"`
	Budgeted        bool      `json:"budgeted,omitempty"`
	ContribIDs      []int64   `json:"contrib_ids,omitempty"`
	ContribBacklog  bool      `json:"contrib_backlog,omitempty"`
	StatusChangedAt time.Time `json:"status_changed_at"`
}

// jobIDHighWater names the counter row holding the highest job id ever handed
// out. Ids are otherwise seeded from the surviving rows, so a clear (or the
// retention sweep) plus a restart would reissue ids monbooru still holds as
// reconcile handles, and a stale one would resolve against an unrelated job.
const jobIDHighWater = "job_id_high_water"

// HighestJobID reads the id high-water mark, 0 when nothing was ever saved.
func (s *Store) HighestJobID() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT n FROM counters WHERE name = ?`, jobIDHighWater).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// SaveJob upserts one job snapshot and advances the id high-water mark.
func (s *Store) SaveJob(j *Job) error {
	rec := storedJob{
		Job:             j.jobState,
		Offset:          j.Offset,
		Auto:            j.Auto,
		Priority:        j.Priority,
		Background:      j.Background,
		Budgeted:        j.Budgeted,
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
	// The mark rides the job write rather than the enqueue, so it needs no lock
	// of its own and survives the row it came from being deleted.
	_, err = s.db.Exec(
		`INSERT INTO counters (name, day, n) VALUES (?, '', ?)
		 ON CONFLICT(name) DO UPDATE SET n = max(n, excluded.n)`,
		jobIDHighWater, j.ID)
	return err
}

// DeleteJobs removes the rows for the given ids.
func (s *Store) DeleteJobs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	_, err := s.db.Exec(`DELETE FROM jobs WHERE id IN (`+placeholders+`)`, args...)
	return err
}

// LoadCounter reads a daily counter's stamped day and value.
func (s *Store) LoadCounter(name string) (day string, n int, err error) {
	err = s.db.QueryRow(`SELECT day, n FROM counters WHERE name = ?`, name).Scan(&day, &n)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return day, n, err
}

// SaveCounter upserts a daily counter. A restart must not refill a spent
// budget, so this is written through on every increment.
func (s *Store) SaveCounter(name, day string, n int) error {
	_, err := s.db.Exec(
		`INSERT INTO counters (name, day, n) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET day = excluded.day, n = excluded.n`,
		name, day, n)
	return err
}

// LoadJobs reads every stored job in id order, reconstructing live Job
// values. The caller (UseStore) decides where each lands from its
// stored status.
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
		st.Offset = rec.Offset
		st.Auto = rec.Auto
		st.Priority = rec.Priority
		st.Background = rec.Background
		st.Budgeted = rec.Budgeted
		st.ContribIDs = rec.ContribIDs
		st.ContribBacklog = rec.ContribBacklog
		st.StatusChangedAt = rec.StatusChangedAt
		// Carry the row's seq forward so the reloaded job's next writes
		// still land above what the previous life persisted.
		out = append(out, &Job{jobState: st, done: make(chan struct{}), seq: seq})
	}
	return out, rows.Err()
}
