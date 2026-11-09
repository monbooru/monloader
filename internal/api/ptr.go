package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/contrib"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/contract"
)

// Per range, and it caps the rows answered, not the walk: a substring query
// still scans its namespace.
const tagSearchScanCap = 200

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// No ptrReady gate: monbooru's capability check needs an answer in every state.
func (h *Handler) ptrStatus(w http.ResponseWriter, r *http.Request) {
	contract.WriteJSON(w, http.StatusOK, h.ptr.Status())
}

// Not caught up is refused too: a half-built index would hand over a
// partial tag set the caller mirrors as the whole answer.
func (h *Handler) ptrReady(w http.ResponseWriter) bool {
	if !h.contribGate(w) {
		return false
	}
	if !h.ptr.CaughtUp() {
		contract.WriteError(w, http.StatusConflict, errPTRSyncing, "the ptr index is not fully synced yet")
		return false
	}
	return true
}

func (h *Handler) ptrTags(w http.ResponseWriter, r *http.Request) {
	if !h.ptrReady(w) {
		return
	}
	var body contract.PTRTagsRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	if len(body.Tags) == 0 {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "tags must not be empty")
		return
	}
	if len(body.Tags) > contract.MaxPTRTags {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "too many tags in one request")
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
		contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}

	results := make(map[string]contract.PTRTagInfo, len(body.Tags))
	for _, t := range body.Tags {
		results[t] = resolveTagInfo(t, candidates[t], graph)
	}
	contract.WriteJSON(w, http.StatusOK, contract.PTRTagsResponse{Results: results})
}

func (h *Handler) ptrTagSearch(w http.ResponseWriter, r *http.Request) {
	if !h.ptrReady(w) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "q must not be empty")
		return
	}
	limit := contract.PTRSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > contract.MaxPTRSearchLimit {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "limit must be 1.."+strconv.Itoa(contract.MaxPTRSearchLimit))
			return
		}
		limit = n
	}
	mode := r.URL.Query().Get("mode")
	if mode != "" && mode != "prefix" && mode != contract.SearchContains {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "mode must be prefix or contains")
		return
	}
	ranges, ok := tagSearchRanges(q, mode == contract.SearchContains)
	if !ok {
		contract.WriteError(w, http.StatusBadRequest, errNamespaceReq,
			"matching anywhere in the name needs a category: prefix to bound the scan")
		return
	}

	matches, truncated, err := h.ptr.SearchTags(r.Context(), ranges, tagSearchScanCap)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	clusters, more := renderTagClusters(q, matches, limit)
	contract.WriteJSON(w, http.StatusOK, contract.PTRSearchResponse{Clusters: clusters, Truncated: truncated || more})
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
func renderTagClusters(queried string, matches []ptr.TagMatch, limit int) (rows []contract.PTRCluster, more bool) {
	out := make([]contract.PTRCluster, 0, len(matches))
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
	slices.SortStableFunc(out, func(a, b contract.PTRCluster) int {
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

func relations(c contract.PTRCluster) int {
	return c.Aliases + c.Implications + c.ImpliedBy
}

func mergeTagCluster(into, from contract.PTRCluster) contract.PTRCluster {
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

func renderTagCluster(m ptr.TagMatch) (contract.PTRCluster, bool) {
	ideal := mapping.MapPTRTag(m.Ideal)
	if ideal == "" {
		return contract.PTRCluster{}, false
	}
	c := contract.PTRCluster{Ideal: ideal, Matched: []string{}, Aliases: m.Aliases, Implications: m.Implications, ImpliedBy: m.ImpliedBy}
	for _, raw := range m.Matched {
		if name := mapping.MapPTRTag(raw); name != "" && !slices.Contains(c.Matched, name) {
			c.Matched = append(c.Matched, name)
		}
	}
	return c, true
}

func (h *Handler) ptrLookup(w http.ResponseWriter, r *http.Request) {
	if !h.ptrReady(w) {
		return
	}
	var body contract.PTRLookupRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	if len(body.Images) == 0 {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "images must not be empty")
		return
	}
	if len(body.Images) > contract.MaxPTRHashes {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "too many images in one request")
		return
	}

	results, withheld := map[string][]string{}, map[string][]string{}
	var matched []queue.Item
	for _, img := range body.Images {
		if r.Context().Err() != nil {
			return
		}
		if img.ImageID <= 0 {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "each image_id is required")
			return
		}
		if !config.IsHexHash(img.SHA256, 64) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "each sha256 must be 64 hex characters")
			return
		}
		raw, ok, err := h.ptr.TagsForHash(strings.ToLower(img.SHA256))
		if err != nil {
			contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
			return
		}
		if !ok {
			continue
		}
		if _, skip := h.mapper.PTRBlacklisted(raw); skip {
			continue
		}
		tags, held := h.mapper.MapPTRWithheld(raw)
		if len(tags) == 0 {
			continue
		}
		results[img.SHA256] = tags
		if len(held) > 0 {
			withheld[img.SHA256] = held
		}
		matched = append(matched, queue.Item{
			PostID:     fmt.Sprintf("%d tag%s", len(tags), plural(len(tags))),
			Status:     queue.ItemDone,
			Outcome:    outcome.Matched,
			MonbooruID: img.ImageID,
		})
	}
	h.queue.RecordPTRLookup(body.Gallery, body.Scheduled, len(body.Images), matched)
	contract.WriteJSON(w, http.StatusOK, contract.PTRLookupResponse{
		Index:              h.ptr.Status().Progress.UpdateIndex,
		Results:            results,
		WithheldTags:       withheld,
		WithheldCategories: h.cfg.Current().Tags.DropCategories,
	})
}

// A verbatim spelling can sit in the index as an orphan while the
// space-joined form carries the cluster, so a connected candidate beats
// an earlier bare hit.
func resolveTagInfo(queried string, candidates []string, graph map[string]ptr.TagInfo) contract.PTRTagInfo {
	var fallback *contract.PTRTagInfo
	for _, c := range candidates {
		info, ok := graph[c]
		if !ok || !info.Known {
			continue
		}
		rendered := renderTagInfo(queried, info)
		if tagInfoEmpty(rendered) {
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
	return contract.PTRTagInfo{Known: false}
}

// An answer that projects to nothing counts as unknown: known would
// promise a graph the caller then finds empty.
func tagInfoEmpty(t contract.PTRTagInfo) bool {
	return t.Ideal == "" && len(t.Aliases)+len(t.Implications)+len(t.ImpliedBy) == 0
}

func renderTagInfo(queried string, info ptr.TagInfo) contract.PTRTagInfo {
	return contract.PTRTagInfo{
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
	if !h.ptr.Enabled() {
		contract.WriteError(w, http.StatusConflict, errPTRUnavailable, "the ptr backend is not enabled")
		return
	}
	if h.ptr.HasPersonalKey() {
		contract.WriteError(w, http.StatusConflict, errAccountExists, "a personal account key is already set")
		return
	}
	failure := contrib.CreateAccount(r.Context(), h.ptr, func(key string) error {
		return h.cfg.Update(func(c *config.Config) error {
			c.PTR.AccessKey = key
			return nil
		})
	})
	if failure != nil {
		switch failure.Kind {
		case contrib.AccountVelocity:
			contract.WriteError(w, http.StatusServiceUnavailable, errPTRVelocity, failure.Msg)
		case contrib.AccountExists:
			contract.WriteError(w, http.StatusConflict, errAccountExists, failure.Msg)
		case contrib.AccountNotSaved:
			contract.WriteError(w, http.StatusInternalServerError, errSaveFailed, failure.Msg)
		default:
			contract.WriteError(w, http.StatusBadGateway, errPTRError, failure.Msg)
		}
		return
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"created": true})
}
