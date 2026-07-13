package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
)

func TestExtensionPairingHandshake(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	reqBody := `{"app":"monsender","requested_scopes":["read","write"]}`

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(reqBody)))
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["request_id"] == "" {
		t.Fatalf("no request_id: %s", rr.Body)
	}
	id := out["request_id"]

	// Approve via the operator handler (direct call bypasses CSRF middleware).
	areq := httptest.NewRequest("POST", "/settings/auth/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	srv.monsenderPairApprove(httptest.NewRecorder(), areq)

	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
	var st map[string]string
	_ = json.Unmarshal(srr.Body.Bytes(), &st)
	if len(st["token"]) != 32 || !srv.hasPairedToken("monsender") {
		t.Fatalf("claim failed: %v", st)
	}
	if srr.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("pairing status missing CORS header")
	}
	if srr.Header().Get("Cache-Control") != "no-store" {
		t.Error("the token response must be no-store")
	}
	tok := srv.cfg.Current().FindTokenByHash(config.HashToken(st["token"]))
	if tok == nil || !tok.HasScope(config.ScopeWrite) || !tok.HasScope(config.ScopeRead) {
		t.Errorf("issued token scopes wrong: %+v", tok)
	}

	// Re-pair guard.
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(reqBody)))
	if rr2.Code != http.StatusConflict {
		t.Errorf("re-pair guard code = %d, want 409", rr2.Code)
	}

	// Remove.
	srv.monsenderPairRemove(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/auth/pair/remove", nil))
	if srv.hasPairedToken("monsender") {
		t.Error("remove did not drop the extension token")
	}
}

// The monbooru peer name belongs to the dedicated pairing flow; an extension
// request under it would mint a phantom "connected to monbooru" state.
func TestExtPairRequestRejectsReservedApp(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	for _, app := range []string{"monbooru", "MonBooru"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"`+app+`"}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("app %q code = %d, want 400", app, rr.Code)
		}
	}
}

func TestExtPairingEmptyScopesMintsReadOnly(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monsender","requested_scopes":[]}`)))
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]
	if id == "" {
		t.Fatalf("no request_id: %s", rr.Body)
	}

	areq := httptest.NewRequest("POST", "/settings/auth/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	srv.monsenderPairApprove(httptest.NewRecorder(), areq)

	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
	var st map[string]string
	_ = json.Unmarshal(srr.Body.Bytes(), &st)
	tok := srv.cfg.Current().FindTokenByHash(config.HashToken(st["token"]))
	if tok == nil {
		t.Fatalf("no token minted: %v", st)
	}
	if !tok.HasScope(config.ScopeRead) || tok.HasScope(config.ScopeWrite) {
		t.Errorf("empty scopes should mint read-only, got %v", tok.Scopes)
	}
}

func TestMonsenderPairingRefreshesTokensOnTransition(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	tok, _ := config.GenerateToken("monsender (paired)", config.AllScopes)
	tok.Paired = "monsender"
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.monsenderPairingFragment(rr, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false", nil))
	if body := rr.Body.String(); !strings.Contains(body, `id="auth-tokens"`) || !strings.Contains(body, "hx-swap-oob") {
		t.Errorf("transition to paired should OOB-refresh the token list, got %q", body)
	}

	rr2 := httptest.NewRecorder()
	srv.monsenderPairingFragment(rr2, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=true", nil))
	if strings.Contains(rr2.Body.String(), "hx-swap-oob") {
		t.Errorf("no state change should not OOB the token list, got %q", rr2.Body.String())
	}
}

func TestExtPairTeardownRemovesLocally(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	tok, secret := config.GenerateToken("monbooru (paired)", config.AllScopes)
	tok.Paired = "monbooru"
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		c.Monbooru.APIToken = "x"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/v1/pair/remove", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	srv.extPairTeardown(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if srv.hasPairedToken("monbooru") || srv.cfg.Current().Monbooru.APIToken != "" {
		t.Error("teardown did not remove the monbooru pairing")
	}
}

func TestExtPairTeardownRejectsBadToken(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	// A real but non-paired token exists, so the guard must reject it too.
	tok, secret := config.GenerateToken("api", config.AllScopes)
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer nope", "Basic x", "Bearer " + secret} {
		req := httptest.NewRequest("POST", "/api/v1/pair/remove", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		srv.extPairTeardown(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status = %d, want 401", auth, w.Code)
		}
	}
}

func TestExtPairRequestTooManyPending(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	body := `{"app":"monsender","requested_scopes":["read"]}`
	for i := 0; i < pairMaxPending; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: code %d, want 200", i, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body)))
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("code = %d, want 429 once %d requests are pending", rr.Code, pairMaxPending)
	}
}

func TestExtPairStatusUnknownID(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/pair/status?id=nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", rr.Code)
	}
}

func TestMonsenderPairDeny(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monsender","requested_scopes":["read"]}`)))
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]

	dreq := httptest.NewRequest("POST", "/settings/auth/pair/"+id+"/deny", nil)
	dreq.SetPathValue("id", id)
	srv.monsenderPairDeny(httptest.NewRecorder(), dreq)

	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
	var st map[string]string
	_ = json.Unmarshal(srr.Body.Bytes(), &st)
	if st["status"] != "denied" || st["token"] != "" {
		t.Errorf("after deny: status=%q token=%q, want denied and no token", st["status"], st["token"])
	}
	if srv.hasPairedToken("monsender") {
		t.Error("a denied request must not mint a token")
	}
}

func TestMonsenderPairingEmphasizesPending(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/pair/request",
		strings.NewReader(`{"app":"monsender","requested_scopes":["read","write"]}`)))

	rr := httptest.NewRecorder()
	srv.monsenderPairingFragment(rr, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false", nil))
	if body := rr.Body.String(); !strings.Contains(body, `class="pairing-pending"`) || !strings.Contains(body, "requesting to pair") {
		t.Errorf("a pending request should be emphasized, got %q", body)
	}
}

func TestMonsenderPairingStopsPollingWhenPaired(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")

	// Unpaired: the panel polls so it surfaces requests and the claim flip.
	rr := httptest.NewRecorder()
	srv.monsenderPairingFragment(rr, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false", nil))
	if !strings.Contains(rr.Body.String(), "every 3s") {
		t.Errorf("unpaired panel should poll, got %q", rr.Body.String())
	}

	// Paired: no poll, so a confirm-gated Remove isn't detached mid-dialog by a
	// poll swap (which would drop the first click).
	tok, _ := config.GenerateToken("monsender (paired)", config.AllScopes)
	tok.Paired = "monsender"
	if err := srv.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rr2 := httptest.NewRecorder()
	srv.monsenderPairingFragment(rr2, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=true", nil))
	if strings.Contains(rr2.Body.String(), "every 3s") {
		t.Errorf("paired panel must not poll, got %q", rr2.Body.String())
	}
}
