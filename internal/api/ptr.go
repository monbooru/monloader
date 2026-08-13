package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
)

// maxTagGraphBatch caps a single tag-graph query so a sweep pages its tag list
// rather than asking for everything at once.
const maxTagGraphBatch = 500

// maxHashLookupBatch caps a single batch hash lookup. Lower than the tag
// batch: one hit answers a whole tag list rather than one tag's edges.
const maxHashLookupBatch = 100

// plural is the "s" a count needs.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

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
	if !h.ptr.CaughtUp() {
		// A half-built index would hand over a graph missing edges the caller
		// then mirrors as the repository's whole answer.
		apiError(w, http.StatusConflict, "ptr_syncing", "the ptr index is not fully synced yet")
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
	graph, err := h.ptr.TagGraph(r.Context(), all)
	if err != nil {
		if r.Context().Err() != nil {
			// The caller gave up mid-walk; there is nobody left to answer.
			return
		}
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	results := make(map[string]ptrTagInfo, len(body.Tags))
	for _, t := range body.Tags {
		results[t] = resolveTagInfo(t, candidates[t], graph)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

type ptrLookupImage struct {
	ImageID int64  `json:"image_id"`
	SHA256  string `json:"sha256"`
}

type ptrLookupRequest struct {
	Images    []ptrLookupImage `json:"images"`
	Gallery   string           `json:"gallery"`
	Scheduled bool             `json:"scheduled"`
}

// ptrLookup handles POST /api/v1/ptr/lookup: the tags the index holds for a
// batch of files, mapped into monbooru form and keyed by sha256. A
// whole-library sweep costs one call per hundred images here instead of a
// queue job per image, and the answer is the outcome - no enrich callback to
// lose. Misses are absent from the results rather than present as empty
// lists. index is the applied-update cursor at answer time, so the caller can
// tell "the index has not moved since I last asked" from "it has, ask again".
// Each image carries its monbooru id so the history row can name what it
// matched.
func (h *Handler) ptrLookup(w http.ResponseWriter, r *http.Request) {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the ptr index is not available")
		return
	}
	if !h.ptr.CaughtUp() {
		apiError(w, http.StatusConflict, "ptr_syncing", "the ptr index is not fully synced yet")
		return
	}
	var body ptrLookupRequest
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if len(body.Images) == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "images must not be empty")
		return
	}
	if len(body.Images) > maxHashLookupBatch {
		apiError(w, http.StatusBadRequest, "invalid_request", "too many images in one request")
		return
	}

	results := map[string][]string{}
	var matched []queue.Item
	for _, img := range body.Images {
		if r.Context().Err() != nil {
			// A caller that gave up should not leave the rest of its batch
			// walking the index behind it.
			return
		}
		if img.ImageID <= 0 {
			apiError(w, http.StatusBadRequest, "invalid_request", "each image_id is required")
			return
		}
		if !config.IsHexHash(img.SHA256, 64) {
			apiError(w, http.StatusBadRequest, "invalid_request", "each sha256 must be 64 hex characters")
			return
		}
		raw, ok, err := h.ptr.TagsForHash(strings.ToLower(img.SHA256))
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if !ok {
			continue
		}
		tags := mapping.MapPTRTags(raw)
		results[img.SHA256] = tags
		matched = append(matched, queue.Item{
			PostID:     fmt.Sprintf("%d tag%s", len(tags), plural(len(tags))),
			Status:     queue.ItemDone,
			Outcome:    queue.OutcomeMatched,
			MonbooruID: img.ImageID,
		})
	}
	h.queue.RecordPTRLookup(body.Gallery, body.Scheduled, len(body.Images), matched)
	writeJSON(w, http.StatusOK, map[string]any{
		"index":   h.ptr.Status().Progress.UpdateIndex,
		"results": results,
	})
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
