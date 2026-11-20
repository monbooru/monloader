package mapping

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func TestTagNameVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "shared", "contract", "testdata", "tagnames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Names, Forms []struct{ In, Out string } }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range append(v.Names, v.Forms...) {
		if got := normalizeTag(c.In); got != c.Out {
			t.Errorf("normalizeTag(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func withCustomCategories(t *testing.T, names ...string) {
	t.Helper()
	customCategoriesMu.Lock()
	customCategories = map[string]bool{}
	customCategoriesLearned = false
	for _, name := range names {
		customCategories[name] = true
	}
	customCategoriesMu.Unlock()
	t.Cleanup(func() {
		customCategoriesMu.Lock()
		customCategories = map[string]bool{}
		customCategoriesLearned = false
		customCategoriesMu.Unlock()
	})
}

func TestNormalizeTagSplitsAKnownCustomCategory(t *testing.T) {
	if got := normalizeTag("VA: So And So"); got != "va:_so_and_so" {
		t.Errorf("unknown prefix = %q, want it folded as part of the name", got)
	}
	withCustomCategories(t, "va")
	if got := normalizeTag("VA: So And So"); got != "va:so_and_so" {
		t.Errorf("custom category = %q, want its prefix split before the name folds", got)
	}
}

func TestApplyTagRuleCustomCategory(t *testing.T) {
	rules := map[string]string{"artist:soandso_(va)": "va:soandso", "artist:other": "re:zero", "va:old": "new"}
	if got := applyTagRule(rules, "artist:soandso_(va)"); got != "artist:va:soandso" {
		t.Errorf("before monbooru answers = %q, want a rename inside the tag's own category", got)
	}
	withCustomCategories(t, "va")
	if got := applyTagRule(rules, "artist:soandso_(va)"); got != "va:soandso" {
		t.Errorf("retarget = %q, want the tag moved to the custom category", got)
	}
	if got := applyTagRule(rules, "artist:other"); got != "artist:re:zero" {
		t.Errorf("incidental colon = %q, want a rename, not a retarget", got)
	}
	if got := applyTagRule(rules, "va:old"); got != "va:new" {
		t.Errorf("a bare replacement = %q, want the custom category kept", got)
	}
}

func TestApplyTagRuleStar(t *testing.T) {
	withCustomCategories(t, "va")
	rules := map[string]string{
		"*_(va)":            "va:*",
		"artist:*_(studio)": "copyright:*",
		"*_(cosplay)":       "",
		"*_(style)":         "style",
		"bad_*":             "worse_*",
		"bad_*_(va)":        "va:bad_*",
		"x_(va)":            "kept",
		"a*z":               "first",
		"*bz":               "second",
	}
	for tag, want := range map[string]string{
		"artist:soandso_(va)":  "va:soandso",
		"soandso_(va)":         "va:soandso",
		"_(va)":                "_(va)",
		"artist:kyo_(studio)":  "copyright:kyo",
		"kyo_(studio)":         "kyo_(studio)",
		"reimu_(cosplay)":      "",
		"artist:ink_(style)":   "artist:style",
		"bad_hands":            "worse_hands",
		"character:bad_apple":  "character:worse_apple",
		"artist:bad_egg_(va)":  "va:bad_egg",
		"artist:x_(va)":        "artist:kept",
		"abz":                  "second",
		"smile":                "smile",
		"character:(va)_first": "character:(va)_first",
	} {
		if got := applyTagRule(rules, tag); got != want {
			t.Errorf("%q = %q, want %q", tag, got, want)
		}
	}
}

func TestRuleKeyFoldKeepsTheStar(t *testing.T) {
	for in, want := range map[string]string{"*_(VA)": "*_(va)", "Artist: * (va)": "artist:*_(va)", " smile ": "smile"} {
		if got := NormalizeRuleKey(in); got != want {
			t.Errorf("key %q = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"VA:*": "va:*", "general:*": "general:*", "a * b": "a_*_b"} {
		withCustomCategories(t, "va")
		if got := NormalizeRuleTarget(in); got != want {
			t.Errorf("target %q = %q, want %q", in, got, want)
		}
	}
	if got := normalizeTag("*_(va)"); got != "(va)" {
		t.Errorf("a tag = %q, want its * still folded", got)
	}
}

func TestBaseCategoryNamesMonboorusCategories(t *testing.T) {
	newMapper(t, nil).LearnCustomCategories([]string{"fashion"}, false)
	for suffix, want := range map[string]string{
		"medium": "medium", "person": "person", "year": "year",
		"fashion": "fashion", "something_else": "general",
	} {
		if got, keep := baseCategory(suffix); got != want || !keep {
			t.Errorf("baseCategory(%q) = %q, %v, want %q", suffix, got, keep, want)
		}
	}
}

func TestAPartialCategoryListLeavesAnUnresolvedRuleMissing(t *testing.T) {
	withCustomCategories(t)
	m := blacklistMapper(t, "va:x")
	m.LearnCustomCategories([]string{"other"}, false)
	if !m.CategoryListMissing() {
		t.Error("a rule on a category no gallery has named yet should still hold the job")
	}
	m.LearnCustomCategories([]string{"va"}, false)
	if m.CategoryListMissing() {
		t.Error("a partial list holding the category resolves the rule")
	}
	withCustomCategories(t)
	m.LearnCustomCategories([]string{"other"}, true)
	if m.CategoryListMissing() {
		t.Error("a complete list settles it: the rule is skipped with a warning")
	}
}

func TestACompleteCategoryListReplacesAPartialOneAdds(t *testing.T) {
	withCustomCategories(t, "old")
	m := newMapper(t, nil)
	m.LearnCustomCategories([]string{"partial"}, false)
	if !isTagCategory("old") || !isTagCategory("partial") {
		t.Error("a partial list should only add")
	}
	m.LearnCustomCategories([]string{"renamed"}, true)
	if isTagCategory("old") || isTagCategory("partial") || !isTagCategory("renamed") {
		t.Error("a complete list should replace what was known")
	}
}

func TestCategoryListMissing(t *testing.T) {
	withCustomCategories(t)
	plain := newMapper(t, nil)
	if plain.CategoryListMissing() {
		t.Error("no rule names a custom category, so no list is needed")
	}
	for name, edit := range map[string]func(*config.Config){
		"override":  func(c *config.Config) { c.TagOverrides = []config.TagOverride{{Site: "e621", From: "lore", To: "va"}} },
		"blacklist": func(c *config.Config) { c.Tags.Blacklist = []string{"va:x"} },
		"site list": func(c *config.Config) { c.Sites = []config.Site{{Name: "danbooru", Blacklist: []string{"va:x"}}} },
	} {
		cfg := config.Default()
		edit(cfg)
		m := newMapper(t, cfg)
		if !m.CategoryListMissing() {
			t.Errorf("%s naming va: want the list missing", name)
		}
		m.LearnCustomCategories([]string{"va"}, true)
		if m.CategoryListMissing() {
			t.Errorf("%s: the list was learned", name)
		}
		withCustomCategories(t)
	}
}

func TestParseTagFieldStringShapes(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"space-joined, comma inside a name", "hello,_happy_world! bang_dream!", []string{"hello,_happy_world!", "bang_dream!"}},
		{"a tag that is only punctuation", "smile ,_, solo", []string{"smile", ",_,", "solo"}},
		{"a list ending on a comma name", "smile comma,", []string{"smile", "comma,"}},
		{"realbooru's join", "1girl, green_eyes, school_uniform", []string{"1girl", "green_eyes", "school_uniform"}},
		{"rule34us's join", "big_breasts, blue_eyes", []string{"big_breasts", "blue_eyes"}},
		{"one name", "hello,_happy_world!", []string{"hello,_happy_world!"}},
		{"blank", "  ", []string{}},
	}
	for _, tc := range cases {
		if got := parseTagField(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parseTagField(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestNormalizeRuleTarget(t *testing.T) {
	for in, want := range map[string]string{
		"general:Some Tag": "general:some_tag",
		" GENERAL:y ":      "general:y",
		"general:":         "",
		"artist:Some Name": "artist:some_name",
		"Bare Name":        "bare_name",
		"":                 "",
	} {
		if got := NormalizeRuleTarget(in); got != want {
			t.Errorf("NormalizeRuleTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDroppedRuleTargetNamesGeneral(t *testing.T) {
	if got := DroppedRuleTarget([]string{"general"}, "artist:x", "general:y"); got != "general" {
		t.Errorf("DroppedRuleTarget = %q", got)
	}
	if got := DroppedRuleTarget([]string{"meta"}, "artist:x", "general:y"); got != "" {
		t.Errorf("DroppedRuleTarget = %q", got)
	}
}
