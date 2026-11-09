// Package kwdict coerces gallery-dl kwdict fields, whose types vary
// across sites and releases.
package kwdict

import (
	"cmp"
	"encoding/json"
	"strconv"
	"strings"
)

func String(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// Path reads gallery-dl's format-string notation: author[handle] is
// m["author"]["handle"].
func Path(m map[string]any, path string) string {
	keys := strings.FieldsFunc(path, func(r rune) bool { return r == '[' || r == ']' })
	if len(keys) == 0 {
		return ""
	}
	last := len(keys) - 1
	for _, key := range keys[:last] {
		m, _ = m[key].(map[string]any)
	}
	return String(m, keys[last])
}

// ID falls back to gallery_id, which manga extractors such as nhentai and
// hitomi key a post by, then to file.id, where civitai nests it, then to
// bluesky's post_id.
func ID(m map[string]any) string {
	file, _ := m["file"].(map[string]any)
	return cmp.Or(String(m, "id"), String(m, "gallery_id"), String(file, "id"), String(m, "post_id"))
}

func Num(m map[string]any) int {
	return cmp.Or(Int(m, "num"), Int(m, "no"))
}

func Int(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
		f, _ := v.Float64()
		return int(f)
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}
