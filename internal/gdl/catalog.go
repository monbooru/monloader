package gdl

import (
	"context"
	"sync/atomic"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/logx"
)

type catalogData struct {
	version        string
	bundledVersion string
	managed        bool
	extractors     []Extractor
	supported      map[string]SupportedSite
}

// Catalog is replaced wholesale, so a reader never mixes two binaries'
// data; returned slices and maps are never mutated.
type Catalog struct {
	data atomic.Pointer[catalogData]
}

func NewCatalog(version, bundledVersion string, managed bool, extractors []Extractor, supported map[string]SupportedSite) *Catalog {
	c := &Catalog{}
	c.data.Store(&catalogData{version, bundledVersion, managed, extractors, supported})
	return c
}

func ProbeCatalog(ctx context.Context, runner Runner, cfg *config.Config, root string) *Catalog {
	c := NewCatalog("", "", false, nil, nil)
	c.Refresh(ctx, runner, cfg, root)
	return c
}

// A failed probe keeps the previous listing: an empty one reads as "no
// sites supported". A context that ran out says nothing about the binary, so
// its version stays too.
func (c *Catalog) Refresh(ctx context.Context, runner Runner, cfg *config.Config, root string) {
	version := runner.Version(ctx)
	if version == "" && ctx.Err() != nil {
		version = c.Version()
	}
	extractors, err := runner.ListExtractors(ctx)
	if err != nil {
		logx.Warnf("listing gallery-dl extractors: %v", err)
		extractors = c.Extractors()
	}
	path := EffectiveSupportedSitesPath(cfg, root)
	supported, err := ParseSupportedSites(path)
	if err != nil {
		logx.Infof("supportedsites data unavailable at %q: %v", path, err)
		supported = c.Supported()
	}
	managed := ManagedActive(cfg, root)
	bundled := version
	if managed {
		// The binary a revert lands on; a bundled one is not on PATH, so
		// the bare binary_path would probe nothing.
		bundled = BinaryVersion(ctx, EffectiveBinary(cfg, ""))
	}
	c.data.Store(&catalogData{version, bundled, managed, extractors, supported})
}

func (c *Catalog) Version() string { return c.data.Load().version }

func (c *Catalog) BundledVersion() string { return c.data.Load().bundledVersion }

func (c *Catalog) Managed() bool { return c.data.Load().managed }

func (c *Catalog) Extractors() []Extractor { return c.data.Load().extractors }

func (c *Catalog) ExampleFor(category string) string {
	for _, ex := range c.Extractors() {
		if ex.Category == category && ex.Example != "" {
			return ex.Example
		}
	}
	return ""
}

func (c *Catalog) Supported() map[string]SupportedSite { return c.data.Load().supported }
