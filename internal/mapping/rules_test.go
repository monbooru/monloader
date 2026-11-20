package mapping

import (
	"slices"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func TestTagRulesNameWhatEachSiteDoesWithATag(t *testing.T) {
	m := traceFixture(t, []string{"meta"}, []string{"ai_generated"})
	sites := []string{"danbooru", "gelbooru", config.SitePTR}

	rules := m.TagRules("general", "lowres", sites)
	if !rules.CategoryKept || rules.Sites[0].Rule != "drop" || rules.Sites[1].Rule != "none" || rules.Sites[2].Rule != "none" {
		t.Errorf("lowres = %+v, want danbooru dropping it and the others untouched", rules)
	}
	if r := m.TagRules("general", "smile", sites).Sites[0]; r.Rule != "rename" || r.Target != "smiling" {
		t.Errorf("smile on danbooru = %+v, want a rename to smiling", r)
	}
	if r := m.TagRules("general", "sketch", sites).Sites[0]; r.Rule != "move" || r.Target != "meta:sketch" || r.Kept {
		t.Errorf("sketch on danbooru = %+v, want a move to meta:sketch that meta's drop then loses", r)
	}
	if r := m.TagRules("general", "smile", sites).Sites[1]; !r.Kept {
		t.Errorf("smile on gelbooru = %+v, want kept", r)
	}
	if rules := m.TagRules("meta", "highres", sites); rules.CategoryKept {
		t.Errorf("meta:highres should not be kept with meta dropped: %+v", rules)
	}
	if rules := m.TagRules("general", "ai_generated", sites); rules.Sites[0].Blacklist != "global" || rules.Sites[1].Blacklist != "global" || rules.Sites[2].Blacklist != "" {
		t.Errorf("ai_generated = %+v, want the global blacklist on the sites and not on the PTR", rules.Sites)
	}
	if rules := m.TagRules("general", "site_only", sites); rules.Sites[0].Blacklist != "site" || rules.Sites[1].Blacklist != "" {
		t.Errorf("site_only = %+v, want danbooru's own blacklist alone", rules.Sites)
	}
	if r := m.TagRules("general", "ai_generated", sites).Sites[0]; r.BlacklistLine != "ai_generated" {
		t.Errorf("ai_generated on danbooru = %+v, want the global line carried", r)
	}
	if r := m.TagRules("general", "touhou", sites).Sites[0]; !slices.Equal(r.MappedFrom, []string{"copyright"}) {
		t.Errorf("a general tag on danbooru = %+v, want the copyright map named", r)
	}
	if r := m.TagRules("general", "smiling", sites).Sites[0]; !slices.Equal(r.RenamedFrom, []string{"smile"}) {
		t.Errorf("smiling on danbooru = %+v, want smile named as renamed into it", r)
	}
	if r := m.TagRules("meta", "sketch", sites).Sites[0]; !slices.Equal(r.RenamedFrom, []string{"sketch"}) {
		t.Errorf("meta:sketch on danbooru = %+v, want the general sketch the rule moves into it", r)
	}
	if r := m.TagRules("general", "smile", sites).Sites; r[0].RenamedFrom != nil || r[1].RenamedFrom != nil {
		t.Errorf("smile = %+v, want nothing renamed into a name the rule renames away", r)
	}
}

func TestRenamedIntoReadsAStarBackwards(t *testing.T) {
	withCustomCategories(t, "va")
	rules := map[string]string{
		"*_(va)":            "va:*",
		"artist:*_(studio)": "copyright:*",
		"*_(style)":         "style",
		"bad_*":             "worse_*",
		"x_(va)":            "va:y",
		"*_(cosplay)":       "",
	}
	for _, c := range []struct {
		category, tag string
		want          []string
	}{
		{"va", "va:soandso", []string{"soandso_(va)"}},
		{"copyright", "copyright:kyo", []string{"artist:kyo_(studio)"}},
		{"artist", "artist:style", []string{"*_(style)"}},
		{"general", "worse_hands", []string{"bad_hands"}},
		{"character", "character:worse_apple", []string{"bad_apple"}},
		{"va", "va:y", []string{"x_(va)", "y_(va)"}},
		{"va", "va:x", nil},
		{"general", "smile", nil},
	} {
		apply := func(tag string) string { return applyTagRule(rules, tag) }
		if got := renamedInto(rules, apply, c.category, c.tag); !slices.Equal(got, c.want) {
			t.Errorf("%s = %v, want %v", c.tag, got, c.want)
		}
	}
}

func TestTagRulesNameTheRuleAndItsList(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "danbooru.json", `{"family":"danbooru","auth":"none",
		"tag_rules":{"smile":"smiling","grin_(va)":"grin_(va)","kept":"kept"}}`)
	cfg := config.Default()
	cfg.Tags.Rules = map[string]string{"*_(va)": "character:*", "smile": "grin", "tagme": "", "laugh": "smiling"}
	m := newMapperDir(t, cfg, dir)
	sites := []string{"danbooru", "gelbooru", config.SitePTR}

	rows := m.TagRules("artist", "soandso_(va)", sites).Sites
	for _, r := range rows {
		if r.Rule != "move" || r.Target != "character:soandso" || r.RuleKey != "*_(va)" || r.RuleList != "global" {
			t.Errorf("soandso_(va) on %s = %+v, want the rule for all sites named", r.Site, r)
		}
	}
	rows = m.TagRules("general", "smile", sites).Sites
	if r := rows[0]; r.Target != "smiling" || r.RuleKey != "smile" || r.RuleList != "site" {
		t.Errorf("smile on danbooru = %+v, want its own rule named", r)
	}
	if r := rows[1]; r.Target != "grin" || r.RuleKey != "smile" || r.RuleList != "global" {
		t.Errorf("smile on gelbooru = %+v, want the rule for all sites named", r)
	}
	if r := m.TagRules("general", "kept", sites).Sites[0]; r.Rule != "none" || r.RuleKey != "" || r.RuleList != "" {
		t.Errorf("kept on danbooru = %+v, want no rule named for a tag left as it is", r)
	}
	if r := m.TagRules("general", "tagme", sites).Sites[2]; r.Rule != "drop" || r.RuleKey != "tagme" || r.RuleList != "global" {
		t.Errorf("tagme on the PTR = %+v, want the drop for all sites named", r)
	}

	rows = m.TagRules("general", "smiling", sites).Sites
	if r := rows[0]; !slices.Equal(r.RenamedFrom, []string{"smile"}) || !slices.Equal(r.RenamedFromGlobal, []string{"laugh"}) {
		t.Errorf("smiling on danbooru = %+v, want smile from its own rules and laugh from the rules for all sites", r)
	}
	if r := rows[1]; r.RenamedFrom != nil || !slices.Equal(r.RenamedFromGlobal, []string{"laugh"}) {
		t.Errorf("smiling on gelbooru = %+v, want laugh alone", r)
	}
	rows = m.TagRules("general", "grin", sites).Sites
	if r := rows[0]; r.RenamedFromGlobal != nil {
		t.Errorf("grin on danbooru = %+v, want nothing: its own rule takes smile first", r)
	}
	if r := rows[1]; !slices.Equal(r.RenamedFromGlobal, []string{"smile"}) {
		t.Errorf("grin on gelbooru = %+v, want smile", r)
	}
	rows = m.TagRules("character", "grin", sites).Sites
	if r := rows[0]; r.RenamedFromGlobal != nil {
		t.Errorf("character:grin on danbooru = %+v, want nothing: its own rule keeps grin_(va)", r)
	}
	if r := rows[1]; !slices.Equal(r.RenamedFromGlobal, []string{"grin_(va)", "smile"}) {
		t.Errorf("character:grin on gelbooru = %+v, want grin_(va) rebuilt and a character's smile", r)
	}
}

func TestTagRulesNameWhatABlacklistLineAlsoNeeds(t *testing.T) {
	m := traceFixture(t, nil, []string{
		"watermark", "Signature text", "lowres rating:explicit", "-solo comic", "artist:foo photo",
		"*_(cosplay) photo", "[sample] text", "smiling solo",
	})
	sites := []string{"danbooru", "gelbooru"}
	for _, c := range []struct{ category, name, site, want string }{
		{"general", "watermark", "danbooru", ""},
		{"general", "signature", "danbooru", "text"},
		{"general", "lowres", "gelbooru", "rating:explicit"},
		{"general", "comic", "danbooru", "-solo"},
		{"artist", "foo", "danbooru", "photo"},
		{"general", "miku_(cosplay)", "danbooru", "photo"},
		{"general", "[sample]", "danbooru", "text"},
		{"general", "smile", "danbooru", "solo"},
	} {
		rules := m.TagRules(c.category, c.name, sites)
		var r *contract.TagRule
		for i := range rules.Sites {
			if rules.Sites[i].Site == c.site {
				r = &rules.Sites[i]
			}
		}
		if r == nil || r.BlacklistCondition == nil || *r.BlacklistCondition != c.want {
			t.Errorf("%s:%s on %s = %+v, want the condition %q", c.category, c.name, c.site, r, c.want)
		}
	}
	if r := m.TagRules("general", "smile", sites).Sites[1]; r.BlacklistCondition != nil || r.BlacklistLine != "" {
		t.Errorf("smile on gelbooru, which keeps the name, = %+v, want no line", r)
	}
	if r := m.TagRules("general", "touhou", sites).Sites[0]; r.BlacklistCondition != nil {
		t.Errorf("touhou = %+v, want no condition where no line names it", r)
	}
}
