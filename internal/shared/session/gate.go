package session

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Anon is the session a request carries where no login applies, so the
// CSRF check still has one to validate.
const Anon = "anon"

type ctxKey struct{}

// FromContext is the session Gate put on the request.
func FromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

func FromCookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// The API checks its own bearer tokens.
func apiPath(p string) bool { return strings.HasPrefix(p, "/api/v1/") }

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// Gate admits a request with a valid session in the cookie named cookie, or
// as Anon one whose path open passes, or any while loginOn says no.
func (s *Store) Gate(next http.Handler, cookie string, loginOn func() bool, open func(path string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		id := Anon
		if !open(r.URL.Path) && loginOn() {
			id = FromCookie(r, cookie)
			if !s.Valid(id) {
				// No HX-Redirect: a poll would take the page, and whatever
				// was typed on it, to the login.
				if isHTMX(r) {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				target := "/login"
				if r.Method == http.MethodGet && r.URL.Path != "/" {
					target += "?next=" + url.QueryEscape(r.URL.RequestURI())
				}
				http.Redirect(w, r, target, http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// Guard refuses a state-changing request whose CSRF token does not match
// its session. Safe methods, /api/v1/ and what exempt passes go through.
func (s *Store) Guard(next http.Handler, exempt func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions ||
			apiPath(r.URL.Path) || (exempt != nil && exempt(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.CheckCSRF(r, FromContext(r.Context())) {
			http.Error(w, "This page's form token is no longer valid. Copy anything unsaved, then reload the page.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
