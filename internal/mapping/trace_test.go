package mapping

import (
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func traceFixture(t *testing.T, drop, global []string) *Mapper {
	t.Helper()
	dir := t.TempDir()
	writeProfile(t, dir, "danbooru.json", `{"family":"danbooru","auth":"none",
		"category_overrides":{"meta":"","copyright":"general"},
		"tag_rules":{"lowres":"","smile":"smiling","sketch":"meta:sketch"}}`)
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "danbooru", Blacklist: []string{"site_only"}}}
	cfg.Tags.DropCategories = drop
	cfg.Tags.Blacklist = global
	return newMapperDir(t, cfg, dir)
}

var tracePost = map[string]any{
	"category": "danbooru", "id": float64(7), "rating": "s",
	"tags_general":   []any{"lowres", "sketch", "smile"},
	"tags_meta":      []any{"absurdres"},
	"tags_copyright": []any{"original"},
	"tags_artist":    []any{"foo"},
}

func traceRow(t *testing.T, tr PostTrace, listed string) TraceRow {
	t.Helper()
	for _, r := range tr.Rows {
		if r.Listed == listed {
			return r
		}
	}
	t.Fatalf("no row lists %q: %+v", listed, tr.Rows)
	return TraceRow{}
}

func TestTraceRowsAddUpToWhatMapPushes(t *testing.T) {
	for _, drop := range [][]string{nil, {"meta"}, {"artist", "general"}} {
		m := traceFixture(t, drop, nil)
		tr := m.Trace(tracePost)
		var sent []string
		for _, r := range tr.Rows {
			if r.Sent != "" {
				sent = append(sent, r.Sent)
			}
		}
		sent = append(sent, "rating:"+tr.Rating)
		slices.Sort(sent)
		if want := m.Map(tracePost).Tags; !slices.Equal(sent, want) || !slices.Equal(tr.Tags, want) {
			t.Errorf("drop %v: rows %q, verdict %q, Map %q", drop, sent, tr.Tags, want)
		}
	}
}

func TestTraceFollowsATagThroughTheSteps(t *testing.T) {
	tr := traceFixture(t, []string{"meta"}, nil).Trace(tracePost)
	if tr.Skipped != "" || tr.Site != "danbooru" || tr.RatingListed != "s" || tr.Rating != RatingSensitive {
		t.Errorf("header = %+v", tr)
	}
	if r := traceRow(t, tr, "absurdres"); !r.MapDropped || r.Field != "meta" || r.Sent != "" {
		t.Errorf("a site-dropped category: %+v", r)
	}
	if r := traceRow(t, tr, "lowres"); !r.RuleDropped || r.Sent != "" || len(r.Reads) != 1 {
		t.Errorf("a blank tag rule: %+v", r)
	}
	if r := traceRow(t, tr, "smile"); r.Mapped != "smile" || r.Ruled != "smiling" || r.Sent != "smiling" || len(r.Reads) != 2 {
		t.Errorf("a rename: %+v", r)
	}
	if r := traceRow(t, tr, "original"); r.Field != "copyright" || r.Category != "general" {
		t.Errorf("a remapped category: %+v", r)
	}
	if r := traceRow(t, tr, "sketch"); r.Unchecked != "meta" || r.Sent != "" {
		t.Errorf("a rule into an unchecked category: %+v", r)
	}
	if len(tr.Warnings) != 1 || !strings.Contains(tr.Warnings[0], "sketch -> meta:sketch") {
		t.Errorf("warnings = %q", tr.Warnings)
	}
}

func TestTraceNamesTheRuleThatSkipsAndMarksWhatItRead(t *testing.T) {
	m := traceFixture(t, []string{"artist"}, []string{"nope", "absurdres"})
	tr := m.Trace(tracePost)
	note, _ := m.Blacklisted(tracePost)
	if tr.Skipped != "global rule: absurdres" || tr.Skipped != note {
		t.Errorf("skipped = %q, Blacklisted = %q", tr.Skipped, note)
	}
	if r := traceRow(t, tr, "absurdres"); !r.Reads[0].Hit {
		t.Errorf("the tag that matched should be marked: %+v", r)
	}
	if r := traceRow(t, tr, "foo"); r.Reads[0].Hit {
		t.Errorf("a tag that did not match should not be: %+v", r)
	}

	tr = traceFixture(t, nil, []string{"rating:sensitive"}).Trace(tracePost)
	if !tr.RatingHit || tr.Skipped != "global rule: rating:sensitive" {
		t.Errorf("a rating rule: %+v", tr)
	}
	tr = traceFixture(t, nil, nil).Trace(map[string]any{"category": "danbooru", "tags_general": []any{"site_only"}})
	if tr.Skipped != "danbooru rule: site_only" {
		t.Errorf("the site's list: %q", tr.Skipped)
	}
}

func TestTraceWarnsOfACategoryRuleTheMapMisfiles(t *testing.T) {
	tr := traceFixture(t, nil, []string{"copyright:original", "general:original", "artist:elsewhere", "-artist:foo"}).Trace(tracePost)
	if tr.Skipped != "global rule: general:original" {
		t.Errorf("skipped = %q", tr.Skipped)
	}
	if len(tr.Warnings) != 1 || !strings.Contains(tr.Warnings[0], `"copyright:original" misses`) || !strings.Contains(tr.Warnings[0], "under general") {
		t.Errorf("warnings = %q", tr.Warnings)
	}
}

func TestTraceWarnsOfACategoryMonbooruDoesNotList(t *testing.T) {
	withCustomCategories(t)
	m := newMapper(t, config.Default())
	m.LearnCustomCategories([]string{"renamedcat"}, true)
	m.view().profiles["newgrounds"] = Profile{Family: FamilyGeneric, CategoryOverrides: map[string]string{"meta": "tempcat"}}
	meta := map[string]any{"category": "newgrounds", "id": float64(1), "tags_meta": []any{"tagged_meta"}, "tags_general": []any{"x"}}

	if got := m.Map(meta).UnknownTargets; len(got) != 1 || !strings.Contains(got[0], "tempcat") {
		t.Errorf("UnknownTargets = %v, want tempcat named", got)
	}
	if tr := m.Trace(meta); !slices.ContainsFunc(tr.Warnings, func(w string) bool { return strings.Contains(w, "tempcat") }) {
		t.Errorf("Trace warnings = %v, want tempcat named", tr.Warnings)
	}
	m.LearnCustomCategories([]string{"tempcat"}, true)
	if got := m.Map(meta).UnknownTargets; len(got) != 0 {
		t.Errorf("a listed category warned: %v", got)
	}
}

func TestTraceShowsAMangaFieldTheMapDrops(t *testing.T) {
	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "mangadex", From: "group", To: ""}}
	tr := newMapper(t, cfg).Trace(map[string]any{"category": "mangadex", "chapter_id": "x", "group": []any{"TCB Scans"}})
	for _, r := range tr.Rows {
		if r.Field == "group" {
			if !r.MapDropped || r.Sent != "" {
				t.Errorf("group row = %+v, want dropped by the map", r)
			}
			return
		}
	}
	t.Errorf("no group row in %+v", tr.Rows)
}

func TestTraceRefusesAPostThePushRefuses(t *testing.T) {
	flat := map[string]any{"category": "safebooru", "id": float64(199), "rating": "s", "tags": "gel_artist199 smile"}
	tr := newMapper(t, nil).Trace(flat)
	if tr.Refused == "" {
		t.Error("a flat post of a categorizing site fails its push, so Trace should say so")
	}
	categorized := map[string]any{"category": "safebooru", "id": float64(121), "rating": "s", "tags": "x smile", "tags_artist": "x", "tags_general": "smile"}
	if tr := newMapper(t, nil).Trace(categorized); tr.Refused != "" {
		t.Errorf("Refused = %q on a categorized post", tr.Refused)
	}
	cfg := config.Default()
	cfg.Tags.Blacklist = []string{"smile"}
	tr = newMapper(t, cfg).Trace(flat)
	if tr.Skipped == "" {
		t.Errorf("a blacklisted post skips first, as a push does: %+v", tr)
	}
}

func TestTraceListsTheRatingThePushReads(t *testing.T) {
	m := newMapper(t, nil)
	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{"a rating key", map[string]any{"category": "danbooru", "id": float64(1), "rating": "s", "tags_general": []any{"x"}}, "s"},
		{"a philomena tag", map[string]any{"category": "derpibooru", "id": float64(1), "tags": []any{"safe", "pony"}}, "safe"},
		{"civitai's level", map[string]any{"category": "civitai", "id": float64(1), "file": map[string]any{"nsfwLevel": float64(8)}}, "8"},
		{"nothing, on a default rating", map[string]any{"category": "hitomi", "gallery_id": float64(1)}, ""},
	} {
		if got := m.Trace(tc.meta).RatingListed; got != tc.want {
			t.Errorf("%s: RatingListed = %q, want %q", tc.name, got, tc.want)
		}
	}
}
