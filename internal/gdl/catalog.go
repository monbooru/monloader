package gdl

import "sync"

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

// NewCatalog seeds the catalog with the boot-time probe results.
func NewCatalog(version, bundledVersion string, managed bool, extractors []Extractor, supported map[string]SupportedSite) *Catalog {
	c := &Catalog{}
	c.Replace(version, bundledVersion, managed, extractors, supported)
	return c
}

// Replace swaps the whole inventory after the active binary changed.
func (c *Catalog) Replace(version, bundledVersion string, managed bool, extractors []Extractor, supported map[string]SupportedSite) {
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

// Supported is the parsed supportedsites table by category. Read-only.
func (c *Catalog) Supported() map[string]SupportedSite {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.supported
}
