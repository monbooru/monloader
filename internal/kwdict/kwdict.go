// Package kwdict coerces values out of a gallery-dl metadata kwdict, whose
// field types vary across sites and gallery-dl releases: a post id arrives as a
// number or a string, page numbers as either. The gdl, mapping, and pipeline
// packages share these so the coercion rules cannot drift apart.
package kwdict

import "strconv"

// String returns m[key] as a string, formatting a numeric value, or "" when the
// key is absent or carries another type.
func String(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// ID returns the post id. gallery-dl emits it as a number or a string;
// manga/comic gallery extractors (nhentai, hitomi, ...) key the post by
// gallery_id instead, so fall back to it for a stable bundle id.
func ID(m map[string]any) string {
	if s := String(m, "id"); s != "" {
		return s
	}
	return String(m, "gallery_id")
}

// Num is the per-file ordinal that tells a post's files apart: `num`, falling
// back to `no` when num is empty.
func Num(m map[string]any) int {
	if n := Int(m, "num"); n != 0 {
		return n
	}
	return Int(m, "no")
}

// Int returns m[key] as an int, parsing a string form, or 0.
func Int(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}
