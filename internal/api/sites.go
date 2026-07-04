package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/leqwin/monloader/internal/gdl"
)

// SiteEntry is one supported site for GET /api/v1/sites. Curated entries carry
// their profile's auth kind and sort to the top.
type SiteEntry struct {
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	Example     string `json:"example"`
	Curated     bool   `json:"curated"`
	Auth        string `json:"auth,omitempty"`
}

// listSites handles GET /api/v1/sites?q=. The list is the cached
// --list-extractors result plus each curated profile not already named there,
// filtered by a substring query and sorted curated-first then alphabetically.
func (h *Handler) listSites(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := make([]SiteEntry, 0, len(h.extractors))
	named := make(map[string]bool)
	for _, ex := range h.extractors {
		if ex.Category != "" {
			named[ex.Category] = true
		}
		if q != "" && !strings.Contains(strings.ToLower(ex.Category), q) &&
			!strings.Contains(strings.ToLower(ex.Subcategory), q) {
			continue
		}
		e := SiteEntry{Category: ex.Category, Subcategory: ex.Subcategory, Example: ex.Example}
		if p, ok := h.mapper.Lookup(ex.Category); ok {
			e.Curated = true
			e.Auth = p.Auth
		}
		out = append(out, e)
	}
	// gallery-dl lists a multi-instance family (the danbooru family, ...) under a
	// blank category with one example host, so a curated instance like
	// aibooru.online is never named in --list-extractors. Surface each curated
	// profile not already a named extractor under its own category and example,
	// so a client recognizes every supported instance, not just the listed one.
	for _, cat := range h.mapper.CuratedCategories() {
		if named[cat] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(cat), q) {
			continue
		}
		if p, ok := h.mapper.Lookup(cat); ok {
			out = append(out, SiteEntry{Category: cat, Example: p.Example, Curated: true, Auth: p.Auth})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Curated != out[j].Curated {
			return out[i].Curated // curated first
		}
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Subcategory < out[j].Subcategory
	})
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "sites": out})
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
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			probeURL = strings.TrimSpace(body.URL)
		}
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
