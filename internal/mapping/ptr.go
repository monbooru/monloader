package mapping

import (
	"cmp"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/ptr"
)

// MapPTR takes a hash's raw tags through the PTR's own mappings, then the
// global category drop. Contributions and the tag graph read MapPTRTag instead:
// a local rule must not change how a tag is spelled to the public repository.
func (m *Mapper) MapPTR(raw []string) []string {
	tags, _ := m.MapPTRWithheld(raw)
	return tags
}

// MapPTRWithheld also names what the PTR's row held back, under the category it
// would otherwise land in. A namespace nothing routes was never going to land.
func (m *Mapper) MapPTRWithheld(raw []string) (tags, withheld []string) {
	profile := m.profileFor(config.SitePTR)
	drop := m.cfg.Current().Tags.DropCategories
	for _, tag := range raw {
		mapped := m.ptrMapped(profile, tag)
		s := applyTagRule(profile.TagRules, mapped)
		switch {
		case s != "":
			if !categoryDropped(drop, s) {
				tags = append(tags, s)
			}
		case cmp.Or(mapped, MapPTRTag(tag)) != "":
			withheld = append(withheld, cmp.Or(mapped, MapPTRTag(tag)))
		}
	}
	slices.Sort(tags)
	slices.Sort(withheld)
	return slices.Compact(tags), slices.Compact(withheld)
}

// PTRBlacklisted reads only the PTR's own list: the global one exists to stop
// downloads, and these images are already in the library.
func (m *Mapper) PTRBlacklisted(raw []string) (note string, blacklisted bool) {
	site := m.cfg.Current().FindSite(config.SitePTR)
	if site == nil {
		return "", false
	}
	rules := parseBlacklistRules(site.Name, site.Blacklist)
	if len(rules) == 0 {
		return "", false
	}
	profile := m.profileFor(config.SitePTR)
	var tags []postTag
	var rating string
	for _, tag := range raw {
		mapped := m.ptrMapped(profile, tag)
		if level, ok := strings.CutPrefix(mapped, "rating:"); ok {
			// monbooru lands the highest of the ratings a push lists.
			rating = Stricter(rating, level)
			continue
		}
		for _, read := range append(blacklistReads(profile, mapped), MapPTRTag(tag)) {
			if read != "" {
				tags = append(tags, splitPostTag(read))
			}
		}
	}
	rule, hit := firstBlacklistRule(rules, tags, rating, false)
	if !hit {
		return "", false
	}
	return rule.note(), true
}

// Bare tags answer to the namespace "general" in the category map.
func (m *Mapper) ptrMapped(profile Profile, tag string) string {
	ns, sub, hasNS := strings.Cut(tag, ":")
	if !hasNS || sub == "" || (ns == "" && strings.HasPrefix(sub, ":")) {
		ns, sub = "general", strings.TrimPrefix(tag, ":")
	}
	ns = strings.ToLower(ns)
	if to, ok := m.categoryOverride(config.SitePTR, profile, ns); ok {
		return normalizeTag(formatTag(to, sub))
	}
	if ns == "rating" {
		if level := m.ratingFor(config.SitePTR, profile, sub); level != "" {
			return "rating:" + level
		}
		return ""
	}
	return MapPTRTag(tag)
}

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
	if ns == "" && strings.HasPrefix(sub, ":") {
		return normalizeTag(sub) // hydrus escapes the bare tag :d as ::d
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
	if !hasCat || cat == "" {
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

// The forms double per underscore; a name with more than this many is past
// what a booru spelling mixes anyway.
const maxSubtagMixes = 3

func escapeLeadingColon(tag string) string {
	if strings.HasPrefix(tag, ":") {
		return ":" + tag
	}
	return tag
}

func spellings(ns, name string) []PTRSpelling {
	forms := subtagForms(name)
	out := make([]PTRSpelling, 0, len(forms))
	for _, f := range forms {
		if ns == "" {
			out = append(out, PTRSpelling{Full: escapeLeadingColon(f), Subtag: f})
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
	if len(at) > maxSubtagMixes {
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
	if !hasCat || cat == "" {
		cat, name, hasCat = "", monbooruTag, false
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
	candidate := escapeLeadingColon(sub)
	if ns != "" {
		candidate = ns + ":" + sub
	}
	if !ptr.TagOK(candidate) {
		return ContribTag{Note: "the repository would rewrite this tag"}
	}
	return ContribTag{PTR: candidate}
}
