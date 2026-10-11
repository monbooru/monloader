package mapping

import (
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/ptr"
)

func MapPTRTags(tags []string) []string {
	var out []string
	for _, tag := range tags {
		if s := MapPTRTag(tag); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// MapPTRTag returns "" when the namespace is not routed or the tag folds
// away entirely.
func MapPTRTag(tag string) string {
	ns, sub, hasNS := strings.Cut(tag, ":")
	if !hasNS || sub == "" {
		return normalizeTag(tag)
	}
	cat, keep, rating := ptrNamespace(strings.ToLower(ns))
	switch {
	case rating:
		if lvl := mapRating(FamilyGeneric, sub); lvl != "" {
			return "rating:" + lvl
		}
		return ""
	case !keep:
		return ""
	default:
		return normalizeTag(formatTag(cat, sub))
	}
}

// Row order matters: a category's first row is the namespace a
// contribution sends, and the inverse tries rows in order. Lifted or
// dropped namespaces stay out.
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

// Anything outside the table is dropped: the PTR's other namespaces are
// bookkeeping (title:, pixiv work:, booru:, ...) whose subtags are ids or
// prose.
func ptrNamespace(ns string) (category string, keep, rating bool) {
	for _, row := range ptrNamespaceTable {
		if row.ns == ns {
			return row.category, true, false
		}
	}
	return "", false, ns == "rating"
}

// PTRNamespacesFor lists every hydrus spelling a monbooru-form tag could have
// come from: each namespace routing to its category, in each separator form.
func PTRNamespacesFor(monbooruTag string) []string {
	parts := PTRSpellingsFor(monbooruTag)
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.Full
	}
	return out
}

// Namespace keeps its trailing colon and is empty for a bare tag.
type PTRSpelling struct {
	Full      string
	Namespace string
	Subtag    string
}

func PTRSpellingsFor(monbooruTag string) []PTRSpelling {
	cat, name, hasCat := strings.Cut(monbooruTag, ":")
	if !hasCat {
		return spellings("", monbooruTag)
	}
	var out []PTRSpelling
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

// The forms double per underscore; a name long enough to pass the bound
// is past what a booru spelling mixes anyway.
const maxSubtagForms = 8

func spellings(ns, name string) []PTRSpelling {
	forms := subtagForms(name)
	out := make([]PTRSpelling, 0, len(forms))
	for _, f := range forms {
		if ns == "" {
			out = append(out, PTRSpelling{Full: f, Subtag: f})
			continue
		}
		out = append(out, PTRSpelling{Full: ns + ":" + f, Namespace: ns + ":", Subtag: f})
	}
	return out
}

// Verbatim first, then space-joined, the two hydrus commonly holds. The
// mixes follow because the projection folds spaces to underscores:
// "kagamine_rin cosplay" is otherwise unreachable.
func subtagForms(name string) []string {
	at := []int{}
	for i, r := range name {
		if r == '_' {
			at = append(at, i)
		}
	}
	if len(at) == 0 {
		return []string{name}
	}
	spaced := strings.ReplaceAll(name, "_", " ")
	forms := []string{name, spaced}
	if 1<<len(at) > maxSubtagForms {
		return forms
	}
	// Bit i spaces the i-th underscore; all-clear and all-set are the two
	// forms above.
	b := []byte(name)
	for mask := 1; mask < 1<<len(at)-1; mask++ {
		for i, pos := range at {
			if mask&(1<<i) != 0 {
				b[pos] = ' '
			} else {
				b[pos] = '_'
			}
		}
		forms = append(forms, string(b))
	}
	return forms
}

// The PTR guide's supported namespaces. Contributions may only emit these
// or the bare form: anything else gets siblinged out, removed or mocked.
var ptrSupportedNamespaces = map[string]bool{
	"creator": true, "character": true, "person": true, "series": true,
	"photoset": true, "studio": true, "species": true, "title": true,
	"medium": true, "meta": true,
}

// Rating conventions are local opinion, and year: is off the PTR's
// supported list though the inbound table routes it.
var contribDeniedCategories = map[string]bool{
	"rating": true,
	"year":   true,
}

func ContribDenied(monbooruTag string) bool {
	cat, _, hasCat := strings.Cut(monbooruTag, ":")
	return hasCat && contribDeniedCategories[cat]
}

type ContribTag struct {
	PTR  string
	Note string
}

func (c ContribTag) Eligible() bool { return c.PTR != "" }

// ContribTagFor never adjusts a tag: one the server's cleaning would
// change is refused with a note.
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
			// A user category passes only when it is itself a supported
			// namespace; any other would mint one the PTR does not want.
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
