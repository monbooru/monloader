// Package kwdict coerces gallery-dl kwdict fields, whose types vary
// across sites and releases.
package kwdict

import "strconv"

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

// ID falls back to gallery_id, which manga extractors such as nhentai and
// hitomi key a post by, then to file.id, where civitai nests it.
func ID(m map[string]any) string {
	if s := String(m, "id"); s != "" {
		return s
	}
	if s := String(m, "gallery_id"); s != "" {
		return s
	}
	file, _ := m["file"].(map[string]any)
	return String(file, "id")
}

func Num(m map[string]any) int {
	if n := Int(m, "num"); n != 0 {
		return n
	}
	return Int(m, "no")
}

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
