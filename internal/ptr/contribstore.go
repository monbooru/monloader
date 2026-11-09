package ptr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/shared/contract"
)

// ContribStore's staged rows are operator intent, not a rebuildable
// cache: they outlive a failed or interrupted send.
type ContribStore struct {
	db *sql.DB
}

const contribFile = "contrib.sqlite"

// Tag is the mapping tag, the sibling's bad or the parent's child; Tag2
// the sibling's good or the parent's parent.
const (
	ContribMappingAdd      = contract.ContribMappingAdd
	ContribMappingPetition = contract.ContribMappingPetition
	ContribSibling         = contract.ContribSibling
	ContribParent          = contract.ContribParent
	ContribSiblingPetition = contract.ContribSiblingPetition
	ContribParentPetition  = contract.ContribParentPetition
)

// Ledger outcomes; the empty string means pending.
const (
	OutcomeApplied   = "applied"
	OutcomeApproved  = "approved"
	OutcomeRemoved   = "removed"
	OutcomeRescinded = "rescinded"
	OutcomeNotTaken  = "not_taken"
)

// A year plus the six days the activity panel's Sunday-aligned first
// column can reach past it.
const contribLogKeepDays = 371

const contribSchema = `
CREATE TABLE IF NOT EXISTS contrib_items (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,
    tag        TEXT NOT NULL,
    tag2       TEXT NOT NULL DEFAULT '',
    hash       BLOB,
    reason     TEXT NOT NULL DEFAULT '',
    origin     TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'staged',
    error      TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (kind, tag, tag2, hash)
);
CREATE TABLE IF NOT EXISTS contrib_log (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    tag          TEXT NOT NULL,
    tag2         TEXT NOT NULL DEFAULT '',
    hash         BLOB,
    reason       TEXT NOT NULL,
    committed_at INTEGER NOT NULL,
    outcome      TEXT NOT NULL DEFAULT '',
    outcome_at   INTEGER
);
CREATE INDEX IF NOT EXISTS contrib_log_kind_tag_hash ON contrib_log (kind, tag, hash);
CREATE INDEX IF NOT EXISTS contrib_log_committed_at ON contrib_log (committed_at);
`

type ContribItem struct {
	ID        int64
	Kind      string
	Tag       string
	Tag2      string
	Hash      []byte
	Reason    string
	Origin    string
	Status    string
	Error     string
	CreatedAt int64
}

type ContribLogRow struct {
	ID          int64
	Kind        string
	Tag         string
	Tag2        string
	Hash        []byte
	Reason      string
	CommittedAt int64
	Outcome     string
	OutcomeAt   int64
}

func OpenContribStore(dataPath string) (*ContribStore, error) {
	db, err := sql.Open("sqlite", dsn(filepath.Join(dataPath, contribFile), 1024))
	if err != nil {
		return nil, fmt.Errorf("opening contrib store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(contribSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating contrib schema: %w", err)
	}
	// Rows a crash left mid-send go back to staged; the server's
	// idempotency makes the re-send safe.
	if _, err := db.Exec(`UPDATE contrib_items SET status = 'staged' WHERE status = 'sending'`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("resetting sending rows: %w", err)
	}
	s := &ContribStore{db: db}
	if err := s.PruneLog(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pruning the contribution log: %w", err)
	}
	return s, nil
}

func (s *ContribStore) Close() error { return s.db.Close() }

func removeContribStore(dataPath string) error {
	base := filepath.Join(dataPath, contribFile)
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Stage stores a pair's absent hash as an empty blob, not NULL: SQLite
// treats NULLs as distinct in a UNIQUE index.
func (s *ContribStore) Stage(it ContribItem) (id int64, duplicate bool, err error) {
	hash := it.Hash
	if hash == nil {
		hash = []byte{}
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO contrib_items (kind, tag, tag2, hash, reason, origin, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		it.Kind, it.Tag, it.Tag2, hash, it.Reason, it.Origin, time.Now().Unix(),
	)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, true, nil
	}
	id, err = res.LastInsertId()
	return id, false, err
}

func (s *ContribStore) claimWhere(where string, args ...any) ([]ContribItem, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	items, err := queryAll(context.Background(), tx, (*ContribItem).fields,
		`SELECT `+contribItemCols+` FROM contrib_items WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if _, err := tx.Exec(`UPDATE contrib_items SET status = 'sending' WHERE id = ?`, it.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *ContribStore) ClaimIDs(ids []int64) ([]ContribItem, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	return s.claimWhere(`id IN (`+placeholders+`) AND status IN ('staged', 'failed')`, args...)
}

func (s *ContribStore) ClaimBacklog() ([]ContribItem, error) {
	return s.claimWhere(`status IN ('staged', 'failed')`)
}

func (s *ContribStore) Complete(it ContribItem) (logID int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	outcome, outcomeAt := "", sql.Null[int64]{}
	if it.Kind == ContribMappingAdd {
		// The server answers before it filters; the outcome pass settles a dropped add.
		outcome = OutcomeApplied
		outcomeAt = sql.Null[int64]{V: time.Now().Unix(), Valid: true}
	}
	res, err := tx.Exec(
		`INSERT INTO contrib_log (kind, tag, tag2, hash, reason, committed_at, outcome, outcome_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		it.Kind, it.Tag, it.Tag2, it.Hash, it.Reason, time.Now().Unix(), outcome, outcomeAt,
	)
	if err != nil {
		return 0, err
	}
	if it.Kind == ContribMappingPetition {
		if _, err := tx.Exec(
			`UPDATE contrib_log SET outcome = ?, outcome_at = ? WHERE kind = ? AND tag = ? AND hash = ? AND outcome = ?`,
			OutcomeRescinded, time.Now().Unix(), ContribMappingAdd, it.Tag, it.Hash, OutcomeApplied,
		); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM contrib_items WHERE id = ?`, it.ID); err != nil {
		return 0, err
	}
	logID, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return logID, tx.Commit()
}

// Unsettled rows never age out: they guard against re-sending what the
// repository has not answered yet.
func (s *ContribStore) PruneLog() error {
	_, err := s.db.Exec(
		`DELETE FROM contrib_log WHERE outcome != '' AND committed_at < ?`,
		time.Now().AddDate(0, 0, -contribLogKeepDays).Unix())
	return err
}

func (s *ContribStore) FailItem(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE contrib_items SET status = 'failed', error = ? WHERE id = ?`, msg, id)
	return err
}

func (s *ContribStore) Rescind(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM contrib_items WHERE id = ? AND status IN ('staged', 'failed')`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *ContribStore) RescindAll() (int, error) {
	res, err := s.db.Exec(`DELETE FROM contrib_items WHERE status IN ('staged', 'failed')`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Unsent leaves out rows mid-send, as Counts does, so the list and the
// count agree during a send.
func (s *ContribStore) Unsent() ([]ContribItem, error) {
	return queryAll(context.Background(), s.db, (*ContribItem).fields,
		`SELECT `+contribItemCols+` FROM contrib_items WHERE status != 'sending' ORDER BY id`)
}

func (s *ContribStore) Counts() (unsent, failed int, err error) {
	err = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(status = 'failed'), 0) FROM contrib_items WHERE status != 'sending'`,
	).Scan(&unsent, &failed)
	return unsent, failed, err
}

// LogDaily buckets in Go, not SQL, so a day follows the process timezone
// like every rendered date.
func (s *ContribStore) LogDaily(since int64) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT committed_at FROM contrib_log WHERE committed_at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		counts[time.Unix(at, 0).In(time.Local).Format("2006-01-02")]++
	}
	return counts, rows.Err()
}

func (s *ContribStore) LogCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM contrib_log`).Scan(&n)
	return n, err
}

const contribLogCols = `id, kind, tag, tag2, hash, reason, committed_at, outcome, COALESCE(outcome_at, 0)`

func (r *ContribLogRow) fields() []any {
	return []any{&r.ID, &r.Kind, &r.Tag, &r.Tag2, &r.Hash, &r.Reason, &r.CommittedAt, &r.Outcome, &r.OutcomeAt}
}

func (s *ContribStore) LogPage(limit, offset int) ([]ContribLogRow, error) {
	return queryAll(context.Background(), s.db, (*ContribLogRow).fields,
		`SELECT `+contribLogCols+` FROM contrib_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
}

func (s *ContribStore) LogRow(id int64) (*ContribLogRow, error) {
	var r ContribLogRow
	err := s.db.QueryRow(`SELECT `+contribLogCols+` FROM contrib_log WHERE id = ?`, id).Scan(r.fields()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *ContribStore) SetOutcome(id int64, outcome string) error {
	_, err := s.db.Exec(
		`UPDATE contrib_log SET outcome = ?, outcome_at = ? WHERE id = ?`,
		outcome, time.Now().Unix(), id)
	return err
}

// PendingLog sees suggestions in the janitor queue, which the index
// cannot; without it the same pair would re-stage forever.
func (s *ContribStore) PendingLog(kind, tag, tag2 string) (bool, error) {
	return countPositive(s.db,
		`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND tag2 = ? AND outcome = ''`,
		kind, tag, tag2)
}

func countPositive(q rowQuerier, query string, args ...any) (bool, error) {
	var n int
	err := q.QueryRow(query, args...).Scan(&n)
	return n > 0, err
}

func (s *ContribStore) PendingMappingPetition(tag string, hash []byte) (bool, error) {
	return countPositive(s.db,
		`SELECT (SELECT COUNT(*) FROM contrib_items WHERE kind = ? AND tag = ? AND hash = ?)
		      + (SELECT COUNT(*) FROM contrib_log   WHERE kind = ? AND tag = ? AND hash = ? AND outcome = '')`,
		ContribMappingPetition, tag, hash, ContribMappingPetition, tag, hash)
}

// MappingAddCommittedSince covers a committed add until coverage passes
// it: the add reaches the index only with a later update.
func (s *ContribStore) MappingAddCommittedSince(tag string, hash []byte, since int64) (bool, error) {
	return countPositive(s.db,
		`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND hash = ? AND committed_at > ?`,
		ContribMappingAdd, tag, hash, since)
}

// MappingAddNotTaken says the index passed a committed add without taking it:
// hydrus drops an add on a mapping in its deleted table and still answers 200.
func (s *ContribStore) MappingAddNotTaken(tag string, hash []byte) (bool, error) {
	return countPositive(s.db,
		`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND hash = ? AND outcome = ?`,
		ContribMappingAdd, tag, hash, OutcomeNotTaken)
}

func (s *ContribStore) UnsentByKindTag(kind, tag string, hash []byte) (bool, error) {
	return countPositive(s.db,
		`SELECT COUNT(*) FROM contrib_items WHERE kind = ? AND tag = ? AND hash = ?`,
		kind, tag, hash)
}

const contribItemCols = `id, kind, tag, tag2, hash, reason, origin, status, error, created_at`

func (it *ContribItem) fields() []any {
	return []any{&it.ID, &it.Kind, &it.Tag, &it.Tag2, &it.Hash, &it.Reason, &it.Origin, &it.Status, &it.Error, &it.CreatedAt}
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// fields names where each selected column lands, in SELECT order.
func queryAll[T any](ctx context.Context, q rowsQuerier, fields func(*T) []any, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		var v T
		if err := rows.Scan(fields(&v)...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *ContribStore) Drop(id int64) error {
	_, err := s.db.Exec(`DELETE FROM contrib_items WHERE id = ?`, id)
	return err
}

func (s *ContribStore) StagedID(kind, tag, tag2 string, hash []byte) (int64, bool, error) {
	// A pair's hash is stored as an empty blob, which IS NULL would not match.
	if hash == nil {
		hash = []byte{}
	}
	return scanOne[int64](s.db.QueryRow(
		`SELECT id FROM contrib_items WHERE kind = ? AND tag = ? AND tag2 = ? AND hash IS ?`,
		kind, tag, tag2, hash,
	))
}

// CoveredAddRows lists the adds the index should already show. A settled row
// stays listed, since committed_at is this machine's clock and the coverage the
// server's.
func (s *ContribStore) CoveredAddRows(covered int64) ([]ContribLogRow, error) {
	return queryAll(context.Background(), s.db, (*ContribLogRow).fields,
		`SELECT `+contribLogCols+` FROM contrib_log WHERE kind = ? AND outcome IN (?, ?) AND committed_at <= ? ORDER BY id`,
		ContribMappingAdd, OutcomeApplied, OutcomeNotTaken, covered)
}

// SetReason leaves a row mid-send alone: its reason already left with the post.
func (s *ContribStore) SetReason(id int64, reason string) error {
	_, err := s.db.Exec(`UPDATE contrib_items SET reason = ? WHERE id = ? AND status != 'sending'`, reason, id)
	return err
}

func (s *ContribStore) PendingOutcomeRows() ([]ContribLogRow, error) {
	return queryAll(context.Background(), s.db, (*ContribLogRow).fields,
		`SELECT `+contribLogCols+` FROM contrib_log WHERE outcome = '' AND kind != ? ORDER BY id`, ContribMappingAdd)
}
