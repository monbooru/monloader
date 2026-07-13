package api

import (
	"encoding/json"
	"net/http"

	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/ptr"
)

// maxTagGraphBatch caps a single tag-graph query so a sweep pages its tag list
// rather than asking for everything at once.
const maxTagGraphBatch = 500

// ptrStatus handles GET /api/v1/ptr/status. It always answers, so monbooru's
// capability check is one unconditional GET; a run without the PTR built in
// reports disabled.
func (h *Handler) ptrStatus(w http.ResponseWriter, r *http.Request) {
	if h.ptr == nil {
		writeJSON(w, http.StatusOK, ptr.Status{State: ptr.StateDisabled})
		return
	}
	writeJSON(w, http.StatusOK, h.ptr.Status())
}

type ptrTagsRequest struct {
	Tags []string `json:"tags"`
}

// ptrTagInfo is one tag's answer in monbooru's own tag vocabulary.
type ptrTagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Implications []string `json:"implications,omitempty"`
}

// ptrTags handles POST /api/v1/ptr/tags: for each monbooru-form tag it returns
// the PTR's ideal spelling, aliases, and implications, mapped back to monbooru
// form. monbooru sweeps its tag list through this to propose aliases and
// implications.
func (h *Handler) ptrTags(w http.ResponseWriter, r *http.Request) {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the ptr index is not available")
		return
	}
	var body ptrTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if len(body.Tags) == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "tags must not be empty")
		return
	}
	if len(body.Tags) > maxTagGraphBatch {
		apiError(w, http.StatusBadRequest, "invalid_request", "too many tags in one request")
		return
	}

	// Each monbooru tag maps to the hydrus spellings it could have come from; the
	// union is queried once, then each tag reads back its own candidates.
	candidates := map[string][]string{}
	var all []string
	for _, t := range body.Tags {
		cands := mapping.PTRNamespacesFor(t)
		candidates[t] = cands
		all = append(all, cands...)
	}
	graph, err := h.ptr.TagGraph(all)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	results := make(map[string]ptrTagInfo, len(body.Tags))
	for _, t := range body.Tags {
		results[t] = resolveTagInfo(candidates[t], graph)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveTagInfo picks the first known candidate spelling for a monbooru tag and
// renders its graph answer back into monbooru form.
func resolveTagInfo(candidates []string, graph map[string]ptr.TagInfo) ptrTagInfo {
	for _, c := range candidates {
		info, ok := graph[c]
		if !ok || !info.Known {
			continue
		}
		return ptrTagInfo{
			Known:        true,
			Ideal:        mapping.MapPTRTag(info.Ideal),
			Aliases:      mapping.MapPTRTags(info.Aliases),
			Implications: mapping.MapPTRTags(info.Implications),
		}
	}
	return ptrTagInfo{Known: false}
}
