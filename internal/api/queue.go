package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/queue"
)

// maxWaitSeconds caps the synchronous enqueue wait so a caller can never pin a
// worker indefinitely.
const maxWaitSeconds = 60

type enqueueRequest struct {
	URL     string `json:"url"`
	Options *struct {
		Gallery  string `json:"gallery"`
		Folder   string `json:"folder"`
		MaxItems int    `json:"max_items"`
		PageURL  string `json:"page_url"`
	} `json:"options"`
}

// monbooruReady refuses work whose push step could only fail: with no monbooru
// configured there is nothing to push to, and a held link is the operator's own
// hold. The web add bar refuses on both counts; the extension sends through
// here, so this refuses too.
func (h *Handler) monbooruReady(w http.ResponseWriter) bool {
	cfg := h.cfg.Current()
	if cfg.Monbooru.APIURL == "" {
		apiError(w, http.StatusConflict, "monbooru_unconfigured",
			"monbooru is not configured - set its connection in monloader's settings")
		return false
	}
	if cfg.Monbooru.Paused {
		apiError(w, http.StatusConflict, "monbooru_paused",
			"the monbooru link is paused - resume it from the light in monloader's footer")
		return false
	}
	return true
}

// enqueue handles POST /api/v1/queue. Downloads are asynchronous, so it
// returns 202 with a job id; with ?wait=N it blocks up to N seconds and
// returns the resolved job inline if it finished in time.
func (h *Handler) enqueue(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	var body enqueueRequest
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	// A URL copied out of a browser or a chat client routinely arrives padded,
	// which the add bar trims and this refused as not an http(s) URL.
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "url is required")
		return
	}
	// "md5:<hash>" imports the post carrying that hash via the lookup walk.
	// The prefix is required on the API so a bare hash is never mistaken for a
	// malformed URL; the web add bar accepts both forms. Case is forgiven on
	// both, since a pasted hash carries whatever the source rendered.
	md5, isHash := strings.CutPrefix(strings.ToLower(body.URL), "md5:")
	if isHash && !config.IsHexHash(md5, 32) {
		apiError(w, http.StatusBadRequest, "invalid_request", "md5: must carry a 32-hex hash")
		return
	}
	if !isHash && !config.IsHTTPURL(body.URL) {
		apiError(w, http.StatusBadRequest, "invalid_request", "url must be an http(s) URL")
		return
	}

	opts := queue.Options{}
	if body.Options != nil {
		opts.Gallery = body.Options.Gallery
		opts.Folder = body.Options.Folder
		// A zero max_items is indistinguishable from omitted, so it means "no
		// override"; a negative value is a mistake.
		if body.Options.MaxItems < 0 {
			apiError(w, http.StatusBadRequest, "invalid_request", "max_items must not be negative")
			return
		}
		opts.MaxItems = body.Options.MaxItems
		if body.Options.PageURL != "" && !config.IsHTTPURL(body.Options.PageURL) {
			apiError(w, http.StatusBadRequest, "invalid_request", "page_url must be an http(s) URL")
			return
		}
		opts.PageURL = body.Options.PageURL
	}

	wait := waitSeconds(r)
	if wait > 0 {
		// A wait request jumps ahead of bulk jobs so it stays responsive under one
		// worker. A job's size is not known until it resolves, so any wait takes
		// the priority lane, not only the extension's single-image case; a bulk
		// wait shares it but usually exceeds the timeout and falls back to polling.
		opts.Priority = true
	}

	var id int64
	if isHash {
		id = h.queue.EnqueueHashImport(md5, opts)
	} else {
		id = h.queue.Enqueue(body.URL, opts)
	}

	if wait > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(wait)*time.Second)
		defer cancel()
		if job, err := h.queue.Wait(ctx, id); err == nil {
			writeJSON(w, http.StatusOK, job)
			return
		}
		// Timed out: fall through to the async response so the caller polls.
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id})
}

// imageJobRequest is the body both source-refetch endpoints take: an image
// monbooru already holds and the post URL to read it from.
type imageJobRequest struct {
	ImageID int64  `json:"image_id"`
	Gallery string `json:"gallery"`
	URL     string `json:"url"`
}

// enqueueImageJob validates one image-and-url body and queues it through
// enqueue, answering 202 with the job id.
func (h *Handler) enqueueImageJob(w http.ResponseWriter, r *http.Request, enqueue func(imageID int64, gallery, url string) int64) {
	var body imageJobRequest
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if body.ImageID <= 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "image_id is required")
		return
	}
	if !config.IsHTTPURL(body.URL) {
		apiError(w, http.StatusBadRequest, "invalid_request", "url must be an http(s) URL")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": enqueue(body.ImageID, body.Gallery, body.URL)})
}

// metadata handles POST /api/v1/metadata: monbooru asks monloader to re-read a
// post URL for its tags / commentary / notes (metadata only, no download) and
// enrich the monbooru image it names. Returns 202 with the queued job id.
func (h *Handler) metadata(w http.ResponseWriter, r *http.Request) {
	h.enqueueImageJob(w, r, h.queue.EnqueueMetadata)
}

// replace handles POST /api/v1/replace: monbooru asks monloader to download
// the file a post URL serves and push it back into the monbooru image it
// names, replacing its bytes in place. Returns 202 with the queued job id.
func (h *Handler) replace(w http.ResponseWriter, r *http.Request) {
	h.enqueueImageJob(w, r, h.queue.EnqueueReplace)
}

type lookupRequest struct {
	ImageID int64  `json:"image_id"`
	Backend string `json:"backend"`
	MD5     string `json:"md5"`
	SHA256  string `json:"sha256"`
	Gallery string `json:"gallery"`
	// Background waits behind anything a person is watching; Budgeted also
	// spends a slot of the daily budget and can be refused. Both default
	// false, so a caller that knows neither field keeps today's behaviour.
	Background bool `json:"background"`
	Budgeted   bool `json:"budgeted"`
}

// lookup handles POST /api/v1/lookup: monbooru asks monloader to find tags for
// an image by file hash - the booru backend walks the opted-in sites' md5
// search, the ptr backend queries the local PTR index by sha256 - and enrich
// the image with what it finds. Returns 202 with the queued job id.
func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) {
	var body lookupRequest
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if body.ImageID <= 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "image_id is required")
		return
	}
	switch body.Backend {
	case queue.BackendBooru:
		if !config.IsHexHash(body.MD5, 32) {
			apiError(w, http.StatusBadRequest, "invalid_request", "the booru backend needs a 32-hex md5")
			return
		}
	case queue.BackendPTR:
		if !config.IsHexHash(body.SHA256, 64) {
			apiError(w, http.StatusBadRequest, "invalid_request", "the ptr backend needs a 64-hex sha256")
			return
		}
		if h.ptr == nil || !h.ptr.Enabled() {
			// Refuse rather than queue a job doomed to fail, so monbooru's button
			// can degrade in place if its capability check was stale.
			apiError(w, http.StatusConflict, "ptr_unavailable", "the ptr lookup backend is disabled")
			return
		}
		if !h.ptr.CaughtUp() {
			// A half-built index answers with a partial tag set that reads like
			// the whole truth, so it answers nothing until it is current.
			apiError(w, http.StatusConflict, "ptr_syncing", "the ptr index is not fully synced yet")
			return
		}
	case queue.BackendAll:
		// "all" means "whatever is enabled": a disabled PTR is not a conflict
		// here, the job just runs the booru walk alone, so the sha256 is only
		// demanded while the PTR could use it.
		if !config.IsHexHash(body.MD5, 32) {
			apiError(w, http.StatusBadRequest, "invalid_request", "the all backend needs a 32-hex md5")
			return
		}
		if h.ptr != nil && h.ptr.Enabled() && !config.IsHexHash(body.SHA256, 64) {
			apiError(w, http.StatusBadRequest, "invalid_request", "the all backend needs a 64-hex sha256 while the ptr is enabled")
			return
		}
		if body.SHA256 != "" && !config.IsHexHash(body.SHA256, 64) {
			apiError(w, http.StatusBadRequest, "invalid_request", "sha256 must be 64 hex characters")
			return
		}
	default:
		apiError(w, http.StatusBadRequest, "invalid_request", "backend must be \"booru\", \"ptr\", or \"all\"")
		return
	}
	if body.Budgeted && !h.queue.TakeLookupBudget() {
		// Authoritative: monbooru does not keep its own count, it stops its
		// nightly walk for the day on this answer.
		apiError(w, http.StatusTooManyRequests, "budget_exhausted",
			"today's scheduled-lookup budget is spent")
		return
	}
	id := h.queue.EnqueueLookup(body.ImageID, body.Gallery, body.Backend,
		strings.ToLower(body.MD5), strings.ToLower(body.SHA256), body.Background, body.Budgeted)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id})
}

// lookupStatus handles GET /api/v1/lookup/status: what an unattended caller
// needs to know before it starts a nightly walk - how much of today's budget
// is left, and whether any source is configured to walk. The refusal on the enqueue stays
// authoritative; this is a readout, so a caller must not keep its own count.
func (h *Handler) lookupStatus(w http.ResponseWriter, r *http.Request) {
	limit, left, resets := h.queue.LookupBudget()
	sources := h.mapper.LookupChain()
	chain := make([]string, 0, len(sources))
	for _, src := range sources {
		chain = append(chain, src.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"daily_budget": limit,
		"left_today":   left,
		"resets_at":    resets.Unix(),
		"chain":        chain,
	})
}

// waitSeconds reads and clamps the ?wait= parameter.
func waitSeconds(r *http.Request) int {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return min(n, maxWaitSeconds)
}

// listJobs handles GET /api/v1/queue with optional status filter and
// pagination.
func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request) {
	page, limit := parsePage(r, 50, 200)
	status := queue.JobStatus(r.URL.Query().Get("status"))
	if status != "" && !queue.ValidJobStatus(status) {
		apiError(w, http.StatusBadRequest, "invalid_request", "unknown status filter")
		return
	}
	jobs, total := h.queue.List(queue.ListOptions{
		Status: status,
		Limit:  limit,
		Page:   page,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"page":   page,
		"limit":  limit,
		"total":  total,
		"paused": h.queue.Paused(),
		"jobs":   jobs,
	})
}

// pauseQueue handles POST /api/v1/queue/pause: hold new downloads globally.
func (h *Handler) pauseQueue(w http.ResponseWriter, r *http.Request) {
	h.queue.Pause()
	writeJSON(w, http.StatusOK, map[string]any{"paused": true})
}

// resumeQueue handles POST /api/v1/queue/resume: let held downloads run.
func (h *Handler) resumeQueue(w http.ResponseWriter, r *http.Request) {
	h.queue.Resume()
	writeJSON(w, http.StatusOK, map[string]any{"paused": false})
}

// getJob handles GET /api/v1/queue/{id}.
func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	job, err := h.queue.Get(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// jobAction runs one queue action addressed by the {id} path segment and
// answers 202 with the resulting job id, mapping ErrNotFound to 404 and any
// other queue refusal to 409.
func (h *Handler) jobAction(w http.ResponseWriter, r *http.Request, action func(id int64) (int64, error)) {
	id, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	jobID, err := action(id)
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			apiError(w, http.StatusNotFound, "not_found", "job not found")
			return
		}
		apiError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

// retryJob handles POST /api/v1/queue/{id}/retry. With ?force=1 the re-run
// bypasses the download-archive so already-fetched posts are downloaded again.
func (h *Handler) retryJob(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	h.jobAction(w, r, func(id int64) (int64, error) {
		return id, h.queue.Retry(id, r.URL.Query().Get("force") == "1")
	})
}

// continueJob handles POST /api/v1/queue/{id}/continue. It enqueues a follow-up
// job for the window after a capped job's, returning the new job id.
func (h *Handler) continueJob(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	h.jobAction(w, r, h.queue.Continue)
}

// continueAllJob handles POST /api/v1/queue/{id}/continue-all. It queues the
// next window and keeps fetching each following one until the capped search
// runs short, returning the first follow-up job's id.
func (h *Handler) continueAllJob(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	h.jobAction(w, r, h.queue.ContinueAll)
}

// deleteJob handles DELETE /api/v1/queue/{id}: cancels a running job, else
// removes it. It acts on the whole continue-series like the web route, so a
// caller clearing a collapsed row drops every window, not just the one named.
func (h *Handler) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := h.queue.CancelSeries(id); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
