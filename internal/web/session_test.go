package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLoginPageRedirectsWhenNoPassword(t *testing.T) {
	_, ts := serveProc(t, noopProc{})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("login with no password set should redirect to /, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLogoutClearsSession(t *testing.T) {
	srv, ts := serveWeb(t, "secret")

	id := srv.sessions.Create(7)
	cookie := &http.Cookie{Name: "monloader_session", Value: id}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", ts.URL+"/logout", strings.NewReader("_csrf="+srv.sessions.CSRFToken(id)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("logout status = %d, want 303", resp.StatusCode)
	}
	if srv.sessions.Valid(id) {
		t.Error("session should be deleted after logout")
	}
}

func TestSweepSessionsStopsWithItsContext(t *testing.T) {
	srv := serverWith(t, noopProc{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.SweepSessions(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep outlived its context")
	}
}
