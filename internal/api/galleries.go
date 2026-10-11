package api

import (
	"net/http"
	"strings"

	"github.com/monbooru/monloader/internal/config"
)

func (h *Handler) galleryRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "invalid JSON body")
		return
	}
	from, to := strings.TrimSpace(body.From), strings.TrimSpace(body.To)
	if from == "" || to == "" {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "from and to are required")
		return
	}
	if h.host.SaveConfig == nil {
		apiError(w, http.StatusInternalServerError, errSaveFailed, "settings cannot be saved here")
		return
	}
	changed := 0
	if err := h.host.SaveConfig(func(c *config.Config) error {
		changed = c.RenameGallery(from, to)
		return nil
	}); err != nil {
		apiError(w, http.StatusInternalServerError, errSaveFailed, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}
