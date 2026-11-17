package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func (h *Handler) galleryRename(w http.ResponseWriter, r *http.Request) {
	var body contract.GalleryRename
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	from, to := strings.TrimSpace(body.From), strings.TrimSpace(body.To)
	if from == "" || to == "" {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "from and to are required")
		return
	}
	changed := 0
	if err := h.cfg.Update(func(c *config.Config) error {
		changed = c.RenameGallery(from, to)
		return nil
	}); err != nil {
		contract.WriteError(w, http.StatusInternalServerError, errSaveFailed, err.Error())
		return
	}
	h.queue.RenameGallery(from, to)
	contract.WriteJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

func (h *Handler) listGalleries(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	galleries, err := h.host.Monbooru.ListGalleries(r.Context())
	if err != nil {
		writeMonbooruError(w, r, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"galleries": galleries})
}

func (h *Handler) listFolders(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	root, err := h.host.Monbooru.ListFolders(r.Context(), strings.TrimSpace(r.URL.Query().Get("gallery")))
	if err != nil {
		writeMonbooruError(w, r, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, root)
}

// A 502 either way: monloader is up, the monbooru behind it is not, or it
// refused (an unknown gallery among the reasons).
func writeMonbooruError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	code, msg := outcome.ErrCodeMonbooruRejected, err.Error()
	var ce *outcome.CodedError
	if errors.As(err, &ce) {
		code, msg = ce.Code, ce.Msg
	}
	contract.WriteError(w, http.StatusBadGateway, code, msg)
}
