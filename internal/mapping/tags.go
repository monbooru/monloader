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

// baseCategories maps a gallery-dl tag suffix to a monbooru category per the
// universal table. An empty category drops the suffix entirely (invalid and
// deprecated are not real tags).
var baseCategories = map[string]string{
	"general":     "general",
	"artist":      "artist",
	"character":   "character",
	"copyright":   "copyright",
	"meta":        "meta",
	"metadata":    "meta", // gelbooru's spelling
	"species":     "species",
	"lore":        "meta",
	"contributor": "artist",
	"circle":      "artist",    // doujin circle
	"group":       "artist",    // doujin group
	"parody":      "copyright", // the parodied work
	"series":      "copyright", // the source work (e-hentai, schalenetwork)
	"studio":      "copyright", // animation / production studio (sankaku)
	"model":       "person",    // real-person model (realbooru and other photo boorus)
	"faults":      "meta",
	"invalid":     "",
	"deprecated":  "",
}

// baseCategory looks a suffix up in the universal table. keep=false drops the
// tag; an unknown suffix falls back to general.
func baseCategory(suffix string) (category string, keep bool) {
	cat, ok := baseCategories[suffix]
	if !ok {
		return "general", true // best-effort fallback
	}
	return cat, cat != ""
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

// normalizeTag rewrites a tag into the form monbooru stores: lowercased, control
// runes dropped, surrounding whitespace trimmed and internal whitespace runs
// folded to `_`, and the grammar-reserved `"` and `*` folded to `_`. Every other
// rune - accents, CJK, the full ASCII punctuation - is kept, so a PTR or booru
// spelling round-trips instead of being dropped or mangled. Mirrors monbooru's
// tags.NormalizeName byte for byte.
func normalizeTag(tag string) string {
	tag = strings.ToLower(tag)
	var b strings.Builder
	b.Grow(len(tag))
	pendingFold := false
	for _, r := range tag {
		switch {
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co):
		case unicode.IsSpace(r) || r == '"' || r == '*':
			pendingFold = true
		default:
			if pendingFold && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingFold = false
			b.WriteRune(r)
		}
	}
	tag = strings.Trim(b.String(), "_")
	if strings.HasSuffix(tag, ":") {
		return "" // a category prefix whose name folded away entirely
	}
	return tag
}

// legacyDisallowedTagChars is the pre-widening charset rule, kept so tags
// monbooru stored before the punctuation widening (and never bulk-rewrote)
// can still be recognized against their raw PTR spelling.
var (
	legacyDisallowedTagChars = regexp.MustCompile(`[^a-z0-9_()!@#$.~+:?<>=^-]+`)
	underscoreRuns           = regexp.MustCompile(`_+`)
)

// LegacyFoldTag returns the pre-widening projection of a monbooru-form tag:
// the spelling an old pull would have stored for the same raw tag. Underscore
// runs collapse because the old fold merged an admitted character and its
// neighbouring spaces into one run ("girls' frontline" -> "girls_frontline").
func LegacyFoldTag(tag string) string {
	tag = legacyDisallowedTagChars.ReplaceAllString(tag, "_")
	tag = underscoreRuns.ReplaceAllString(tag, "_")
	return strings.Trim(tag, "_")
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
