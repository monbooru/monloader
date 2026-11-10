package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGateAdmitsBySessionAndSendsTheRestToTheLogin(t *testing.T) {
	s := New()
	valid := s.Create(1)
	loginOn := true
	var seen string
	h := s.Gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = FromContext(r.Context()) }),
		"app_session", func() bool { return loginOn }, func(p string) bool { return p == "/login" })

	for _, tc := range []struct {
		name, method, path, cookie string
		htmx                       bool
		status                     int
		location, session          string
	}{
		{name: "valid", method: "GET", path: "/queue", cookie: valid, status: 200, session: valid},
		{name: "open", method: "GET", path: "/login", status: 200, session: Anon},
		{name: "api", method: "POST", path: "/api/v1/queue", status: 200},
		{name: "page", method: "GET", path: "/queue?x=1", cookie: "stale", status: 303, location: "/login?next=%2Fqueue%3Fx%3D1"},
		{name: "root", method: "GET", path: "/", status: 303, location: "/login"},
		{name: "post", method: "POST", path: "/queue", status: 303, location: "/login"},
		{name: "htmx", method: "GET", path: "/queue", htmx: true, status: 401},
	} {
		seen = "unset"
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "app_session", Value: tc.cookie})
		}
		if tc.htmx {
			r.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Location") != tc.location {
			t.Errorf("%s: %d %q, want %d %q", tc.name, w.Code, w.Header().Get("Location"), tc.status, tc.location)
		}
		if want := tc.session; tc.status == 200 && seen != want {
			t.Errorf("%s: session %q, want %q", tc.name, seen, want)
		}
	}

	loginOn = false
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/queue", nil))
	if seen != Anon {
		t.Errorf("login off: session %q, want %q", seen, Anon)
	}
}

func TestGuardChecksOnlyStateChangingPageRequests(t *testing.T) {
	s := New()
	h := s.Gate(s.Guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), func(r *http.Request) bool { return r.URL.Path == "/mount" }),
		"app_session", func() bool { return false }, func(string) bool { return false })
	withToken := httptest.NewRequest("POST", "/save", nil)
	withToken.Header.Set("X-CSRF-Token", s.CSRFToken(Anon))
	for name, tc := range map[string]struct {
		r    *http.Request
		want int
	}{
		"Get":       {httptest.NewRequest("GET", "/save", nil), 200},
		"Api":       {httptest.NewRequest("POST", "/api/v1/queue", nil), 200},
		"Exempt":    {httptest.NewRequest("POST", "/mount", nil), 200},
		"Token":     {withToken, 200},
		"Missing":   {httptest.NewRequest("POST", "/save", nil), 403},
		"DeleteBad": {httptest.NewRequest("DELETE", "/save", nil), 403},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, tc.r)
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d", name, w.Code, tc.want)
		}
	}
}
