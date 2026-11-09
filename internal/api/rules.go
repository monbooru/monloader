package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/contract"
)

// maxRulesNames bounds one batched call: a tag page asks for its name and
// its aliases.
const maxRulesNames = 50

type tagRuleRow struct {
	mapping.TagRule
	Kind    string `json:"kind"`
	Builtin bool   `json:"builtin,omitempty"`
}

type tagRulesResponse struct {
	mapping.TagRules
	Sites []tagRuleRow `json:"sites"`
}

func (h *Handler) tagRules(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("tag"))
	if name == "" {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "tag is required")
		return
	}
	sites, builtin := h.rulesSites()
	contract.WriteJSON(w, http.StatusOK, h.rulesFor(sites, builtin, r.URL.Query().Get("category"), name))
}

func (h *Handler) tagRulesBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Names []struct {
			Tag      string `json:"tag"`
			Category string `json:"category"`
		} `json:"names"`
	}
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	if len(body.Names) == 0 || len(body.Names) > maxRulesNames {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, fmt.Sprintf("names must hold 1 to %d tags", maxRulesNames))
		return
	}
	sites, builtin := h.rulesSites()
	results := make([]tagRulesResponse, 0, len(body.Names))
	for _, n := range body.Names {
		name := strings.TrimSpace(n.Tag)
		if name == "" {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "every name needs a tag")
			return
		}
		results = append(results, h.rulesFor(sites, builtin, n.Category, name))
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

// The sites listed are the ones the settings tables show and the ones the
// history and the watches reached, then the PTR.
func (h *Handler) rulesSites() ([]string, map[string]bool) {
	cfg := h.cfg.Current()
	names := map[string]bool{}
	for _, site := range cfg.Sites {
		if site.HasUserData() {
			names[site.Name] = true
		}
	}
	for _, cat := range h.mapper.CustomCategories() {
		names[cat] = true
	}
	builtin := map[string]bool{}
	jobs, _ := h.queue.List(queue.ListOptions{OmitItems: true})
	for _, j := range jobs {
		if !names[j.Site] {
			builtin[j.Site] = true
		}
	}
	for _, wt := range h.queue.Watches() {
		if !names[wt.Last.Site] {
			builtin[wt.Last.Site] = true
		}
	}
	delete(builtin, "")
	for n := range builtin {
		names[n] = true
	}
	delete(names, config.SitePTR)
	sites := slices.Sorted(func(yield func(string) bool) {
		for n := range names {
			if !yield(n) {
				return
			}
		}
	})
	return append(sites, config.SitePTR), builtin
}

func (h *Handler) rulesFor(sites []string, builtin map[string]bool, category, name string) tagRulesResponse {
	category = strings.TrimSpace(category)
	if category == "" {
		category = "general"
	}
	rules := h.mapper.TagRules(category, name, sites)
	resp := tagRulesResponse{TagRules: rules, Sites: make([]tagRuleRow, 0, len(rules.Sites))}
	for _, s := range rules.Sites {
		kind := "lookup"
		if s.Site != config.SitePTR {
			kind = h.siteView(s.Site).Kind
		}
		resp.Sites = append(resp.Sites, tagRuleRow{TagRule: s, Kind: kind, Builtin: builtin[s.Site]})
	}
	return resp
}
