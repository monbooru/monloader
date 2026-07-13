package mapping

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// parseTagField reads a tag field that gallery-dl emits as either a list or a
// string, returning the names in order. A string field is split on whitespace
// and commas: most boorus space-join their tags, but some (realbooru) join with
// ", ", which would otherwise leave a comma stuck to every tag.
func parseTagField(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.FieldsFunc(t, func(r rune) bool {
			return unicode.IsSpace(r) || r == ','
		})
	default:
		return nil
	}
}

// parseNameField reads a manga credit field (author, group, parody, ...) that
// gallery-dl emits as either a list or a string. Unlike parseTagField it never
// splits on whitespace: a string credit is one name ("Nakatani Nio"), or
// several joined with commas, and splitting on spaces would fragment a
// multi-word name into per-word tags.
func parseNameField(v any) []string {
	if s, ok := v.(string); ok {
		var out []string
		for _, name := range strings.Split(s, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	return parseTagField(v)
}

// tagSuffixes returns the per-category suffixes present in meta as
// `tags_<suffix>` keys, sorted for deterministic output. The combined `tags`
// key and the `tag_string_*` variants are not per-category fields and are
// ignored here.
func tagSuffixes(meta map[string]any) []string {
	var out []string
	for k := range meta {
		if suffix, ok := strings.CutPrefix(k, "tags_"); ok && suffix != "" {
			out = append(out, suffix)
		}
	}
	sort.Strings(out)
	return out
}

// mangaTagFields maps the separate list-typed metadata fields that gallery-dl's
// manga/comic gallery extractors emit (nhentai, hitomi, mangadex, ...) onto
// monbooru categories. Unlike boorus, these sites do not expose tags_<category>
// keys, so the categorized tags would otherwise be dropped.
var mangaTagFields = []struct {
	field, category string
}{
	{"artist", "artist"},
	{"artists", "artist"},
	{"author", "artist"},
	{"authors", "artist"},
	{"circle", "artist"},
	{"group", "artist"},
	{"characters", "character"},
	{"character", "character"},
	{"parody", "copyright"},
	{"parodies", "copyright"},
	{"genre", "general"},
	{"genres", "general"},
	{"type", "meta"},
}

// baseCategory maps a gallery-dl tag suffix to a monbooru category per the
// universal table. keep=false drops the suffix entirely
// (invalid / deprecated are not real tags). Unknown suffixes fall back to
// general.
func baseCategory(suffix string) (category string, keep bool) {
	switch suffix {
	case "general":
		return "general", true
	case "artist":
		return "artist", true
	case "character":
		return "character", true
	case "copyright":
		return "copyright", true
	case "meta", "metadata":
		return "meta", true
	case "species":
		return "general", true // no monbooru equivalent
	case "lore":
		return "meta", true
	case "contributor":
		return "artist", true
	case "circle", "group":
		return "artist", true // doujin circle / group
	case "parody", "series":
		return "copyright", true // the parodied / source work (e-hentai, schalenetwork)
	case "studio":
		return "copyright", true // animation / production studio (sankaku)
	case "model":
		return "person", true // real-person model (realbooru and other photo boorus)
	case "faults":
		return "meta", true
	case "invalid", "deprecated":
		return "", false
	default:
		return "general", true // best-effort fallback
	}
}

// splitNamespace recognizes a `namespace:name` flat tag (zerochan, philomena
// boorus, ...) and maps the namespace to a monbooru category. ok is false when
// the prefix is not a recognized namespace, so the tag is kept verbatim as a
// general tag rather than being split on an incidental colon.
func splitNamespace(tag string) (category, name string, ok bool) {
	ns, rest, found := strings.Cut(tag, ":")
	if !found || rest == "" {
		return "", "", false
	}
	switch strings.ToLower(ns) {
	case "artist", "mangaka", "creator":
		return "artist", rest, true
	case "group", "circle":
		return "artist", rest, true
	case "character":
		return "character", rest, true
	case "copyright", "parody", "series", "game", "franchise", "studio":
		return "copyright", rest, true
	case "meta", "source", "medium":
		return "meta", rest, true
	case "general", "theme", "tag":
		return "general", rest, true
	}
	return "", "", false
}

// disallowedTagChars matches a run of characters outside monbooru's tag-name
// charset (internal/tags ValidateTagName). The category-separating ':' is in
// the set, so a "category:name" tag keeps its prefix.
var disallowedTagChars = regexp.MustCompile(`[^a-z0-9_()!@#$.~+:?<>=^-]+`)

// normalizeTag rewrites a tag into the form monbooru stores: lowercased, every
// run of characters monbooru's tag-name rule rejects collapsed to one underscore,
// then trimmed of leading/trailing underscores. Booru tags arrive with spaces,
// commas, slashes, and apostrophes monbooru would otherwise reject; a name with
// no usable characters (a CJK-only artist, since monbooru is ASCII-only) collapses
// to empty and is left out.
func normalizeTag(tag string) string {
	tag = strings.ToLower(tag)
	tag = disallowedTagChars.ReplaceAllString(tag, "_")
	tag = strings.Trim(tag, "_")
	if strings.HasSuffix(tag, ":") {
		return "" // a category prefix whose name folded away entirely
	}
	return tag
}

// formatTag renders a (category, name) pair as monbooru expects: bare for
// general, `category:name` otherwise.
func formatTag(category, name string) string {
	if category == "" || name == "" {
		return ""
	}
	if category == "general" {
		return name
	}
	return category + ":" + name
}
