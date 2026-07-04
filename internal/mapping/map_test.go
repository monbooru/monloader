package mapping

import (
	"reflect"
	"slices"
	"testing"

	"github.com/leqwin/monloader/internal/config"
)

func newMapper(t *testing.T, cfg *config.Config) *Mapper {
	t.Helper()
	if cfg == nil {
		cfg = config.Default()
	}
	m, err := New(config.NewProvider(cfg))
	if err != nil {
		t.Fatalf("New mapper: %v", err)
	}
	return m
}

func TestMapDanbooruPost(t *testing.T) {
	m := newMapper(t, nil)
	meta := map[string]any{
		"category":       "danbooru",
		"id":             float64(11474309),
		"rating":         "g",
		"tags_general":   []any{"1girl", "landscape", "outdoors", "sky"},
		"tags_artist":    []any{"paruperu"},
		"tags_copyright": []any{"original"},
		"tags_meta":      []any{"highres", "absurdres"},
	}
	pf := m.Map(meta)
	wantTags := []string{
		"1girl", "artist:paruperu", "copyright:original", "landscape",
		"meta:absurdres", "meta:highres", "outdoors", "rating:general", "sky",
	}
	if !reflect.DeepEqual(pf.Tags, wantTags) {
		t.Errorf("tags =\n %v\nwant\n %v", pf.Tags, wantTags)
	}
	if pf.Rating != RatingGeneral {
		t.Errorf("rating = %q, want general (danbooru g)", pf.Rating)
	}
	if pf.Source != "danbooru" {
		t.Errorf("source = %q, want danbooru", pf.Source)
	}
	if u := m.PostURL(meta); u != "https://danbooru.donmai.us/posts/11474309" {
		t.Errorf("url = %q", u)
	}
	if pf.Via != "monloader" {
		t.Errorf("via = %q, want monloader", pf.Via)
	}
	if g := m.Gallery("danbooru"); g != "" {
		t.Errorf("gallery = %q, want empty (unset default)", g)
	}
}

func TestMapDanbooruCommentaryTranslatedPreferred(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "danbooru",
		"id":       float64(1),
		"artist_commentary": map[string]any{
			"original_title":         "元タイトル",
			"original_description":   "raw text",
			"translated_title":       "Title",
			"translated_description": "the translation",
		},
	})
	if pf.Commentary != "Title\n\nthe translation" {
		t.Errorf("commentary = %q, want the translated title folded above the body", pf.Commentary)
	}
}

func TestMapDanbooruCommentaryFallsBackToOriginal(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "danbooru",
		"id":       float64(1),
		"artist_commentary": map[string]any{
			"original_description": "only the original",
		},
	})
	if pf.Commentary != "only the original" {
		t.Errorf("commentary = %q, want the original description", pf.Commentary)
	}
}

func TestMapDanbooruNotesActiveOnlyAndStripped(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "danbooru",
		"id":       float64(1),
		"notes": []any{
			map[string]any{"x": float64(61), "y": float64(274), "width": float64(432), "height": float64(451), "body": "Otokura Yuuki-chan", "is_active": true},
			map[string]any{"x": float64(10), "y": float64(20), "width": float64(30), "height": float64(40), "body": "line1<br>line2", "is_active": true},
			map[string]any{"x": float64(0), "y": float64(0), "width": float64(1), "height": float64(1), "body": "hidden", "is_active": false},
		},
	})
	if len(pf.Notes) != 2 {
		t.Fatalf("got %d notes, want 2 active", len(pf.Notes))
	}
	if pf.Notes[0].X != 61 || pf.Notes[0].W != 432 || pf.Notes[0].Body != "Otokura Yuuki-chan" {
		t.Errorf("note[0] = %+v", pf.Notes[0])
	}
	if pf.Notes[1].Body != "line1\nline2" {
		t.Errorf("note[1] body = %q, want <br> turned into a newline", pf.Notes[1].Body)
	}
}

func TestMapGelbooruNotes(t *testing.T) {
	// The gelbooru family carries the danbooru note field names, minus is_active
	// (its post page only renders active notes).
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "gelbooru",
		"id":       float64(1),
		"notes": []any{
			map[string]any{"x": float64(12), "y": float64(34), "width": float64(56), "height": float64(78), "body": "translated line"},
			map[string]any{"x": float64(0), "y": float64(0), "width": float64(1), "height": float64(1), "body": ""},
		},
	})
	if len(pf.Notes) != 1 {
		t.Fatalf("got %d notes, want 1 (the empty body dropped)", len(pf.Notes))
	}
	if pf.Notes[0].X != 12 || pf.Notes[0].W != 56 || pf.Notes[0].Body != "translated line" {
		t.Errorf("note[0] = %+v", pf.Notes[0])
	}
}

func TestMapMoebooruNotes(t *testing.T) {
	// moebooru pages carry the same note markup as the gelbooru family (active
	// notes only, no is_active flag).
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "konachan",
		"id":       float64(1),
		"notes": []any{
			map[string]any{"x": float64(1), "y": float64(2), "width": float64(3), "height": float64(4), "body": "text"},
		},
	})
	if len(pf.Notes) != 1 || pf.Notes[0].H != 4 || pf.Notes[0].Body != "text" {
		t.Errorf("notes = %+v, want the konachan note mapped", pf.Notes)
	}
}

func TestMapE621NotesActiveOnly(t *testing.T) {
	// e621's notes API returns danbooru-shaped notes including inactive ones.
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "e621",
		"id":       float64(1),
		"notes": []any{
			map[string]any{"x": float64(5), "y": float64(6), "width": float64(7), "height": float64(8), "body": "kept", "is_active": true},
			map[string]any{"x": float64(0), "y": float64(0), "width": float64(1), "height": float64(1), "body": "gone", "is_active": false},
		},
	})
	if len(pf.Notes) != 1 || pf.Notes[0].Body != "kept" {
		t.Errorf("notes = %+v, want only the active e621 note", pf.Notes)
	}
}

func TestMapSankakuNotes(t *testing.T) {
	// sankaku is generic-family; its profile's has_notes flag wires the same
	// danbooru-shaped note boxes its API returns.
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "sankaku",
		"id":       "abc",
		"notes": []any{
			map[string]any{"x": float64(9), "y": float64(10), "width": float64(11), "height": float64(12), "body": "api note"},
		},
	})
	if len(pf.Notes) != 1 || pf.Notes[0].X != 9 || pf.Notes[0].Body != "api note" {
		t.Errorf("notes = %+v, want the sankaku note mapped", pf.Notes)
	}
}

func TestMapGenericNotesDropped(t *testing.T) {
	// A stray notes field on a source that is not wired for them must not leak
	// into the push.
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "zerochan",
		"id":       float64(1),
		"notes": []any{
			map[string]any{"x": float64(1), "y": float64(2), "width": float64(3), "height": float64(4), "body": "text"},
		},
	})
	if len(pf.Notes) != 0 {
		t.Errorf("zerochan notes are not mapped, got %+v", pf.Notes)
	}
}

func TestMapE621Description(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":     "e621",
		"id":           float64(1),
		"rating":       "s",
		"description":  "an <b>uploader</b> note",
		"tags_general": []any{"x"},
	})
	if pf.Commentary != "an uploader note" {
		t.Errorf("commentary = %q, want the stripped description", pf.Commentary)
	}
}

func TestMapE621SafeOverloadAndSuffixes(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":         "e621",
		"rating":           "s",
		"tags_general":     []any{"solo", "outdoors", "tree"},
		"tags_artist":      []any{"someartist"},
		"tags_character":   []any{"charname"},
		"tags_copyright":   []any{"original"},
		"tags_species":     []any{"canine", "wolf"},
		"tags_meta":        []any{"digital_media_(artwork)"},
		"tags_lore":        []any{"male_(lore)"},
		"tags_contributor": []any{"uploader_name"},
		"tags_invalid":     []any{"conditional_dnp"},
	})
	// e621 `s` means safe -> general (the overload that differs from danbooru).
	if pf.Rating != RatingGeneral {
		t.Errorf("e621 rating s = %q, want general", pf.Rating)
	}
	must := []string{
		"canine", "wolf", // species -> general (bare)
		"artist:someartist",
		"artist:uploader_name", // contributor -> artist
		"character:charname",
		"copyright:original",
		"meta:digital_media_(artwork)",
		"meta:male_(lore)", // lore -> meta
		"rating:general",
	}
	for _, tag := range must {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing expected tag %q in %v", tag, pf.Tags)
		}
	}
	// invalid tags are dropped entirely.
	for _, tag := range pf.Tags {
		if tag == "conditional_dnp" || tag == "invalid:conditional_dnp" {
			t.Errorf("invalid tag should be dropped, found %q", tag)
		}
	}
}

func TestMapMoebooruStringTags(t *testing.T) {
	m := newMapper(t, nil)
	meta := map[string]any{
		"category":       "konachan",
		"id":             float64(345678),
		"rating":         "q",
		"tags_general":   "landscape scenery cloud no_humans",
		"tags_artist":    "scenicartist",
		"tags_copyright": "original",
	}
	pf := m.Map(meta)
	if pf.Rating != RatingQuestionable {
		t.Errorf("konachan rating q = %q, want questionable", pf.Rating)
	}
	// Tag fields arrive as space-joined strings and must still split.
	for _, tag := range []string{"landscape", "scenery", "artist:scenicartist", "copyright:original"} {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
	if u := m.PostURL(meta); u != "https://konachan.com/post/show/345678" {
		t.Errorf("url = %q", u)
	}
}

func TestMapGelbooruMetadataSuffix(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":       "safebooru",
		"rating":         "general",
		"tags_general":   "landscape scenery tree mountain",
		"tags_copyright": "original",
		"tags_metadata":  "highres",
	})
	// gelbooru_v02 uses the `metadata` suffix, not `meta`.
	if !slices.Contains(pf.Tags, "meta:highres") {
		t.Errorf("gelbooru metadata suffix should map to meta:, got %v", pf.Tags)
	}
	// Word-form rating "general" normalizes to general.
	if pf.Rating != RatingGeneral {
		t.Errorf("safebooru rating general = %q, want general", pf.Rating)
	}
	if !slices.Contains(pf.Tags, "copyright:original") {
		t.Errorf("missing copyright:original in %v", pf.Tags)
	}
}

func TestSafebooruStaleQRatingIsGeneral(t *testing.T) {
	// safebooru.org is safe-only; its legacy DAPI returns "q" for content its UI
	// labels General, so the profile override pins q -> general.
	pf := newMapper(t, nil).Map(map[string]any{"category": "safebooru", "id": float64(1), "rating": "q", "tags_general": []any{"x"}})
	if pf.Rating != RatingGeneral {
		t.Errorf("safebooru q = %q, want general", pf.Rating)
	}
	// A gelbooru_v02 sibling that is not safe-only keeps the family q.
	rule := newMapper(t, nil).Map(map[string]any{"category": "rule34us", "id": float64(1), "rating": "q", "tags_general": []any{"x"}})
	if rule.Rating != RatingQuestionable {
		t.Errorf("rule34us q = %q, want questionable (no override)", rule.Rating)
	}
}

func TestMapPoolOrderingAndStrictestRating(t *testing.T) {
	pool := map[string]any{"name": "A Quiet Afternoon"}
	metas := []map[string]any{
		{"category": "danbooru", "subcategory": "pool", "num": float64(1), "rating": "s", "pool": pool, "tags_general": []any{"comic", "monochrome"}},
		{"category": "danbooru", "subcategory": "pool", "num": float64(2), "rating": "q", "pool": pool, "tags_general": []any{"comic", "speech_bubble"}},
		{"category": "danbooru", "subcategory": "pool", "num": float64(3), "rating": "e", "pool": pool, "tags_general": []any{"comic", "action"}},
	}
	if len(metas) != 3 {
		t.Fatalf("pool fixture had %d items, want 3", len(metas))
	}
	m := newMapper(t, nil)
	strictest := ""
	wantRatings := []string{RatingSensitive, RatingQuestionable, RatingExplicit} // s/q/e on danbooru
	for i, meta := range metas {
		pf := m.Map(meta)
		if pf.Collection != "A Quiet Afternoon" {
			t.Errorf("item %d collection = %q", i, pf.Collection)
		}
		if pf.CollectionOrder != i+1 {
			t.Errorf("item %d order = %d, want %d", i, pf.CollectionOrder, i+1)
		}
		if pf.Rating != wantRatings[i] {
			t.Errorf("item %d rating = %q, want %q", i, pf.Rating, wantRatings[i])
		}
		strictest = Stricter(strictest, pf.Rating)
	}
	if strictest != RatingExplicit {
		t.Errorf("strictest pool rating = %q, want explicit", strictest)
	}
}

func TestPoolName(t *testing.T) {
	if got := PoolName(map[string]any{"pool": map[string]any{"name": "A Quiet Afternoon"}}); got != "A Quiet Afternoon" {
		t.Errorf("danbooru pool name = %q, want the map name", got)
	}
	if got := PoolName(map[string]any{"pool": float64(12)}); got != "pool 12" {
		t.Errorf("moebooru pool name = %q, want \"pool 12\"", got)
	}
	if got := PoolName(map[string]any{}); got != "" {
		t.Errorf("no pool = %q, want empty", got)
	}
}

func TestMapNhentaiGallery(t *testing.T) {
	m := newMapper(t, nil)
	meta := map[string]any{
		"category":   "nhentai",
		"gallery_id": float64(12345),
		"artist":     []any{"artistname"},
		"group":      []any{"circlename"},
		"parody":     []any{"seriesname"},
		"characters": []any{"charname"},
		"tags":       []any{"tag_one", "tag_two"},
		"type":       "doujinshi",
	}
	pf := m.Map(meta)
	want := []string{
		"artist:artistname",
		"artist:circlename",    // group -> artist (doujin circle)
		"copyright:seriesname", // parody -> the copyrighted work
		"character:charname",
		"tag_one", "tag_two", // the flat tag list lands as general
		"meta:doujinshi",  // type -> meta
		"rating:explicit", // adult site with no source rating defaults to explicit
	}
	for _, tag := range want {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
	if pf.Rating != RatingExplicit {
		t.Errorf("nhentai rating = %q, want explicit (adult default)", pf.Rating)
	}
	if pf.Source != "nhentai" {
		t.Errorf("source = %q, want nhentai", pf.Source)
	}
	// The post URL resolves from gallery_id, since nhentai has no id field.
	if u := m.PostURL(meta); u != "https://nhentai.net/g/12345/" {
		t.Errorf("url = %q, want the gallery_id URL", u)
	}
}

func TestMapMangadexRatingVocabulary(t *testing.T) {
	m := newMapper(t, nil)
	// mangadex emits its own contentRating words: safe/suggestive land through
	// mapRating, erotica/pornographic through the profile override table.
	for _, tc := range []struct{ raw, want string }{
		{"safe", RatingGeneral},
		{"suggestive", RatingSensitive},
		{"erotica", RatingQuestionable},
		{"pornographic", RatingExplicit},
	} {
		pf := m.Map(map[string]any{"category": "mangadex", "rating": tc.raw, "tags": []any{"x"}})
		if pf.Rating != tc.want {
			t.Errorf("mangadex %q = %q, want %q", tc.raw, pf.Rating, tc.want)
		}
	}
}

func TestMapMangadexGallery(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "mangadex",
		"rating":   "erotica",
		"artist":   []any{"Some Artist", "Another Artist"},
		"author":   []any{"Some Author"},
		"group":    []any{"Scan Group"},
		"tags":     []any{"Romance", "Slice of Life"},
	})
	if pf.Rating != RatingQuestionable {
		t.Errorf("mangadex erotica = %q, want questionable", pf.Rating)
	}
	want := []string{
		"artist:some_artist", "artist:another_artist", // artist list
		"artist:some_author",       // author -> artist
		"artist:scan_group",        // group -> artist
		"romance", "slice_of_life", // flat tags -> general, normalized
		"rating:questionable",
	}
	for _, tag := range want {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
}

func TestDefaultRatingOnlyForAdultSites(t *testing.T) {
	m := newMapper(t, nil)
	// A general manga aggregator carries no default; with no source rating it
	// stays unset rather than being forced to explicit.
	if pf := m.Map(map[string]any{"category": "mangadex", "tags": []any{"x"}}); pf.Rating != "" {
		t.Errorf("mangadex rating = %q, want unset", pf.Rating)
	}
	// A real source rating still wins over the adult default (s -> general for
	// the generic family).
	if pf := m.Map(map[string]any{"category": "nhentai", "rating": "s"}); pf.Rating != RatingGeneral {
		t.Errorf("source rating should win over the adult default, got %q", pf.Rating)
	}
}

// TestMapMangaFieldVariants covers the field-name variants other manga
// extractors use (author/artists/parodies/genres/character) on a manga-kind
// source. Booru sources are unaffected: they carry tags_<category> suffixes and
// are not manga-kind, so these named fields are never read for them.
func TestMapMangaFieldVariants(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":  "mangadex", // a curated manga-kind profile
		"author":    []any{"writer"},
		"artists":   []any{"penciller"},
		"genres":    []any{"action", "romance"},
		"parodies":  []any{"some_series"},
		"character": []any{"hero"},
	})
	for _, tag := range []string{"artist:writer", "artist:penciller", "action", "romance", "copyright:some_series", "character:hero"} {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
}

func TestNamespacedFlatTags(t *testing.T) {
	// zerochan emits a flat tags list with capitalized namespace prefixes and
	// human-readable names; the namespace routes the category and the name is
	// normalized to monbooru's lower_snake_case.
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "zerochan",
		"id":       float64(1),
		"tags": []any{
			"Mangaka:Some Artist",
			"Game:Some Franchise",
			"Character:Some Hero",
			"Theme:Hakama", // theme -> general (bare)
			"Source:Pixiv", // source -> meta
		},
	})
	for _, tag := range []string{"artist:some_artist", "copyright:some_franchise", "character:some_hero", "hakama", "meta:pixiv"} {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
	// An unrecognized namespace keeps the tag verbatim (no split on an incidental
	// colon), still normalized.
	pf2 := newMapper(t, nil).Map(map[string]any{"category": "zerochan", "id": float64(1), "tags": []any{"OS:Windows"}})
	if !slices.Contains(pf2.Tags, "os:windows") {
		t.Errorf("unrecognized namespace should keep the tag verbatim, got %v", pf2.Tags)
	}
}

func TestMapZerochanNormalizesSpacedTags(t *testing.T) {
	m := newMapper(t, nil)
	meta := map[string]any{
		"category": "zerochan",
		"id":       float64(7654321),
		"tags": []any{
			"Mangaka:Some Artist", "Series:Some Series Title", "Character:Some Character",
			"Outfit:Plain Dress", "Theme:Frilled Top", "Theme:Green Jacket",
			"Source:Fanart", "Source:Pixiv",
		},
	}
	pf := m.Map(meta)
	// Every name carries spaces and capitals; without normalization monbooru
	// rejects all but the space-free ones (the observed "only fanart and pixiv").
	want := []string{
		"artist:some_artist",          // Mangaka -> artist
		"copyright:some_series_title", // Series -> copyright
		"character:some_character",
		"outfit:plain_dress", // Outfit is not a known namespace: kept verbatim
		"frilled_top",        // Theme -> general
		"green_jacket",
		"meta:fanart", // Source -> meta
		"meta:pixiv",
	}
	for _, tag := range want {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
	if u := m.PostURL(meta); u != "https://www.zerochan.net/7654321" {
		t.Errorf("url = %q", u)
	}
}

func TestMapExhentaiNormalizesSpacedTags(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":      "exhentai",
		"rating":        "1.64",
		"tags_female":   []any{"slime girl"},
		"tags_language": []any{"chinese", "translated"},
		"tags_other":    []any{"ai generated", "rough translation"},
	})
	// tags_<namespace> values arrive with spaces; only the space-free ones
	// (chinese, translated) survived before normalization.
	want := []string{
		"chinese", "translated", // tags_language -> general
		"slime_girl",                        // tags_female "slime girl"
		"ai_generated", "rough_translation", // tags_other
		"rating:explicit", // adult default; the numeric "1.64" is not a content rating
	}
	for _, tag := range want {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
	if pf.Rating != RatingExplicit {
		t.Errorf("rating = %q, want explicit", pf.Rating)
	}
}

func TestNormalizeFoldsDisallowedChars(t *testing.T) {
	m := newMapper(t, nil)

	// realbooru joins its tag string with ", "; neither the comma nor the space
	// may cling to a tag.
	rb := m.Map(map[string]any{"category": "realbooru", "id": float64(1),
		"tags_general": "1girl, green_eyes, school_uniform"})
	for _, tag := range []string{"1girl", "green_eyes", "school_uniform"} {
		if !slices.Contains(rb.Tags, tag) {
			t.Errorf("realbooru comma-joined tags: missing %q in %v", tag, rb.Tags)
		}
	}

	// Characters outside monbooru's charset fold to underscore so the tag lands
	// instead of being rejected.
	got := m.Map(map[string]any{"category": "danbooru", "id": float64(1),
		"tags_copyright": []any{"fate/grand_order", "Red & Green"},
		"tags_artist":    []any{"DECO*27", "欧阳锦绮"},
	})
	for _, tag := range []string{"copyright:fate_grand_order", "copyright:red_green", "artist:deco_27"} {
		if !slices.Contains(got.Tags, tag) {
			t.Errorf("missing folded tag %q in %v", tag, got.Tags)
		}
	}
	// An all-non-ASCII name folds away entirely: no stray "artist:" prefix and no
	// non-ASCII left in any tag (monbooru is ASCII-only).
	if slices.Contains(got.Tags, "artist:") {
		t.Errorf("empty-name tag should be dropped, got %v", got.Tags)
	}
	for _, tag := range got.Tags {
		for _, r := range tag {
			if r > 127 {
				t.Errorf("non-ASCII char survived in %q", tag)
			}
		}
	}
}

func TestMapPhilomenaRatingFromTag(t *testing.T) {
	m := newMapper(t, nil)
	meta := map[string]any{
		"category": "twibooru",
		"id":       float64(7654321),
		"tags":     []any{"safe", "solo", "pony", "spread wings", "gradient background", "artist:some_artist"},
	}
	pf := m.Map(meta)
	// Philomena boorus carry no rating field; "safe" is a tag, lifted to general.
	if pf.Rating != RatingGeneral {
		t.Errorf("twibooru rating = %q, want general (the safe tag)", pf.Rating)
	}
	if !slices.Contains(pf.Tags, "rating:general") {
		t.Errorf("missing rating:general in %v", pf.Tags)
	}
	// The rating word is lifted, not also kept as a content tag.
	if slices.Contains(pf.Tags, "safe") {
		t.Errorf("the rating tag should not also be a content tag: %v", pf.Tags)
	}
	for _, tag := range []string{"solo", "pony", "spread_wings", "gradient_background", "artist:some_artist"} {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
	if u := m.PostURL(meta); u != "https://twibooru.org/7654321" {
		t.Errorf("url = %q", u)
	}
}

func TestPhilomenaRatingTagLevels(t *testing.T) {
	m := newMapper(t, nil)
	for _, tc := range []struct{ tag, want string }{
		{"safe", RatingGeneral},
		{"suggestive", RatingSensitive},
		{"questionable", RatingQuestionable},
		{"explicit", RatingExplicit},
	} {
		pf := m.Map(map[string]any{"category": "derpibooru", "id": float64(1), "tags": []any{tc.tag, "solo"}})
		if pf.Rating != tc.want {
			t.Errorf("derpibooru %q = %q, want %q", tc.tag, pf.Rating, tc.want)
		}
		if slices.Contains(pf.Tags, tc.tag) {
			t.Errorf("rating tag %q should be excluded from content tags: %v", tc.tag, pf.Tags)
		}
	}
}

func TestSOverloadDanbooruVsE621(t *testing.T) {
	m := newMapper(t, nil)
	dan := m.Map(map[string]any{"category": "danbooru", "id": float64(1), "rating": "s", "tags_general": []any{"x"}})
	e6 := m.Map(map[string]any{"category": "e621", "id": float64(1), "rating": "s", "tags_general": []any{"x"}})
	if dan.Rating != RatingSensitive {
		t.Errorf("danbooru s = %q, want sensitive", dan.Rating)
	}
	if e6.Rating != RatingGeneral {
		t.Errorf("e621 s = %q, want general", e6.Rating)
	}
}

func TestGenericFallback(t *testing.T) {
	m := newMapper(t, nil)
	meta := map[string]any{
		"category":     "wackybooru",
		"id":           float64(42),
		"rating":       "s", // generic: s = safe -> general
		"tags_general": []any{"foo"},
		"tags_artist":  []any{"bar"},
		"tags_mystery": []any{"baz"}, // unknown suffix -> general
	}
	pf := m.Map(meta)
	if pf.Source != "wackybooru" {
		t.Errorf("source = %q", pf.Source)
	}
	if u := m.PostURL(meta); u != "" {
		t.Errorf("generic profile has no URL template, got %q", u)
	}
	if pf.Rating != RatingGeneral {
		t.Errorf("generic s = %q, want general", pf.Rating)
	}
	for _, tag := range []string{"foo", "artist:bar", "baz", "rating:general"} {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing %q in %v", tag, pf.Tags)
		}
	}
}

func TestMapDirectlink(t *testing.T) {
	m := newMapper(t, nil)
	// A bare media URL: the host becomes the source and the file URL is rebuilt
	// as the canonical link. There is no booru post, so no tags and no rating.
	meta := map[string]any{
		"category": "directlink", "subcategory": "example.com",
		"domain": "img.example.com", "path": "art/2024",
		"filename": "picture", "extension": "jpg", "query": nil, "fragment": nil,
	}
	pf := m.Map(meta)
	if pf.Source != "img.example.com" {
		t.Errorf("source = %q, want the file host", pf.Source)
	}
	if u := m.PostURL(meta); u != "https://img.example.com/art/2024/picture.jpg" {
		t.Errorf("url = %q, want the rebuilt file URL", u)
	}
	if len(pf.Tags) != 0 {
		t.Errorf("a bare media URL carries no tags, got %v", pf.Tags)
	}
	if pf.Rating != "" {
		t.Errorf("a bare media URL has no rating, got %q", pf.Rating)
	}

	// The file URL is rebuilt from whichever parts are present: a root-level file
	// has no path segment, and a query/fragment are reattached.
	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{"root-level file", map[string]any{"category": "directlink", "domain": "example.com", "path": "", "filename": "image", "extension": "png"}, "https://example.com/image.png"},
		{"query and fragment", map[string]any{"category": "directlink", "domain": "cdn.example.com", "path": "i", "filename": "p", "extension": "webp", "query": "size=large", "fragment": "frag"}, "https://cdn.example.com/i/p.webp?size=large#frag"},
		// An extension-less media URL (a CDN avatar) carries no extension, so the
		// rebuilt URL must not gain a trailing dot - the gdl directlink fallback
		// relies on this for the pushed source link.
		{"extensionless host-served file", map[string]any{"category": "directlink", "domain": "yt3.ggpht.com", "path": "ytc", "filename": "AIdro_avatar=s400-c-k-no-rj", "extension": ""}, "https://yt3.ggpht.com/ytc/AIdro_avatar=s400-c-k-no-rj"},
	} {
		if got := m.PostURL(tc.meta); got != tc.want {
			t.Errorf("%s: url = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCombinedTagsFallbackWhenNoSuffixes(t *testing.T) {
	// A flat-tag site without tags:true emits only the combined list, which
	// must land as general.
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "gelbooru",
		"id":       float64(7),
		"rating":   "q",
		"tags":     "alpha beta gamma",
	})
	for _, tag := range []string{"alpha", "beta", "gamma"} {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("combined tags should land as general, missing %q in %v", tag, pf.Tags)
		}
	}
}

func TestUserOverridesWin(t *testing.T) {
	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "e621", From: "species", To: "copyright"}}
	cfg.RatingOverrides = []config.RatingOverride{{Site: "danbooru", From: "s", To: "questionable"}}
	m := newMapper(t, cfg)

	e6 := m.Map(map[string]any{"category": "e621", "id": float64(1), "tags_species": []any{"canine"}})
	if !slices.Contains(e6.Tags, "copyright:canine") {
		t.Errorf("tag override should route species to copyright, got %v", e6.Tags)
	}
	dan := m.Map(map[string]any{"category": "danbooru", "id": float64(1), "rating": "s"})
	if dan.Rating != RatingQuestionable {
		t.Errorf("rating override should make danbooru s = questionable, got %q", dan.Rating)
	}
}

func TestPerSiteGallery(t *testing.T) {
	cfg := config.Default()
	cfg.Monbooru.DefaultGallery = "main"
	cfg.Sites = []config.Site{{Name: "e621", Gallery: "furry"}}
	m := newMapper(t, cfg)
	if g := m.Gallery("e621"); g != "furry" {
		t.Errorf("e621 gallery = %q, want furry", g)
	}
	if g := m.Gallery("danbooru"); g != "main" {
		t.Errorf("danbooru gallery = %q, want main (default)", g)
	}
}

func TestFlatTagSites(t *testing.T) {
	got := newMapper(t, nil).FlatTagSites()
	for _, want := range []string{"gelbooru", "rule34", "safebooru", "realbooru", "konachan", "yandere", "sakugabooru", "sankaku", "idolcomplex"} {
		if !slices.Contains(got, want) {
			t.Errorf("FlatTagSites missing %q: %v", want, got)
		}
	}
	for _, notWanted := range []string{"danbooru", "e621"} {
		if slices.Contains(got, notWanted) {
			t.Errorf("FlatTagSites should not include %q: %v", notWanted, got)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("FlatTagSites should be sorted: %v", got)
	}
}

func TestNotesSites(t *testing.T) {
	got := newMapper(t, nil).NotesSites()
	for _, want := range []string{"gelbooru", "safebooru", "konachan", "yandere", "sankaku", "idolcomplex"} {
		if !slices.Contains(got, want) {
			t.Errorf("NotesSites missing %q: %v", want, got)
		}
	}
	// danbooru and e621 carry notes behind the metadata include, not notes:true.
	for _, notWanted := range []string{"danbooru", "e621", "zerochan"} {
		if slices.Contains(got, notWanted) {
			t.Errorf("NotesSites should not include %q: %v", notWanted, got)
		}
	}
}

func TestMetadataSites(t *testing.T) {
	got := newMapper(t, nil).MetadataSites()
	for _, want := range []string{"danbooru", "aibooru", "e621", "e926"} {
		if !slices.Contains(got, want) {
			t.Errorf("MetadataSites missing %q: %v", want, got)
		}
	}
	if slices.Contains(got, "gelbooru") {
		t.Errorf("MetadataSites should not include gelbooru: %v", got)
	}
}

func TestKindOf(t *testing.T) {
	m := newMapper(t, nil)
	if got := m.KindOf("nhentai"); got != KindManga {
		t.Errorf("nhentai KindOf = %q, want %q", got, KindManga)
	}
	if got := m.KindOf("danbooru"); got != KindBooru {
		t.Errorf("danbooru KindOf = %q, want %q", got, KindBooru)
	}
	// An unmapped site is booru-shaped (pushed per post, not bundled).
	if got := m.KindOf("totally-unknown"); got != KindBooru {
		t.Errorf("unmapped KindOf = %q, want %q", got, KindBooru)
	}
}

func TestCuratedByKind(t *testing.T) {
	m := newMapper(t, nil)
	boorus := m.CuratedByKind(KindBooru)
	manga := m.CuratedByKind(KindManga)
	if !slices.Contains(boorus, "danbooru") || slices.Contains(boorus, "nhentai") {
		t.Errorf("booru group should hold danbooru, not nhentai: %v", boorus)
	}
	if !slices.Contains(manga, "nhentai") || slices.Contains(manga, "danbooru") {
		t.Errorf("manga group should hold nhentai, not danbooru: %v", manga)
	}
	if !slices.IsSorted(boorus) || !slices.IsSorted(manga) {
		t.Error("groups should be sorted")
	}
}

func TestMapRatingTable(t *testing.T) {
	cases := []struct {
		family, raw, want string
	}{
		{FamilyDanbooru, "g", RatingGeneral},
		{FamilyDanbooru, "s", RatingSensitive},
		{FamilyDanbooru, "q", RatingQuestionable},
		{FamilyDanbooru, "e", RatingExplicit},
		{FamilyDanbooru, "sensitive", RatingSensitive},
		{FamilyDanbooru, "EXPLICIT", RatingExplicit},
		{FamilyDanbooru, "bogus", ""},
		{FamilyDanbooru, "", ""},
		{FamilyE621, "s", RatingGeneral},
		{FamilyMoebooru, "q", RatingQuestionable},
		{FamilyGelbooruV02, "safe", RatingGeneral},
		{FamilyGelbooruV02, "e", RatingExplicit},
		{FamilyGeneric, "g", RatingGeneral},
		{FamilyGeneric, "s", RatingGeneral},
	}
	for _, tc := range cases {
		if got := mapRating(tc.family, tc.raw); got != tc.want {
			t.Errorf("mapRating(%s, %q) = %q, want %q", tc.family, tc.raw, got, tc.want)
		}
	}
}

func TestBaseCategoryTable(t *testing.T) {
	cases := []struct {
		suffix, cat string
		keep        bool
	}{
		{"general", "general", true},
		{"metadata", "meta", true},
		{"species", "general", true},
		{"lore", "meta", true},
		{"contributor", "artist", true},
		{"circle", "artist", true},
		{"group", "artist", true},
		{"parody", "copyright", true},
		{"series", "copyright", true},
		{"studio", "copyright", true},
		{"model", "person", true},
		{"faults", "meta", true},
		{"invalid", "", false},
		{"deprecated", "", false},
		{"somethingnew", "general", true},
	}
	for _, tc := range cases {
		cat, keep := baseCategory(tc.suffix)
		if cat != tc.cat || keep != tc.keep {
			t.Errorf("baseCategory(%q) = (%q,%v), want (%q,%v)", tc.suffix, cat, keep, tc.cat, tc.keep)
		}
	}
}

func TestSpecialSuffixesThroughMap(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":        "somebooru",
		"id":              float64(1),
		"tags_circle":     []any{"comiket_circle"},
		"tags_model":      []any{"jane_doe"},
		"tags_faults":     []any{"bad_anatomy"},
		"tags_deprecated": []any{"old_tag"},
		"tags_invalid":    []any{"not_a_tag"},
	})
	if !slices.Contains(pf.Tags, "artist:comiket_circle") {
		t.Errorf("circle should map to artist, got %v", pf.Tags)
	}
	if !slices.Contains(pf.Tags, "person:jane_doe") {
		t.Errorf("model should map to person, got %v", pf.Tags)
	}
	if !slices.Contains(pf.Tags, "meta:bad_anatomy") {
		t.Errorf("faults should map to meta, got %v", pf.Tags)
	}
	for _, dropped := range []string{"old_tag", "not_a_tag"} {
		if slices.Contains(pf.Tags, dropped) {
			t.Errorf("deprecated/invalid tag %q should be dropped, got %v", dropped, pf.Tags)
		}
	}
}

func TestSuspectFlattenedTags(t *testing.T) {
	m := newMapper(t, nil)
	cases := []struct {
		name string
		meta map[string]any
		want bool
	}{
		{"danbooru with per-category tags", map[string]any{"category": "danbooru", "tags_general": []any{"x"}}, false},
		{"danbooru with only the combined list", map[string]any{"category": "danbooru", "tags": "x y"}, true},
		{"e621 with no per-category tags", map[string]any{"category": "e621", "tags": "x"}, true},
		// monloader writes tags:true for the flat-tag families, so a resolve with
		// only the combined list means that path broke, not a benign config.
		{"moebooru with no per-category tags despite tags:true", map[string]any{"category": "konachan", "tags": "x"}, true},
		{"gelbooru_v02 with no per-category tags despite tags:true", map[string]any{"category": "safebooru", "tags": "x"}, true},
		{"flat-tag family with per-category tags", map[string]any{"category": "safebooru", "tags_general": []any{"x"}}, false},
		{"needs-tags generic with no per-category tags", map[string]any{"category": "sankaku", "tags": "x"}, true},
		{"unmapped generic site", map[string]any{"category": "wackybooru", "tags": "x"}, false},
	}
	for _, tc := range cases {
		if got := m.SuspectFlattenedTags(tc.meta); got != tc.want {
			t.Errorf("%s: SuspectFlattenedTags = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFormatTag(t *testing.T) {
	if formatTag("general", "x") != "x" {
		t.Error("general tags should be bare")
	}
	if formatTag("artist", "y") != "artist:y" {
		t.Error("non-general tags should be prefixed")
	}
	if formatTag("", "z") != "" || formatTag("artist", "") != "" {
		t.Error("empty category or name should produce no tag")
	}
}

func TestMapHandlesMissingFields(t *testing.T) {
	// No category, no id, no tags, odd id type: must not panic and must
	// produce a usable (mostly empty) result.
	m := newMapper(t, nil)
	meta := map[string]any{"id": true}
	pf := m.Map(meta)
	if m.PostURL(meta) != "" || len(pf.Tags) != 0 || pf.Rating != "" {
		t.Errorf("empty item should map to empty fields, got %+v", pf)
	}
	if g := m.Gallery(""); g != "" {
		t.Errorf("gallery should be the unset default, got %q", g)
	}
}

func TestProfileRatingOverride(t *testing.T) {
	m := newMapper(t, config.Default())
	m.profiles["custombooru"] = Profile{Family: FamilyGeneric, RatingOverrides: map[string]string{"x": RatingExplicit}}
	pf := m.Map(map[string]any{"category": "custombooru", "id": float64(1), "rating": "x"})
	if pf.Rating != RatingExplicit {
		t.Errorf("profile rating override should apply, got %q", pf.Rating)
	}
}

func TestRatingRankAndStricter(t *testing.T) {
	if RatingRank("") != -1 {
		t.Error("unset rating should rank -1")
	}
	if RatingRank(RatingGeneral) >= RatingRank(RatingExplicit) {
		t.Error("general should rank below explicit")
	}
	if Stricter(RatingGeneral, "") != RatingGeneral {
		t.Error("a set level should beat unset")
	}
	if Stricter(RatingSensitive, RatingExplicit) != RatingExplicit {
		t.Error("explicit should win over sensitive")
	}
}
