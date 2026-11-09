package api

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/monbooru/monloader/internal/contrib"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func (h *Handler) contribGate(w http.ResponseWriter) bool {
	if !h.ptr.Enabled() {
		contract.WriteError(w, http.StatusConflict, errPTRUnavailable, "the ptr index is not available")
		return false
	}
	return true
}

func (h *Handler) contribStore(w http.ResponseWriter) (*ptr.ContribStore, bool) {
	if !h.contribGate(w) {
		return nil, false
	}
	store := h.ptr.Contrib()
	if store == nil {
		contract.WriteError(w, http.StatusConflict, errPTRUnavailable, "the contribution store is not available")
		return nil, false
	}
	return store, true
}

func (h *Handler) ptrContribPreview(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	var body contract.ContribPreviewRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	if !contrib.ValidHash(body.SHA256) {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "sha256 must be 64 lowercase hex characters")
		return
	}
	// A missing field would answer an empty diff, which reads like a file
	// with nothing to contribute.
	if body.Tags == nil {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "tags is required; send an empty list for an untagged file")
		return
	}

	resp, err := contrib.Preview(h.ptr, body.SHA256, body.Tags, body.Implied)
	if err != nil {
		contract.WriteError(w, http.StatusConflict, errPTRUnavailable, err.Error())
		return
	}
	contract.WriteJSON(w, http.StatusOK, resp)
}

func refuse(w http.ResponseWriter, r *contrib.Refusal) {
	status := http.StatusConflict
	switch r.Code {
	case contrib.CodeNotFound:
		status = http.StatusNotFound
	case contrib.CodeInternal:
		status = http.StatusInternalServerError
	}
	contract.WriteError(w, status, r.Code, r.Msg)
}

func (h *Handler) contribAccountGate(ctx context.Context, w http.ResponseWriter) bool {
	if refusal := contrib.SendRefusal(ctx, h.ptr); refusal != nil {
		refuse(w, refusal)
		return false
	}
	return true
}

func (h *Handler) ptrContribStage(w http.ResponseWriter, r *http.Request) {
	var body contract.ContribRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	commit := body.Commit == nil || *body.Commit
	if commit && !h.contribAccountGate(r.Context(), w) {
		return
	}
	// A sender that gave up waiting has already told its user the send
	// failed, so nothing may land behind its back.
	if r.Context().Err() != nil {
		return
	}
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	if len(body.Items) == 0 {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "items must not be empty")
		return
	}

	results, accepted := contrib.StageAll(h.ptr, store, body.Items, body.Origin)
	resp := contract.ContribResponse{Results: results}
	status := http.StatusOK
	if commit && len(accepted) > 0 && r.Context().Err() == nil {
		resp.JobID = h.queue.EnqueueContrib(accepted, false)
		status = http.StatusAccepted
	}
	contract.WriteJSON(w, status, resp)
}

type contribUnsentJSON struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Tag       string `json:"tag"`
	Tag2      string `json:"tag2,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type contribLogJSON struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Tag         string `json:"tag"`
	Tag2        string `json:"tag2,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CommittedAt int64  `json:"committed_at"`
	Outcome     string `json:"outcome,omitempty"`
	OutcomeAt   int64  `json:"outcome_at,omitempty"`
}

func (h *Handler) ptrContribLedger(w http.ResponseWriter, r *http.Request) {
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	unsent, err := store.Unsent()
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	logRows, err := store.LogPage(100, 0)
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	unsentJSON := make([]contribUnsentJSON, 0, len(unsent))
	countsByKind := map[string]int{}
	countsByStatus := map[string]int{}
	for _, it := range unsent {
		countsByKind[it.Kind]++
		countsByStatus[it.Status]++
		unsentJSON = append(unsentJSON, contribUnsentJSON{
			ID: it.ID, Kind: it.Kind, Tag: it.Tag, Tag2: it.Tag2,
			SHA256: hex.EncodeToString(it.Hash), Reason: it.Reason, Origin: it.Origin,
			Status: it.Status, Error: it.Error, CreatedAt: it.CreatedAt,
		})
	}
	logJSON := make([]contribLogJSON, 0, len(logRows))
	outcomes := map[string]int{}
	for _, row := range logRows {
		outcomes[cmp.Or(row.Outcome, contract.ContribPending)]++
		logJSON = append(logJSON, contribLogJSON{
			ID: row.ID, Kind: row.Kind, Tag: row.Tag, Tag2: row.Tag2,
			SHA256: hex.EncodeToString(row.Hash), Reason: row.Reason,
			CommittedAt: row.CommittedAt, Outcome: row.Outcome, OutcomeAt: row.OutcomeAt,
		})
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{
		"unsent": unsentJSON,
		"counts": map[string]any{"by_kind": countsByKind, "by_status": countsByStatus, "outcomes": outcomes},
		"log":    logJSON,
	})
}

func (h *Handler) ptrContribRescind(w http.ResponseWriter, r *http.Request) {
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	id, ok := contract.PathInt64(w, r, "id")
	if !ok {
		return
	}
	found, err := store.Rescind(id)
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	if !found {
		contract.WriteError(w, http.StatusNotFound, errNotFound, "no unsent item with that id")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ptrContribRescindAll(w http.ResponseWriter, r *http.Request) {
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	n, err := store.RescindAll()
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"rescinded": n})
}

// Only this path needs the already_running guard: per-confirm sends claim
// disjoint id sets.
func (h *Handler) ptrContribCommit(w http.ResponseWriter, r *http.Request) {
	if !h.contribAccountGate(r.Context(), w) {
		return
	}
	if h.queue.ContribSendLive() {
		contract.WriteError(w, http.StatusConflict, errAlreadyRunning, "a contribution send is already running")
		return
	}
	jobID := h.queue.EnqueueContrib(nil, true)
	contract.WriteJSON(w, http.StatusAccepted, contract.JobAccepted{JobID: jobID})
}

func (h *Handler) ptrContribLogRescind(w http.ResponseWriter, r *http.Request) {
	if !h.contribAccountGate(r.Context(), w) {
		return
	}
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	id, ok := contract.PathInt64(w, r, "id")
	if !ok {
		return
	}
	itemID, rerr := contrib.RescindCommittedAdd(h.ptr, store, id)
	if rerr != nil {
		refuse(w, rerr)
		return
	}
	jobID := h.queue.EnqueueContrib([]int64{itemID}, false)
	contract.WriteJSON(w, http.StatusAccepted, contract.JobAccepted{JobID: jobID})
}

func (h *Handler) ptrContribPairPreview(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	var body contract.PairPreviewRequest
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	if strings.TrimSpace(body.A) == "" || strings.TrimSpace(body.B) == "" {
		// Without this a field-name typo reads as a verdict about a tag
		// rather than about the request.
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "a and b are required")
		return
	}
	resp, err := contrib.PairPreview(h.ptr, body)
	switch {
	case errors.Is(err, contrib.ErrUnknownPairKind):
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, err.Error())
	case err != nil:
		contract.WriteError(w, http.StatusConflict, errPTRUnavailable, err.Error())
	default:
		contract.WriteJSON(w, http.StatusOK, resp)
	}
}
