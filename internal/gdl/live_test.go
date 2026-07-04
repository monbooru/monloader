package gdl

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/kwdict"
	"github.com/leqwin/monloader/internal/queue"
)

// The wrapper is exercised against the real gallery-dl rather than a fake. A
// machine without gallery-dl skips these; CI installs the pinned version, so
// they always run there.
const danbooruURL = "https://danbooru.donmai.us/posts?tags=landscape+rating:general"

// liveTool wires a Tool to the real gallery-dl on PATH, skipping when it is
// absent.
func liveTool(t *testing.T) *Tool {
	t.Helper()
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"
	cfg.GalleryDL.ConfigPath = ""
	tool := New(cfg, nil, nil, nil)
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}
	return tool
}

func TestLiveVersion(t *testing.T) {
	if v := liveTool(t).Version(context.Background()); v == "" {
		t.Error("expected a version string from the real gallery-dl")
	}
}

func TestLiveListExtractors(t *testing.T) {
	ex, err := liveTool(t).ListExtractors(context.Background())
	if err != nil {
		t.Fatalf("ListExtractors: %v", err)
	}
	// The real 1.32.1 lists hundreds of extractors; assert a generous floor.
	if len(ex) < 100 {
		t.Errorf("got %d extractors, want >= 100", len(ex))
	}
}

func TestLiveResolveUnsupportedURL(t *testing.T) {
	// A URL no extractor matches exits non-zero offline; the wrapper must turn
	// that into the stable unsupported-URL code.
	_, err := liveTool(t).Resolve(context.Background(), "http://nope", "", false)
	var ge *queue.CodedError
	if e, ok := err.(*queue.CodedError); ok {
		ge = e
	}
	if ge == nil || ge.Code != queue.ErrCodeUnsupportedURL {
		t.Errorf("error = %v, want code %s", err, queue.ErrCodeUnsupportedURL)
	}
}

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

func TestLiveDownload(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"
	// A managed config writes the .json sidecars the download pass reads; no
	// archive so the post is always fetched rather than skipped on a re-run.
	dir := t.TempDir()
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = ""
	if err := WriteManagedConfig(cfg, nil, nil, nil); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	tool := New(cfg, nil, nil, nil)
	if tool.Version(context.Background()) == "" {
		t.Skip("real gallery-dl not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var streamed []Downloaded
	downloaded, err := tool.Download(ctx, danbooruURL, "1-1", filepath.Join(dir, "work"), false, func(_ int, d Downloaded) {
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
	// onFile reports each file as gallery-dl prints it, so the queue advances
	// items before the whole download returns.
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
