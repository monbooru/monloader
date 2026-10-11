package api

import "net/http"

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	writeJSON(w, http.StatusOK, map[string]string{
		// A second launch reads "app" to tell our own instance from
		// another program on the port.
		"app":               "monloader",
		"status":            "ok",
		"version":           h.version,
		"gallerydl_version": h.catalog.Version(),
	})
}
