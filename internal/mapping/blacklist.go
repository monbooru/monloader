package mapping

import (
	"fmt"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/kwdict"
)

type blacklistToken struct {
	negate   bool
	category string
	rating   string
	pattern  string
}

type blacklistRule struct {
	list   string
	text   string
	tokens []blacklistToken
}

var blacklistRatings = map[string]string{
	"g": RatingGeneral, RatingGeneral: RatingGeneral,
	"s": RatingSensitive, RatingSensitive: RatingSensitive,
	"q": RatingQuestionable, RatingQuestionable: RatingQuestionable,
	"e": RatingExplicit, RatingExplicit: RatingExplicit,
}

func parseBlacklistToken(raw string) (blacklistToken, bool) {
	var t blacklistToken
	body, negate := strings.CutPrefix(raw, "-")
	t.negate = negate
	if body == "" || strings.HasPrefix(body, "~") {
		return t, false
	}
	if prefix, rest, found := strings.Cut(body, ":"); found && prefix != "" {
		prefix = strings.ToLower(prefix)
		if prefix == "rating" {
			t.rating = blacklistRatings[strings.ToLower(rest)]
			return t, t.rating != ""
		}
		if !isTagCategory(prefix) {
			return t, false
		}
		t.category, body = prefix, rest
	}
	// normalizeName folds a * away, and trims the underscore a glob like
	// *_(cosplay) means to keep.
	if strings.Contains(body, "*") {
		t.pattern = strings.ToLower(body)
	} else {
		t.pattern = normalizeName(body)
	}
	return t, t.pattern != ""
}

func parseBlacklistRule(line string) (rule blacklistRule, bad string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return blacklistRule{}, ""
	}
	rule.text = line
	for _, raw := range strings.Fields(line) {
		t, ok := parseBlacklistToken(raw)
		if !ok {
			return blacklistRule{}, raw
		}
		rule.tokens = append(rule.tokens, t)
	}
	return rule, ""
}

// CleanBlacklist reads a textarea into the config's lines: comments stay, blank lines go.
func CleanBlacklist(text string) ([]string, error) {
	var lines []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, bad := parseBlacklistRule(line); bad != "" {
			return nil, fmt.Errorf("blacklist line %d: %q is not a tag, a category:name or a rating", i+1, bad)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// globMatch has no ? or [ ]: both are ordinary characters in a tag name.
func globMatch(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	rest, ok := strings.CutPrefix(name, parts[0])
	if !ok {
		return false
	}
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		_, after, found := strings.Cut(rest, part)
		if !found {
			return false
		}
		rest = after
	}
	return strings.HasSuffix(rest, last)
}

func BlacklistRuleLines(lines []string) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(line string) bool { return strings.HasPrefix(line, "#") })
}

type postTag struct{ category, name string }

func splitPostTag(tag string) postTag {
	if prefix, rest, found := strings.Cut(tag, ":"); found && isTagCategory(prefix) {
		return postTag{prefix, rest}
	}
	return postTag{"general", tag}
}

func (t blacklistToken) holds(tags []postTag, rating string) bool {
	found := t.rating == rating
	if t.rating == "" {
		found = slices.ContainsFunc(tags, func(tag postTag) bool {
			return (t.category == "" || t.category == tag.category) && globMatch(t.pattern, tag.name)
		})
	}
	return found != t.negate
}

func (r blacklistRule) matches(tags []postTag, rating string) bool {
	return !slices.ContainsFunc(r.tokens, func(t blacklistToken) bool { return !t.holds(tags, rating) })
}

func (r blacklistRule) namesCategory() bool {
	return slices.ContainsFunc(r.tokens, func(t blacklistToken) bool { return t.category != "" })
}

func parseBlacklistRules(list string, lines []string) []blacklistRule {
	var rules []blacklistRule
	for _, line := range lines {
		if rule, bad := parseBlacklistRule(line); bad == "" && rule.text != "" {
			rule.list = list
			rules = append(rules, rule)
		}
	}
	return rules
}

func (m *Mapper) blacklistRules(category string) []blacklistRule {
	cfg := m.cfg.Current()
	rules := parseBlacklistRules("global", cfg.Tags.Blacklist)
	if site := cfg.FindSite(category); site != nil {
		rules = append(rules, parseBlacklistRules(site.Name, site.Blacklist)...)
	}
	return rules
}

// Blacklisted names the first matching rule and its list, reading every tag before
// any drop, as sent and after a tag rule. On a flat-tag resolve pass, a rule naming
// a category is left for the sidecar: its -artist:x would hold on any post.
func (m *Mapper) Blacklisted(meta map[string]any) (note string, blacklisted bool) {
	category := kwdict.String(meta, "category")
	rules := m.blacklistRules(category)
	if len(rules) == 0 {
		return "", false
	}
	profile := m.profileFor(category)
	var tags []postTag
	m.eachTag(category, profile, meta, func(_, s string) {
		for _, read := range blacklistReads(profile, s) {
			tags = append(tags, splitPostTag(read))
		}
	}, func(_, s string) {
		tags = append(tags, splitPostTag(normalizeTag(s)))
	})
	rule, hit := firstBlacklistRule(rules, tags, m.postRating(category, profile, meta), m.flatOnly(profile, meta))
	if !hit {
		return "", false
	}
	return rule.note(), true
}

func (r blacklistRule) note() string { return r.list + " rule: " + r.text }

func blacklistReads(profile Profile, tag string) []string {
	source := normalizeTag(tag)
	reads := []string{source}
	if mapped := applyTagRule(profile.TagRules, source); mapped != "" && mapped != source {
		reads = append(reads, mapped)
	}
	return reads
}

func (m *Mapper) flatOnly(profile Profile, meta map[string]any) bool {
	return len(tagSuffixes(meta)) == 0 && (needsTagsFamily(profile.Family) || profile.NeedsTags)
}

func firstBlacklistRule(rules []blacklistRule, tags []postTag, rating string, flatOnly bool) (blacklistRule, bool) {
	for _, r := range rules {
		if flatOnly && r.namesCategory() {
			continue
		}
		if r.matches(tags, rating) {
			return r, true
		}
	}
	return blacklistRule{}, false
}
