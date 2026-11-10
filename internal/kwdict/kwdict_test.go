package kwdict

import (
	"encoding/json"
	"testing"
)

func TestNum(t *testing.T) {
	if got := Num(map[string]any{"num": float64(3), "no": float64(108984043)}); got != 3 {
		t.Errorf("Num = %d, want 3 (num present)", got)
	}
	if got := Num(map[string]any{"no": float64(108984043)}); got != 108984043 {
		t.Errorf("Num = %d, want 108984043 (from no)", got)
	}
	if got := Num(map[string]any{"num": json.Number("4")}); got != 4 {
		t.Errorf("Num = %d, want 4 (json number)", got)
	}
	if got := Int(map[string]any{"x": json.Number("12.5")}, "x"); got != 12 {
		t.Errorf("Int = %d, want 12 (fractional json number)", got)
	}
	if got := Num(map[string]any{}); got != 0 {
		t.Errorf("Num = %d, want 0", got)
	}
}

func TestID(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{"top-level id", map[string]any{"id": float64(7), "gallery_id": float64(8), "file": map[string]any{"id": float64(9)}}, "7"},
		{"gallery id", map[string]any{"gallery_id": float64(8), "file": map[string]any{"id": float64(9)}}, "8"},
		{"nested file id", map[string]any{"file": map[string]any{"id": float64(9)}}, "9"},
		{"post id", map[string]any{"post_id": "3mz4"}, "3mz4"},
		{"id over post id", map[string]any{"id": float64(7), "post_id": "3mz4"}, "7"},
		{"none", map[string]any{"file": "not an object"}, ""},
		{"json number", map[string]any{"id": json.Number("735284192837419123")}, "735284192837419123"},
	} {
		if got := ID(tc.meta); got != tc.want {
			t.Errorf("%s: ID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPath(t *testing.T) {
	m := map[string]any{
		"post_id": "3mz4",
		"author":  map[string]any{"handle": "bsky.app", "labels": map[string]any{"count": json.Number("2")}},
	}
	for _, tc := range []struct{ path, want string }{
		{"post_id", "3mz4"},
		{"author[handle]", "bsky.app"},
		{"author[labels][count]", "2"},
		{"author[missing]", ""},
		{"post_id[handle]", ""},
		{"missing[handle]", ""},
		{"", ""},
	} {
		if got := Path(m, tc.path); got != tc.want {
			t.Errorf("Path(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
