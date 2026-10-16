package gdl

import (
	"context"
	"sync"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/logx"
)

// Catalog is replaced wholesale, so a reader never mixes two binaries'
// data; returned slices and maps are never mutated.
type Catalog struct {
	mu             sync.RWMutex
	version        string
	bundledVersion string
	managed        bool
	extractors     []Extractor
	supported      map[string]SupportedSite
}

func NewCatalog(version, bundledVersion string, managed bool, extractors []Extractor, supported map[string]SupportedSite) *Catalog {
	c := &Catalog{}
	c.replace(version, bundledVersion, managed, extractors, supported)
	return c
}

func ProbeCatalog(ctx context.Context, runner Runner, cfg *config.Config, root string) *Catalog {
	c := &Catalog{}
	c.Refresh(ctx, runner, cfg, root)
	return c
}

// A failed probe keeps the previous listing: an empty one reads as "no
// sites supported".
func (c *Catalog) Refresh(ctx context.Context, runner Runner, cfg *config.Config, root string) {
	version := runner.Version(ctx)
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
	c.replace(version, bundled, managed, extractors, supported)
}

func (c *Catalog) replace(version, bundledVersion string, managed bool, extractors []Extractor, supported map[string]SupportedSite) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version, c.bundledVersion, c.managed = version, bundledVersion, managed
	c.extractors, c.supported = extractors, supported
}

func (c *Catalog) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

func (c *Catalog) BundledVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bundledVersion
}

func (c *Catalog) Managed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.managed
}

func (c *Catalog) Extractors() []Extractor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.extractors
}

func (c *Catalog) ExampleFor(category string) string {
	for _, ex := range c.Extractors() {
		if ex.Category == category && ex.Example != "" {
			return ex.Example
		}
	}
	return ""
}

func (c *Catalog) Supported() map[string]SupportedSite {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.supported
}
