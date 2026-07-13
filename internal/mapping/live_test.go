package mapping

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/kwdict"
)

// liveDownloadOne runs the real gallery-dl through the managed-config download
// path and returns the mapper plus the first downloaded item's sidecar
// metadata. The managed config sets tags:true for the flat-tag families so
// their per-category tags appear. A machine without gallery-dl skips (CI
// installs the pinned version). The cases below assert metadata shape, not
// exact posts - live results vary - so a gallery-dl bump that renamed a field
// drops the categorized tags or the rating here, which the pure mapping tests
// on fixed input cannot see. They download one item to a temp dir and read its
// metadata, so the content rating is incidental.
func liveDownloadOne(t *testing.T, url string) (*Mapper, map[string]any) {
	t.Helper()
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"
	// A managed config writes the .json sidecars the download pass reads; no
	// archive so the post is always fetched rather than skipped on a re-run.
	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = ""
	mapper, err := New(config.NewProvider(cfg))
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	if err := gdl.WriteManagedConfig(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites()); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	tool := gdl.New(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites())
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	downloaded, err := tool.Download(ctx, url, "1-1", filepath.Join(dir, "work"), false, nil, false)
	if err != nil {
		t.Fatalf("real Download: %v", err)
	}
	if len(downloaded) == 0 {
		t.Fatal("expected at least one downloaded file with a sidecar")
	}
	return mapper, downloaded[0].Meta
}

// hasCategoryTags reports whether meta carries any tags_<category> field. Its
// absence on a site that categorizes is the fingerprint of a gallery-dl bump
// renaming or dropping those fields: the mapper would then flatten every tag
// into general.
func hasCategoryTags(meta map[string]any) bool {
	for k := range meta {
		if strings.HasPrefix(k, "tags_") {
			return true
		}
	}
	return false
}

// TestLiveMappingFamilies maps one live post per booru family and pins the
// shape each must keep: per-category tag fields (native on danbooru/e621, via
// the managed tags:true on the flat-tag families), the post-url template, and
// the family's rating letters. A gallery-dl bump that renamed a tag field or a
// rating value fails here while the pure fixed-input tests stay green.
func TestLiveMappingFamilies(t *testing.T) {
	cases := []struct {
		name, url string
		source    string
		urlPrefix string
		rating    string
		ratingWhy string
	}{
		// danbooru categorizes natively, so the sidecar carries tags_<category>
		// fields without tags:true.
		{
			name: "danbooru", url: "https://danbooru.donmai.us/posts?tags=landscape+rating:general",
			source: "danbooru", urlPrefix: "https://danbooru.donmai.us/posts/",
			rating: RatingGeneral, ratingWhy: "the search filtered rating:general",
		},
		// safebooru.org is a flat-tag gelbooru_v02 site: it categorizes only with
		// tags:true, which the managed config sets - a bump breaking the tags
		// option drops the per-category tags here while danbooru (native) stays
		// green. The family maps s -> general and the profile pins its stale
		// q -> general, so any post maps to general.
		{
			name: "safebooru", url: "https://safebooru.org/index.php?page=post&s=list&tags=1girl",
			source: "safebooru", urlPrefix: "https://safebooru.org/index.php?page=post&s=view&id=",
			rating: RatingGeneral, ratingWhy: "s and stale q both map to general",
		},
		// e621 categorizes natively with tag classes the other families lack
		// (species, lore, contributor) and overloads the rating letter: the
		// search pins rating:s, which is "safe" here (general), not danbooru's
		// "sensitive".
		{
			name: "e621", url: "https://e621.net/posts?tags=wolf+rating:s",
			source: "e621", urlPrefix: "https://e621.net/posts/",
			rating: RatingGeneral, ratingWhy: "e621 s = safe",
		},
		// konachan is a moebooru site - a second flat-tag family on a different
		// extractor than gelbooru - that categorizes only with tags:true.
		{
			name: "konachan", url: "https://konachan.com/post?tags=landscape+rating:s",
			source: "konachan", urlPrefix: "https://konachan.com/post/show/",
			rating: RatingGeneral, ratingWhy: "moebooru s = safe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapper, meta := liveDownloadOne(t, tc.url)
			if !hasCategoryTags(meta) {
				t.Errorf("%s sidecar carried no tags_<category> fields; the tags:true path or gallery-dl's tag shape may have changed", tc.name)
			}
			pf := mapper.Map(meta)
			if pf.Source != tc.source {
				t.Errorf("source = %q, want %s", pf.Source, tc.source)
			}
			if u := mapper.PostURL(meta); !strings.HasPrefix(u, tc.urlPrefix) {
				t.Errorf("url = %q, want the %s post-url template", u, tc.name)
			}
			if len(pf.Tags) == 0 {
				t.Error("mapped post produced no tags")
			}
			if pf.Rating != tc.rating {
				t.Errorf("rating = %q, want %s (%s)", pf.Rating, tc.rating, tc.ratingWhy)
			}
		})
	}
}

// TestLiveLookupDanbooruMD5 takes a live post's md5 (from a resolve of a SFW
// search) and re-finds the same post through the danbooru md5 search template,
// which is the booru hash lookup's plumbing against the real site. A
// danbooru-side change to the md5: metatag or a gallery-dl change to the
// tag-search extractor fails here while the fixed-input tests stay green.
func TestLiveLookupDanbooruMD5(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"
	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = ""
	mapper, err := New(config.NewProvider(cfg))
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	if err := gdl.WriteManagedConfig(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites()); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	tool := gdl.New(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites())
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Seed via the capped resolve pass: a bare FetchMeta on a broad search
	// would fetch full metadata for the whole first page. The md5 rides the
	// resolve's base fields.
	seed, err := tool.Resolve(ctx, "https://danbooru.donmai.us/posts?tags=landscape+rating:general", "1-1", false)
	if err != nil {
		t.Fatalf("seed resolve: %v", err)
	}
	if len(seed.Items) == 0 {
		t.Fatal("seed search resolved no posts")
	}
	md5, _ := seed.Items[0].Meta["md5"].(string)
	if md5 == "" {
		t.Fatal("live post carried no md5")
	}
	found, err := tool.FetchMeta(ctx, mapper.LookupURL("danbooru", md5))
	if err != nil {
		t.Fatalf("md5 lookup: %v", err)
	}
	if kwdict.ID(found) != seed.Items[0].ID {
		t.Errorf("md5 lookup found post %s, want %s", kwdict.ID(found), seed.Items[0].ID)
	}
	if pf := mapper.Map(found); len(pf.Tags) == 0 {
		t.Error("looked-up post mapped to no tags")
	}
}

// TestLiveMappingDirectlink maps a live bare media URL through gallery-dl's
// directlink pseudo-extractor. It has no booru behind it, so the host is the
// source and the file URL is rebuilt from the parts gallery-dl exposes; the
// fixture is the extractor's own example. A gallery-dl bump renaming the
// domain/path parts would change the rebuilt URL here, which the fixed-input
// test cannot see.
func TestLiveMappingDirectlink(t *testing.T) {
	const fileURL = "https://en.wikipedia.org/static/images/project-logos/enwiki.png"
	mapper, meta := liveDownloadOne(t, fileURL)
	pf := mapper.Map(meta)
	if pf.Source != "en.wikipedia.org" {
		t.Errorf("source = %q, want the file host", pf.Source)
	}
	if u := mapper.PostURL(meta); u != fileURL {
		t.Errorf("url = %q, want the file URL %q", u, fileURL)
	}
}

// TestLiveMappingPhilomena maps a live derpibooru post. Philomena boorus carry
// no rating field and no tags_<category> shape: the rating is one of the tags
// and the flat tags route by namespace prefix. The search pins the "safe" tag,
// which is lifted to the general rating rather than kept as a content tag.
func TestLiveMappingPhilomena(t *testing.T) {
	mapper, meta := liveDownloadOne(t, "https://derpibooru.org/search?q=safe")
	pf := mapper.Map(meta)
	if pf.Source != "derpibooru" {
		t.Errorf("source = %q, want derpibooru", pf.Source)
	}
	if u := mapper.PostURL(meta); !strings.HasPrefix(u, "https://derpibooru.org/images/") {
		t.Errorf("url = %q, want the derpibooru post-url template", u)
	}
	if len(pf.Tags) == 0 {
		t.Error("mapped post produced no tags")
	}
	if pf.Rating != RatingGeneral {
		t.Errorf("rating = %q, want general (lifted from the safe tag)", pf.Rating)
	}
	if slices.Contains(pf.Tags, "safe") {
		t.Error("the rating tag should be lifted, not kept as a content tag")
	}
}

// TestLiveMappingManga maps a live mangadex chapter. A manga/comic source has no
// tags_<category> shape; its categorized fields (artist, author, group) are
// separate lists the manga-kind path folds into the artist category. A chapter
// URL keeps --range 1-1 to one page rather than a whole title's chapters. The
// chapter is a fixed fixture; if mangadex removes it, point this at another.
func TestLiveMappingManga(t *testing.T) {
	mapper, meta := liveDownloadOne(t, "https://mangadex.org/chapter/bdb2b04b-6120-448e-b16c-8706fa37b526")
	pf := mapper.Map(meta)
	if pf.Source != "mangadex" {
		t.Errorf("source = %q, want mangadex", pf.Source)
	}
	if len(pf.Tags) == 0 {
		t.Error("mapped chapter produced no tags")
	}
	// artist / author / group all fold to the artist category; a manga-kind
	// source that stopped folding them would drop every credited name.
	if !slices.ContainsFunc(pf.Tags, func(s string) bool { return strings.HasPrefix(s, "artist:") }) {
		t.Errorf("expected a folded artist tag from the manga fields, got %v", pf.Tags)
	}
}
