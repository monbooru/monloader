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
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
)

// maxTagGraphBatch caps a single tag-graph query so a sweep pages its tag list
// rather than asking for everything at once.
const maxTagGraphBatch = 500

// maxHashLookupBatch caps a single batch hash lookup. Lower than the tag
// batch: one hit answers a whole tag list rather than one tag's edges.
const maxHashLookupBatch = 100

// Spelling-search bounds. The scan cap is per range and bounds the rows a range
// answers with - a substring query still walks its namespace to find them - and
// the limit bounds what a dropdown has to render.
const (
	defaultTagSearchLimit = 20
	maxTagSearchLimit     = 50
	tagSearchScanCap      = 200
)

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

// ptrTagCluster is one sibling cluster a spelling search found, in monbooru
// form. The counts describe the cluster's graph so a caller can rank and label
// the list without a query per row.
type ptrTagCluster struct {
	Ideal        string   `json:"ideal"`
	Matched      []string `json:"matched"`
	Aliases      int      `json:"aliases"`
	Implications int      `json:"implications"`
	ImpliedBy    int      `json:"implied_by"`
}

// ptrTagSearch handles GET /api/v1/ptr/tags/search: the spellings the index
// holds under a monbooru-form prefix, folded to one row per sibling cluster.
// The graph query answers about a name the caller already knows; this one is
// how the caller finds out what to ask about, which is otherwise a guess at
// the repository's exact spelling.
func (h *Handler) ptrTagSearch(w http.ResponseWriter, r *http.Request) {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the ptr index is not available")
		return
	}
	if !h.ptr.CaughtUp() {
		apiError(w, http.StatusConflict, "ptr_syncing", "the ptr index is not fully synced yet")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "q must not be empty")
		return
	}
	limit := defaultTagSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxTagSearchLimit {
			apiError(w, http.StatusBadRequest, "invalid_request", "limit must be 1.."+strconv.Itoa(maxTagSearchLimit))
			return
		}
		limit = n
	}
	mode := r.URL.Query().Get("mode")
	if mode != "" && mode != "prefix" && mode != "contains" {
		apiError(w, http.StatusBadRequest, "invalid_request", "mode must be prefix or contains")
		return
	}
	ranges, ok := tagSearchRanges(q, mode == "contains")
	if !ok {
		apiError(w, http.StatusBadRequest, "namespace_required",
			"matching anywhere in the name needs a category: prefix to bound the scan")
		return
	}

	matches, truncated, err := h.ptr.SearchTags(r.Context(), ranges, tagSearchScanCap)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	clusters, more := renderTagClusters(q, matches, limit)
	writeJSON(w, http.StatusOK, map[string]any{"clusters": clusters, "truncated": truncated || more})
}

// tagSearchRanges renders a monbooru-form query into the index ranges to scan:
// one per candidate hydrus spelling for a prefix search, one per namespace for
// a substring search. A bare (general) query has no namespace to bound a
// substring scan, and the whole tag table is far too big to walk, so that one
// shape is refused rather than served slowly.
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

// renderTagClusters maps the raw matches into monbooru form, drops the ones
// whose ideal lands in a namespace the router does not carry, and orders them:
// the exact query first, then clusters that carry relations, then the shortest
// ideal, then by name. Ranking on the counts is what floats the real cluster
// above the near-misses sharing its stem.
func renderTagClusters(queried string, matches []ptr.TagMatch, limit int) (rows []ptrTagCluster, more bool) {
	out := make([]ptrTagCluster, 0, len(matches))
	at := map[string]int{}
	for _, m := range matches {
		c, ok := renderTagCluster(m)
		if !ok {
			continue
		}
		// Two clusters whose raw ideals differ only by the underscore-space
		// fold project to one monbooru name. The caller picks a name, so a
		// second row under it is a row it cannot tell apart; fold them and
		// let the graph query sort the spellings out on the way back.
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

// relations is a cluster's total graph size, the second ranking key.
func relations(c ptrTagCluster) int {
	return c.Aliases + c.Implications + c.ImpliedBy
}

// mergeTagCluster folds a second cluster onto the row already holding its
// monbooru name, keeping every matched spelling and the larger of each count.
func mergeTagCluster(into, from ptrTagCluster) ptrTagCluster {
	seen := make(map[string]bool, len(into.Matched))
	for _, n := range into.Matched {
		seen[n] = true
	}
	for _, n := range from.Matched {
		if !seen[n] {
			seen[n] = true
			into.Matched = append(into.Matched, n)
		}
	}
	into.Aliases = max(into.Aliases, from.Aliases)
	into.Implications = max(into.Implications, from.Implications)
	into.ImpliedBy = max(into.ImpliedBy, from.ImpliedBy)
	return into
}

// renderTagCluster maps one match into monbooru form. A cluster whose ideal
// projects to nothing is not offerable - the caller could not hold the name -
// and a matched spelling that projects away simply drops off the row.
func renderTagCluster(m ptr.TagMatch) (ptrTagCluster, bool) {
	ideal := mapping.MapPTRTag(m.Ideal)
	if ideal == "" {
		return ptrTagCluster{}, false
	}
	c := ptrTagCluster{Ideal: ideal, Matched: []string{}, Aliases: m.Aliases, Implications: m.Implications, ImpliedBy: m.ImpliedBy}
	seen := map[string]bool{}
	for _, raw := range m.Matched {
		name := mapping.MapPTRTag(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		c.Matched = append(c.Matched, name)
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
