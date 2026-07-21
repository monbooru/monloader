package api

import (
	"errors"
	"net/http"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/ptr"
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
	ImpliedBy    []string `json:"implied_by,omitempty"`
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
	if err := decodeBody(w, r, &body); err != nil {
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
		results[t] = resolveTagInfo(t, candidates[t], graph)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveTagInfo picks the candidate spelling for a monbooru tag whose graph
// answer carries relations and renders it back into monbooru form. A verbatim
// spelling can sit in the index as an orphan while the space-joined form
// carries the cluster, so a connected candidate wins over an earlier bare
// hit; a tag only known as an orphan still answers known. The queried tag and
// any unrepresentable spelling are dropped from its relation lists, so a tag
// never reads as its own alias or implication and no empty entry slips
// through.
func resolveTagInfo(queried string, candidates []string, graph map[string]ptr.TagInfo) ptrTagInfo {
	var fallback *ptrTagInfo
	for _, c := range candidates {
		info, ok := graph[c]
		if !ok || !info.Known {
			continue
		}
		rendered := renderTagInfo(queried, info)
		if rendered.empty() {
			continue
		}
		if info.Ideal != c || len(info.Aliases)+len(info.Implications)+len(info.ImpliedBy) > 0 {
			return rendered
		}
		if fallback == nil {
			fallback = &rendered
		}
	}
	if fallback != nil {
		return *fallback
	}
	return ptrTagInfo{Known: false}
}

// empty reports an answer that projects to nothing monbooru can hold: the
// spelling's ideal and every relation sit in namespaces the router drops
// (title:, booru:, ...). Answering known for one promises a graph the caller
// then finds nothing in, so it counts as no answer.
func (t ptrTagInfo) empty() bool {
	return t.Ideal == "" && len(t.Aliases)+len(t.Implications)+len(t.ImpliedBy) == 0
}

// renderTagInfo maps one raw graph answer into monbooru form.
func renderTagInfo(queried string, info ptr.TagInfo) ptrTagInfo {
	return ptrTagInfo{
		Known:        true,
		Ideal:        mapping.MapPTRTag(info.Ideal),
		Aliases:      dropTag(mapping.MapPTRTags(info.Aliases), queried),
		Implications: dropTag(mapping.MapPTRTags(info.Implications), queried),
		ImpliedBy:    dropTag(mapping.MapPTRTags(info.ImpliedBy), queried),
	}
}

// dropTag removes empty entries and the queried tag from a projected list, so
// a tag never reads as its own alias or implication.
func dropTag(tags []string, queried string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t == "" || t == queried {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ptrAccountCreate handles POST /api/v1/ptr/account: the one-time
// auto-creation of the personal contribution account. The velocity
// refusal (the repository's 400) re-maps to 503 with a retry-later
// message so callers need not special-case a PTR status code.
func (h *Handler) ptrAccountCreate(w http.ResponseWriter, r *http.Request) {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the ptr backend is not enabled")
		return
	}
	if h.ptr.HasPersonalKey() {
		apiError(w, http.StatusConflict, "account_exists", "a personal account key is already set")
		return
	}
	key, err := h.ptr.CreateContribAccount(r.Context())
	if err != nil {
		var serr *ptr.ServerError
		switch {
		case errors.As(err, &serr) && serr.Status == 400:
			apiError(w, http.StatusServiceUnavailable, "ptr_velocity",
				"the repository is not taking new accounts right now, try again later")
		case errors.Is(err, ptr.ErrAccountExists):
			apiError(w, http.StatusConflict, "account_exists", err.Error())
		default:
			apiError(w, http.StatusBadGateway, "ptr_error", err.Error())
		}
		return
	}
	if h.saveConfig != nil {
		if err := h.saveConfig(func(c *config.Config) error {
			c.PTR.AccessKey = key
			return nil
		}); err != nil {
			// The redemption is single-use, so the key exists only in memory
			// now; point at the reveal control the way the web path does.
			apiError(w, http.StatusInternalServerError, "save_failed",
				"account created but saving the key failed: "+err.Error()+
					" - reveal and back it up from the ptr page before restarting")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": true})
}
