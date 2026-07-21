package mapping

import (
	"sort"
	"strings"

	"github.com/monbooru/monloader/internal/ptr"
)

// MapPTRTags maps raw Hydrus PTR tags onto monbooru's form: category-prefixed,
// normalized, deduplicated, with a rating tag lifted from a `rating:` namespace.
// Hydrus tags are `namespace:subtag` or bare; the namespace routing extends the
// booru table with hydrus vocabulary (person, species, medium). Only the routed
// namespaces survive: the PTR carries thousands of bookkeeping namespaces
// (`title:`, `pixiv work:`, `booru:`, ...) whose subtags are ids or prose, so
// an unrecognized namespace drops the whole tag rather than leak an opaque
// subtag into general. A bare tag is general.
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

// MapPTRTag maps one raw hydrus tag to its monbooru form, or "" when its
// namespace is not routed or it folds away entirely.
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
		return "" // an unrouted namespace: drop the tag
	default:
		return normalizeTag(formatTag(cat, sub))
	}
}

// ptrNamespaceTable routes the content namespaces both ways: ptrNamespace maps
// hydrus -> monbooru through it, and PTRNamespacesFor derives the inverse from
// the same rows, so the two directions cannot drift apart. Order matters: the
// inverse tries a category's namespaces in row order (creator before artist).
// The dropped or lifted namespaces stay out of the table - they have no
// monbooru category to invert from.
var ptrNamespaceTable = []struct{ ns, category string }{
	{"creator", "artist"},
	{"artist", "artist"},
	{"character", "character"},
	{"series", "copyright"},
	{"copyright", "copyright"},
	{"studio", "copyright"},
	{"person", "person"},
	{"species", "species"},
	{"medium", "medium"},
	{"meta", "meta"},
	{"year", "year"},
}

// ptrNamespace routes a hydrus namespace. keep=false drops the tag;
// rating=true lifts it to the rating tag. Anything outside the table is
// dropped: the PTR's namespace population is unbounded and mostly
// bookkeeping, so only the routed rows carry monbooru meaning.
func ptrNamespace(ns string) (category string, keep, rating bool) {
	for _, row := range ptrNamespaceTable {
		if row.ns == ns {
			return row.category, true, false
		}
	}
	if ns == "rating" {
		return "", false, true
	}
	return "", false, false
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

// ptrSupportedNamespaces is the PTR's documented supported-namespace
// list (the PTR guide's "Namespaces" section). Contribution may only
// ever emit a namespace on it: unlisted namespaces get siblinged out,
// removed, or mocked, so the outbound codomain is this closed set plus
// the bare form, never "whatever the category table can spell".
var ptrSupportedNamespaces = map[string]bool{
	"creator": true, "character": true, "person": true, "series": true,
	"photoset": true, "studio": true, "species": true, "title": true,
	"medium": true, "meta": true,
}

// contribDeniedCategories are refused outright before the namespace
// table is consulted: rating conventions are local opinion, and year:
// is not on the PTR's supported list even though the inbound table can
// spell it.
var contribDeniedCategories = map[string]bool{
	"rating": true,
	"year":   true,
}

// ContribDenied reports whether a monbooru-form tag's category is one the
// contribution mapper refuses outright, so the removal-petition side can stay
// symmetric with the add side rather than offering to petition a mapping the
// operator could never have contributed.
func ContribDenied(monbooruTag string) bool {
	cat, _, hasCat := strings.Cut(monbooruTag, ":")
	return hasCat && contribDeniedCategories[cat]
}

// ContribTag is the outbound verdict for one monbooru-form tag: the
// exact PTR spelling to send, or an empty PTR form with the reason it
// is ineligible. A tag is never silently adjusted into something the
// operator did not write.
type ContribTag struct {
	PTR  string
	Note string
}

// Eligible reports whether the tag maps to a sendable PTR form.
func (c ContribTag) Eligible() bool { return c.PTR != "" }

// ContribTagFor maps one monbooru-form tag to the PTR spelling a
// contribution would send. The category routes through the shared
// namespace table's primary row (creator before artist, series before
// copyright); a user-created category qualifies only when its name is
// itself a supported PTR namespace. Underscores become the spaces
// hydrus stores, then the result must survive the server's cleaning
// unchanged or the tag is refused with a note.
func ContribTagFor(monbooruTag string) ContribTag {
	cat, name, hasCat := strings.Cut(monbooruTag, ":")
	if !hasCat {
		cat, name = "", monbooruTag
	}
	ns := ""
	switch {
	case !hasCat:
	case contribDeniedCategories[cat]:
		return ContribTag{Note: "unsupported namespace"}
	default:
		found := false
		for _, row := range ptrNamespaceTable {
			if row.category == cat {
				ns, found = row.ns, true
				break
			}
		}
		if !found {
			// The carve-out: an operator who deliberately mirrors a
			// supported hydrus namespace as a category name gets the
			// round trip; any other user category would mint a namespace
			// the PTR does not want.
			if !ptrSupportedNamespaces[cat] {
				return ContribTag{Note: "unmapped category"}
			}
			ns = cat
		}
	}
	if ns != "" && !ptrSupportedNamespaces[ns] {
		return ContribTag{Note: "unsupported namespace"}
	}
	sub := strings.ReplaceAll(name, "_", " ")
	candidate := sub
	if ns != "" {
		candidate = ns + ":" + sub
	}
	if !ptr.TagOK(candidate) {
		return ContribTag{Note: "the repository would rewrite this tag"}
	}
	return ContribTag{PTR: candidate}
}
