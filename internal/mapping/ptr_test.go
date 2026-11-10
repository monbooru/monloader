package mapping

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func TestMapPTRTags(t *testing.T) {
	in := []string{
		"blue_sky",
		"creator:some_artist",
		"character:reimu",
		"series:touhou",
		"person:jane_doe",
		"species:fox",
		"medium:watercolor",
		"year:2024",
		"rating:explicit",
		"page:5",
		"title:some doujin",
		"pixiv work:75168783",
		"weirdns:whole_dropped",
	}
	got := MapPTRTags(in)
	want := []string{
		"artist:some_artist",
		"blue_sky",
		"character:reimu",
		"copyright:touhou",
		"medium:watercolor",
		"person:jane_doe",
		"rating:explicit",
		"species:fox",
		"year:2024",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MapPTRTags = %v\n         want %v", got, want)
	}
}

func TestMapPTRTagsRatingLifted(t *testing.T) {

	got := MapPTRTags([]string{"rating:safe", "rating:nonsense"})
	if !reflect.DeepEqual(got, []string{"rating:general"}) {
		t.Errorf("rating mapping = %v, want [rating:general]", got)
	}
}

func TestMapPTRTagsNormalizesAndDedups(t *testing.T) {
	got := MapPTRTags([]string{"blue eyes", "blue_eyes", "creator:Some Artist"})
	want := []string{"artist:some_artist", "blue_eyes"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MapPTRTags = %v, want %v", got, want)
	}
}

func TestMapPTRTagsKeepsPunctuation(t *testing.T) {
	got := MapPTRTags([]string{
		"series:girls' frontline 2: exilium",
		"series:jill & her friends",
		"series:fate/grand order",
		"a;b,c%d[e]f{g}h|i`j",
		`say "cheese"`,
		"five*star",
	})
	want := []string{
		"a;b,c%d[e]f{g}h|i`j",
		"copyright:fate/grand_order",
		"copyright:girls'_frontline_2:_exilium",
		"copyright:jill_&_her_friends",
		"five_star",
		"say_cheese",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MapPTRTags = %v\n         want %v", got, want)
	}
}

func TestLegacyFoldTag(t *testing.T) {
	cases := map[string]string{
		"copyright:girls'_frontline_2:_exilium": "copyright:girls_frontline_2:_exilium",
		"copyright:jill_&_her_friends":          "copyright:jill_her_friends",
		"copyright:fate/grand_order":            "copyright:fate_grand_order",
		"blue_eyes":                             "blue_eyes",
	}
	for in, want := range cases {
		if got := LegacyFoldTag(in); got != want {
			t.Errorf("LegacyFoldTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPTRRoundTripIdentity(t *testing.T) {
	for _, raw := range []string{
		"series:girls' frontline 2: exilium",
		"series:jill & her friends",
		"series:fate/grand order",
		"::d",
		"creator:d'orsay",
		"rock & roll",
		"pokémon",
		"creator:欧陽鋒",
		"艦これ",
	} {
		mapped := MapPTRTag(raw)
		if mapped == "" {
			t.Errorf("MapPTRTag(%q) dropped the tag", raw)
			continue
		}
		got := ContribTagFor(mapped)
		if got.PTR != raw {
			t.Errorf("round trip %q -> %q -> %q (note %q)", raw, mapped, got.PTR, got.Note)
		}
	}
}

func TestMapPTRTagsKeepsUnicode(t *testing.T) {
	got := MapPTRTags([]string{
		"ぼっち・ざ・ろっく!10000users入り",
		"creator:欧陽鋒",
		"pokémon",
		"blue_sky",
	})
	joined := strings.Join(got, " ")
	for _, want := range []string{"ぼっち・ざ・ろっく!10000users入り", "欧陽鋒", "pokémon", "blue_sky"} {
		if !strings.Contains(joined, want) {
			t.Errorf("MapPTRTags = %v, want it to keep %q", got, want)
		}
	}
}

func TestSubtagFormsStayBoundedAtAnyUnderscoreCount(t *testing.T) {
	cases := []struct{ underscores, forms int }{{3, 8}, {4, 2}, {62, 2}, {63, 2}, {64, 2}, {200, 2}}
	for _, tc := range cases {
		name := strings.Repeat("a_", tc.underscores) + "a"
		if got := len(subtagForms(name)); got != tc.forms {
			t.Errorf("%d underscores: %d forms, want %d", tc.underscores, got, tc.forms)
		}
	}
}

func TestPTRNamespacesFor(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"blue_eyes", []string{"blue_eyes", "blue eyes"}},
		{"artist:some_name", []string{"creator:some_name", "creator:some name", "artist:some_name", "artist:some name"}},
		{"character:reimu", []string{"character:reimu"}},
		{":d", []string{"::d"}},
		{"species:red_fox", []string{"species:red_fox", "species:red fox"}},
		{"year:2024", []string{"year:2024"}},
		{"copyright:touhou_project", []string{
			"series:touhou_project", "series:touhou project",
			"copyright:touhou_project", "copyright:touhou project",
			"studio:touhou_project", "studio:touhou project",
		}},

		{"character:kagamine_rin_cosplay", []string{
			"character:kagamine_rin_cosplay", "character:kagamine rin cosplay",
			"character:kagamine rin_cosplay", "character:kagamine_rin cosplay",
		}},
	}
	for _, tc := range cases {
		if got := PTRNamespacesFor(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("PTRNamespacesFor(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPTRNamespacesForInvertsTheProjection(t *testing.T) {
	for _, name := range []string{
		"character:kagamine_rin_cosplay",
		"copyright:hololive_dev_is",
		"artist:some_name",
		"blue_eyes",
		":d",
	} {
		for _, raw := range PTRNamespacesFor(name) {
			if got := MapPTRTag(raw); got != name {
				t.Errorf("MapPTRTag(%q) = %q, want %q", raw, got, name)
			}
		}
	}
}

func TestContribTagFor(t *testing.T) {
	cases := []struct {
		in   string
		ptr  string
		note string
	}{
		{"blonde_hair", "blonde hair", ""},
		{":d", "::d", ""},
		{"artist:some_name", "creator:some name", ""},
		{"copyright:touhou_project", "series:touhou project", ""},
		{"character:reimu", "character:reimu", ""},
		{"person:jane_doe", "person:jane doe", ""},
		{"species:red_fox", "species:red fox", ""},
		{"medium:watercolor", "medium:watercolor", ""},
		{"meta:highres", "meta:highres", ""},
		{"rating:explicit", "", "unsupported namespace"},
		{"year:2024", "", "unsupported namespace"},

		{"studio:kyoani", "studio:kyoani", ""},
		{"photoset:beach_set", "photoset:beach set", ""},
		{"title:some_doujin", "title:some doujin", ""},

		{"booru:danbooru", "", "unmapped category"},
		{"mood:cozy", "", "unmapped category"},

		{"double__underscore", "", "the repository would rewrite this tag"},
		{"artist:", "", "the repository would rewrite this tag"},
	}
	for _, tc := range cases {
		got := ContribTagFor(tc.in)
		if got.PTR != tc.ptr || got.Note != tc.note {
			t.Errorf("ContribTagFor(%q) = %+v, want ptr %q note %q", tc.in, got, tc.ptr, tc.note)
		}
	}
}

func TestContribCodomainIsSupportedSet(t *testing.T) {
	supported := map[string]bool{
		"creator": true, "character": true, "person": true, "series": true,
		"photoset": true, "studio": true, "species": true, "title": true,
		"medium": true, "meta": true,
	}
	categories := []string{
		"", "general", "artist", "character", "copyright", "meta", "medium",
		"person", "species", "year", "rating", "studio", "photoset", "title",
		"creator", "series", "booru", "mood", "filename", "url",
	}
	for _, cat := range categories {
		in := "name_part"
		if cat != "" {
			in = cat + ":name_part"
		}
		got := ContribTagFor(in)
		if !got.Eligible() {
			if cat == "rating" || cat == "year" {
				continue
			}
			continue
		}
		if cat == "rating" || cat == "year" {
			t.Errorf("category %q must be refused, emitted %q", cat, got.PTR)
			continue
		}
		ns, _, hasNS := strings.Cut(got.PTR, ":")
		if hasNS && !supported[ns] {
			t.Errorf("category %q emitted off-list namespace %q", cat, ns)
		}
	}
	for _, cat := range []string{"studio", "photoset", "title"} {
		if got := ContribTagFor(cat + ":x"); !got.Eligible() {
			t.Errorf("carve-out %q refused: %+v", cat, got)
		}
	}
}

func ptrMapper(t *testing.T, cfg *config.Config, p Profile) *Mapper {
	t.Helper()
	m := newMapperDir(t, cfg, t.TempDir())
	p.Family, p.Kind, p.Auth = FamilyGeneric, KindOther, AuthNone
	if err := m.SaveUserProfile(config.SitePTR, p); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMapPTRWithNoSettingsMatchesTheNamespaceTable(t *testing.T) {
	raw := []string{"blue_sky", "creator:a", "series:b", "rating:explicit", "title:dropped", "weird:dropped"}
	got := newMapper(t, config.Default()).MapPTR(raw)
	if want := MapPTRTags(raw); !slices.Equal(got, want) {
		t.Errorf("MapPTR = %v, want the table's %v", got, want)
	}
}

func TestMapPTRRunsThePTRMappings(t *testing.T) {
	m := ptrMapper(t, config.Default(), Profile{
		RatingOverrides:   map[string]string{"questionable": RatingSensitive},
		CategoryOverrides: map[string]string{"studio": "artist", "title": "meta", "character": "", "general": "meta"},
		TagRules:          map[string]string{"copyright:touhou": "copyright:gensokyo", "meta:blue_sky": ""},
	})
	got := m.MapPTR([]string{
		"rating:questionable", "studio:ufotable", "title:a_title", "character:reimu",
		"series:touhou", "blue sky", "other", "creator:kept",
	})
	want := []string{"artist:kept", "artist:ufotable", "copyright:gensokyo", "meta:a_title", "meta:other", "rating:sensitive"}
	if !slices.Equal(got, want) {
		t.Errorf("MapPTR = %v, want %v", got, want)
	}
}

func TestMapPTRWithheldNamesWhatTheRowHeldBack(t *testing.T) {
	m := ptrMapper(t, config.Default(), Profile{
		CategoryOverrides: map[string]string{"species": ""},
		TagRules:          map[string]string{"blue_sky": ""},
	})
	tags, withheld := m.MapPTRWithheld([]string{"species:cat", "blue sky", "title:x", "smile"})
	if !slices.Equal(tags, []string{"smile"}) {
		t.Errorf("tags = %v, want smile", tags)
	}
	if !slices.Equal(withheld, []string{"blue_sky", "species:cat"}) {
		t.Errorf("withheld = %v, want the row's drop and blank rule, and no unrouted namespace", withheld)
	}
}

func TestAPTREmoticonTagIsBareAndAnswersTheGeneralRow(t *testing.T) {
	if got := MapPTRTags([]string{"::d", ":d", "smile"}); !slices.Equal(got, []string{":d", "smile"}) {
		t.Errorf("MapPTRTags = %v", got)
	}
	m := ptrMapper(t, config.Default(), Profile{CategoryOverrides: map[string]string{"general": "meta"}})
	if got := m.MapPTR([]string{"::d"}); !slices.Equal(got, []string{"meta::d"}) {
		t.Errorf("MapPTR = %v", got)
	}
}

func TestMapPTRDropsUncheckedCategoriesButNeverTheRating(t *testing.T) {
	cfg := config.Default()
	cfg.Tags.DropCategories = []string{"general", "va"}
	withCustomCategories(t, "va")
	m := ptrMapper(t, cfg, Profile{CategoryOverrides: map[string]string{"voice": "va"}})
	got := m.MapPTR([]string{"1girl", "creator:a", "rating:general", "voice:c"})
	if want := []string{"artist:a", "rating:general"}; !slices.Equal(got, want) {
		t.Errorf("MapPTR = %v, want %v", got, want)
	}
}

func TestMapPTRTomlOverridesWinOverThePTRProfile(t *testing.T) {
	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: config.SitePTR, From: "studio", To: "meta"}}
	cfg.RatingOverrides = []config.RatingOverride{{Site: config.SitePTR, From: "safe", To: RatingSensitive}}
	m := ptrMapper(t, cfg, Profile{
		CategoryOverrides: map[string]string{"studio": "artist"},
		RatingOverrides:   map[string]string{"safe": RatingExplicit},
	})
	got := m.MapPTR([]string{"studio:s", "rating:safe"})
	if want := []string{"meta:s", "rating:sensitive"}; !slices.Equal(got, want) {
		t.Errorf("MapPTR = %v, want %v", got, want)
	}
}

func TestMapPTRTagIgnoresThePTRProfile(t *testing.T) {
	ptrMapper(t, config.Default(), Profile{CategoryOverrides: map[string]string{"studio": "artist"}})
	if got := MapPTRTag("studio:s"); got != "copyright:s" {
		t.Errorf("MapPTRTag = %q, want the fixed table's spelling", got)
	}
}

func TestPTRBlacklisted(t *testing.T) {
	blacklist := func(rules ...string) *config.Config {
		cfg := config.Default()
		cfg.Sites = append(cfg.Sites, config.Site{Name: config.SitePTR, Blacklist: rules})
		return cfg
	}
	profile := Profile{
		TagRules:          map[string]string{"old_name": "new_name"},
		CategoryOverrides: map[string]string{"character": ""},
	}
	for _, tc := range []struct {
		name, rule string
		raw        []string
		hit        bool
	}{
		{"a bare tag", "bad", []string{"bad", "creator:a"}, true},
		{"no match", "bad", []string{"fine"}, false},
		{"the namespace reads as its category", "artist:a", []string{"creator:a"}, true},
		{"the tag as the PTR lists it", "old_name", []string{"old_name"}, true},
		{"the tag after a tag rule", "new_name", []string{"old_name"}, true},
		{"a namespace the map drops", "character:reimu", []string{"character:reimu"}, true},
		{"a rating", "rating:explicit", []string{"rating:explicit"}, true},
		{"another rating", "rating:explicit", []string{"rating:safe"}, false},
		{"the highest of several ratings", "rating:explicit", []string{"rating:explicit", "rating:questionable", "rating:safe"}, true},
		{"not a lower one of several", "rating:general", []string{"rating:explicit", "rating:safe"}, false},
		{"a negation", "-bad", []string{"fine"}, true},
		{"every token of a line", "bad worse", []string{"bad"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note, hit := ptrMapper(t, blacklist(tc.rule), profile).PTRBlacklisted(tc.raw)
			if hit != tc.hit {
				t.Fatalf("hit = %v (%q), want %v", hit, note, tc.hit)
			}
			if hit && note != "ptr rule: "+tc.rule {
				t.Errorf("note = %q, want it to name the rule and its list", note)
			}
		})
	}
}

func TestPTRBlacklistedIgnoresTheGlobalList(t *testing.T) {
	cfg := config.Default()
	cfg.Tags.Blacklist = []string{"bad"}
	if _, hit := newMapper(t, cfg).PTRBlacklisted([]string{"bad"}); hit {
		t.Error("the global list stops downloads, it must not skip a PTR answer")
	}
}

func TestPTRProfileStaysOutOfTheGalleryDLSiteLists(t *testing.T) {
	m := ptrMapper(t, config.Default(), Profile{TagRules: map[string]string{"a": "b"}})
	if slices.Contains(m.CuratedCategories(), config.SitePTR) {
		t.Error("the ptr profile must not read as a gallery-dl site")
	}
}
