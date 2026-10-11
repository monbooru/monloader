package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
)

const maxTagGraphBatch = 500

// Lower than the tag batch: one hit answers a whole tag list, not one
// tag's edges.
const maxHashLookupBatch = 100

// The scan cap is per range and caps the rows it answers with, not the
// walk: a substring query still scans its namespace.
const (
	defaultTagSearchLimit = 20
	maxTagSearchLimit     = 50
	tagSearchScanCap      = 200
)

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// No ptrReady gate: monbooru's capability check needs an answer in every state.
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

type ptrTagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Implications []string `json:"implications,omitempty"`
	ImpliedBy    []string `json:"implied_by,omitempty"`
}

// Not caught up is refused too: a half-built index would hand over a
// partial tag set the caller mirrors as the whole answer.
func (h *Handler) ptrReady(w http.ResponseWriter) bool {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, errPTRUnavailable, "the ptr index is not available")
		return false
	}
	if !h.ptr.CaughtUp() {
		apiError(w, http.StatusConflict, errPTRSyncing, "the ptr index is not fully synced yet")
		return false
	}
	return true
}

func (h *Handler) ptrTags(w http.ResponseWriter, r *http.Request) {
	if !h.ptrReady(w) {
		return
	}
	var body ptrTagsRequest
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "invalid JSON body")
		return
	}
	if len(body.Tags) == 0 {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "tags must not be empty")
		return
	}
	if len(body.Tags) > maxTagGraphBatch {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "too many tags in one request")
		return
	}

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
			return
		}
		apiError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}

	results := make(map[string]ptrTagInfo, len(body.Tags))
	for _, t := range body.Tags {
		results[t] = resolveTagInfo(t, candidates[t], graph)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

type ptrTagCluster struct {
	Ideal        string   `json:"ideal"`
	Matched      []string `json:"matched"`
	Aliases      int      `json:"aliases"`
	Implications int      `json:"implications"`
	ImpliedBy    int      `json:"implied_by"`
}

func (h *Handler) ptrTagSearch(w http.ResponseWriter, r *http.Request) {
	if !h.ptrReady(w) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "q must not be empty")
		return
	}
	limit := defaultTagSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxTagSearchLimit {
			apiError(w, http.StatusBadRequest, errInvalidRequest, "limit must be 1.."+strconv.Itoa(maxTagSearchLimit))
			return
		}
		limit = n
	}
	mode := r.URL.Query().Get("mode")
	if mode != "" && mode != "prefix" && mode != "contains" {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "mode must be prefix or contains")
		return
	}
	ranges, ok := tagSearchRanges(q, mode == "contains")
	if !ok {
		apiError(w, http.StatusBadRequest, errNamespaceReq,
			"matching anywhere in the name needs a category: prefix to bound the scan")
		return
	}

	matches, truncated, err := h.ptr.SearchTags(r.Context(), ranges, tagSearchScanCap)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		apiError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	clusters, more := renderTagClusters(q, matches, limit)
	writeJSON(w, http.StatusOK, map[string]any{"clusters": clusters, "truncated": truncated || more})
}

// A substring query with no namespace is refused: the whole tag table is
// too big to walk.
func tagSearchRanges(q string, contains bool) ([]ptr.TagRange, bool) {
	spellings := mapping.PTRSpellingsFor(q)
	if !contains {
		ranges := make([]ptr.TagRange, 0, len(spellings))
		for _, sp := range spellings {
			ranges = append(ranges, ptr.TagRange{Prefix: sp.Full})
		}
		return ranges, true
	}
	var ranges []ptr.TagRange
	at := map[string]int{}
	for _, sp := range spellings {
		if sp.Namespace == "" {
			return nil, false
		}
		i, ok := at[sp.Namespace]
		if !ok {
			i = len(ranges)
			at[sp.Namespace] = i
			ranges = append(ranges, ptr.TagRange{Prefix: sp.Namespace})
		}
		ranges[i].Contains = append(ranges[i].Contains, sp.Subtag)
	}
	return ranges, len(ranges) > 0
}

// Clusters that carry relations rank first: that floats the real cluster
// above near-misses sharing its stem.
func renderTagClusters(queried string, matches []ptr.TagMatch, limit int) (rows []ptrTagCluster, more bool) {
	out := make([]ptrTagCluster, 0, len(matches))
	at := map[string]int{}
	for _, m := range matches {
		c, ok := renderTagCluster(m)
		if !ok {
			continue
		}
		// Raw ideals differing only by the underscore-space fold project
		// to one monbooru name, and the caller could not tell two rows
		// under it apart.
		if i, seen := at[c.Ideal]; seen {
			out[i] = mergeTagCluster(out[i], c)
			continue
		}
		at[c.Ideal] = len(out)
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b ptrTagCluster) int {
		return cmp.Or(
			cmp.Compare(boolRank(b.Ideal == queried), boolRank(a.Ideal == queried)),
			cmp.Compare(boolRank(relations(b) > 0), boolRank(relations(a) > 0)),
			cmp.Compare(len(a.Ideal), len(b.Ideal)),
			cmp.Compare(a.Ideal, b.Ideal))
	})
	if len(out) > limit {
		return out[:limit], true
	}
	return out, false
}

func relations(c ptrTagCluster) int {
	return c.Aliases + c.Implications + c.ImpliedBy
}

func mergeTagCluster(into, from ptrTagCluster) ptrTagCluster {
	for _, n := range from.Matched {
		if !slices.Contains(into.Matched, n) {
			into.Matched = append(into.Matched, n)
		}
	}
	into.Aliases = max(into.Aliases, from.Aliases)
	into.Implications = max(into.Implications, from.Implications)
	into.ImpliedBy = max(into.ImpliedBy, from.ImpliedBy)
	return into
}

func renderTagCluster(m ptr.TagMatch) (ptrTagCluster, bool) {
	ideal := mapping.MapPTRTag(m.Ideal)
	if ideal == "" {
		return ptrTagCluster{}, false
	}
	c := ptrTagCluster{Ideal: ideal, Matched: []string{}, Aliases: m.Aliases, Implications: m.Implications, ImpliedBy: m.ImpliedBy}
	for _, raw := range m.Matched {
		if name := mapping.MapPTRTag(raw); name != "" && !slices.Contains(c.Matched, name) {
			c.Matched = append(c.Matched, name)
		}
	}
	return c, true
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

func (h *Handler) ptrLookup(w http.ResponseWriter, r *http.Request) {
	if !h.ptrReady(w) {
		return
	}
	var body ptrLookupRequest
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "invalid JSON body")
		return
	}
	if len(body.Images) == 0 {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "images must not be empty")
		return
	}
	if len(body.Images) > maxHashLookupBatch {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "too many images in one request")
		return
	}

	results := map[string][]string{}
	var matched []queue.Item
	for _, img := range body.Images {
		if r.Context().Err() != nil {
			return
		}
		if img.ImageID <= 0 {
			apiError(w, http.StatusBadRequest, errInvalidRequest, "each image_id is required")
			return
		}
		if !config.IsHexHash(img.SHA256, 64) {
			apiError(w, http.StatusBadRequest, errInvalidRequest, "each sha256 must be 64 hex characters")
			return
		}
		raw, ok, err := h.ptr.TagsForHash(strings.ToLower(img.SHA256))
		if err != nil {
			apiError(w, http.StatusInternalServerError, errInternal, err.Error())
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
			Outcome:    outcome.Matched,
			MonbooruID: img.ImageID,
		})
	}
	h.queue.RecordPTRLookup(body.Gallery, body.Scheduled, len(body.Images), matched)
	writeJSON(w, http.StatusOK, map[string]any{
		"index":   h.ptr.Status().Progress.UpdateIndex,
		"results": results,
	})
}

// A verbatim spelling can sit in the index as an orphan while the
// space-joined form carries the cluster, so a connected candidate beats
// an earlier bare hit.
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

// An answer that projects to nothing counts as unknown: known would
// promise a graph the caller then finds empty.
func (t ptrTagInfo) empty() bool {
	return t.Ideal == "" && len(t.Aliases)+len(t.Implications)+len(t.ImpliedBy) == 0
}

func renderTagInfo(queried string, info ptr.TagInfo) ptrTagInfo {
	return ptrTagInfo{
		Known:        true,
		Ideal:        mapping.MapPTRTag(info.Ideal),
		Aliases:      dropTag(mapping.MapPTRTags(info.Aliases), queried),
		Implications: dropTag(mapping.MapPTRTags(info.Implications), queried),
		ImpliedBy:    dropTag(mapping.MapPTRTags(info.ImpliedBy), queried),
	}
}

func dropTag(tags []string, queried string) []string {
	return slices.DeleteFunc(tags, func(t string) bool { return t == queried })
}

// The repository's 400 here is its account-creation velocity limit, not a
// malformed request.
func (h *Handler) ptrAccountCreate(w http.ResponseWriter, r *http.Request) {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, errPTRUnavailable, "the ptr backend is not enabled")
		return
	}
	if h.ptr.HasPersonalKey() {
		apiError(w, http.StatusConflict, errAccountExists, "a personal account key is already set")
		return
	}
	if h.host.SaveConfig == nil {
		apiError(w, http.StatusInternalServerError, errSaveFailed, "settings cannot be saved here")
		return
	}
	key, err := h.ptr.CreateContribAccount(r.Context())
	if err != nil {
		var serr *ptr.ServerError
		switch {
		case errors.As(err, &serr) && serr.Status == 400:
			apiError(w, http.StatusServiceUnavailable, errPTRVelocity,
				"the repository is not taking new accounts right now, try again later")
		case errors.Is(err, ptr.ErrAccountExists):
			apiError(w, http.StatusConflict, errAccountExists, err.Error())
		default:
			apiError(w, http.StatusBadGateway, errPTRError, err.Error())
		}
		return
	}
	if err := h.host.SaveConfig(func(c *config.Config) error {
		c.PTR.AccessKey = key
		return nil
	}); err != nil {
		// The redemption is single-use, so the key now exists only in
		// memory.
		apiError(w, http.StatusInternalServerError, errSaveFailed,
			"account created but saving the key failed: "+err.Error()+
				" - reveal and back it up from the ptr page before restarting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": true})
}
