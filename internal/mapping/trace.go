package mapping

import (
	"fmt"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/shared/contract"
)

type TraceRead struct {
	Tag string
	Hit bool
}

// MapDropped and RuleDropped end a row early; Sent is empty when the tag never
// reaches monbooru.
type TraceRow struct {
	Field       string
	Listed      string
	Category    string
	Mapped      string
	MapDropped  bool
	Ruled       string
	RuleDropped bool
	RuleGlobal  bool
	Reads       []TraceRead
	Unchecked   string
	Sent        string
}

// Only a listing's first post is worth a link: a post or a bare file is
// the URL traced.
func (m *Mapper) tracePostURL(meta map[string]any) string {
	if kwdict.String(meta, "category") == CategoryDirectlink || kwdict.String(meta, "subcategory") == "post" {
		return ""
	}
	return m.PostURL(meta)
}

// Tags is Map's own answer, so the verdict cannot drift from a real push.
type PostTrace struct {
	Site         string
	PostURL      string
	Rows         []TraceRow
	RatingListed string
	Rating       string
	RatingHit    bool
	Skipped      string
	Refused      string
	Tags         []string
	Warnings     []string
}

func (m *Mapper) Trace(meta map[string]any) PostTrace {
	category := kwdict.String(meta, "category")
	profile := m.profileFor(category)
	drop := m.cfg.Current().Tags.DropCategories
	mapped := m.Map(meta)
	t := PostTrace{Site: category, PostURL: m.tracePostURL(meta), RatingListed: listedRating(profile, meta), Tags: mapped.Tags, Warnings: mapped.UnknownTargets}

	var tags []postTag
	row := func(r TraceRow, spellings []string) {
		r.Field = strings.TrimPrefix(r.Field, "tags_")
		for _, s := range spellings {
			r.Reads = append(r.Reads, TraceRead{Tag: s})
			tags = append(tags, splitPostTag(s))
		}
		t.Rows = append(t.Rows, r)
	}
	tagRules := m.rulesFor(profile)
	m.eachTag(category, profile, meta, func(field, s string) {
		source := normalizeTag(s)
		after, _, list := tagRules.match(source)
		mapped := splitPostTag(source)
		r := TraceRow{Field: field, Listed: mapped.name, Category: mapped.category, Mapped: source, Ruled: after, RuleDropped: after == "" && source != "", RuleGlobal: list == contract.ListGlobal}
		if after != "" {
			r.Unchecked = droppedCategory(drop, after)
		}
		if sentTag(drop, after) {
			r.Sent = after
		}
		if r.Unchecked != "" && r.Unchecked != mapped.category {
			t.Warnings = append(t.Warnings, fmt.Sprintf("tag rule %s -> %s: %s is unchecked in Rules, so this tag never reaches monbooru", source, after, r.Unchecked))
		}
		row(r, blacklistReads(tagRules, s))
	}, func(field, s string) {
		source := normalizeTag(s)
		row(TraceRow{Field: field, Listed: splitPostTag(source).name, MapDropped: true}, []string{source})
	})

	t.Rating = m.postRating(category, profile, meta)
	rules := m.blacklistRules(category)
	if rule, hit := firstBlacklistRule(rules, tags, t.Rating, m.flatOnly(profile, meta)); hit {
		t.Skipped = rule.note()
		t.RatingHit = rule.namesRating(t.Rating)
		for _, r := range t.Rows {
			for i, read := range r.Reads {
				r.Reads[i].Hit = rule.names(splitPostTag(read.Tag))
			}
		}
	}
	if m.SuspectFlattenedTags(meta) {
		t.Refused = FlattenedTagsMsg
	}
	for _, rule := range rules {
		if name, filed := rule.misfiled(tags); filed != "" {
			t.Warnings = append(t.Warnings, fmt.Sprintf("blacklist rule %q misses: this site files %s under %s", rule.text, name, filed))
		}
	}
	return t
}

func (t blacklistToken) names(tag postTag) bool {
	return !t.negate && t.rating == "" && (t.category == "" || t.category == tag.category) && globMatch(t.pattern, tag.name)
}

func (r blacklistRule) names(tag postTag) bool {
	return slices.ContainsFunc(r.tokens, func(t blacklistToken) bool { return t.names(tag) })
}

// condition is the rest of the line, as written, once the token naming the
// tag is left out: empty when the line names the tag alone.
func (r blacklistRule) condition(tag postTag) (string, bool) {
	i := slices.IndexFunc(r.tokens, func(t blacklistToken) bool { return t.names(tag) })
	if i < 0 {
		return "", false
	}
	rest := make([]string, 0, len(r.tokens)-1)
	for j, t := range r.tokens {
		if j != i {
			rest = append(rest, t.raw)
		}
	}
	return strings.Join(rest, " "), true
}

func (r blacklistRule) namesRating(rating string) bool {
	return slices.ContainsFunc(r.tokens, func(t blacklistToken) bool {
		return !t.negate && t.rating != "" && t.rating == rating
	})
}

func (r blacklistRule) misfiled(tags []postTag) (name, filed string) {
	for _, t := range r.tokens {
		if t.negate || t.category == "" {
			continue
		}
		for _, tag := range tags {
			if !globMatch(t.pattern, tag.name) {
				continue
			}
			if tag.category == t.category {
				filed = ""
				break
			}
			name, filed = tag.name, tag.category
		}
		if filed != "" {
			return name, filed
		}
	}
	return "", ""
}
