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

// fill completes an entry from the effective site view.
func (h *Handler) fill(e *SiteEntry) {
	e.Name = h.catalog.Supported()[e.Category].Name
	v := h.siteView(e.Category)
	e.Auth, e.Hosts, e.Kind = v.Auth, v.Hosts, v.Kind
	e.Curated, e.CustomProfile, e.Configured = v.Curated, v.CustomProfile, v.Configured
}

// siteView assembles what the app knows about a category, seeding the auth
// kind from the supportedsites data when no profile carries one.
func (h *Handler) siteView(category string) mapping.SiteView {
	return h.mapper.SiteView(category, h.catalog.Supported()[category].Auth)
}

// listSites handles GET /api/v1/sites?q=. The list is the cached
// --list-extractors result plus each profiled category not already named
// there, filtered by a substring query (category, subcategory, or display
// name) and sorted curated-first then alphabetically.
func (h *Handler) listSites(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	extractors, supported := h.catalog.Extractors(), h.catalog.Supported()
	out := make([]SiteEntry, 0, len(extractors))
	named := make(map[string]bool)
	for _, ex := range extractors {
		if ex.Category == "" {
			// gallery-dl prints no category for a family's base extractors (the
			// donmai and e621 instances among them), so the entry names no site
			// a client could attribute a URL to. The settings search drops them
			// the same way; the family is listed under its curated categories.
			continue
		}
		named[ex.Category] = true
		if q != "" && !mapping.MatchesQuery(q, ex.Category, ex.Subcategory, supported[ex.Category].Name) {
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
		if q != "" && !mapping.MatchesQuery(q, cat, supported[cat].Name) {
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

// boolRank orders true ahead of false in a cmp.Compare chain.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// siteProbe is a probe's answer: gallery-dl's verdict, plus the credential the
// site needs and the config does not carry. A failure with that set is a site
// nobody has signed in to yet, which is a different fix from one whose
// credentials were refused - the same distinction the settings test button
// reports as "needs cookies".
type siteProbe struct {
	gdl.ProbeResult
	MissingCredential string `json:"missing_credential,omitempty"`
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
		apiError(w, http.StatusBadRequest, errInvalidRequest, "url must be an http(s) URL")
		return
	}
	if probeURL == "" {
		probeURL = h.mapper.ExampleURL(name, h.catalog.ExampleFor(name))
	}
	if probeURL == "" {
		apiError(w, http.StatusNotFound, errNotFound, "no example URL for site "+name+"; supply a url")
		return
	}

	res, err := h.runner.Probe(r.Context(), probeURL)
	if err != nil {
		apiError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	out := siteProbe{ProbeResult: res}
	if res.Status == gdl.ProbeOK {
		// Record the success like the web probe does, so the settings "last ok"
		// stays fresh whichever surface ran the test.
		h.siteState.Reached(name, time.Now())
	} else if v := h.siteView(name); v.NeedsCredential {
		out.MissingCredential = v.Login
	}
	writeJSON(w, http.StatusOK, out)
}
