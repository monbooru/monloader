package mapping

import (
	"sort"
	"strings"
)

// MapPTRTags maps raw Hydrus PTR tags onto monbooru's form: category-prefixed,
// normalized, deduplicated, with a rating tag lifted from a `rating:` namespace.
// Hydrus tags are `namespace:subtag` or bare; the namespace routing extends the
// booru table with hydrus vocabulary (person, species, medium) and drops the
// bookkeeping namespaces that carry no monbooru meaning. An unrecognized
// namespace keeps its subtag as a general tag - the namespace itself is not
// monbooru-meaningful - and a bare tag is general.
func MapPTRTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, tag := range tags {
		s := MapPTRTag(tag)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// MapPTRTag maps one raw hydrus tag to its monbooru form, or "" when it is a
// dropped bookkeeping tag or folds away entirely.
func MapPTRTag(tag string) string {
	ns, sub, hasNS := strings.Cut(tag, ":")
	if !hasNS || sub == "" {
		return normalizeTag(tag) // bare tag: general
	}
	cat, keep, rating := ptrNamespace(strings.ToLower(ns))
	switch {
	case rating:
		if lvl := mapRating(FamilyGeneric, sub); lvl != "" {
			return "rating:" + lvl
		}
		return ""
	case !keep:
		return "" // a bookkeeping namespace: drop the tag
	case cat == "":
		return normalizeTag(sub) // unrecognized namespace: subtag as general
	default:
		return normalizeTag(formatTag(cat, sub))
	}
}

// ptrNamespaceTable routes the content namespaces both ways: ptrNamespace maps
// hydrus -> monbooru through it, and PTRNamespacesFor derives the inverse from
// the same rows, so the two directions cannot drift apart. Order matters: the
// inverse tries a category's namespaces in row order (creator before artist).
// The namespaces routed to general (species) and the dropped or lifted ones
// stay out of the table - they have no monbooru category to invert from.
var ptrNamespaceTable = []struct{ ns, category string }{
	{"creator", "artist"},
	{"artist", "artist"},
	{"character", "character"},
	{"series", "copyright"},
	{"copyright", "copyright"},
	{"studio", "copyright"},
	{"person", "person"},
	{"medium", "medium"},
	{"meta", "meta"},
	{"year", "year"},
}

// ptrNamespace routes a hydrus namespace. keep=false drops the tag (bookkeeping
// namespaces); rating=true lifts it to the rating tag; an empty category with
// keep=true means "unrecognized, keep the subtag as general".
func ptrNamespace(ns string) (category string, keep, rating bool) {
	for _, row := range ptrNamespaceTable {
		if row.ns == ns {
			return row.category, true, false
		}
	}
	switch ns {
	case "species":
		return "general", true, false // no monbooru equivalent
	case "rating":
		return "", false, true
	case "title", "filename", "page", "chapter", "volume", "booru", "source", "url":
		return "", false, false // bookkeeping: not content
	default:
		return "", true, false // unrecognized: keep the subtag as general
	}
}

// PTRNamespacesFor returns the hydrus namespaces whose tags a monbooru-form tag
// name could have come from, so the tag-graph query can look a monbooru tag up
// in the PTR's vocabulary. A bare name (general) matches an unnamespaced hydrus
// tag; a `category:name` name matches every namespace that routes to that
// category. Underscores also try the space-joined hydrus spelling.
func PTRNamespacesFor(monbooruTag string) []string {
	cat, name, hasCat := strings.Cut(monbooruTag, ":")
	if !hasCat {
		return spellings("", monbooruTag)
	}
	var out []string
	for _, row := range ptrNamespaceTable {
		if row.category == cat {
			out = append(out, spellings(row.ns, name)...)
		}
	}
	if out == nil {
		return spellings("", monbooruTag)
	}
	return out
}

// spellings renders a namespace + name into the hydrus spellings to try: the
// verbatim name and, when it carries underscores, the space-joined form hydrus
// commonly stores.
func spellings(ns, name string) []string {
	forms := []string{name}
	if spaced := strings.ReplaceAll(name, "_", " "); spaced != name {
		forms = append(forms, spaced)
	}
	out := make([]string, 0, len(forms))
	for _, f := range forms {
		if ns == "" {
			out = append(out, f)
		} else {
			out = append(out, ns+":"+f)
		}
	}
	return out
}
