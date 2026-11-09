package mapping

import (
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
)

// TagRule is what one site does with a tag of a monbooru name when it
// arrives from there.
type TagRule struct {
	Site          string   `json:"site"`
	Rule          string   `json:"rule"` // none, drop, rename, move
	Target        string   `json:"target,omitempty"`
	Blacklist     string   `json:"blacklist,omitempty"` // global, site
	BlacklistLine string   `json:"blacklist_line,omitempty"`
	MappedFrom    []string `json:"mapped_from,omitempty"`
	RenamedFrom   []string `json:"renamed_from,omitempty"`
	// Whether the tag, as this site sends it, survives the category drop.
	Kept bool `json:"kept"`
}

type TagRules struct {
	Tag          string    `json:"tag"`
	Category     string    `json:"category"`
	CategoryKept bool      `json:"category_kept"`
	Sites        []TagRule `json:"sites"`
}

// TagRules answers for the sites named, in that order. The PTR's own
// blacklist is the only one that reads its answers.
func (m *Mapper) TagRules(category, name string, sites []string) TagRules {
	tag := normalizeTag(formatTag(category, name))
	drop := m.cfg.Current().Tags.DropCategories
	out := TagRules{Tag: name, Category: category, CategoryKept: !categoryDropped(drop, tag), Sites: []TagRule{}}
	if tag == "" {
		return out
	}
	for _, site := range sites {
		profile := m.profileFor(site)
		r := TagRule{Site: site, Rule: "none"}
		after := applyTagRule(profile.TagRules, tag)
		switch {
		case after == "":
			r.Rule = "drop"
		case after != tag:
			r.Target = after
			r.Rule = "rename"
			if splitPostTag(after).category != splitPostTag(tag).category {
				r.Rule = "move"
			}
		}
		r.Kept = after != "" && !categoryDropped(drop, after)
		for _, rule := range m.blacklistRules(site) {
			if site == config.SitePTR && rule.list == "global" {
				continue
			}
			if rule.names(splitPostTag(tag)) || (after != "" && rule.names(splitPostTag(after))) {
				r.Blacklist = "site"
				if rule.list == "global" {
					r.Blacklist = "global"
				}
				r.BlacklistLine = rule.text
				break
			}
		}
		for from, to := range profile.CategoryOverrides {
			if to == category {
				r.MappedFrom = append(r.MappedFrom, from)
			}
		}
		slices.Sort(r.MappedFrom)
		r.RenamedFrom = renamedInto(profile.TagRules, category, tag)
		out.Sites = append(out.Sites, r)
	}
	return out
}

// A bare rule key matches the name in any category, so it is tried as the
// tag's category and as general, the two a site would file it under.
func renamedInto(rules map[string]string, category, tag string) []string {
	var from []string
	for key, to := range rules {
		if to == "" {
			continue
		}
		candidates := []string{key}
		if prefix, _, found := strings.Cut(key, ":"); !found || !isTagCategory(prefix) {
			candidates = []string{formatTag(category, key), key}
		}
		for _, c := range candidates {
			if c = normalizeTag(c); c != tag && applyTagRule(rules, c) == tag {
				from = append(from, key)
				break
			}
		}
	}
	slices.Sort(from)
	return from
}
