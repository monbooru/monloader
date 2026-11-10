//go:build live

package gdl

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/kwdict"
)

const danbooruURL = "https://danbooru.donmai.us/posts?tags=landscape+rating:general"

func TestLiveResolve(t *testing.T) {
	tool := liveTool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := tool.Resolve(ctx, danbooruURL, "1-1", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	items := res.Items
	if len(items) == 0 {
		t.Fatal("expected at least one resolved item")
	}
	if items[0].Category != "danbooru" {
		t.Errorf("category = %q, want danbooru", items[0].Category)
	}
	if items[0].ID == "" {
		t.Error("resolved item has no id")
	}
}

func TestLiveResolveStopsABroadListingPastItsWindow(t *testing.T) {
	tool := liveTool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := tool.Resolve(ctx, "https://e-hentai.org/?f_search=language%3Aenglish", "1-2", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Queue) != 3 || res.Subcategory != "search" {
		t.Errorf("got %d handoffs from %q, want 3 from search", len(res.Queue), res.Subcategory)
	}
}

func TestLiveDownload(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"

	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = ""
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	tool := New(config.NewProvider(cfg), SiteLists{})
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var streamed []Downloaded
	downloaded, err := tool.Download(ctx, danbooruURL, "1-1", filepath.Join(dir, "work"), ArchiveRead, func(d Downloaded) {
		streamed = append(streamed, d)
	}, false)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(downloaded) == 0 {
		t.Fatal("expected at least one downloaded file with a sidecar")
	}
	if got := kwdict.String(downloaded[0].Meta, "category"); got != "danbooru" {
		t.Errorf("category = %q, want danbooru", got)
	}
	if len(streamed) == 0 {
		t.Error("onFile was not called for the streamed download")
	} else if kwdict.String(streamed[0].Meta, "category") != "danbooru" || kwdict.ID(streamed[0].Meta) == "" {
		t.Errorf("streamed item = %+v, want a danbooru id", streamed[0])
	}
}

func TestLiveProbeOK(t *testing.T) {
	tool := liveTool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := tool.Probe(ctx, danbooruURL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Status != ProbeOK {
		t.Errorf("status = %s, want ok (detail %q)", res.Status, res.Detail)
	}
}

func TestLiveDebugLog(t *testing.T) {
	tool := liveTool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	log, _, err := tool.DebugLog(ctx, danbooruURL, t.TempDir())
	if err != nil {
		t.Fatalf("DebugLog: %v", err)
	}
	if !strings.Contains(log, "[debug]") {
		t.Errorf("log = %q, want gallery-dl debug lines", truncate(log))
	}
	if !strings.Contains(log, "danbooru") {
		t.Errorf("log = %q, want the extractor it chose", truncate(log))
	}
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
