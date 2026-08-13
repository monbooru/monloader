package ptr

import (
	"context"
	"database/sql"
	"encoding/hex"
	"slices"
)

// maxGraphDepth bounds sibling-chain following and parent-closure recursion, so
// a cyclic or pathological graph cannot spin the query.
const maxGraphDepth = 64

// maxImpliedBy bounds a tag's reverse-implication answer: a mega-parent (a
// series every character implies) can have thousands of children, and the
// answer feeds a per-row pair-preview fan-out on the caller's side.
const maxImpliedBy = 200

// TagsForHash returns the PTR tags for a file sha256 (hex), sibling-resolved to
// their ideal form and closed over their parents (implications). The strings
// are raw hydrus tags (namespaced or bare); the caller maps them to monbooru
// categories. A hash the index does not know returns no tags and ok=false.
func (s *Store) TagsForHash(hashHex string) (tags []string, ok bool, err error) {
	raw, derr := hex.DecodeString(hashHex)
	if derr != nil || len(raw) != 32 {
		return nil, false, nil
	}
	var hashID uint64
	err = s.db.QueryRow(`SELECT hash_id FROM hashes WHERE hash=?`, raw).Scan(&hashID)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	tagIDs, err := s.tagIDsForHash(hashID)
	if err != nil {
		return nil, false, err
	}
	ideals := map[uint64]bool{}
	for _, id := range tagIDs {
		ideal, rerr := s.resolveSibling(id)
		if rerr != nil {
			return nil, false, rerr
		}
		ideals[ideal] = true
	}
	closed, err := s.parentClosure(ideals)
	if err != nil {
		return nil, false, err
	}
	names, err := s.tagNames(closed)
	if err != nil {
		return nil, false, err
	}
	return names, true, nil
}

// queryUint64s runs a one-argument query and collects its single uint64
// column.
func (s *Store) queryUint64s(query string, arg int64) ([]uint64, error) {
	rows, err := s.db.Query(query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// tagIDsForHash returns the tag ids mapped to a hash id.
func (s *Store) tagIDsForHash(hashID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT tag_id FROM mappings WHERE hash_id=?`, int64(hashID))
}

// resolveSibling follows the sibling (alias) chain from a tag id to its ideal,
// with a visit set so a cycle stops instead of looping. Only a missing row is
// a chain end; a real query failure propagates so the caller fails instead of
// silently answering with an unresolved alias.
func (s *Store) resolveSibling(id uint64) (uint64, error) {
	seen := map[uint64]bool{id: true}
	for i := 0; i < maxGraphDepth; i++ {
		var good uint64
		err := s.db.QueryRow(`SELECT good_tag_id FROM siblings WHERE bad_tag_id=?`, int64(id)).Scan(&good)
		if err == sql.ErrNoRows {
			return id, nil
		}
		if err != nil {
			return 0, err
		}
		if seen[good] {
			return id, nil
		}
		seen[good] = true
		id = good
	}
	return id, nil
}

// parentClosure adds the transitive parents (implications) of every tag in the
// seed set, each itself sibling-resolved, bounded by depth.
func (s *Store) parentClosure(seed map[uint64]bool) (map[uint64]bool, error) {
	out := map[uint64]bool{}
	for id := range seed {
		out[id] = true
	}
	frontier := make([]uint64, 0, len(seed))
	for id := range seed {
		frontier = append(frontier, id)
	}
	for depth := 0; depth < maxGraphDepth && len(frontier) > 0; depth++ {
		var next []uint64
		for _, id := range frontier {
			parents, err := s.parentsOf(id)
			if err != nil {
				return nil, err
			}
			for _, p := range parents {
				ideal, err := s.resolveSibling(p)
				if err != nil {
					return nil, err
				}
				if !out[ideal] {
					out[ideal] = true
					next = append(next, ideal)
				}
			}
		}
		frontier = next
	}
	return out, nil
}

// parentsOf returns the direct parent (implication) tag ids of a tag id, in a
// stable order so the graph answer does not shuffle between runs.
func (s *Store) parentsOf(childID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT parent_tag_id FROM parents WHERE child_tag_id=? ORDER BY parent_tag_id`, int64(childID))
}

// tagNames resolves a set of tag ids to their strings, dropping any id without a
// definition (a mapping can reference a tag defined in a not-yet-processed
// update during an incomplete sync).
func (s *Store) tagNames(ids map[uint64]bool) ([]string, error) {
	out := make([]string, 0, len(ids))
	for id := range ids {
		var name string
		err := s.db.QueryRow(`SELECT tag FROM tags WHERE tag_id=?`, int64(id)).Scan(&name)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

// TagInfo is what the PTR knows about one tag: its ideal (sibling-resolved)
// form, the other tags that alias to that ideal, its implications (the
// transitive parent closure), and the tags that imply it (direct children
// only - declared pairs, not a descendant closure, which explodes on a
// mega-parent). All strings are raw hydrus tags.
type TagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Implications []string `json:"implications,omitempty"`
	ImpliedBy    []string `json:"implied_by,omitempty"`
}

// TagGraph answers alias / implication questions for raw hydrus tag spellings.
// For each queried name it returns the ideal form, every alias that resolves to
// that ideal (the queried spelling excluded), and the implications. A name the
// index does not know returns Known=false. A batch is hundreds of names deep
// and each one walks the graph, so ctx is checked between them: a caller that
// timed out and retried would otherwise leave the first walk running to
// completion and stack a second on the same small connection pool.
func (s *Store) TagGraph(ctx context.Context, names []string) (map[string]TagInfo, error) {
	out := make(map[string]TagInfo, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := s.tagInfo(name)
		if err != nil {
			return nil, err
		}
		out[name] = info
	}
	return out, nil
}

func (s *Store) tagInfo(name string) (TagInfo, error) {
	var id uint64
	// tags carries no unique index on the spelling, so order the pick: a
	// repository anomaly publishing two ids for one tag must at least resolve
	// the same way on every query.
	err := s.db.QueryRow(`SELECT tag_id FROM tags WHERE tag=? ORDER BY tag_id LIMIT 1`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return TagInfo{Known: false}, nil
	}
	if err != nil {
		return TagInfo{}, err
	}
	idealID, err := s.resolveSibling(id)
	if err != nil {
		return TagInfo{}, err
	}
	idealName, err := s.tagNames(map[uint64]bool{idealID: true})
	if err != nil {
		return TagInfo{}, err
	}
	info := TagInfo{Known: true}
	if len(idealName) > 0 {
		info.Ideal = idealName[0]
	}
	// Aliases: every tag that resolves to the same ideal, minus the query.
	aliasIDs, err := s.aliasesOf(idealID)
	if err != nil {
		return TagInfo{}, err
	}
	for _, aid := range aliasIDs {
		if aid == id {
			continue
		}
		names, err := s.tagNames(map[uint64]bool{aid: true})
		if err != nil {
			return TagInfo{}, err
		}
		info.Aliases = append(info.Aliases, names...)
	}
	// Implications and implied-by: the direct edges declared on any spelling in
	// the cluster, each end resolved to its ideal. Only declared edges - the
	// ancestor closure belongs to the file lookup, where every implied tag has
	// to land on the file; handing it out here would have the caller mirror
	// edges the PTR never declared and then offer them back as contributions.
	cluster, err := s.clusterOf(id)
	if err != nil {
		return TagInfo{}, err
	}
	parentIDs, err := s.relationsOfCluster(cluster, s.parentsOf)
	if err != nil {
		return TagInfo{}, err
	}
	info.Implications, err = s.idealNames(parentIDs, idealID, 0)
	if err != nil {
		return TagInfo{}, err
	}
	childIDs, err := s.relationsOfCluster(cluster, s.childrenOf)
	if err != nil {
		return TagInfo{}, err
	}
	info.ImpliedBy, err = s.idealNames(childIDs, idealID, maxImpliedBy)
	if err != nil {
		return TagInfo{}, err
	}
	return info, nil
}

// clusterOf returns the tag ids sharing an ideal with id: the ideal itself and
// the aliases resolving to it. A declared relation can sit on any spelling in
// the cluster, so every question about one has to ask over the whole set - a
// check that pins an endpoint to its ideal alone denies edges the graph
// reports, and the two sides then disagree about what the PTR holds.
func (s *Store) clusterOf(id uint64) ([]uint64, error) {
	ideal, err := s.resolveSibling(id)
	if err != nil {
		return nil, err
	}
	aliases, err := s.aliasesOf(ideal)
	if err != nil {
		return nil, err
	}
	return append(aliases, ideal), nil
}

// relationsOfCluster collects one relation side across every spelling in the
// cluster, deduped and id-ordered so a capped answer is deterministic.
func (s *Store) relationsOfCluster(cluster []uint64, of func(uint64) ([]uint64, error)) ([]uint64, error) {
	var out []uint64
	for _, id := range cluster {
		ids, err := of(id)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// idealNames resolves each tag id to its ideal's name, dropping exclude and
// repeats and stopping at limit (0 = no limit).
func (s *Store) idealNames(ids []uint64, exclude uint64, limit int) ([]string, error) {
	var out []string
	seen := map[uint64]bool{exclude: true}
	for _, id := range ids {
		if limit > 0 && len(out) >= limit {
			break
		}
		ideal, err := s.resolveSibling(id)
		if err != nil {
			return nil, err
		}
		if seen[ideal] {
			continue
		}
		seen[ideal] = true
		names, err := s.tagNames(map[uint64]bool{ideal: true})
		if err != nil {
			return nil, err
		}
		out = append(out, names...)
	}
	return out, nil
}

// childrenOf returns the direct child (implication) tag ids of a tag id, in a
// stable order so the capped answer is deterministic.
func (s *Store) childrenOf(parentID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT child_tag_id FROM parents WHERE parent_tag_id=? ORDER BY child_tag_id`, int64(parentID))
}

// aliasesOf returns the bad-tag ids that resolve (in one step) to a good tag id,
// plus that good id's own further chain is not followed here - the graph is
// shallow in practice and the ideal is already the chain end.
func (s *Store) aliasesOf(goodID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT bad_tag_id FROM siblings WHERE good_tag_id=?`, int64(goodID))
}
