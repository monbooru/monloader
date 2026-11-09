package api

import (
	"net/http"
	"strings"

	"github.com/monbooru/monloader/internal/config"
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
