package ptr

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ContribStore holds staged contributions and the committed-history
// ledger in <data_path>/contrib.sqlite, beside the index it is
// meaningless without. Staged rows are durable operator intent, not a
// rebuildable cache: in the normal flow they live for seconds (the
// confirm that staged them commits them in the same request), and
// persist only when a send fails partway or the process dies mid-send.
type ContribStore struct {
	db *sql.DB
}

const contribFile = "contrib.sqlite"

// Contribution kinds. Tag carries the mapping tag / sibling bad /
// parent child; Tag2 the sibling good / parent parent.
const (
	ContribMappingAdd      = "mapping_add"
	ContribMappingPetition = "mapping_petition"
	ContribSibling         = "sibling"
	ContribParent          = "parent"
	ContribSiblingPetition = "sibling_petition"
	ContribParentPetition  = "parent_petition"
)

// Ledger outcomes; the empty string means pending.
const (
	OutcomeApplied   = "applied"
	OutcomeApproved  = "approved"
	OutcomeRemoved   = "removed"
	OutcomeRescinded = "rescinded"
)

// contribLogKeepDays bounds the committed-history ledger by age: the
// activity panel's year plus the week its Sunday-aligned first column
// can reach past it, so the panel is exact over its whole window.
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
`

// ContribItem is one staged (or failed) contribution.
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

// ContribLogRow is one committed-history entry.
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

// OpenContribStore opens (creating if absent) the contribution store
// under dataPath.
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
	// Rows a crash left mid-send go back to staged; the retry claims
	// them cleanly and the server's idempotency makes the re-send safe.
	if _, err := db.Exec(`UPDATE contrib_items SET status = 'staged' WHERE status = 'sending'`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("resetting sending rows: %w", err)
	}
	return &ContribStore{db: db}, nil
}

// Close releases the store handle.
func (s *ContribStore) Close() error { return s.db.Close() }

// RemoveContribStore deletes the store file, for the confirmed
// delete-ptr-data action; pending and historical contributions go with
// it.
func RemoveContribStore(dataPath string) error {
	base := filepath.Join(dataPath, contribFile)
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Stage inserts one item; duplicate reports a row with the same
// (kind, tag, tag2, hash) already waiting. A pair kind carries no hash;
// an empty blob rather than NULL keys it into the UNIQUE index, since
// SQLite treats NULLs as distinct and would let identical pairs pile up.
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

// claimWhere flips matching rows to sending in one transaction and
// returns them; two concurrent claims never share a row.
func (s *ContribStore) claimWhere(where string, args ...any) ([]ContribItem, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(
		`SELECT id, kind, tag, tag2, hash, reason, origin, status, error, created_at
		 FROM contrib_items WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanContribItems(rows)
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

// ClaimIDs claims exactly the accepted ids of one confirm.
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

// ClaimBacklog claims every unsent row - the retry path.
func (s *ContribStore) ClaimBacklog() ([]ContribItem, error) {
	return s.claimWhere(`status IN ('staged', 'failed')`)
}

// Complete moves one sent item into the ledger and prunes it to the
// newest contribLogKeep rows.
func (s *ContribStore) Complete(it ContribItem) (logID int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	outcome, outcomeAt := "", sql.Null[int64]{}
	if it.Kind == ContribMappingAdd {
		// Mapping adds apply immediately server-side; nothing to wait for.
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
	if _, err := tx.Exec(`DELETE FROM contrib_items WHERE id = ?`, it.ID); err != nil {
		return 0, err
	}
	// Rows still awaiting a janitor outcome are the dedup guard against
	// re-sending the same contribution; only settled rows age out.
	if _, err := tx.Exec(
		`DELETE FROM contrib_log WHERE outcome != '' AND committed_at < ?`,
		time.Now().AddDate(0, 0, -contribLogKeepDays).Unix(),
	); err != nil {
		return 0, err
	}
	logID, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return logID, tx.Commit()
}

// FailItem marks one claimed item failed, keeping the error for the
// ledger row.
func (s *ContribStore) FailItem(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE contrib_items SET status = 'failed', error = ? WHERE id = ?`, msg, id)
	return err
}

// Rescind deletes one unsent item exactly; a row already sending is
// left alone.
func (s *ContribStore) Rescind(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM contrib_items WHERE id = ? AND status IN ('staged', 'failed')`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RescindAll deletes every unsent item.
func (s *ContribStore) RescindAll() (int, error) {
	res, err := s.db.Exec(`DELETE FROM contrib_items WHERE status IN ('staged', 'failed')`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Unsent lists the staged and failed rows, oldest first.
func (s *ContribStore) Unsent() ([]ContribItem, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, tag, tag2, hash, reason, origin, status, error, created_at
		 FROM contrib_items ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanContribItems(rows)
}

// Counts reports the unsent backlog: total rows waiting and how many of
// them are failed leftovers. Rows mid-send are neither.
func (s *ContribStore) Counts() (unsent, failed int, err error) {
	err = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(status = 'failed'), 0) FROM contrib_items WHERE status != 'sending'`,
	).Scan(&unsent, &failed)
	return unsent, failed, err
}

// LogRows returns the newest slice of the ledger.
func (s *ContribStore) LogRows(limit int) ([]ContribLogRow, error) {
	return s.LogPage(limit, 0)
}

// LogDaily counts ledger rows per local calendar day since the given
// unix time. Bucketing happens here rather than in SQL so the day
// boundary follows the process timezone like every rendered date.
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

// LogCount is the total number of ledger rows, for paging the history.
func (s *ContribStore) LogCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM contrib_log`).Scan(&n)
	return n, err
}

// contribLogCols is the one SELECT list every ledger read scans, so a
// schema change touches one string.
const contribLogCols = `id, kind, tag, tag2, hash, reason, committed_at, outcome, COALESCE(outcome_at, 0)`

// scanLogRows collects a ledger query's rows, closing them.
func scanLogRows(rows *sql.Rows) ([]ContribLogRow, error) {
	defer func() { _ = rows.Close() }()
	var out []ContribLogRow
	for rows.Next() {
		var r ContribLogRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Tag, &r.Tag2, &r.Hash, &r.Reason, &r.CommittedAt, &r.Outcome, &r.OutcomeAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LogPage reads one page of the ledger, newest first.
func (s *ContribStore) LogPage(limit, offset int) ([]ContribLogRow, error) {
	rows, err := s.db.Query(
		`SELECT `+contribLogCols+` FROM contrib_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanLogRows(rows)
}

// LogRow reads one ledger entry.
func (s *ContribStore) LogRow(id int64) (*ContribLogRow, error) {
	var r ContribLogRow
	err := s.db.QueryRow(
		`SELECT `+contribLogCols+` FROM contrib_log WHERE id = ?`, id,
	).Scan(&r.ID, &r.Kind, &r.Tag, &r.Tag2, &r.Hash, &r.Reason, &r.CommittedAt, &r.Outcome, &r.OutcomeAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetOutcome stamps a ledger row's outcome.
func (s *ContribStore) SetOutcome(id int64, outcome string) error {
	_, err := s.db.Exec(
		`UPDATE contrib_log SET outcome = ?, outcome_at = ? WHERE id = ?`,
		outcome, time.Now().Unix(), id)
	return err
}

// PendingLog reports whether a pair suggestion or petition of this kind
// sits in the ledger without a terminal outcome - a suggestion in the
// janitor queue is invisible in the index, so without this the same
// pair would re-stage forever.
func (s *ContribStore) PendingLog(kind, tag, tag2 string) (bool, error) {
	return s.countPositive(
		`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND tag2 = ? AND outcome = ''`,
		kind, tag, tag2)
}

// countPositive answers an existence question the store asks as a count.
func (s *ContribStore) countPositive(query string, args ...any) (bool, error) {
	var n int
	err := s.db.QueryRow(query, args...).Scan(&n)
	return n > 0, err
}

// PendingMappingPetition reports whether a removal petition for
// (tag, hash) is staged or awaiting janitor review, so the preview can
// disable the affordance rather than offer a duplicate.
func (s *ContribStore) PendingMappingPetition(tag string, hash []byte) (bool, error) {
	return s.countPositive(
		`SELECT (SELECT COUNT(*) FROM contrib_items WHERE kind = ? AND tag = ? AND hash = ?)
		      + (SELECT COUNT(*) FROM contrib_log   WHERE kind = ? AND tag = ? AND hash = ? AND outcome = '')`,
		ContribMappingPetition, tag, hash, ContribMappingPetition, tag, hash)
}

// MappingAddCommittedSince reports whether an add for (tag, hash) was
// committed after the given timestamp. A committed add applies
// server-side immediately but only reaches the local index with the
// next synced update, so until the coverage timestamp passes the
// commit the index cannot answer for it and the log is the only
// witness. Once coverage catches up the index takes over, so an add a
// janitor later removed becomes re-addable.
func (s *ContribStore) MappingAddCommittedSince(tag string, hash []byte, since int64) (bool, error) {
	return s.countPositive(
		`SELECT COUNT(*) FROM contrib_log WHERE kind = ? AND tag = ? AND hash = ? AND committed_at > ?`,
		ContribMappingAdd, tag, hash, since)
}

// UnsentByKindTag reports whether an identical item already waits in
// the unsent set (for the preview's "unsent" verdict on mapping adds).
func (s *ContribStore) UnsentByKindTag(kind, tag string, hash []byte) (bool, error) {
	return s.countPositive(
		`SELECT COUNT(*) FROM contrib_items WHERE kind = ? AND tag = ? AND hash = ?`,
		kind, tag, hash)
}

func scanContribItems(rows *sql.Rows) ([]ContribItem, error) {
	defer func() { _ = rows.Close() }()
	var out []ContribItem
	for rows.Next() {
		var it ContribItem
		if err := rows.Scan(&it.ID, &it.Kind, &it.Tag, &it.Tag2, &it.Hash, &it.Reason, &it.Origin, &it.Status, &it.Error, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Diff rules: read-only checks the stage and commit paths run against
// the synced index so nothing the PTR already has (or cannot hold) is
// uploaded.

// tagIDByName resolves a raw hydrus tag to its index id.
func (idx *Store) tagIDByName(tag string) (uint64, bool, error) {
	var id uint64
	err := idx.db.QueryRow(`SELECT tag_id FROM tags WHERE tag = ? ORDER BY tag_id LIMIT 1`, tag).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (idx *Store) hashIDByHex(hashHex string) (uint64, bool, error) {
	raw, err := hexDecode32(hashHex)
	if err != nil {
		return 0, false, nil
	}
	var id uint64
	err = idx.db.QueryRow(`SELECT hash_id FROM hashes WHERE hash = ?`, raw).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// HashHasIdeal reports whether the hash effectively carries tag on its
// display view: sibling resolution on both sides plus the parent
// closure of the hash's ideals. A tag the PTR shows through an alias
// spelling or an implication is already there, and re-sending it would
// only add a redundant raw mapping. Comparing on the display view is
// what the hydrus pipeline does.
func (idx *Store) HashHasIdeal(hashHex, tag string) (bool, error) {
	has, err := idx.HashHasIdeals(hashHex, []string{tag})
	return has[tag], err
}

// HashHasIdeals answers HashHasIdeal for a whole list against one hash.
// The hash side - every raw mapping sibling-resolved, then the parent
// closure over the result - depends only on the hash, so a caller
// comparing a file's whole tag list derives it once here rather than
// once per tag. A tag the index does not know is absent from the answer.
func (idx *Store) HashHasIdeals(hashHex string, tags []string) (map[string]bool, error) {
	has := make(map[string]bool, len(tags))
	hashID, ok, err := idx.hashIDByHex(hashHex)
	if err != nil || !ok {
		return has, err
	}
	tagIDs, err := idx.tagIDsForHash(hashID)
	if err != nil {
		return nil, err
	}
	ideals := map[uint64]bool{}
	for _, id := range tagIDs {
		ideal, err := idx.resolveSibling(id)
		if err != nil {
			return nil, err
		}
		ideals[ideal] = true
	}
	displayed, err := idx.parentClosure(ideals)
	if err != nil {
		return nil, err
	}
	for _, tag := range tags {
		tagID, ok, err := idx.tagIDByName(tag)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		wantIdeal, err := idx.resolveSibling(tagID)
		if err != nil {
			return nil, err
		}
		has[tag] = displayed[wantIdeal]
	}
	return has, nil
}

// IdealTag resolves a raw tag spelling to its ideal form; ok=false when
// the index does not know the spelling.
func (idx *Store) IdealTag(tag string) (string, bool, error) {
	id, ok, err := idx.tagIDByName(tag)
	if err != nil || !ok {
		return "", false, err
	}
	idealID, err := idx.resolveSibling(id)
	if err != nil {
		return "", false, err
	}
	names, err := idx.tagNames(map[uint64]bool{idealID: true})
	if err != nil || len(names) == 0 {
		return "", false, err
	}
	return names[0], true, nil
}

// HashHasRaw reports whether the exact raw (tag, hash) mapping is
// current - what a removal petition must target; the ideal trick does
// not apply to removals.
func (idx *Store) HashHasRaw(hashHex, tag string) (bool, error) {
	tagID, ok, err := idx.tagIDByName(tag)
	if err != nil || !ok {
		return false, err
	}
	hashID, ok, err := idx.hashIDByHex(hashHex)
	if err != nil || !ok {
		return false, err
	}
	var n int
	err = idx.db.QueryRow(
		`SELECT COUNT(*) FROM mappings WHERE hash_id = ? AND tag_id = ?`,
		hashID, tagID,
	).Scan(&n)
	return n > 0, err
}

// RawTagsForHash returns the hash's current raw mappings with no
// sibling resolution or parent closure - the removal-petition
// candidates the preview offers.
func (idx *Store) RawTagsForHash(hashHex string) ([]string, error) {
	hashID, ok, err := idx.hashIDByHex(hashHex)
	if err != nil || !ok {
		return nil, err
	}
	tagIDs, err := idx.tagIDsForHash(hashID)
	if err != nil {
		return nil, err
	}
	set := map[uint64]bool{}
	for _, id := range tagIDs {
		set[id] = true
	}
	return idx.tagNames(set)
}

// SiblingCurrent reports whether the exact bad -> good pair is current.
func (idx *Store) SiblingCurrent(bad, good string) (bool, error) {
	return idx.pairCurrent(`SELECT COUNT(*) FROM siblings WHERE bad_tag_id = ? AND good_tag_id = ?`, bad, good)
}

// ParentCurrent reports whether the exact child -> parent pair is
// current.
func (idx *Store) ParentCurrent(child, parent string) (bool, error) {
	return idx.pairCurrent(`SELECT COUNT(*) FROM parents WHERE child_tag_id = ? AND parent_tag_id = ?`, child, parent)
}

// ParentEdgeCovered reports whether the PTR already implies "child implies
// parent", directly or through intermediates, with every tag taken as its
// cluster. Two reasons it is wider than the stored edge: the edge can sit on
// any spelling of either side (grey_hair's lives on gray_hair, and futanari's
// parent is declared as "solo focus" whose ideal is solo), and an edge the PTR
// derives through a chain is already in force, so suggesting the shortcut adds
// nothing. This is what decides whether a relation is worth contributing; the
// exact stored pair, which a petition has to name, is ParentCurrent.
func (idx *Store) ParentEdgeCovered(child, parent string) (bool, error) {
	childID, ok, err := idx.tagIDByName(child)
	if err != nil || !ok {
		return false, err
	}
	parentID, ok, err := idx.tagIDByName(parent)
	if err != nil || !ok {
		return false, err
	}
	frontier, err := idx.clusterOf(childID)
	if err != nil {
		return false, err
	}
	parentCluster, err := idx.clusterOf(parentID)
	if err != nil {
		return false, err
	}
	wanted := make(map[uint64]bool, len(parentCluster))
	for _, p := range parentCluster {
		wanted[p] = true
	}
	seen := make(map[uint64]bool, len(frontier))
	for _, c := range frontier {
		seen[c] = true
	}
	for depth := 0; depth < maxGraphDepth && len(frontier) > 0; depth++ {
		var next []uint64
		for _, c := range frontier {
			parents, err := idx.parentsOf(c)
			if err != nil {
				return false, err
			}
			for _, p := range parents {
				if wanted[p] {
					return true, nil
				}
				if seen[p] {
					continue
				}
				cluster, err := idx.clusterOf(p)
				if err != nil {
					return false, err
				}
				for _, m := range cluster {
					if !seen[m] {
						seen[m] = true
						next = append(next, m)
					}
				}
			}
		}
		frontier = next
	}
	return false, nil
}

// pairCurrent resolves two tag names and counts the relation rows the
// query names; a tag the index does not know cannot be in any pair.
func (idx *Store) pairCurrent(countQuery, a, b string) (bool, error) {
	aID, ok, err := idx.tagIDByName(a)
	if err != nil || !ok {
		return false, err
	}
	bID, ok, err := idx.tagIDByName(b)
	if err != nil || !ok {
		return false, err
	}
	var n int
	err = idx.db.QueryRow(countQuery, aID, bID).Scan(&n)
	return n > 0, err
}

// hexDecode32 decodes a 64-hex sha256, erroring on any other shape.
func hexDecode32(hashHex string) ([]byte, error) {
	raw, err := hex.DecodeString(hashHex)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("hash is %d bytes, want 32", len(raw))
	}
	return raw, nil
}

// Drop deletes one claimed row without a ledger entry - the commit
// engine's already-resolved and tag-filtered drops.
func (s *ContribStore) Drop(id int64) error {
	_, err := s.db.Exec(`DELETE FROM contrib_items WHERE id = ?`, id)
	return err
}

// StagedID finds the unsent row matching an identity, for the rescind
// flow when a petition it wants to stage already waits.
func (s *ContribStore) StagedID(kind, tag, tag2 string, hash []byte) (int64, bool, error) {
	// Stage writes a pair kind's absent hash as an empty blob, and `X'' IS NULL`
	// is false, so a nil here would miss the very row it just tried to insert.
	if hash == nil {
		hash = []byte{}
	}
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM contrib_items WHERE kind = ? AND tag = ? AND tag2 = ? AND hash IS ?`,
		kind, tag, tag2, hash,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// PendingOutcomeRows lists the ledger rows still awaiting a janitor
// outcome - everything but mapping adds, which are stamped applied at
// commit time.
func (s *ContribStore) PendingOutcomeRows() ([]ContribLogRow, error) {
	rows, err := s.db.Query(
		`SELECT `+contribLogCols+` FROM contrib_log WHERE outcome = '' AND kind != ? ORDER BY id`, ContribMappingAdd)
	if err != nil {
		return nil, err
	}
	return scanLogRows(rows)
}
