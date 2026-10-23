package session

import (
	"mime"
	"net"
	"net/http"
	"strings"
)

// X-Forwarded-For is trusted only from a loopback peer, a same-host
// proxy, and only the entry that proxy appended: the ones before it are
// whatever the client sent.
func ClientIP(r *http.Request) string {
	host, loopback := peerHost(r)
	if loopback {
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			xff := vals[len(vals)-1]
			if last := strings.TrimSpace(xff[strings.LastIndex(xff, ",")+1:]); last != "" {
				return last
			}
		}
	}
	return host
}

// X-Forwarded-Proto is trusted from a loopback peer only, like
// X-Forwarded-For in ClientIP.
func RequestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	_, loopback := peerHost(r)
	return loopback && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func peerHost(r *http.Request) (host string, loopback bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return host, ip != nil && ip.IsLoopback()
}

// Browsers read "//host", a backslash, and a path whose tab or newline they
// strip into "//", as another host.
func LocalPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, `\`) {
		return false
	}
	return !strings.ContainsFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f })
}

func SetCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   RequestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

var sameOrigin http.CrossOriginProtection

// Without either header nothing vouches for the request (a script), so it
// still needs the token.
func BrowserSameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Origin") == "" {
		return false
	}
	return sameOrigin.Check(r) == nil
}

// A same-origin request passes too: a page rendered before a restart holds
// a token that no longer validates, and is no forgery.
func (s *Store) CheckCSRF(r *http.Request, id string) bool {
	// Header first: on multipart, FormValue drains up to 32 MiB past any
	// handler's MaxBytesReader.
	token := r.Header.Get("X-CSRF-Token")
	if token == "" && !isMultipart(r) {
		token = r.FormValue("_csrf")
	}
	return s.ValidCSRF(id, token) || BrowserSameOrigin(r)
}

func isMultipart(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.HasPrefix(mt, "multipart/")
}
