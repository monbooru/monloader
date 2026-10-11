package ptr

import (
	"context"
	"maps"
	"slices"
	"strings"
)

const maxGraphDepth = 64

// A mega-parent can have thousands of children, and each one answered
// fans out a pair preview on the caller's side.
const maxImpliedBy = 200

// TagsForHash answers raw hydrus tags; mapping them to monbooru
// categories is the caller's job.
func (s *Store) TagsForHash(hashHex string) (tags []string, ok bool, err error) {
	hashID, ok, err := s.hashIDByHex(hashHex)
	if err != nil || !ok {
		return nil, false, err
	}
	closed, err := s.displayIdeals(hashID)
	if err != nil {
		return nil, false, err
	}
	names, err := s.tagNames(closed)
	if err != nil {
		return nil, false, err
	}
	return names, true, nil
}

func (s *Store) queryUint64s(query string, arg int64) ([]uint64, error) {
	return queryAll(s.db, func(id *uint64) []any { return []any{id} }, query, arg)
}

func (s *Store) tagIDsForHash(hashID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT tag_id FROM mappings WHERE hash_id=?`, int64(hashID))
}

// Only a missing row ends the chain: a failed query must not pass as an
// unresolved alias.
func (s *Store) resolveSibling(id uint64) (uint64, error) {
	seen := map[uint64]bool{id: true}
	for i := 0; i < maxGraphDepth; i++ {
		good, ok, err := scanOne[uint64](s.db.QueryRow(`SELECT good_tag_id FROM siblings WHERE bad_tag_id=?`, int64(id)))
		if err != nil {
			return 0, err
		}
		if !ok || seen[good] {
			return id, nil
		}
		seen[good] = true
		id = good
	}
	return id, nil
}

func (s *Store) parentClosure(seed map[uint64]bool) (map[uint64]bool, error) {
	out := maps.Clone(seed)
	frontier := slices.Collect(maps.Keys(seed))
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

func (s *Store) parentsOf(childID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT parent_tag_id FROM parents WHERE child_tag_id=? ORDER BY parent_tag_id`, int64(childID))
}

func (s *Store) displayIdeals(hashID uint64) (map[uint64]bool, error) {
	tagIDs, err := s.tagIDsForHash(hashID)
	if err != nil {
		return nil, err
	}
	ideals := map[uint64]bool{}
	for _, id := range tagIDs {
		ideal, err := s.resolveSibling(id)
		if err != nil {
			return nil, err
		}
		ideals[ideal] = true
	}
	return s.parentClosure(ideals)
}

// Mid-sync a mapping can outrun its tag's definition, so a missing id
// answers "" rather than an error.
func (s *Store) tagName(id uint64) (string, error) {
	name, _, err := scanOne[string](s.db.QueryRow(`SELECT tag FROM tags WHERE tag_id=?`, int64(id)))
	return name, err
}

func (s *Store) tagNames(ids map[uint64]bool) ([]string, error) {
	out := make([]string, 0, len(ids))
	for id := range ids {
		name, ok, err := scanOne[string](s.db.QueryRow(`SELECT tag FROM tags WHERE tag_id=?`, int64(id)))
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, name)
		}
	}
	return out, nil
}

type TagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Implications []string `json:"implications,omitempty"`
	ImpliedBy    []string `json:"implied_by,omitempty"`
}

// TagRange with Contains set walks its whole Prefix range, so a substring
// search needs a namespace prefix.
type TagRange struct {
	Prefix   string
	Contains []string
}

type TagMatch struct {
	Ideal        string
	Matched      []string
	Aliases      int
	Implications int
	ImpliedBy    int
}

func (s *Store) SearchTags(ctx context.Context, ranges []TagRange, scanCap int) (matches []TagMatch, truncated bool, err error) {
	seen := map[uint64]bool{}
	byIdeal := map[uint64]int{}
	for _, rng := range ranges {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		rows, n, err := s.scanRange(ctx, rng, scanCap)
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || n >= scanCap
		for _, row := range rows {
			if seen[row.id] {
				continue
			}
			seen[row.id] = true
			// Each row walks its sibling and graph tables on the pool
			// lookups share, so an abandoned keystroke stops here.
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			ideal, err := s.resolveSibling(row.id)
			if err != nil {
				return nil, false, err
			}
			at, ok := byIdeal[ideal]
			if !ok {
				m, err := s.tagMatch(ideal)
				if err != nil {
					return nil, false, err
				}
				at = len(matches)
				byIdeal[ideal] = at
				matches = append(matches, m)
			}
			matches[at].Matched = append(matches[at].Matched, row.name)
		}
	}
	return matches, truncated, nil
}

// LIMIT bounds the rows answered, not the rows LIKE walks past; only ctx
// stops an abandoned walk on the pool lookups share.
func (s *Store) scanRange(ctx context.Context, rng TagRange, cap int) (out []struct {
	id   uint64
	name string
}, n int, err error) {
	query := `SELECT tag_id, tag FROM tags WHERE tag >= ?`
	args := []any{rng.Prefix}
	if end := prefixEnd(rng.Prefix); end != "" {
		query += ` AND tag < ?`
		args = append(args, end)
	}
	if len(rng.Contains) > 0 {
		var clauses []string
		// Spellings that differ only in separators render the same pattern.
		seen := map[string]bool{}
		for _, term := range rng.Contains {
			for _, pattern := range containsPatterns(rng.Prefix, term) {
				if seen[pattern] {
					continue
				}
				seen[pattern] = true
				clauses = append(clauses, `tag LIKE ? ESCAPE '\'`)
				args = append(args, pattern)
			}
		}
		query += ` AND (` + strings.Join(clauses, " OR ") + `)`
	}
	query += ` ORDER BY tag LIMIT ?`
	args = append(args, cap)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var r struct {
			id   uint64
			name string
		}
		if err := rows.Scan(&r.id, &r.name); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, len(out), rows.Err()
}

func (s *Store) tagMatch(idealID uint64) (TagMatch, error) {
	name, err := s.tagName(idealID)
	if err != nil {
		return TagMatch{}, err
	}
	m := TagMatch{Ideal: name}
	aliases, err := s.aliasesOf(idealID)
	if err != nil {
		return TagMatch{}, err
	}
	m.Aliases = len(aliases)
	parents, children, err := s.clusterRelations(idealID, append(aliases, idealID))
	if err != nil {
		return TagMatch{}, err
	}
	m.Implications, m.ImpliedBy = len(parents), len(children)
	return m, nil
}

// Search rows and graph answers both come from here, so their counts
// cannot drift apart.
func (s *Store) clusterRelations(idealID uint64, cluster []uint64) (implications, impliedBy []string, err error) {
	parentIDs, err := s.relationsOfCluster(cluster, s.parentsOf)
	if err != nil {
		return nil, nil, err
	}
	implications, err = s.idealNames(parentIDs, idealID, 0)
	if err != nil {
		return nil, nil, err
	}
	childIDs, err := s.relationsOfCluster(cluster, s.childrenOf)
	if err != nil {
		return nil, nil, err
	}
	impliedBy, err = s.idealNames(childIDs, idealID, maxImpliedBy)
	if err != nil {
		return nil, nil, err
	}
	return implications, impliedBy, nil
}

func prefixEnd(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// Word starts only: a plain %term% fills the scan cap with mid-word hits.
// Separators stay LIKE's _ wildcard, as hydrus stores a word break as a
// space and booru imports keep the underscore.
func containsPatterns(namespace, term string) []string {
	e := strings.NewReplacer(`\`, `\\`, `%`, `\%`, " ", "_").Replace(term)
	return []string{namespace + e + "%", "% " + e + "%", `%\_` + e + "%", "%(" + e + "%", "%-" + e + "%"}
}

// TagGraph checks ctx per name: an abandoned batch would otherwise walk
// on and stack under the retry on the small pool.
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
	id, ok, err := s.tagIDByName(name)
	if err != nil {
		return TagInfo{}, err
	}
	if !ok {
		return TagInfo{Known: false}, nil
	}
	idealID, err := s.resolveSibling(id)
	if err != nil {
		return TagInfo{}, err
	}
	idealName, err := s.tagName(idealID)
	if err != nil {
		return TagInfo{}, err
	}
	info := TagInfo{Known: true, Ideal: idealName}
	aliasIDs, err := s.aliasesOf(idealID)
	if err != nil {
		return TagInfo{}, err
	}
	for _, aid := range aliasIDs {
		if aid == id {
			continue
		}
		name, err := s.tagName(aid)
		if err != nil {
			return TagInfo{}, err
		}
		if name != "" {
			info.Aliases = append(info.Aliases, name)
		}
	}
	// Declared edges only, not the closure: the caller mirrors these and
	// would offer undeclared ones back as contributions.
	cluster, err := s.clusterOf(id)
	if err != nil {
		return TagInfo{}, err
	}
	info.Implications, info.ImpliedBy, err = s.clusterRelations(idealID, cluster)
	if err != nil {
		return TagInfo{}, err
	}
	return info, nil
}

// A relation can sit on any spelling, so every relation question asks
// over the whole cluster.
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

// Sorted so a capped answer is deterministic.
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
		name, err := s.tagName(ideal)
		if err != nil {
			return nil, err
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

func (s *Store) childrenOf(parentID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT child_tag_id FROM parents WHERE parent_tag_id=? ORDER BY child_tag_id`, int64(parentID))
}

// One step only: the PTR's sibling chains are shallow in practice.
func (s *Store) aliasesOf(goodID uint64) ([]uint64, error) {
	return s.queryUint64s(`SELECT bad_tag_id FROM siblings WHERE good_tag_id=?`, int64(goodID))
}
