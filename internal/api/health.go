package api

import (
	"net/http"

	"github.com/monbooru/monloader/internal/shared/contract"
)

// APIVersion moves on any change to /api/v1 a paired peer would trip
// over: a path or field gone, a status code or a vocabulary word changed.
// MinPeerAPI is the oldest monbooru API this side still drives.
const (
	APIVersion = 2
	MinPeerAPI = 1
)

// PeerMismatch is empty when the two sides agree, and when the peer
// predates the field.
func PeerMismatch(peer contract.Health) string {
	return peer.APIMismatch(contract.AppMonloader, APIVersion, MinPeerAPI)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	SetCORS(w, r)
	contract.WriteJSON(w, http.StatusOK, contract.Health{
		App:              contract.AppMonloader,
		Status:           "ok",
		Version:          h.version,
		GalleryDLVersion: h.catalog.Version(),
		API:              APIVersion,
		MinPeerAPI:       MinPeerAPI,
	})
}
