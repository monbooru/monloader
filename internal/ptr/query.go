package ptr

import (
	"database/sql"
	"encoding/hex"
)

// maxGraphDepth bounds sibling-chain following and parent-closure recursion, so
// a cyclic or pathological graph cannot spin the query.
const maxGraphDepth = 64

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

// parentsOf returns the direct parent (implication) tag ids of a tag id.
func (s *Store) parentsOf(childID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT parent_tag_id FROM parents WHERE child_tag_id=?`, int64(childID))
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
// form, the other tags that alias to that ideal, and its implications (the
// transitive parent closure). All strings are raw hydrus tags.
type TagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Implications []string `json:"implications,omitempty"`
}

// TagGraph answers alias / implication questions for raw hydrus tag spellings.
// For each queried name it returns the ideal form, every alias that resolves to
// that ideal (the queried spelling excluded), and the implications. A name the
// index does not know returns Known=false.
func (s *Store) TagGraph(names []string) (map[string]TagInfo, error) {
	out := make(map[string]TagInfo, len(names))
	for _, name := range names {
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
	err := s.db.QueryRow(`SELECT tag_id FROM tags WHERE tag=? LIMIT 1`, name).Scan(&id)
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
	// Implications: the parent closure of the ideal, minus the ideal itself.
	closed, err := s.parentClosure(map[uint64]bool{idealID: true})
	if err != nil {
		return TagInfo{}, err
	}
	delete(closed, idealID)
	impl, err := s.tagNames(closed)
	if err != nil {
		return TagInfo{}, err
	}
	info.Implications = impl
	return info, nil
}

// aliasesOf returns the bad-tag ids that resolve (in one step) to a good tag id,
// plus that good id's own further chain is not followed here - the graph is
// shallow in practice and the ideal is already the chain end.
func (s *Store) aliasesOf(goodID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT bad_tag_id FROM siblings WHERE good_tag_id=?`, int64(goodID))
}
