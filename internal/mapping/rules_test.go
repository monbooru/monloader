package mapping

import (
	"slices"
	"testing"

	"github.com/monbooru/monloader/internal/config"
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
