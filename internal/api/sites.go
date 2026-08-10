package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
)

// SiteEntry is one supported site for GET /api/v1/sites. Curated entries
// (shipped or user profile) carry their profile's auth kind and host aliases
// - the example URL names only one host, so the aliases are what let a
// client recognize a mirror like safebooru.donmai.us - and sort to the top;
// the rest seed theirs from the bundled supportedsites data. Configured
// mirrors the settings tables' visibility rule: user-set site data or a
// custom profile.
type SiteEntry struct {
	Category      string   `json:"category"`
	Subcategory   string   `json:"subcategory"`
	Name          string   `json:"name,omitempty"`
	Example       string   `json:"example"`
	Hosts         []string `json:"hosts,omitempty"`
	Kind          string   `json:"kind"`
	Curated       bool     `json:"curated"`
	Configured    bool     `json:"configured"`
	CustomProfile bool     `json:"custom_profile"`
	Auth          string   `json:"auth,omitempty"`
}

// fill completes an entry from the effective profile, the supportedsites
// data, and the site's configured state.
func (h *Handler) fill(e *SiteEntry) {
	e.Name = h.supported[e.Category].Name
	if p, ok := h.mapper.Lookup(e.Category); ok {
		e.Curated = true
		e.Auth = p.Auth
		e.Hosts = p.Hosts
		e.Kind = p.Kind
		if e.Kind == "" {
			e.Kind = mapping.KindBooru
		}
	} else {
		e.Auth = mapping.SeedAuthKind(h.supported[e.Category].Auth)
		e.Kind = mapping.KindOther
	}
	e.CustomProfile = h.mapper.CustomProfile(e.Category)
	site := h.cfg.Current().FindSite(e.Category)
	e.Configured = (site != nil && site.HasUserData()) || e.CustomProfile
}

// listSites handles GET /api/v1/sites?q=. The list is the cached
// --list-extractors result plus each profiled category not already named
// there, filtered by a substring query (category, subcategory, or display
// name) and sorted curated-first then alphabetically.
func (h *Handler) listSites(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := make([]SiteEntry, 0, len(h.extractors))
	named := make(map[string]bool)
	for _, ex := range h.extractors {
		if ex.Category == "" {
			// gallery-dl prints no category for a family's base extractors (the
			// donmai and e621 instances among them), so the entry names no site
			// a client could attribute a URL to. The settings search drops them
			// the same way; the family is listed under its curated categories.
			continue
		}
		named[ex.Category] = true
		if q != "" && !matchesQuery(q, ex.Category, ex.Subcategory, h.supported[ex.Category].Name) {
			continue
		}
		e := SiteEntry{Category: ex.Category, Subcategory: ex.Subcategory, Example: ex.Example}
		h.fill(&e)
		out = append(out, e)
	}
	// gallery-dl lists a multi-instance family (the danbooru family, ...) under a
	// blank category with one example host, so a curated instance like
	// aibooru.online is never named in --list-extractors. Surface each profiled
	// category not already a named extractor under its own category and example,
	// so a client recognizes every supported instance, not just the listed one.
	for _, cat := range h.mapper.CuratedCategories() {
		if named[cat] {
			continue
		}
		if q != "" && !matchesQuery(q, cat, h.supported[cat].Name) {
			continue
		}
		if p, ok := h.mapper.Lookup(cat); ok {
			e := SiteEntry{Category: cat, Example: p.Example}
			h.fill(&e)
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b SiteEntry) int {
		return cmp.Or(
			// A curated entry sorts ahead of an uncurated one.
			cmp.Compare(boolRank(b.Curated), boolRank(a.Curated)),
			cmp.Compare(a.Category, b.Category),
			cmp.Compare(a.Subcategory, b.Subcategory))
	})
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "sites": out})
}

// matchesQuery reports whether the lower-cased query is a substring of any
// field, the same matching rule the settings add-site box applies.
func matchesQuery(q string, fields ...string) bool {
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// boolRank orders true ahead of false in a cmp.Compare chain.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// testSite handles POST /api/v1/sites/{name}/test: run a live probe against
// the site's example URL (or a caller-supplied url) and report ok / auth /
// failed.
func (h *Handler) testSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	probeURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if probeURL == "" && r.Body != nil {
		var body struct {
			URL string `json:"url"`
		}
		if decodeBody(w, r, &body) == nil {
			probeURL = strings.TrimSpace(body.URL)
		}
	}
	if probeURL != "" && !config.IsHTTPURL(probeURL) {
		// The probe URL becomes a gallery-dl argument, where a leading dash
		// would read as an option rather than a URL.
		apiError(w, http.StatusBadRequest, "invalid_request", "url must be an http(s) URL")
		return
	}
	if probeURL == "" {
		probeURL = h.mapper.ExampleURL(h.extractors, name)
	}
	if probeURL == "" {
		apiError(w, http.StatusNotFound, "not_found", "no example URL for site "+name+"; supply a url")
		return
	}

	res, err := h.runner.Probe(r.Context(), probeURL)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Record the success like the web probe does, so the settings "last ok"
	// stays fresh whichever surface ran the test.
	if res.Status == gdl.ProbeOK {
		h.siteState.Reached(name, time.Now())
	}
	writeJSON(w, http.StatusOK, res)
}
