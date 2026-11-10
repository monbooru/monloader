package mapping

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func newMapper(t *testing.T, cfg *config.Config) *Mapper {
	t.Helper()
	if cfg == nil {
		cfg = config.Default()
	}
	m, err := New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatalf("New mapper: %v", err)
	}
	return m
}

func assertTags(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, tag := range want {
		if !slices.Contains(got, tag) {
			t.Errorf("missing %q in %v", tag, got)
		}
	}
}

func assertNoTags(t *testing.T, got []string, reject ...string) {
	t.Helper()
	for _, tag := range reject {
		if slices.Contains(got, tag) {
			t.Errorf("unexpected %q in %v", tag, got)
		}
	}
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

func TestMapDanbooruCommentary(t *testing.T) {
	for _, tc := range []struct {
		name             string
		commentary       map[string]any
		body, translated string
	}{{
		name: "both bodies",
		commentary: map[string]any{
			"original_title":         "元タイトル",
			"original_description":   "raw text",
			"translated_title":       "Title",
			"translated_description": "the translation",
		},
		body:       "元タイトル\n\nraw text",
		translated: "Title\n\nthe translation",
	}, {
		name:       "original only",
		commentary: map[string]any{"original_description": "only the original"},
		body:       "only the original",
	}, {

		name: "split per language",
		commentary: map[string]any{
			"translated_title":     "Title",
			"original_description": "the original body",
		},
		body:       "the original body",
		translated: "Title",
	}, {
		name:       "translation only leads",
		commentary: map[string]any{"translated_description": "only the translation"},
		body:       "only the translation",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			pf := newMapper(t, nil).Map(map[string]any{
				"category": "danbooru", "id": float64(1), "artist_commentary": tc.commentary,
			})
			if pf.Commentary != tc.body {
				t.Errorf("commentary = %q, want %q", pf.Commentary, tc.body)
			}
			if pf.CommentaryTranslated != tc.translated {
				t.Errorf("translation = %q, want %q", pf.CommentaryTranslated, tc.translated)
			}
		})
	}
}

func TestMapDanbooruCommentaryKeepsTheSourcesOwnMarkup(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "danbooru",
		"id":       float64(1),
		"artist_commentary": map[string]any{
			"original_description":   `[b]bold[/b] "pixiv":[https://www.pixiv.net/artworks/1]`,
			"translated_description": "[b]bold[/b] there",
		},
	})
	want := `[b]bold[/b] "pixiv":[https://www.pixiv.net/artworks/1]`
	if pf.CommentaryDText != want {
		t.Errorf("commentary dtext = %q, want %q", pf.CommentaryDText, want)
	}
	if pf.CommentaryTranslatedDText != "[b]bold[/b] there" {
		t.Errorf("translation dtext = %q, want the source's own", pf.CommentaryTranslatedDText)
	}
	if pf.Commentary != want {
		t.Errorf("commentary = %q, want %q", pf.Commentary, want)
	}
}

func TestMapDanbooruCommentaryTranslationOnlyLeadsInBothForms(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "danbooru",
		"id":       float64(1),
		"artist_commentary": map[string]any{
			"translated_description": "[b]only the translation[/b]",
		},
	})
	if pf.CommentaryDText != "[b]only the translation[/b]" {
		t.Errorf("commentary dtext = %q, want the translation promoted", pf.CommentaryDText)
	}
	if pf.CommentaryTranslatedDText != "" {
		t.Errorf("translation dtext = %q, want empty once it led", pf.CommentaryTranslatedDText)
	}
}

func TestMapCommentaryStripsEntityEncodedMarkup(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "danbooru",
		"id":       float64(1),
		"artist_commentary": map[string]any{
			"translated_description": "see &lt;img src=x onerror=alert(1)&gt; here &amp; now",
		},
	})
	if strings.ContainsAny(pf.Commentary, "<>") {
		t.Errorf("commentary still carries markup: %q", pf.Commentary)
	}
	if !strings.Contains(pf.Commentary, "&") {
		t.Errorf("commentary should keep the decoded ampersand: %q", pf.Commentary)
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
	if pf.Notes[0].BodyHTML != "" {
		t.Errorf("note[0] body_html = %q, want empty for a plain body", pf.Notes[0].BodyHTML)
	}
	if pf.Notes[1].BodyHTML != "line1<br>line2" {
		t.Errorf("note[1] body_html = %q, want the source's own markup", pf.Notes[1].BodyHTML)
	}
}

func TestMapNotesKeepOnlyTheFormattingTheyNeed(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"inline formatting", "<b>bold</b> and <i>it</i>", "<b>bold</b> and <i>it</i>"},
		{"a script tag", "<script>alert(1)</script>", ""},
		{"an event handler", `plain <img src=x onerror="alert(1)"> here`, ""},
		{"an http link", `see <a href="https://example.com/x">this</a>`, `see <a href="https://example.com/x">this</a>`},
		{"a javascript link", `see <a href="javascript:alert(1)">this</a>`, `see this</a>`},
		{"a translator note", "<tn>a note</tn>", "<tn>a note</tn>"},
		{"a styled tag link", `<b><a style="color:red" href="/posts?tags=lapis_lazuli">Lapis</a></b>`,
			`<b><a href="/posts?tags=lapis_lazuli">Lapis</a></b>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := newMapper(t, nil).Map(map[string]any{
				"category": "danbooru",
				"id":       float64(1),
				"notes": []any{map[string]any{
					"x": float64(1), "y": float64(2), "width": float64(3), "height": float64(4),
					"body": tc.body, "is_active": true,
				}},
			})
			if len(pf.Notes) != 1 {
				t.Fatalf("got %d notes, want 1", len(pf.Notes))
			}
			if got := pf.Notes[0].BodyHTML; got != tc.want {
				t.Errorf("body_html = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMapGelbooruNotes(t *testing.T) {
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

func TestMapDanbooruParentURL(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":  "danbooru",
		"id":        float64(2),
		"parent_id": float64(11514474),
	})
	if pf.ParentURL != "https://danbooru.donmai.us/posts/11514474" {
		t.Errorf("parent url = %q, want the parent's canonical post page", pf.ParentURL)
	}
}

func TestMapE621ParentURLFromRelationships(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":      "e621",
		"id":            float64(2),
		"relationships": map[string]any{"parent_id": float64(9)},
	})
	if pf.ParentURL != "https://e621.net/posts/9" {
		t.Errorf("parent url = %q, want the nested relationships parent", pf.ParentURL)
	}
}

func TestMapParentIDZeroOrAbsentMeansNone(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":  "gelbooru",
		"id":        float64(2),
		"parent_id": float64(0),
	})
	if pf.ParentURL != "" {
		t.Errorf("parent url = %q, want empty for parent_id 0", pf.ParentURL)
	}
	pf = newMapper(t, nil).Map(map[string]any{
		"category": "danbooru",
		"id":       float64(2),
	})
	if pf.ParentURL != "" {
		t.Errorf("parent url = %q, want empty when no parent field", pf.ParentURL)
	}
}

func TestMapOriginalSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{"danbooru source", map[string]any{"category": "danbooru", "id": float64(1),
			"source": "http://tksn.web.infoseek.co.jp/"}, "http://tksn.web.infoseek.co.jp/"},
		{"e621 sources list", map[string]any{"category": "e621", "id": float64(1),
			"sources": []any{"https://www.pixiv.net/artworks/1", " https://twitter.com/a/2 ", ""}},
			"https://www.pixiv.net/artworks/1\nhttps://twitter.com/a/2"},
		{"philomena source_url", map[string]any{"category": "derpibooru", "id": float64(1),
			"source_url": "https://example.com/art/3"}, "https://example.com/art/3"},
		{"none declared", map[string]any{"category": "danbooru", "id": float64(1)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newMapper(t, nil).Map(tc.meta).Original; got != tc.want {
				t.Errorf("original = %q, want %q", got, tc.want)
			}
		})
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
	if pf.Rating != RatingGeneral {
		t.Errorf("e621 rating s = %q, want general", pf.Rating)
	}
	must := []string{
		"species:canine", "species:wolf",
		"artist:someartist",
		"artist:uploader_name",
		"character:charname",
		"copyright:original",
		"meta:digital_media_(artwork)",
		"meta:male_(lore)",
		"rating:general",
	}
	for _, tag := range must {
		if !slices.Contains(pf.Tags, tag) {
			t.Errorf("missing expected tag %q in %v", tag, pf.Tags)
		}
	}
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
	assertTags(t, pf.Tags, "landscape", "scenery", "artist:scenicartist", "copyright:original")
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
	if !slices.Contains(pf.Tags, "meta:highres") {
		t.Errorf("gelbooru metadata suffix should map to meta:, got %v", pf.Tags)
	}
	if pf.Rating != RatingGeneral {
		t.Errorf("safebooru rating general = %q, want general", pf.Rating)
	}
	if !slices.Contains(pf.Tags, "copyright:original") {
		t.Errorf("missing copyright:original in %v", pf.Tags)
	}
}

func TestSafebooruStaleQRatingIsGeneral(t *testing.T) {

	pf := newMapper(t, nil).Map(map[string]any{"category": "safebooru", "id": float64(1), "rating": "q", "tags_general": []any{"x"}})
	if pf.Rating != RatingGeneral {
		t.Errorf("safebooru q = %q, want general", pf.Rating)
	}

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
	wantRatings := []string{RatingSensitive, RatingQuestionable, RatingExplicit}
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
		"artist:circlename",
		"copyright:seriesname",
		"character:charname",
		"tag_one", "tag_two",
		"meta:doujinshi",
		"rating:explicit",
	}
	assertTags(t, pf.Tags, want...)
	if pf.Rating != RatingExplicit {
		t.Errorf("nhentai rating = %q, want explicit (adult default)", pf.Rating)
	}
	if pf.Source != "nhentai" {
		t.Errorf("source = %q, want nhentai", pf.Source)
	}

	if u := m.PostURL(meta); u != "https://nhentai.net/g/12345/" {
		t.Errorf("url = %q, want the gallery_id URL", u)
	}
}

func TestMapMangadexRatingVocabulary(t *testing.T) {
	m := newMapper(t, nil)

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
		"artist:some_artist", "artist:another_artist",
		"artist:some_author",
		"meta:scan_group",
		"romance", "slice_of_life",
		"rating:questionable",
	}
	assertTags(t, pf.Tags, want...)
	assertNoTags(t, pf.Tags, "artist:scan_group")
}

func TestDefaultRatingOnlyForAdultSites(t *testing.T) {
	m := newMapper(t, nil)
	if pf := m.Map(map[string]any{"category": "mangadex", "tags": []any{"x"}}); pf.Rating != "" {
		t.Errorf("mangadex rating = %q, want unset", pf.Rating)
	}

	if pf := m.Map(map[string]any{"category": "nhentai", "rating": "s"}); pf.Rating != RatingGeneral {
		t.Errorf("source rating should win over the adult default, got %q", pf.Rating)
	}
}

func TestBookmarkTagsDoNotReplaceAWorksTags(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "pixiv", "id": float64(1),
		"tags":          []any{"original", "girl"},
		"tags_bookmark": []any{"mine"},
	})
	assertTags(t, pf.Tags, "original", "girl")
	assertNoTags(t, pf.Tags, "mine")
}

func TestMapSchalenetworkWithOnlyUnnamedNamespaces(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":      "schalenetwork",
		"tags":          []any{"female:foxgirl", "language:english"},
		"tags_female":   []any{"foxgirl"},
		"tags_language": []any{"english"},
	})
	assertTags(t, pf.Tags, "foxgirl", "english")
	assertNoTags(t, pf.Tags, "female:foxgirl", "language:english")
}

func TestAnOverrideOnAnExtraSuffixKeepsTheWorksOwnTags(t *testing.T) {
	post := map[string]any{"category": "pixiv", "id": float64(1), "tags": []any{"own"}, "tags_bookmark": []any{"mine"}}

	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "pixiv", From: "bookmark", To: ""}}
	pf := newMapper(t, cfg).Map(post)
	assertTags(t, pf.Tags, "own")
	assertNoTags(t, pf.Tags, "mine")
	if !slices.Contains(pf.WithheldTags, "mine") {
		t.Errorf("withheld = %v, want mine", pf.WithheldTags)
	}

	cfg = config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "pixiv", From: "bookmark", To: "meta"}}
	pf = newMapper(t, cfg).Map(post)
	assertTags(t, pf.Tags, "own", "meta:mine")
}

func TestCivitaiRESTRatingWords(t *testing.T) {
	m := newMapper(t, nil)
	for word, want := range map[string]string{"None": RatingGeneral, "Soft": RatingSensitive, "Mature": RatingQuestionable, "X": RatingExplicit} {
		pf := m.Map(map[string]any{"category": "civitai", "id": float64(1), "file": map[string]any{"nsfwLevel": word}})
		if pf.Rating != want {
			t.Errorf("nsfwLevel %q rating = %q, want %q", word, pf.Rating, want)
		}
	}
}

func TestOverrideTargetsThatNameNothingAreSkipped(t *testing.T) {
	cfg := config.Default()
	cfg.RatingOverrides = []config.RatingOverride{{Site: "danbooru", From: "e", To: "explict"}}
	cfg.TagOverrides = []config.TagOverride{{Site: "danbooru", From: "artist", To: "artsit"}}
	pf := newMapper(t, cfg).Map(map[string]any{
		"category": "danbooru", "id": float64(1), "rating": "e",
		"tags_artist": []any{"someone"},
	})
	if pf.Rating != RatingExplicit {
		t.Errorf("rating = %q, want the typo skipped for the family's own mapping", pf.Rating)
	}
	assertTags(t, pf.Tags, "artist:someone")
	assertNoTags(t, pf.Tags, "rating:explict", "artsit:someone")
}

func TestAdultBoorusWithoutARatingMapToExplicit(t *testing.T) {
	m := newMapper(t, nil)
	for _, site := range []string{"paheal", "rule34us"} {
		if pf := m.Map(map[string]any{"category": site, "id": float64(1), "tags": []any{"x"}}); pf.Rating != RatingExplicit {
			t.Errorf("%s rating = %q, want %q", site, pf.Rating, RatingExplicit)
		}
	}
}

func TestCategoryMapAppliesToMangaFieldsAndFlatNamespaces(t *testing.T) {
	mangadex := map[string]any{"category": "mangadex", "chapter_id": "x", "group": []any{"TCB Scans"}, "artist": []any{"Oda"}, "author": []any{"Oda"}}

	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "mangadex", From: "group", To: ""}}
	pf := newMapper(t, cfg).Map(mangadex)
	assertNoTags(t, pf.Tags, "artist:tcb_scans")
	assertTags(t, pf.Tags, "artist:oda")
	if !slices.Contains(pf.WithheldTags, "artist:tcb_scans") {
		t.Errorf("withheld = %v, want the group under the category it would land in", pf.WithheldTags)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "somemanga.json"), []byte(`{"family":"generic","kind":"manga","category_overrides":{"author":"meta"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pf = newMapperDir(t, nil, dir).Map(map[string]any{"category": "somemanga", "gallery_id": float64(1), "author": []any{"Oda"}})
	assertTags(t, pf.Tags, "meta:oda")
	assertNoTags(t, pf.Tags, "artist:oda")

	cfg = config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "zerochan", From: "mangaka", To: "meta"}, {Site: "zerochan", From: "general", To: "copyright"}}
	pf = newMapper(t, cfg).Map(map[string]any{"category": "zerochan", "id": float64(1), "tags": []any{"Mangaka:X", "Character:Y", "bare"}})
	assertTags(t, pf.Tags, "meta:x", "character:y", "copyright:bare")
	assertNoTags(t, pf.Tags, "artist:x", "bare")

	cfg = config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "zerochan", From: "os", To: "meta"}}
	pf = newMapper(t, cfg).Map(map[string]any{"category": "zerochan", "id": float64(1), "tags": []any{"OS:Windows", "Re:Zero"}})
	assertTags(t, pf.Tags, "meta:windows", "re:zero")
}

func TestMapMangaFieldVariants(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category":  "mangadex",
		"author":    []any{"writer"},
		"artists":   []any{"penciller"},
		"genres":    []any{"action", "romance"},
		"parodies":  []any{"some_series"},
		"character": []any{"hero"},
	})
	assertTags(t, pf.Tags, "artist:writer", "artist:penciller", "action", "romance", "copyright:some_series", "character:hero")
}

func TestMapMangaStringCredits(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "dynastyscans",
		"author":   "Nakatani Nio",
		"group":    "4s",
		"parody":   "Series One, Series Two",
	})
	assertTags(t, pf.Tags, "artist:nakatani_nio", "meta:4s", "copyright:series_one", "copyright:series_two")
	assertNoTags(t, pf.Tags, "artist:nakatani", "artist:nio", "artist:4s")
}

func TestNamespacedFlatTags(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "zerochan",
		"id":       float64(1),
		"tags": []any{
			"Mangaka:Some Artist",
			"Game:Some Franchise",
			"Character:Some Hero",
			"Theme:Hakama",
			"Medium:Sketch",
			"Source:Pixiv",
		},
	})
	assertTags(t, pf.Tags, "artist:some_artist", "copyright:some_franchise", "character:some_hero", "hakama", "medium:sketch", "meta:pixiv")
	pf2 := newMapper(t, nil).Map(map[string]any{"category": "zerochan", "id": float64(1), "tags": []any{"OS:Windows"}})
	if !slices.Contains(pf2.Tags, "os:windows") {
		t.Errorf("unrecognized namespace should keep the tag verbatim, got %v", pf2.Tags)
	}
}

func TestMapFlatTagsDropASourceRatingTag(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "zerochan",
		"id":       float64(1),
		"rating":   "s",
		"tags":     []any{"rating:explicit", "Theme:Hakama"},
	})
	var ratings []string
	for _, tag := range pf.Tags {
		if strings.HasPrefix(tag, "rating:") {
			ratings = append(ratings, tag)
		}
	}
	if want := []string{"rating:" + pf.Rating}; pf.Rating == "" || !reflect.DeepEqual(ratings, want) {
		t.Errorf("rating tags = %v, want only %v", ratings, want)
	}
	assertTags(t, pf.Tags, "hakama")
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
	want := []string{
		"artist:some_artist",
		"copyright:some_series_title",
		"character:some_character",
		"outfit:plain_dress",
		"frilled_top",
		"green_jacket",
		"meta:fanart",
		"meta:pixiv",
	}
	assertTags(t, pf.Tags, want...)
	if u := m.PostURL(meta); u != "https://www.zerochan.net/7654321" {
		t.Errorf("url = %q", u)
	}
}

func TestMapCivitaiObjectTagsAndNestedFile(t *testing.T) {
	m := newMapper(t, nil)
	image := map[string]any{
		"category":    "civitai",
		"subcategory": "image",
		"file":        map[string]any{"id": float64(9173928), "tags": nil},
		"tags": []any{
			map[string]any{"name": "black dress", "type": "UserGenerated"},
			map[string]any{"name": "Realistic", "type": "Label"},
			map[string]any{"name": "realistic", "type": "UserGenerated"},
			map[string]any{"type": "Label"},
		},
	}
	pf := m.Map(image)
	if !slices.Equal(pf.Tags, []string{"black_dress", "realistic"}) {
		t.Errorf("tags = %v, want the object names normalized and deduplicated", pf.Tags)
	}
	if pf.Source != "civitai" {
		t.Errorf("source = %q, want civitai", pf.Source)
	}
	if u := m.PostURL(image); u != "https://civitai.red/images/9173928" {
		t.Errorf("url = %q, want the post-url template filled from file.id", u)
	}

	post := m.Map(map[string]any{
		"category":    "civitai",
		"subcategory": "post",
		"file": map[string]any{"id": float64(144844323), "tags": []any{
			map[string]any{"name": "no humans"},
			map[string]any{"name": "cat"},
		}},
	})
	assertTags(t, post.Tags, "no_humans", "cat")
}

func TestMapCivitaiRatingFromNsfwLevel(t *testing.T) {
	m := newMapper(t, nil)
	for _, tc := range []struct {
		level any
		want  string
	}{
		{float64(1), RatingGeneral},
		{float64(2), RatingSensitive},
		{float64(4), RatingQuestionable},
		{float64(8), RatingExplicit},
		{float64(16), RatingExplicit},
		{float64(32), RatingExplicit},
		{float64(0), ""},
		{nil, ""},
	} {
		pf := m.Map(map[string]any{"category": "civitai", "file": map[string]any{"id": float64(1), "nsfwLevel": tc.level}})
		if pf.Rating != tc.want {
			t.Errorf("civitai nsfwLevel %v = %q, want %q", tc.level, pf.Rating, tc.want)
		}
	}

	if pf := m.Map(map[string]any{"category": "wackybooru", "file": map[string]any{"nsfwLevel": float64(8)}}); pf.Rating != "" {
		t.Errorf("unprofiled nsfwLevel = %q, want unset", pf.Rating)
	}
}

func TestMapProfileRatingWords(t *testing.T) {
	m := newMapper(t, nil)
	for _, tc := range []struct{ site, raw, want string }{
		{"pixiv", "General", RatingGeneral},
		{"pixiv", "R-18", RatingExplicit},
		{"pixiv", "R-18G", RatingExplicit},

		{"newgrounds", "e", RatingGeneral},
		{"newgrounds", "t", RatingSensitive},
		{"newgrounds", "m", RatingQuestionable},
		{"newgrounds", "a", RatingExplicit},
		{"furaffinity", "General", RatingGeneral},
		{"furaffinity", "Mature", RatingQuestionable},
		{"furaffinity", "Adult", RatingExplicit},
		{"fantia", "general", RatingGeneral},
		{"fantia", "adult", RatingExplicit},

		{"booruvar", "s", RatingSensitive},
	} {
		if pf := m.Map(map[string]any{"category": tc.site, "id": float64(1), "rating": tc.raw}); pf.Rating != tc.want {
			t.Errorf("%s rating %q = %q, want %q", tc.site, tc.raw, pf.Rating, tc.want)
		}
	}
}

func TestMapExhentaiNormalizesSpacedTags(t *testing.T) {
	pf := newMapper(t, nil).Map(map[string]any{
		"category": "exhentai",
		"rating":   "1.64",
		"tags": []any{"female:slime girl", "language:chinese", "language:translated",
			"other:ai generated", "other:rough translation"},
		"tags_female":   []any{"slime girl"},
		"tags_language": []any{"chinese", "translated"},
		"tags_other":    []any{"ai generated", "rough translation"},
	})
	want := []string{
		"chinese", "translated",
		"slime_girl",
		"ai_generated", "rough_translation",
		"rating:explicit",
	}
	assertTags(t, pf.Tags, want...)
	if pf.Rating != RatingExplicit {
		t.Errorf("rating = %q, want explicit", pf.Rating)
	}
}

func TestNormalizeFoldsDisallowedChars(t *testing.T) {
	m := newMapper(t, nil)

	rb := m.Map(map[string]any{"category": "realbooru", "id": float64(1),
		"tags_general": "1girl, green_eyes, school_uniform"})
	for _, tag := range []string{"1girl", "green_eyes", "school_uniform"} {
		if !slices.Contains(rb.Tags, tag) {
			t.Errorf("realbooru comma-joined tags: missing %q in %v", tag, rb.Tags)
		}
	}

	got := m.Map(map[string]any{"category": "danbooru", "id": float64(1),
		"tags_copyright": []any{"fate/grand_order", "Red & Green"},
		"tags_artist":    []any{"DECO*27", "欧阳锦绮"},
	})
	for _, tag := range []string{"copyright:fate/grand_order", "copyright:red_&_green", "artist:deco_27", "artist:欧阳锦绮"} {
		if !slices.Contains(got.Tags, tag) {
			t.Errorf("missing tag %q in %v", tag, got.Tags)
		}
	}
	if slices.Contains(got.Tags, "artist:") {
		t.Errorf("empty-name tag should be dropped, got %v", got.Tags)
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
	if pf.Rating != RatingGeneral {
		t.Errorf("twibooru rating = %q, want general (the safe tag)", pf.Rating)
	}
	if !slices.Contains(pf.Tags, "rating:general") {
		t.Errorf("missing rating:general in %v", pf.Tags)
	}
	if slices.Contains(pf.Tags, "safe") {
		t.Errorf("the rating tag should not also be a content tag: %v", pf.Tags)
	}
	assertTags(t, pf.Tags, "solo", "pony", "spread_wings", "gradient_background", "artist:some_artist")
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
		"rating":       "s",
		"tags_general": []any{"foo"},
		"tags_artist":  []any{"bar"},
		"tags_mystery": []any{"baz"},
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
	assertTags(t, pf.Tags, "foo", "artist:bar", "baz", "rating:general")
}

func TestMapDirectlink(t *testing.T) {
	m := newMapper(t, nil)
	meta := map[string]any{
		"category": "directlink", "subcategory": "example.com",
		"domain": "img.example.com", "path": "art/2024",
		"filename": "picture", "extension": "jpg", "query": nil, "fragment": nil,
	}
	pf := m.Map(meta)
	if pf.Source != "img.example.com" {
		t.Errorf("source = %q, want the file host", pf.Source)
	}
	if len(pf.Tags) != 0 {
		t.Errorf("a bare media URL carries no tags, got %v", pf.Tags)
	}
	if pf.Rating != "" {
		t.Errorf("a bare media URL has no rating, got %q", pf.Rating)
	}
}

func TestCombinedTagsFallbackWhenNoSuffixes(t *testing.T) {
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

func TestTagOverrideToEmptyDropsTheCategory(t *testing.T) {
	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "e621", From: "species", To: ""}}
	pf := newMapper(t, cfg).Map(map[string]any{
		"category": "e621", "id": float64(1),
		"tags_species": []any{"canine"}, "tags_general": []any{"solo"},
	})
	assertTags(t, pf.Tags, "solo")
	for _, tag := range pf.Tags {
		if strings.HasSuffix(tag, "canine") {
			t.Errorf("an override to \"\" should drop the category, got %q in %v", tag, pf.Tags)
		}
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

func TestUserAgentSites(t *testing.T) {
	got := newMapper(t, nil).UserAgentSites()
	if !slices.Contains(got["danbooru"], "danbooru.donmai.us") || !slices.Contains(got["danbooru"], "donmai.us") {
		t.Errorf("danbooru hosts = %v, want its post host and file hosts", got["danbooru"])
	}
	if !slices.Contains(got["aibooru"], "aibooru.online") {
		t.Errorf("aibooru hosts = %v, want its post host", got["aibooru"])
	}
	for _, notWanted := range []string{"e621", "gelbooru"} {
		if _, ok := got[notWanted]; ok {
			t.Errorf("UserAgentSites should not include %q: %v", notWanted, got)
		}
	}
}

func TestDefaultAgent(t *testing.T) {
	m := newMapper(t, nil)
	for category, want := range map[string]string{
		"danbooru": "+gallery-dl",
		"aibooru":  "+gallery-dl",
		"e621":     "gallery-dl (by mikf)",
		"e926":     "gallery-dl (by mikf)",
		"rule34":   "+firefox",
		"yandere":  "+firefox",
		"mangadex": "gallery-dl's default",
		"zerochan": "gallery-dl's default",
		"civitai":  "gallery-dl's default",
	} {
		p, ok := m.BuiltinProfile(category)
		if !ok {
			t.Errorf("%s has no built-in profile", category)
			continue
		}
		if got := DefaultAgent(p); got != want {
			t.Errorf("DefaultAgent(%s) = %q, want %q", category, got, want)
		}
	}
	if got := DefaultAgent(Profile{Family: FamilyGeneric}); got != "gallery-dl's default" {
		t.Errorf("an unprofiled site = %q, want gallery-dl's default", got)
	}
	if got := DefaultAgent(Profile{Family: FamilyDanbooru}); got != "+gallery-dl" {
		t.Errorf("a custom danbooru instance = %q, want +gallery-dl", got)
	}
}

func TestSiteHostsCoverEveryFamily(t *testing.T) {
	got := newMapper(t, nil).SiteHosts()
	if !slices.Contains(got["rule34"], "rule34.xxx") {
		t.Errorf("rule34 hosts = %v, want its post host", got["rule34"])
	}
	if !slices.Contains(got["danbooru"], "donmai.us") || !slices.Contains(got["danbooru"], "danbooru.donmai.us") {
		t.Errorf("danbooru hosts = %v, want its post host and file hosts", got["danbooru"])
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
	if got := m.KindOf("totally-unknown"); got != KindBooru {
		t.Errorf("unmapped KindOf = %q, want %q", got, KindBooru)
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
		{"species", "species", true},
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

		{"moebooru with no per-category tags despite tags:true", map[string]any{"category": "konachan", "tags": "x"}, true},
		{"gelbooru_v02 with no per-category tags despite tags:true", map[string]any{"category": "safebooru", "tags": "x"}, true},
		{"flat-tag family with per-category tags", map[string]any{"category": "safebooru", "tags_general": []any{"x"}}, false},
		{"needs-tags generic with no per-category tags", map[string]any{"category": "sankaku", "tags": "x"}, true},
		{"unmapped generic site", map[string]any{"category": "wackybooru", "tags": "x"}, false},
		{"flat-tag family post with no tags at all", map[string]any{"category": "safebooru", "tags": ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.SuspectFlattenedTags(tc.meta); got != tc.want {
				t.Errorf("SuspectFlattenedTags = %v, want %v", got, tc.want)
			}
		})
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

func TestATagRuleCanRetargetToGeneral(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "wackybooru.json", `{"family":"generic","auth":"none","tag_rules":{"artist:x":"general:y"}}`)
	pf := newMapperDir(t, nil, dir).Map(map[string]any{"category": "wackybooru", "id": "1", "tags_artist": []any{"x"}})
	assertTags(t, pf.Tags, "y")
	assertNoTags(t, pf.Tags, "artist:y", "general:y")
}

func TestATagThatFoldsAwayIsNeitherWithheldNorTraced(t *testing.T) {
	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "e621", From: "lore", To: ""}}
	m := newMapper(t, cfg)
	meta := map[string]any{"category": "e621", "id": float64(1), "tags_lore": []any{"***", "ok"}, "tags_general": []any{"***", "kept"}}
	if pf := m.Map(meta); slices.Contains(pf.WithheldTags, "") || !slices.Contains(pf.WithheldTags, "meta:ok") {
		t.Errorf("withheld = %q", pf.WithheldTags)
	}
	for _, r := range m.Trace(meta).Rows {
		if r.Listed == "" {
			t.Errorf("blank trace row: %+v", r)
		}
	}
}

func TestMapHandlesMissingFields(t *testing.T) {
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
	m.view().profiles["custombooru"] = Profile{Family: FamilyGeneric, RatingOverrides: map[string]string{"x": RatingExplicit}}
	pf := m.Map(map[string]any{"category": "custombooru", "id": float64(1), "rating": "x"})
	if pf.Rating != RatingExplicit {
		t.Errorf("profile rating override should apply, got %q", pf.Rating)
	}
	m.view().profiles["custombooru"] = Profile{Family: FamilyGeneric, RatingOverrides: map[string]string{"R-18": RatingExplicit}}
	for _, raw := range []string{"R-18", "r-18", " r-18 "} {
		if pf := m.Map(map[string]any{"category": "custombooru", "id": float64(1), "rating": raw}); pf.Rating != RatingExplicit {
			t.Errorf("rating %q should match the uppercase key, got %q", raw, pf.Rating)
		}
	}
}

func TestRatingRankAndStricter(t *testing.T) {
	if contract.RatingRank("") != -1 {
		t.Error("unset rating should rank -1")
	}
	if contract.RatingRank(RatingGeneral) >= contract.RatingRank(RatingExplicit) {
		t.Error("general should rank below explicit")
	}
	if Stricter(RatingGeneral, "") != RatingGeneral {
		t.Error("a set level should beat unset")
	}
	if Stricter(RatingSensitive, RatingExplicit) != RatingExplicit {
		t.Error("explicit should win over sensitive")
	}
}

func TestTagRules(t *testing.T) {
	m := newMapper(t, config.Default())
	m.view().profiles["newgrounds"] = Profile{Family: FamilyGeneric, TagRules: map[string]string{
		"newgrounds_logo": "",
		"engrish_text":    "meta:engrish",
		"blue_hair":       "aqua_hair",
		"artist:unknown":  "",
	}}
	pf := m.Map(map[string]any{"category": "newgrounds", "id": float64(1),
		"tags": []any{"newgrounds_logo", "Engrish Text", "blue hair", "landscape"},
	})
	assertTags(t, pf.Tags, "meta:engrish", "aqua_hair", "landscape")
	assertNoTags(t, pf.Tags, "newgrounds_logo", "engrish_text", "blue_hair")
	pf = m.Map(map[string]any{"category": "newgrounds", "id": float64(2),
		"tags_artist": []any{"unknown", "paruperu"},
	})
	assertTags(t, pf.Tags, "artist:paruperu")
	assertNoTags(t, pf.Tags, "artist:unknown")
}

func TestTagRulesCategoryForms(t *testing.T) {
	m := newMapper(t, config.Default())
	m.view().profiles["somesite"] = Profile{Family: FamilyGeneric, TagRules: map[string]string{
		"misspeled": "misspelled",
	}}
	pf := m.Map(map[string]any{"category": "somesite", "id": float64(1),
		"tags_character": []any{"misspeled"},
	})
	assertTags(t, pf.Tags, "character:misspelled")
	assertNoTags(t, pf.Tags, "character:misspeled", "misspelled")
}

func TestSourceLabelSitesAndHosts(t *testing.T) {
	cfg := config.Default()
	cfg.FindSite("danbooru").Label = "Danbooru"
	cfg.HostLabels = []config.HostLabel{
		{Host: "wiki.example", Label: "example wiki"},
		{Host: "media.example:9000", Label: "the media box"},
	}
	dir := t.TempDir()
	writeProfile(t, dir, "webexample.json",
		`{"family":"generic","auth":"none","post_url_template":"https://website.example/posts/{id}","hosts":["img.website.example","mirror.example"]}`)
	m := newMapperDir(t, cfg, dir)

	for name, want := range map[string]string{
		"danbooru":               "Danbooru",
		"gelbooru":               "gelbooru",
		"danbooru.donmai.us":     "Danbooru",
		"img.website.example":    "webexample",
		"x.img.website.example":  "webexample",
		"cdn.donmai.us":          "Danbooru",
		"other.website.example":  "other.website.example",
		"wiki.example":           "example wiki",
		"files.wiki.example":     "example wiki",
		"unknown.example":        "unknown.example",
		"media.example:9000":     "the media box",
		"wiki.example:8080":      "example wiki",
		"wiki.example:":          "example wiki",
		"files.wiki.example:443": "example wiki",
		"other.example:1234":     "other.example:1234",
	} {
		if got := m.SourceLabel(name); got != want {
			t.Errorf("SourceLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSourceLabelConfigWinsOverProfileHosts(t *testing.T) {
	cfg := config.Default()
	cfg.HostLabels = []config.HostLabel{{Host: "mirror.example", Label: "the mirror"}}
	dir := t.TempDir()
	writeProfile(t, dir, "webexample.json",
		`{"family":"generic","auth":"none","hosts":["mirror.example"]}`)
	m := newMapperDir(t, cfg, dir)
	if got := m.SourceLabel("mirror.example"); got != "the mirror" {
		t.Errorf("SourceLabel = %q, want the host_labels row over the profile alias", got)
	}
}

func TestMapStampsSourceLabel(t *testing.T) {
	cfg := config.Default()
	cfg.FindSite("danbooru").Label = "Danbooru"
	cfg.HostLabels = []config.HostLabel{{Host: "example.com", Label: "example"}}
	m := newMapper(t, cfg)
	if pf := m.Map(map[string]any{"category": "danbooru", "id": float64(1)}); pf.Source != "Danbooru" {
		t.Errorf("source = %q, want the site label", pf.Source)
	}
	pf := m.Map(map[string]any{"category": "directlink", "domain": "img.example.com"})
	if pf.Source != "example" {
		t.Errorf("directlink source = %q, want the parent host's label", pf.Source)
	}
}

func TestValidateProfileHosts(t *testing.T) {
	p := Profile{Family: FamilyGeneric, Hosts: []string{"cdn.example.net"}}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("bare hostname alias rejected: %v", err)
	}
	p.Hosts = []string{"https://cdn.example.net/x"}
	if err := ValidateProfile(p); err == nil {
		t.Error("URL-shaped host alias accepted")
	}
}

func TestMapPostFileFactsByFamily(t *testing.T) {
	m := newMapper(t, nil)
	nested := map[string]any{"width": float64(800), "height": float64(600), "size": float64(1234), "ext": "png", "md5": "x", "url": "https://x/y.png"}
	cases := []struct {
		name string
		meta map[string]any
		w, h int
		size int64
	}{
		{"e621", map[string]any{"category": "e621", "id": float64(1), "file": nested}, 800, 600, 1234},
		{"civitai", map[string]any{"category": "civitai", "id": float64(1), "file": map[string]any{"width": float64(800), "height": float64(600)}}, 800, 600, 0},
		{"moebooru", map[string]any{"category": "yandere", "id": float64(1), "width": float64(800), "height": float64(600), "file_size": float64(9)}, 800, 600, 9},
		{"philomena", map[string]any{"category": "derpibooru", "id": float64(1), "width": float64(800), "height": float64(600), "size": float64(9)}, 800, 600, 9},
		{"rule34us", map[string]any{"category": "rule34us", "id": float64(1), "width": "800", "height": "600"}, 800, 600, 0},
	}
	for _, tc := range cases {
		pf := m.Map(tc.meta)
		if pf.PostWidth != tc.w || pf.PostHeight != tc.h || pf.PostSize != tc.size {
			t.Errorf("%s: %dx%d %d, want %dx%d %d", tc.name, pf.PostWidth, pf.PostHeight, pf.PostSize, tc.w, tc.h, tc.size)
		}
	}
}

func TestMapPostFileFacts(t *testing.T) {
	m := newMapper(t, nil)
	pf := m.Map(map[string]any{
		"category":     "danbooru",
		"id":           float64(1),
		"image_width":  float64(2894),
		"image_height": float64(4093),
		"file_size":    float64(3200000),
		"extension":    "PNG",
	})
	if pf.PostWidth != 2894 || pf.PostHeight != 4093 {
		t.Errorf("dimensions = %dx%d, want 2894x4093", pf.PostWidth, pf.PostHeight)
	}
	if pf.PostSize != 3200000 {
		t.Errorf("size = %d, want 3200000", pf.PostSize)
	}
	if pf.PostExt != "png" {
		t.Errorf("extension = %q, want png", pf.PostExt)
	}

	bare := m.Map(map[string]any{"category": "danbooru", "id": float64(2)})
	if bare.PostWidth != 0 || bare.PostHeight != 0 || bare.PostSize != 0 || bare.PostExt != "" {
		t.Errorf("bare post = %+v, want no file facts", bare)
	}

	alt := m.Map(map[string]any{"category": "gelbooru", "id": float64(3), "size": float64(4096)})
	if alt.PostSize != 4096 {
		t.Errorf("size fallback = %d, want 4096", alt.PostSize)
	}
}

func TestMapHashtagsLandAsGeneralTags(t *testing.T) {
	m := newMapper(t, nil)
	pf := m.Map(map[string]any{"category": "bluesky", "post_id": "3mz4", "hashtags": []any{"Art", "Blue Sky"}})
	assertTags(t, pf.Tags, "art", "blue_sky")
	pf = m.Map(map[string]any{"category": "bluesky", "tags": []any{"listed"}, "hashtags": []any{"art"}})
	assertTags(t, pf.Tags, "listed")
	assertNoTags(t, pf.Tags, "art")
}

func TestMapCustomCategoryTargets(t *testing.T) {
	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "e621", From: "lore", To: "va"}, {Site: "e621", From: "meta", To: "typo"}}
	m := newMapper(t, cfg)
	withCustomCategories(t)
	m.LearnCustomCategories([]string{"va"}, false)
	pf := m.Map(map[string]any{"category": "e621", "id": float64(1),
		"tags_lore": []any{"someone"}, "tags_meta": []any{"highres"}})
	assertTags(t, pf.Tags, "va:someone", "meta:highres")
	assertNoTags(t, pf.Tags, "typo:highres")
}

func TestMapWithholdsWhatASiteDrops(t *testing.T) {
	cfg := config.Default()
	cfg.TagOverrides = []config.TagOverride{{Site: "danbooru", From: "copyright", To: ""}}
	dir := t.TempDir()
	writeProfile(t, dir, "danbooru.json", `{"family":"danbooru","auth":"api_optional",
		"category_overrides":{"meta":""},
		"tag_rules":{"artist:gone":"","hat":"","scarf":"neckwear"}}`)
	m := newMapperDir(t, cfg, dir)
	pf := m.Map(map[string]any{
		"category": "danbooru", "id": float64(1),
		"tags_general":   []any{"1girl", "hat", "scarf"},
		"tags_artist":    []any{"gone", "kept"},
		"tags_copyright": []any{"touhou"},
		"tags_meta":      []any{"highres"},
	})
	if want := []string{"1girl", "artist:kept", "neckwear"}; !slices.Equal(pf.Tags, want) {
		t.Errorf("tags = %v, want %v", pf.Tags, want)
	}
	if want := []string{"artist:gone", "copyright:touhou", "hat", "meta:highres"}; !slices.Equal(pf.WithheldTags, want) {
		t.Errorf("withheld = %v, want %v", pf.WithheldTags, want)
	}
}

func TestMapDropsCategories(t *testing.T) {
	cfg := config.Default()
	cfg.Tags.DropCategories = []string{"general", "meta"}
	m := newMapper(t, cfg)
	withCustomCategories(t)
	meta := map[string]any{
		"category": "danbooru", "id": float64(1), "rating": "g",
		"tags_general":   []any{"1girl", "moved_general"},
		"tags_artist":    []any{"paruperu"},
		"tags_character": []any{"reimu", "moved_character"},
		"tags_copyright": []any{"touhou"},
		"tags_meta":      []any{"highres"},
	}
	pf := m.Map(meta)
	if want := []string{"artist:paruperu", "character:moved_character", "character:reimu", "copyright:touhou", "rating:general"}; !slices.Equal(pf.Tags, want) {
		t.Errorf("tags = %v, want %v", pf.Tags, want)
	}
	merged := MergeExtras(pf, Extras{Tags: []string{"sent_by_hand"}}, true)
	assertTags(t, merged.ClientTags, "sent_by_hand")
}

func TestMapDropRunsAfterTagRules(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "danbooru.json", `{"family":"danbooru","auth":"none",
		"tag_rules":{"keep_me":"character:keep_me","character:lose_me":"lose_me"}}`)
	cfg := config.Default()
	cfg.Sites = nil
	cfg.Tags.DropCategories = []string{"general"}
	m := newMapperDir(t, cfg, dir)
	pf := m.Map(map[string]any{"category": "danbooru", "id": float64(1),
		"tags_general":   []any{"keep_me", "other"},
		"tags_character": []any{"lose_me"},
	})
	assertTags(t, pf.Tags, "character:keep_me", "character:lose_me")
	assertNoTags(t, pf.Tags, "other", "keep_me")
}
