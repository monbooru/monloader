package gdl

import (
	"context"
	"sync"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

// Catalog is the cached gallery-dl inventory the settings screens and the
// API read: the active binary's version, the --list-extractors result, and
// the parsed supportedsites rows. Boot seeds it; a managed install or revert
// replaces it wholesale, so a reader never mixes data from two binaries.
// Returned slices and maps are replaced, never mutated.
type Catalog struct {
	mu             sync.RWMutex
	version        string
	bundledVersion string
	managed        bool
	extractors     []Extractor
	supported      map[string]SupportedSite
}

// NewCatalog seeds the catalog with an inventory the caller already has.
func NewCatalog(version, bundledVersion string, managed bool, extractors []Extractor, supported map[string]SupportedSite) *Catalog {
	c := &Catalog{}
	c.replace(version, bundledVersion, managed, extractors, supported)
	return c
}

// ProbeCatalog builds the catalog boot starts from: one probe of whatever
// gallery-dl is active.
func ProbeCatalog(ctx context.Context, runner Runner, cfg *config.Config, root string) *Catalog {
	c := &Catalog{}
	c.Refresh(ctx, runner, cfg, root)
	return c
}

// Refresh re-probes the active binary and swaps the whole inventory. A probe
// that fails keeps the previous listing - an empty site search would read as
// "no sites supported", which is worse than a slightly stale one.
func (c *Catalog) Refresh(ctx context.Context, runner Runner, cfg *config.Config, root string) {
	version := runner.Version(ctx)
	extractors, err := runner.ListExtractors(ctx)
	if err != nil {
		logx.Warnf("listing gallery-dl extractors: %v", err)
		extractors = c.Extractors()
	}
	// supportedsites.md seeds display names and auth kinds for sites without a
	// shipped profile - the managed install's fetched copy when one is active,
	// the bundled one otherwise; without either those just seed empty.
	path := EffectiveSupportedSitesPath(cfg, root)
	supported, err := ParseSupportedSites(path)
	if err != nil {
		logx.Infof("supportedsites data unavailable at %q: %v", path, err)
		supported = c.Supported()
	}
	managed := ManagedActive(cfg, root)
	bundled := version
	if managed {
		// Resolved with no managed root, which is the rung a revert lands on:
		// on a bundled artifact the binary is beside the executable and not on
		// PATH, so the bare binary_path would probe nothing.
		bundled = BinaryVersion(ctx, EffectiveBinary(cfg, ""))
	}
	c.replace(version, bundled, managed, extractors, supported)
}

// replace swaps the whole inventory after the active binary changed.
func (c *Catalog) replace(version, bundledVersion string, managed bool, extractors []Extractor, supported map[string]SupportedSite) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version, c.bundledVersion, c.managed = version, bundledVersion, managed
	c.extractors, c.supported = extractors, supported
}

// Version is the active binary's version, "" when it cannot be run.
func (c *Catalog) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

// BundledVersion is the image's pinned gallery-dl, the fallback a revert
// returns to. Equal to Version when no managed install is active.
func (c *Catalog) BundledVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bundledVersion
}

// Managed reports whether a managed install currently shadows the bundled
// binary.
func (c *Catalog) Managed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.managed
}

// Extractors is the cached --list-extractors result. Read-only.
func (c *Catalog) Extractors() []Extractor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.extractors
}

// ExampleFor is the first example URL the extractor list carries for a
// category, or "". The list is this package's, so the lookup is too - the
// mapper answers with its profile's example and takes this as the fallback.
func (c *Catalog) ExampleFor(category string) string {
	for _, ex := range c.Extractors() {
		if ex.Category == category && ex.Example != "" {
			return ex.Example
		}
	}
	return ""
}

// Supported is the parsed supportedsites table by category. Read-only.
func (c *Catalog) Supported() map[string]SupportedSite {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.supported
}
