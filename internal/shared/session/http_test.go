package session

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		name, remote, xff, want string
	}{
		{"DirectPeer", "203.0.113.9:54321", "", "203.0.113.9"},
		{"BehindLoopbackProxyTakesTheAddressItAppended", "127.0.0.1:1111", "198.51.100.7, 10.0.0.1", "10.0.0.1"},
		{"NonLoopbackIgnoresXFF", "203.0.113.9:54321", "evil.example", "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := ClientIP(r); got != tc.want {
				t.Errorf("ClientIP(%s, XFF %q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
			}
		})
	}
}

func TestRequestIsHTTPS(t *testing.T) {
	for _, tc := range []struct {
		name, url, remote, proto string
		want                     bool
	}{
		{"PlainHTTP", "/", "203.0.113.9:54321", "", false},
		{"TLS", "https://app.example/", "203.0.113.9:54321", "", true},
		{"LoopbackProxyOverHTTPS", "/", "127.0.0.1:1111", "https", true},
		{"LoopbackProxyOverHTTP", "/", "127.0.0.1:1111", "http", false},
		{"NonLoopbackIgnoresTheHeader", "/", "203.0.113.9:54321", "https", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			r.RemoteAddr = tc.remote
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if got := RequestIsHTTPS(r); got != tc.want {
				t.Errorf("RequestIsHTTPS = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLocalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/tags?q=x":        true,
		"/":                true,
		"":                 false,
		"tags":             false,
		"//evil.example":   false,
		`/\evil.example`:   false,
		"/\t/evil.example": false,
		"http://x/":        false,
	} {
		if got := LocalPath(p); got != want {
			t.Errorf("LocalPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSetCookieFlags(t *testing.T) {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "127.0.0.1:1111"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	SetCookie(w, r, "app_session", "", -1)
	c := w.Result().Cookies()[0]
	if c.Path != "/" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge >= 0 {
		t.Errorf("cookie = %+v", c)
	}
}

func TestCheckCSRF(t *testing.T) {
	s := New()
	good := s.CSRFToken("anon")
	form := func(token string) *http.Request {
		r := httptest.NewRequest("POST", "http://app.example/x", strings.NewReader(url.Values{"_csrf": {token}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	header := httptest.NewRequest("POST", "http://app.example/x", nil)
	header.Header.Set("X-CSRF-Token", good)
	sameOriginStale := form("stale")
	sameOriginStale.Header.Set("Sec-Fetch-Site", "same-origin")
	crossOriginStale := form("stale")
	crossOriginStale.Header.Set("Sec-Fetch-Site", "cross-site")
	multipart := httptest.NewRequest("POST", "http://app.example/x", strings.NewReader("--b\r\n"))
	multipart.Header.Set("Content-Type", "multipart/form-data; boundary=b")

	for name, tc := range map[string]struct {
		r    *http.Request
		want bool
	}{
		"Header":                    {header, true},
		"FormField":                 {form(good), true},
		"Missing":                   {form(""), false},
		"AnotherSessionsToken":      {form(s.CSRFToken("other")), false},
		"StaleFromThisOrigin":       {sameOriginStale, true},
		"StaleFromElsewhere":        {crossOriginStale, false},
		"MultipartWithoutTheHeader": {multipart, false},
	} {
		if got := s.CheckCSRF(tc.r, "anon"); got != tc.want {
			t.Errorf("%s: CheckCSRF = %v, want %v", name, got, tc.want)
		}
	}
}
