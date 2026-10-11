package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
)

func mustRandBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("FATAL: failed to generate CSRF secret: %v", err)
	}
	return b
}

func (s *Server) csrfToken(sessionID string) string {
	mac := hmac.New(sha256.New, s.csrfSecret)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validateCSRF(sessionID, token string) bool {
	expected := s.csrfToken(sessionID)
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

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
		if !s.validateCSRF(sessID, token) {
			http.Error(w, "CSRF token invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
