package web

import (
	"net/http"
	"strings"
)

const maxFormBytes = 8 << 20

// The form parse in the CSRF check reads the body before the token is
// checked, and /login reaches it signed out; no browser form here is large.
func limitFormBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		next.ServeHTTP(w, r)
	})
}
