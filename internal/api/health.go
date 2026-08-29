package api

import "net/http"

// health handles GET /health (no auth): liveness plus the running version and
// the active gallery-dl version (the managed install's when one shadows the
// bundled binary). It bypasses auth, so it sets CORS itself for the browser
// extension that reads it cross-origin.
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	writeJSON(w, http.StatusOK, map[string]string{
		// "app" is what lets a second launch tell our own instance from
		// whatever else already took the port.
		"app":               "monloader",
		"status":            "ok",
		"version":           h.version,
		"gallerydl_version": h.catalog.Version(),
	})
}
