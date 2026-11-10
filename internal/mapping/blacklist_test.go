package mapping

import (
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func blacklistMapper(t *testing.T, rules ...string) *Mapper {
	t.Helper()
	cfg := config.Default()
	cfg.Tags.Blacklist = rules
	return newMapper(t, cfg)
}

func TestCleanBlacklist(t *testing.T) {
	withCustomCategories(t, "va")
	got, err := CleanBlacklist("guro\r\n\r\n  # a comment\n furry  -rating:general \nVA:Someone\n*_(cosplay)\n:3\n")
	if err != nil {
		t.Fatalf("CleanBlacklist: %v", err)
	}
	want := []string{"guro", "# a comment", "furry  -rating:general", "VA:Someone", "*_(cosplay)", ":3"}
	if !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	for _, tc := range []struct{ text, line, token string }{
		{"guro\nscore:<0", "line 2", "score:<0"},
		{"\n\nuser:x", "line 3", "user:x"},
		{"a ~b", "line 1", "~b"},
		{"a -", "line 1", "-"},
		{"rating:maybe", "line 1", "rating:maybe"},
		{"re:zero", "line 1", "re:zero"},
		{"artist:", "line 1", "artist:"},
	} {
		_, err := CleanBlacklist(tc.text)
		if err == nil || !strings.Contains(err.Error(), tc.line) || !strings.Contains(err.Error(), tc.token) {
			t.Errorf("CleanBlacklist(%q) = %v, want a refusal naming %s and %s", tc.text, err, tc.line, tc.token)
		}
	}
}

func TestBlacklistedRuleGrammar(t *testing.T) {
	post := map[string]any{
		"category": "danbooru", "id": float64(1), "rating": "q",
		"tags_general":   []any{"blue_sky", "Red Hair", "maid_(cosplay)"},
		"tags_artist":    []any{"paruperu"},
		"tags_copyright": []any{"touhou"},
	}
	for _, tc := range []struct {
		rule string
		want bool
	}{
		{"blue_sky", true},
		{"BLUE_SKY", true},
		{"red_hair", true},
		{"green", false},
		{"blue_sky red_hair", true},
		{"blue_sky green", false},
		{"blue_sky -green", true},
		{"blue_sky -red_hair", false},
		{"-green", true},
		{"paruperu", true},
		{"artist:paruperu", true},
		{"character:paruperu", false},
		{"-artist:paruperu", false},
		{"general:blue_sky", true},
		{"rating:questionable", true},
		{"rating:q", true},
		{"rating:explicit", false},
		{"-rating:general blue_sky", true},
		{"*_(cosplay)", true},
		{"blue_*", true},
		{"*ue_s*", true},
		{"b*l*y", true},
		{"b*x*y", false},
		{"artist:par*", true},
		{"copyright:par*", false},
		{"# blue_sky", false},
	} {
		rule, hit := blacklistMapper(t, tc.rule).Blacklisted(post)
		if hit != tc.want {
			t.Errorf("rule %q: blacklisted = %v, want %v", tc.rule, hit, tc.want)
		}
		if hit && rule != "global rule: "+tc.rule {
			t.Errorf("rule %q: named %q", tc.rule, rule)
		}
	}
}

func TestBlacklistedNamesTheFirstRuleAndNoRulesIsClean(t *testing.T) {
	post := map[string]any{"category": "danbooru", "tags_general": []any{"a", "b"}}
	if _, hit := blacklistMapper(t).Blacklisted(post); hit {
		t.Error("no rules should blacklist nothing")
	}
	if rule, _ := blacklistMapper(t, "nope", "b", "a").Blacklisted(post); rule != "global rule: b" {
		t.Errorf("rule = %q, want the first that matches", rule)
	}
}

func TestBlacklistedAddsTheSiteListToTheGlobalOne(t *testing.T) {
	cfg := config.Default()
	cfg.Tags.Blacklist = []string{"global_tag"}
	cfg.Sites = []config.Site{{Name: "e621", Blacklist: []string{"site_tag"}}}
	m := newMapper(t, cfg)
	check := func(category, tag, want string) {
		t.Helper()
		got, _ := m.Blacklisted(map[string]any{"category": category, "tags_general": []any{tag}})
		if got != want {
			t.Errorf("%s %s = %q, want %q", category, tag, got, want)
		}
	}
	check("e621", "global_tag", "global rule: global_tag")
	check("e621", "site_tag", "e621 rule: site_tag")
	check("danbooru", "global_tag", "global rule: global_tag")
	check("danbooru", "site_tag", "")
	if got := cfg.Tags.Blacklist; len(got) != 1 {
		t.Errorf("reading the site's rules changed the global list: %q", got)
	}
}

func TestBlacklistedReadsBothSpellingsAndIgnoresTheDrop(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "danbooru.json", `{"family":"danbooru","auth":"none",
		"tag_rules":{"old_name":"new_name","artist:moved":"character:moved","suppressed":""}}`)
	cfg := config.Default()
	cfg.Sites = nil
	cfg.Tags.DropCategories = []string{"general"}
	cfg.Tags.Blacklist = []string{"new_name"}
	post := map[string]any{"category": "danbooru", "tags_general": []any{"old_name", "suppressed"}, "tags_artist": []any{"moved"}}

	m := newMapperDir(t, cfg, dir)
	if _, hit := m.Blacklisted(post); !hit {
		t.Error("a rule should match the name after a tag rule, general dropped or not")
	}
	for _, rule := range []string{"old_name", "suppressed", "artist:moved", "character:moved"} {
		cfg.Tags.Blacklist = []string{rule}
		if _, hit := newMapperDir(t, cfg, dir).Blacklisted(post); !hit {
			t.Errorf("rule %q should match", rule)
		}
	}
}

func TestBlacklistedReadsACategoryTheSiteMapDrops(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "danbooru.json", `{"family":"danbooru","auth":"none","category_overrides":{"meta":""}}`)
	cfg := config.Default()
	cfg.Sites = nil
	post := map[string]any{"category": "danbooru", "tags_meta": []any{"absurdres"}, "tags_invalid": []any{"not_a_tag"}}

	for rule, want := range map[string]bool{"absurdres": true, "meta:absurdres": true, "general:absurdres": false, "not_a_tag": false} {
		cfg.Tags.Blacklist = []string{rule}
		if _, hit := newMapperDir(t, cfg, dir).Blacklisted(post); hit != want {
			t.Errorf("rule %q: hit = %v, want %v", rule, hit, want)
		}
	}
}

func TestBlacklistedLeavesACategoryRuleForTheSidecarOnAFlatList(t *testing.T) {
	flat := map[string]any{"category": "gelbooru", "tags": "blue_sky paruperu", "rating": "e"}
	sidecar := map[string]any{"category": "gelbooru", "rating": "e",
		"tags_general": []any{"blue_sky"}, "tags_artist": []any{"paruperu"}}
	for _, tc := range []struct {
		rule          string
		flat, sidecar bool
	}{
		{"blue_sky", true, true},
		{"rating:explicit", true, true},
		{"artist:paruperu", false, true},
		{"blue_sky -artist:nobody", false, true},
		{"blue_sky -artist:paruperu", false, false},
	} {
		m := blacklistMapper(t, tc.rule)
		if _, hit := m.Blacklisted(flat); hit != tc.flat {
			t.Errorf("rule %q on the flat list = %v, want %v", tc.rule, hit, tc.flat)
		}
		if _, hit := m.Blacklisted(sidecar); hit != tc.sidecar {
			t.Errorf("rule %q on the sidecar = %v, want %v", tc.rule, hit, tc.sidecar)
		}
	}
}

func TestBlacklistedSkipsARuleThatStoppedParsing(t *testing.T) {
	m := blacklistMapper(t, "score:9 blue_sky", "gone:x")
	if _, hit := m.Blacklisted(map[string]any{"category": "danbooru", "tags_general": []any{"blue_sky"}}); hit {
		t.Error("a rule that does not parse must not match")
	}
}

func TestBlacklistReadsACommaNameWhole(t *testing.T) {
	m := blacklistMapper(t, "hello,_happy_world!")
	meta := map[string]any{"category": "safebooru", "id": float64(1), "tags_copyright": "hello,_happy_world! bang_dream!"}
	if _, blocked := m.Blacklisted(meta); !blocked {
		t.Error("a rule naming the whole tag should hit")
	}
}

func TestBlacklistReadsANamespaceOutsideTheTable(t *testing.T) {
	m := blacklistMapper(t, "guro")
	meta := map[string]any{"category": "exhentai", "gallery_id": float64(1),
		"tags": []any{"other:guro"}, "tags_other": []any{"guro"}}
	if _, blocked := m.Blacklisted(meta); !blocked {
		t.Error("a bare rule should hit tags_other")
	}
}
