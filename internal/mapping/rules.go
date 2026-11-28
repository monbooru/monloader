package mapping

import (
	"cmp"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
)

// TagRules answers for the sites named, in that order. The PTR's own
// blacklist is the only one that reads its answers.
func (m *Mapper) TagRules(category, name string, sites []string) contract.TagRules {
	tag := normalizeTag(contract.TagForm(category, name))
	drop := m.cfg.Current().Tags.DropCategories
	out := contract.TagRules{Tag: name, Category: category, CategoryKept: !categoryDropped(drop, tag), Sites: []contract.TagRule{}}
	if tag == "" {
		return out
	}
	for _, site := range sites {
		profile := m.profileFor(site)
		r := contract.TagRule{Site: site, Rule: contract.RuleNone}
		rules := m.rulesFor(profile)
		after, key, list := rules.match(tag)
		if after != tag {
			r.RuleKey, r.RuleList = key, list
		}
		switch {
		case after == "":
			r.Rule = contract.RuleDrop
		case after != tag:
			r.Target = after
			r.Rule = contract.RuleRename
			if splitPostTag(after).category != splitPostTag(tag).category {
				r.Rule = contract.RuleMove
			}
		}
		r.Kept = after != "" && !categoryDropped(drop, after)
		for _, rule := range m.blacklistRules(site) {
			if site == config.SitePTR && rule.list == contract.ListGlobal {
				continue
			}
			cond, named := rule.condition(splitPostTag(tag))
			if !named && after != "" {
				cond, named = rule.condition(splitPostTag(after))
			}
			if named {
				r.Blacklist = contract.ListSite
				if rule.list == contract.ListGlobal {
					r.Blacklist = contract.ListGlobal
				}
				r.BlacklistLine = rule.text
				r.BlacklistCondition = &cond
				break
			}
		}
		for from, to := range profile.CategoryOverrides {
			if to == category {
				r.MappedFrom = append(r.MappedFrom, from)
			}
		}
		slices.Sort(r.MappedFrom)
		r.RenamedFrom = renamedInto(rules.site, func(c string) string { return applyTagRule(rules.site, c) }, category, tag)
		r.RenamedFromGlobal = renamedInto(rules.global, func(c string) string {
			if after, _, list := rules.match(c); list == contract.ListGlobal {
				return after
			}
			return c
		}, category, tag)
		out.Sites = append(out.Sites, r)
	}
	return out
}

// A bare rule key matches the name in any category, so it is tried as the
// tag's category and as general, the two a site would file it under.
func renamedInto(rules map[string]string, apply func(string) string, category, tag string) []string {
	var from []string
	for key, to := range rules {
		if to == "" {
			continue
		}
		if strings.Contains(key, "*") {
			if src := patternSource(apply, key, to, category, tag); src != "" {
				from = append(from, src)
			}
			continue
		}
		candidates := []string{key}
		if prefix, _, found := strings.Cut(key, ":"); !found || !isTagCategory(prefix) {
			candidates = []string{contract.TagForm(category, key), key}
		}
		for _, c := range candidates {
			if c = normalizeTag(c); c != tag && apply(c) == tag {
				from = append(from, key)
				break
			}
		}
	}
	slices.Sort(from)
	return from
}

// The spelling a key with a * turns into the tag, rebuilt from the text the
// target's * holds; a target with no * leaves nothing to rebuild from, so the
// key itself is named.
func patternSource(apply func(string) string, key, to, category, tag string) string {
	keyCategory, keyName := splitRuleKey(key)
	made := to
	if cat, rest, found := strings.Cut(to, ":"); !found || !isTagCategory(cat) || rest == "" {
		made = contract.TagForm(cmp.Or(keyCategory, category), to)
	} else if cat == "general" {
		made = rest
	}
	head, tail, found := strings.Cut(made, "*")
	if !found {
		if made == tag {
			return key
		}
		return ""
	}
	text, hit := starText(tag, head, tail)
	if !hit {
		return ""
	}
	src := strings.ReplaceAll(keyName, "*", text)
	for _, c := range []string{contract.TagForm(cmp.Or(keyCategory, category), src), src} {
		if c != tag && apply(c) == tag {
			return contract.TagForm(cmp.Or(keyCategory, "general"), src)
		}
	}
	return ""
}
