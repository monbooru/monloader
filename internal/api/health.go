package api

import (
	"fmt"
	"net/http"
)

// APIVersion moves on any change to /api/v1 a paired peer would trip
// over: a path or field gone, a status code or a vocabulary word changed.
// MinPeerAPI is the oldest monbooru API this side still drives.
const (
	APIVersion = 1
	MinPeerAPI = 1
)

// PeerMismatch is empty when the two sides agree, and when the peer
// predates the field.
func PeerMismatch(peerAPI, peerMin int) string {
	if peerAPI == 0 || (peerAPI >= MinPeerAPI && peerMin <= APIVersion) {
		return ""
	}
	return fmt.Sprintf("api mismatch: it speaks api %d and drives %d or newer, this monloader speaks %d and drives %d or newer",
		peerAPI, peerMin, APIVersion, MinPeerAPI)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	SetCORS(w, r)
	writeJSON(w, http.StatusOK, map[string]any{
		// A second launch reads "app" to tell our own instance from
		// another program on the port.
		"app":               "monloader",
		"status":            "ok",
		"version":           h.version,
		"gallerydl_version": h.catalog.Version(),
		"api":               APIVersion,
		"min_peer_api":      MinPeerAPI,
	})
}
