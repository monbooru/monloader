package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/shared/contract"
)

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

func (h *Handler) fill(e *SiteEntry) {
	e.Name = h.catalog.Supported()[e.Category].Name
	v := h.siteView(e.Category)
	e.Auth, e.Hosts, e.Kind = v.Auth, v.Hosts, v.Kind
	e.Curated, e.CustomProfile, e.Configured = v.Curated, v.CustomProfile, v.Configured
}

func (h *Handler) siteView(category string) mapping.SiteView {
	return h.mapper.SiteView(category, h.catalog.Supported()[category].Auth)
}

func (h *Handler) listSites(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	extractors, supported := h.catalog.Extractors(), h.catalog.Supported()
	out := make([]SiteEntry, 0, len(extractors))
	named := make(map[string]bool)
	for _, ex := range extractors {
		if ex.Category == "" {
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
	// --list-extractors lists a multi-instance family under a blank
	// category with one example host, so a curated instance like
	// aibooru.online appears only through its profile.
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
			cmp.Compare(boolRank(b.Curated), boolRank(a.Curated)),
			cmp.Compare(a.Category, b.Category),
			cmp.Compare(a.Subcategory, b.Subcategory))
	})
	contract.WriteJSON(w, http.StatusOK, map[string]any{"total": len(out), "sites": out})
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

type siteProbe struct {
	gdl.ProbeResult
	MissingCredential string `json:"missing_credential,omitempty"`
}

func (h *Handler) testSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	probeURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if probeURL == "" && r.Body != nil {
		var body struct {
			URL string `json:"url"`
		}
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&body)
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			contract.RefuseJSONBody(w, err, maxRequestBody)
			return
		}
		if err == nil {
			probeURL = strings.TrimSpace(body.URL)
		}
	}
	if probeURL != "" && !config.IsHTTPURL(probeURL) {
		// It becomes a gallery-dl argument, where a leading dash would
		// read as an option.
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "url must be an http(s) URL")
		return
	}
	if probeURL == "" {
		probeURL = h.mapper.ExampleURL(name, h.catalog.ExampleFor(name))
	}
	if probeURL == "" {
		contract.WriteError(w, http.StatusNotFound, errNotFound, "no example URL for site "+name+"; supply a url")
		return
	}

	res, err := h.runner.Probe(r.Context(), probeURL)
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	out := siteProbe{ProbeResult: res}
	if res.Status == gdl.ProbeOK {
		h.siteState.Reached(name, time.Now())
	} else if v := h.siteView(name); v.NeedsCredential {
		out.MissingCredential = v.Login
	}
	contract.WriteJSON(w, http.StatusOK, out)
}
