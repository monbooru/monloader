package gdl

import (
	"context"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
)

func liveTool(t *testing.T) *Tool {
	t.Helper()
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = "gallery-dl"
	cfg.GalleryDL.ConfigPath = ""
	tool := New(config.NewProvider(cfg), SiteLists{})
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

	if len(ex) < 100 {
		t.Errorf("got %d extractors, want >= 100", len(ex))
	}
}

func TestLiveResolveUnsupportedURL(t *testing.T) {
	_, err := liveTool(t).Resolve(context.Background(), "http://nope", "", false)
	var ge *outcome.CodedError
	if e, ok := err.(*outcome.CodedError); ok {
		ge = e
	}
	if ge == nil || ge.Code != outcome.ErrCodeUnsupportedURL {
		t.Errorf("error = %v, want code %s", err, outcome.ErrCodeUnsupportedURL)
	}
}
