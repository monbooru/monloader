package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/contract"
)

const maxWaitSeconds = 60

const (
	maxOptionTags       = 64
	maxOptionCommentary = contract.MaxCommentary
	maxOptionSource     = contract.MaxSourceLabel
)

type enqueueRequest struct {
	URL     string `json:"url"`
	Options *struct {
		Gallery    string   `json:"gallery"`
		Folder     string   `json:"folder"`
		MaxItems   int      `json:"max_items"`
		PageURL    string   `json:"page_url"`
		Tags       []string `json:"tags"`
		Commentary string   `json:"commentary"`
		Source     string   `json:"source"`
		Via        string   `json:"via"`
	} `json:"options"`
}

func (h *Handler) monbooruReady(w http.ResponseWriter) bool {
	cfg := h.cfg.Current()
	if cfg.Monbooru.APIURL == "" {
		contract.WriteError(w, http.StatusConflict, errMonbooruUnconfigured,
			"monbooru is not configured - set its connection in monloader's settings")
		return false
	}
	if cfg.Monbooru.Paused {
		contract.WriteError(w, http.StatusConflict, errMonbooruPaused,
			"the monbooru link is paused - resume it from the light in monloader's footer")
		return false
	}
	return true
}

func (h *Handler) enqueue(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	var body enqueueRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "url is required")
		return
	}
	md5, isHash := strings.CutPrefix(strings.ToLower(body.URL), "md5:")
	if isHash && !config.IsHexHash(md5, 32) {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "md5: must carry a 32-hex hash")
		return
	}
	if !isHash && !config.IsHTTPURL(body.URL) {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "url must be an http(s) URL")
		return
	}

	opts := queue.Options{}
	if body.Options != nil {
		opts.Gallery = body.Options.Gallery
		if !config.IsRelativeFolder(body.Options.Folder) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "folder must be a relative path with no .. segment")
			return
		}
		opts.Folder = body.Options.Folder
		// Zero is indistinguishable from omitted, so it means no override.
		if body.Options.MaxItems < 0 {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "max_items must not be negative")
			return
		}
		opts.MaxItems = body.Options.MaxItems
		if body.Options.PageURL != "" && !config.IsHTTPURL(body.Options.PageURL) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "page_url must be an http(s) URL")
			return
		}
		opts.PageURL = body.Options.PageURL
		if len(body.Options.Tags) > maxOptionTags {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest,
				"at most "+strconv.Itoa(maxOptionTags)+" tags")
			return
		}
		opts.Tags = body.Options.Tags
		if utf8.RuneCountInString(body.Options.Commentary) > maxOptionCommentary {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest,
				"commentary must be "+strconv.Itoa(maxOptionCommentary)+" characters or less")
			return
		}
		opts.Commentary = body.Options.Commentary
		if utf8.RuneCountInString(body.Options.Source) > maxOptionSource {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest,
				"source must be "+strconv.Itoa(maxOptionSource)+" characters or less")
			return
		}
		opts.Source = body.Options.Source
		if err := contract.ValidateVia(body.Options.Via); err != nil {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, err.Error())
			return
		}
		opts.Via = body.Options.Via
	}

	wait := waitSeconds(r)
	if wait > 0 {
		// A wait jumps ahead of bulk jobs to stay responsive under one
		// worker. Size is unknown until the job resolves, so every wait
		// takes the lane.
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
			contract.WriteJSON(w, http.StatusOK, job)
			return
		}
	}
	contract.WriteJSON(w, http.StatusAccepted, contract.JobAccepted{JobID: id})
}

type imageJobRequest = contract.ImageJobRequest

func (h *Handler) enqueueImageJob(w http.ResponseWriter, r *http.Request, enqueue func(imageID int64, gallery, url string) int64) {
	var body imageJobRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	if body.ImageID <= 0 {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "image_id is required")
		return
	}
	if !config.IsHTTPURL(body.URL) {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "url must be an http(s) URL")
		return
	}
	contract.WriteJSON(w, http.StatusAccepted, contract.JobAccepted{JobID: enqueue(body.ImageID, body.Gallery, body.URL)})
}

func (h *Handler) metadata(w http.ResponseWriter, r *http.Request) {
	h.enqueueImageJob(w, r, h.queue.EnqueueMetadata)
}

func (h *Handler) replace(w http.ResponseWriter, r *http.Request) {
	h.enqueueImageJob(w, r, h.queue.EnqueueReplace)
}

type lookupRequest = contract.LookupRequest

func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) {
	var body lookupRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	if body.ImageID <= 0 {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "image_id is required")
		return
	}
	switch body.Backend {
	case queue.BackendBooru:
		if !config.IsHexHash(body.MD5, 32) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "the booru backend needs a 32-hex md5")
			return
		}
	case queue.BackendPTR:
		if !config.IsHexHash(body.SHA256, 64) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "the ptr backend needs a 64-hex sha256")
			return
		}
		if h.ptr == nil || !h.ptr.Enabled() {
			contract.WriteError(w, http.StatusConflict, errPTRUnavailable, "the ptr lookup backend is disabled")
			return
		}
		if !h.ptr.CaughtUp() {
			contract.WriteError(w, http.StatusConflict, errPTRSyncing, "the ptr index is not fully synced yet")
			return
		}
	case queue.BackendAll:
		// A disabled PTR is no conflict here: the job runs the booru walk
		// alone.
		if !config.IsHexHash(body.MD5, 32) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "the all backend needs a 32-hex md5")
			return
		}
		if h.ptr != nil && h.ptr.Enabled() && !config.IsHexHash(body.SHA256, 64) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "the all backend needs a 64-hex sha256 while the ptr is enabled")
			return
		}
		if body.SHA256 != "" && !config.IsHexHash(body.SHA256, 64) {
			contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "sha256 must be 64 hex characters")
			return
		}
	default:
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "backend must be \"booru\", \"ptr\", or \"all\"")
		return
	}
	if body.Budgeted && !h.queue.TakeLookupBudget() {
		// Authoritative: monbooru keeps no count of its own and stops its
		// nightly walk on this answer.
		contract.WriteError(w, http.StatusTooManyRequests, errBudgetExhausted,
			"today's scheduled-lookup budget is spent")
		return
	}
	id := h.queue.EnqueueLookup(body.ImageID, body.Gallery, body.Backend,
		strings.ToLower(body.MD5), strings.ToLower(body.SHA256), body.Background, body.Budgeted)
	contract.WriteJSON(w, http.StatusAccepted, contract.JobAccepted{JobID: id})
}

func (h *Handler) lookupStatus(w http.ResponseWriter, r *http.Request) {
	limit, left, resets := h.queue.LookupBudget()
	sources := h.mapper.LookupChain()
	chain := make([]string, 0, len(sources))
	for _, src := range sources {
		if h.host.Queryable == nil || h.host.Queryable(src) {
			chain = append(chain, src.Name)
		}
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{
		"daily_budget": limit,
		"left_today":   left,
		"resets_at":    resets.Unix(),
		"chain":        chain,
	})
}

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

func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request) {
	page, limit := contract.PageParams(r.URL.Query().Get("page"), r.URL.Query().Get("limit"), 50, 200)
	status := queue.JobStatus(r.URL.Query().Get("status"))
	if status != "" && !queue.ValidJobStatus(status) {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "unknown status filter")
		return
	}
	jobs, total := h.queue.List(queue.ListOptions{
		Status: status,
		Limit:  limit,
		Page:   page,
	})
	contract.WriteJSON(w, http.StatusOK, map[string]any{
		"page":   page,
		"limit":  limit,
		"total":  total,
		"paused": h.queue.Paused(),
		"jobs":   jobs,
	})
}

func (h *Handler) pauseQueue(w http.ResponseWriter, r *http.Request) {
	h.queue.Pause()
	contract.WriteJSON(w, http.StatusOK, map[string]any{"paused": true})
}

func (h *Handler) resumeQueue(w http.ResponseWriter, r *http.Request) {
	h.queue.Resume()
	contract.WriteJSON(w, http.StatusOK, map[string]any{"paused": false})
}

func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := contract.PathInt64(w, r, "id")
	if !ok {
		return
	}
	job, err := h.queue.Get(id)
	if err != nil {
		contract.WriteError(w, http.StatusNotFound, errNotFound, "job not found")
		return
	}
	contract.WriteJSON(w, http.StatusOK, job)
}

// Every action queues work, so each is held while monbooru is not ready;
// the gate goes ahead of the id parse.
func (h *Handler) jobAction(w http.ResponseWriter, r *http.Request, action func(id int64) (int64, error)) {
	if !h.monbooruReady(w) {
		return
	}
	id, ok := contract.PathInt64(w, r, "id")
	if !ok {
		return
	}
	jobID, err := action(id)
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			contract.WriteError(w, http.StatusNotFound, errNotFound, "job not found")
			return
		}
		contract.WriteError(w, http.StatusConflict, errConflict, err.Error())
		return
	}
	contract.WriteJSON(w, http.StatusAccepted, contract.JobAccepted{JobID: jobID})
}

func (h *Handler) retryJob(w http.ResponseWriter, r *http.Request) {
	h.jobAction(w, r, func(id int64) (int64, error) {
		return id, h.queue.Retry(id, r.URL.Query().Get("force") == "1")
	})
}

func (h *Handler) continueJob(w http.ResponseWriter, r *http.Request) {
	h.jobAction(w, r, h.queue.Continue)
}

func (h *Handler) continueAllJob(w http.ResponseWriter, r *http.Request) {
	h.jobAction(w, r, h.queue.ContinueAll)
}

// The whole continue-series goes, so clearing a collapsed row drops every
// window.
func (h *Handler) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := contract.PathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := h.queue.CancelSeries(id); err != nil {
		contract.WriteError(w, http.StatusNotFound, errNotFound, "job not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
