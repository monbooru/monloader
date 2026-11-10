//go:build live

package mapping

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/outcome"
)

func liveDownloadOne(t *testing.T, url string) (*Mapper, map[string]any) {
	t.Helper()
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"

	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = ""
	mapper, err := New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	if err := gdl.WriteManagedConfig(cfg, gdl.SiteLists{FlatTag: mapper.FlatTagSites(), Metadata: mapper.MetadataSites(), Notes: mapper.NotesSites(), UserAgent: mapper.UserAgentSites(), Options: mapper.SiteOptions()}); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	tool := gdl.New(config.NewProvider(cfg), gdl.SiteLists{FlatTag: mapper.FlatTagSites(), Metadata: mapper.MetadataSites(), Notes: mapper.NotesSites()})
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	downloaded, err := tool.Download(ctx, url, "1-1", filepath.Join(dir, "work"), gdl.ArchiveRead, nil, false)
	if err != nil {

		var ce *outcome.CodedError
		if errors.As(err, &ce) &&
			(ce.Code == outcome.ErrCodeRateLimited || ce.Code == outcome.ErrCodeNetworkUnreachable) {
			t.Skipf("live source unavailable (%s): %v", ce.Code, err)
		}
		t.Fatalf("real Download: %v", err)
	}
	if len(downloaded) == 0 {
		t.Fatal("expected at least one downloaded file with a sidecar")
	}
	return mapper, downloaded[0].Meta
}

func hasCategoryTags(meta map[string]any) bool {
	for k := range meta {
		if strings.HasPrefix(k, "tags_") {
			return true
		}
	}
	return false
}

func TestLiveMappingFamilies(t *testing.T) {
	cases := []struct {
		name, url  string
		source     string
		urlPrefix  string
		rating     string
		ratingWhy  string
		wantTag    string
		wantTagWhy string
	}{

		{
			name: "danbooru", url: "https://danbooru.donmai.us/posts?tags=landscape+rating:general",
			source: "danbooru", urlPrefix: "https://danbooru.donmai.us/posts/",
			rating: RatingGeneral, ratingWhy: "the search filtered rating:general",
		},

		{
			name: "safebooru", url: "https://safebooru.org/index.php?page=post&s=list&tags=1girl",
			source: "safebooru", urlPrefix: "https://safebooru.org/index.php?page=post&s=view&id=",
			rating: RatingGeneral, ratingWhy: "s and stale q both map to general",
		},

		{
			name: "e621", url: "https://e621.net/posts?tags=wolf+rating:s",
			source: "e621", urlPrefix: "https://e621.net/posts/",
			rating: RatingGeneral, ratingWhy: "e621 s = safe",
			wantTag: "species:wolf", wantTagWhy: "the search pinned the wolf species tag",
		},

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
			if tc.wantTag != "" && !slices.Contains(pf.Tags, tc.wantTag) {
				t.Errorf("missing tag %q (%s) in %v", tc.wantTag, tc.wantTagWhy, pf.Tags)
			}
		})
	}
}

func TestLiveLookupDanbooruMD5(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"
	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = ""
	mapper, err := New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	if err := gdl.WriteManagedConfig(cfg, gdl.SiteLists{FlatTag: mapper.FlatTagSites(), Metadata: mapper.MetadataSites(), Notes: mapper.NotesSites(), UserAgent: mapper.UserAgentSites(), Options: mapper.SiteOptions()}); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	tool := gdl.New(config.NewProvider(cfg), gdl.SiteLists{FlatTag: mapper.FlatTagSites(), Metadata: mapper.MetadataSites(), Notes: mapper.NotesSites()})
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

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

func TestLiveMappingDanbooruTranslatedCommentary(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"
	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = ""
	mapper, err := New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	if err := gdl.WriteManagedConfig(cfg, gdl.SiteLists{FlatTag: mapper.FlatTagSites(), Metadata: mapper.MetadataSites(), Notes: mapper.NotesSites(), UserAgent: mapper.UserAgentSites(), Options: mapper.SiteOptions()}); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	tool := gdl.New(config.NewProvider(cfg), gdl.SiteLists{FlatTag: mapper.FlatTagSites(), Metadata: mapper.MetadataSites(), Notes: mapper.NotesSites()})
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	seed, err := tool.Resolve(ctx, "https://danbooru.donmai.us/posts?tags=commentary%3Atranslated", "1-1", false)
	if err != nil {
		t.Fatalf("seed resolve: %v", err)
	}
	if len(seed.Items) == 0 {
		t.Skip("no translated-commentary post on the first page")
	}
	meta, err := tool.FetchMeta(ctx, mapper.PostURLFor("danbooru", seed.Items[0].ID))
	if err != nil {
		t.Fatalf("fetch meta: %v", err)
	}
	pf := mapper.Map(meta)
	if pf.Commentary == "" || pf.CommentaryTranslated == "" {
		t.Fatalf("commentary = %q, translation = %q; want both", pf.Commentary, pf.CommentaryTranslated)
	}
	if pf.Commentary == pf.CommentaryTranslated {
		t.Errorf("both bodies are %q; the original was not kept", pf.Commentary)
	}
}

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

func TestLiveMappingPhilomena(t *testing.T) {
	mapper, meta := liveDownloadOne(t, "https://derpibooru.org/search?q=safe&sf=score&sd=desc")
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

func TestLiveMappingCivitai(t *testing.T) {
	mapper, meta := liveDownloadOne(t, "https://civitai.red/images/12097475")
	pf := mapper.Map(meta)
	if pf.Source != "civitai" {
		t.Errorf("source = %q, want civitai", pf.Source)
	}
	if u := mapper.PostURL(meta); u != "https://civitai.red/images/12097475" {
		t.Errorf("url = %q, want the post-url template filled from the nested file id", u)
	}
	if len(pf.Tags) == 0 {
		t.Error("mapped image produced no tags; the tag objects or the metadata option may have changed")
	}
	if pf.Rating == "" {
		t.Error("no rating; civitai's file.nsfwLevel may have moved or changed shape")
	}
}

func TestLiveMappingBluesky(t *testing.T) {
	m, meta := liveDownloadOne(t, "https://bsky.app/profile/bsky.app/media")
	if got := m.Map(meta).Source; got != "bluesky" {
		t.Errorf("source = %q, want bluesky", got)
	}
	if got := m.PostURL(meta); !strings.HasPrefix(got, "https://bsky.app/profile/bsky.app/post/") {
		t.Errorf("post url = %q, want the post page, not the feed", got)
	}
	if kwdict.ID(meta) == "" {
		t.Error("the item has no post id; gallery-dl's post_id may have been renamed")
	}
}

func TestLiveMappingManga(t *testing.T) {
	mapper, meta := liveDownloadOne(t, "https://mangadex.org/chapter/bdb2b04b-6120-448e-b16c-8706fa37b526")
	pf := mapper.Map(meta)
	if pf.Source != "mangadex" {
		t.Errorf("source = %q, want mangadex", pf.Source)
	}
	if len(pf.Tags) == 0 {
		t.Error("mapped chapter produced no tags")
	}
	if !slices.ContainsFunc(pf.Tags, func(s string) bool { return strings.HasPrefix(s, "artist:") }) {
		t.Errorf("expected a folded artist tag from the manga fields, got %v", pf.Tags)
	}
}
