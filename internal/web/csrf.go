package web

import (
	"net/http"
	"strings"
)

const maxFormBytes = 8 << 20

func (s *Server) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		// The form parse below reads the body before the token is checked,
		// and /login reaches it signed out; no browser form here is large.
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		sessID := sessionFromContext(r.Context())
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.FormValue("_csrf")
		}
		if !s.sessions.ValidCSRF(sessID, token) {
			http.Error(w, "CSRF token invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
