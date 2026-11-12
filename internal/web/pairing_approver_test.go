package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/shared/pairing"
)

func offerPair(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	rr := record(h.ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body)))
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["request_id"] == "" {
		t.Fatalf("no request_id: %s", rr.Body)
	}
	return out["request_id"]
}

func pairStatus(h http.Handler, id string) map[string]string {
	rr := record(h.ServeHTTP, httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func TestExtensionPairingHandshake(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	reqBody := `{"app":"monsender","requested_scopes":["read","write"]}`

	id := offerPair(t, h, reqBody)

	if pending := pairStatus(h, id); pending["status"] != "pending" || pending["token"] != "" {
		t.Fatalf("pending poll = %v, want the state and no token", pending)
	}

	areq := httptest.NewRequest("POST", "/settings/auth/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	srv.monsenderPairApprove(httptest.NewRecorder(), areq)

	srr := record(h.ServeHTTP, httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
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
	tok := srv.cfg.Current().Auth.Tokens.ByHash(apitoken.Hash(st["token"]))
	if tok == nil || !tok.HasScope(config.ScopeWrite) || !tok.HasScope(config.ScopeRead) {
		t.Errorf("issued token scopes wrong: %+v", tok)
	}

	if second := pairStatus(h, id); second["token"] != "" {
		t.Errorf("second poll = %v, want no second token", second)
	}
	if n := len(srv.cfg.Current().Auth.Tokens); n != 1 {
		t.Errorf("tokens minted = %d, want 1", n)
	}

	rr2 := record(h.ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(reqBody)))
	if rr2.Code != http.StatusConflict {
		t.Errorf("re-pair guard code = %d, want 409", rr2.Code)
	}

	srv.monsenderPairRemove(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/auth/pair/remove", nil))
	if srv.hasPairedToken("monsender") {
		t.Error("remove did not drop the extension token")
	}
}

func TestMonsenderPairRemoveReportsAFailedSave(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	storeCfg(srv, func(c *config.Config) {
		c.Auth.Tokens = append(c.Auth.Tokens, apitoken.Token{ID: "m", Name: "monsender (paired)", Paired: "monsender"})
	})
	if err := os.WriteFile(srv.configPath, []byte("not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	w := record(srv.monsenderPairRemove, httptest.NewRequest("POST", "/settings/auth/pair/remove", nil))
	if !strings.Contains(w.Body.String(), "Could not save") {
		t.Errorf("the panel says nothing about the failed save: %q", w.Body.String())
	}
}

func TestExtPairRequestRejectsReservedApp(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	for _, app := range []string{"monbooru", "MonBooru"} {
		rr := record(h.ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"`+app+`"}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("app %q code = %d, want 400", app, rr.Code)
		}
	}
}

func TestExtPairingEmptyScopesMintsReadOnly(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()

	id := offerPair(t, h, `{"app":"monsender","requested_scopes":[]}`)

	areq := httptest.NewRequest("POST", "/settings/auth/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	srv.monsenderPairApprove(httptest.NewRecorder(), areq)

	st := pairStatus(h, id)
	tok := srv.cfg.Current().Auth.Tokens.ByHash(apitoken.Hash(st["token"]))
	if tok == nil {
		t.Fatalf("no token minted: %v", st)
	}
	if !tok.HasScope(config.ScopeRead) || tok.HasScope(config.ScopeWrite) {
		t.Errorf("empty scopes should mint read-only, got %v", tok.Scopes)
	}
}

func TestPairingTableNamesTheGrantNotTheAsk(t *testing.T) {
	cases := []struct {
		name, requested, want, absent string
	}{
		{"an unimplemented scope", `["read","write","admin"]`, "read write", "admin"},
		{"no scopes at all", `[]`, "read", "write"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newWebServer(t, "http://mb", "")
			h := srv.Handler()
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request",
				strings.NewReader(`{"app":"monsender","requested_scopes":`+tc.requested+`}`)))
			if rr.Code != http.StatusOK {
				t.Fatalf("pair request status = %d: %s", rr.Code, rr.Body)
			}

			frag := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing", nil))
			body := frag.Body.String()
			if !strings.Contains(body, ">"+tc.want+"<") {
				t.Errorf("scopes column should read %q:\n%s", tc.want, truncate(body))
			}
			if strings.Contains(body, tc.absent) {
				t.Errorf("scopes column names %q, which approving does not grant:\n%s", tc.absent, truncate(body))
			}
		})
	}
}

func TestMonsenderPairingRefreshesTokensOnTransition(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	tok, _ := apitoken.New("monsender (paired)", config.AllScopes)
	tok.Paired = "monsender"
	addToken(t, srv, tok)

	rr := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false", nil))
	if body := rr.Body.String(); !strings.Contains(body, `id="auth-tokens"`) || !strings.Contains(body, "hx-swap-oob") {
		t.Errorf("transition to paired should OOB-refresh the token list, got %q", body)
	}

	rr2 := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=true", nil))
	if strings.Contains(rr2.Body.String(), "hx-swap-oob") {
		t.Errorf("no state change should not OOB the token list, got %q", rr2.Body.String())
	}
}

func TestExtPairTeardownRemovesLocally(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	secret := pairMBWith(t, srv, "x")

	req := httptest.NewRequest("POST", "/api/v1/pair/remove", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	w := record(srv.extPairTeardown, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if srv.hasPairedToken("monbooru") || srv.cfg.Current().Monbooru.APIToken != "" {
		t.Error("teardown did not remove the monbooru pairing")
	}
}

func TestExtPairTeardownRemovesOnlyTheCallingToken(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	laptop, secret := apitoken.New("monsender (paired)", config.AllScopes)
	laptop.Paired = "monsender"
	desktop, _ := apitoken.New("monsender desktop (paired)", config.AllScopes)
	desktop.Paired = "monsender"
	addToken(t, srv, laptop)
	addToken(t, srv, desktop)

	req := httptest.NewRequest("POST", "/api/v1/pair/remove", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	if w := record(srv.extPairTeardown, req); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	toks := srv.cfg.Current().Auth.Tokens
	if toks.ByID(laptop.ID) != nil || toks.ByID(desktop.ID) == nil {
		t.Errorf("want only the calling token gone, have %v", toks)
	}
}

func TestExtPairTeardownRejectsBadToken(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")

	tok, secret := apitoken.New("api", config.AllScopes)
	addToken(t, srv, tok)
	for _, auth := range []string{"", "Bearer nope", "Basic x", "Bearer " + secret} {
		req := httptest.NewRequest("POST", "/api/v1/pair/remove", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := record(srv.extPairTeardown, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status = %d, want 401", auth, w.Code)
		}
	}
}

func TestExtPairRequestTooManyPending(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()

	body := func(i int) string {
		return `{"app":"app-` + itoa(int64(i)) + `","requested_scopes":["read"]}`
	}
	for i := 0; i < pairing.MaxPending; i++ {
		rr := record(h.ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body(i))))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: code %d, want 200", i, rr.Code)
		}
	}
	rr := record(h.ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body(pairing.MaxPending))))
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("code = %d, want 429 once %d requests are pending", rr.Code, pairing.MaxPending)
	}
}

func TestExtPairRequestReplacesARepeatOffer(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	requestID := func() string {
		return offerPair(t, h, `{"app":"monsender","requested_scopes":["read"]}`)
	}
	first, second := requestID(), requestID()
	if first == "" || second == "" || first == second {
		t.Fatalf("request ids %q / %q, want the repeat under a new id", first, second)
	}
	if n := len(srv.pairs.ListPending()); n != 1 {
		t.Errorf("%d pending rows, want one the operator can act on", n)
	}
	rr := record(h.ServeHTTP, httptest.NewRequest("GET", "/api/v1/pair/status?id="+first, nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("the replaced id answers %d, want 404", rr.Code)
	}
}

func TestExtPairStatusUnknownID(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	rr := record(srv.Handler().ServeHTTP, httptest.NewRequest("GET", "/api/v1/pair/status?id=nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", rr.Code)
	}
}

func TestMonsenderPairDeny(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	id := offerPair(t, h, `{"app":"monsender","requested_scopes":["read"]}`)

	dreq := httptest.NewRequest("POST", "/settings/auth/pair/"+id+"/deny", nil)
	dreq.SetPathValue("id", id)
	srv.monsenderPairDeny(httptest.NewRecorder(), dreq)

	st := pairStatus(h, id)
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

	rr := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false", nil))
	if body := rr.Body.String(); !strings.Contains(body, `class="pairing-pending"`) || !strings.Contains(body, "wants to pair") || strings.Contains(body, "To set up the monsender extension") {
		t.Errorf("a pending request should be emphasized, got %q", body)
	}
}

func TestAnApprovedExtensionAwaitsItsTokenInTheColumn(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	id := offerPair(t, h, `{"app":"monsender","requested_scopes":["read"]}`)
	const awaiting = "Approved. Waiting for monsender to finish."
	settings := func() string {
		rec := record(h.ServeHTTP, httptest.NewRequest("GET", "http://127.0.0.1/settings", nil))
		return rec.Body.String()
	}

	areq := httptest.NewRequest("POST", "/settings/auth/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	approved := record(srv.monsenderPairApprove, areq)
	if body := approved.Body.String(); !strings.Contains(body, awaiting) || strings.Contains(body, "To set up the monsender extension") {
		t.Errorf("after approve the column reads %q", body)
	}
	if body := settings(); !strings.Contains(body, awaiting) {
		t.Errorf("a reload lost the awaiting line")
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
	if body := settings(); strings.Contains(body, awaiting) || !strings.Contains(body, "Paired with the extension.") {
		t.Errorf("after the claim the column still awaits")
	}
}

func TestMonsenderPairingStopsPollingWhenPaired(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")

	rr := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false", nil))
	if !strings.Contains(rr.Body.String(), "every 3s") {
		t.Errorf("unpaired panel should poll, got %q", rr.Body.String())
	}

	tok, _ := apitoken.New("monsender (paired)", config.AllScopes)
	tok.Paired = "monsender"
	addToken(t, srv, tok)
	rr2 := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=true", nil))
	if strings.Contains(rr2.Body.String(), "every 3s") {
		t.Errorf("paired panel must not poll, got %q", rr2.Body.String())
	}
}

func TestExtPairRequestRefusesAnAppNameThatCannotBeATokenName(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	h := srv.Handler()
	for _, app := range []string{"two words", "line\nbreak", strings.Repeat("a", 33), "app/x"} {
		body, _ := json.Marshal(map[string]string{"app": app})
		rr := record(h.ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", bytes.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("app %q code = %d, want 400", app, rr.Code)
		}
	}
	if n := len(srv.pairs.ListPending()); n != 0 {
		t.Errorf("%d requests filed, want none", n)
	}
}

func TestExtPairRequestRecordsTheProxiedClient(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	req := httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monsender"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if p := srv.pairs.ListPending(); len(p) != 1 || p[0].Source != "203.0.113.7" {
		t.Errorf("pending = %+v, want the forwarded client as the source", p)
	}
}

func TestARepeatMintReplacesTheAppsToken(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	req := pairing.Request{App: "monsender", Scopes: []string{"read"}}
	if _, err := srv.mintExtToken(req); err != nil {
		t.Fatal(err)
	}
	second, err := srv.mintExtToken(req)
	if err != nil {
		t.Fatal(err)
	}
	toks := srv.cfg.Current().Auth.Tokens
	if len(toks) != 1 || toks.ByHash(apitoken.Hash(second)) == nil {
		t.Errorf("tokens = %d, want only the second", len(toks))
	}
}

func TestTheLightPollSwapsThePairNoticeOnlyWhenTheRequestsChange(t *testing.T) {
	srv, ts := serveWeb(t, "")
	_, body := get(t, ts, "/internal/monbooru-status?seen=")
	if strings.Contains(body, `id="pair-notice"`) {
		t.Errorf("nothing waits, yet the poll swaps the notice: %s", body)
	}

	record(srv.Handler().ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monsender","requested_scopes":["read"]}`)))
	key := srv.pairNotice().Key()
	_, body = get(t, ts, "/internal/monbooru-status?seen=")
	for _, want := range []string{`<a id="pair-notice" class="pair-notice" href="/settings#pairing" hx-swap-oob="true"`, "[! monsender wants to pair]", `"seen":"` + key + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("poll lacks %q: %s", want, body)
		}
	}
	if _, body = get(t, ts, "/internal/monbooru-status?seen="+url.QueryEscape(key)); strings.Contains(body, `id="pair-notice"`) {
		t.Errorf("an unchanged set is swapped again: %s", body)
	}
}

func TestPagesCarryThePairNoticeAndSettingsItsCount(t *testing.T) {
	srv, ts := serveWeb(t, "")
	_, body := get(t, ts, "/queue")
	if !strings.Contains(body, `<a id="pair-notice" class="pair-notice" href="/settings#pairing" hidden></a>`) {
		t.Errorf("the queue nav lacks the hidden notice: %s", body)
	}

	record(srv.Handler().ServeHTTP, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monsender","requested_scopes":["read"]}`)))
	for _, path := range []string{"/", "/queue"} {
		if _, body := get(t, ts, path); !strings.Contains(body, `aria-label="monsender wants to pair"`) {
			t.Errorf("%s lacks the notice", path)
		}
	}
	_, body = get(t, ts, "/settings")
	for _, want := range []string{`<span id="settings-pair-count" class="nav-count"> (1)</span>`, `<td data-label="Expires">5 min</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings lacks %q", want)
		}
	}
}

func TestTheExtensionCardPollSwapsTheCountOnlyOnAChange(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monsender","requested_scopes":["read"]}`)))
	key := srv.pairNotice().Key()

	rr := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false&pair=", nil))
	if !strings.Contains(rr.Body.String(), `<span id="settings-pair-count" class="nav-count" hx-swap-oob="true"> (1)</span>`) {
		t.Errorf("a changed set does not swap the count: %s", rr.Body)
	}
	same := record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false&pair="+url.QueryEscape(key), nil))
	if strings.Contains(same.Body.String(), "hx-swap-oob") {
		t.Errorf("an unchanged set is swapped again: %s", same.Body)
	}
}

func TestTheSettingsPageShowsThePairingsGrantNotItsAsk(t *testing.T) {
	srv := stubbedServer(t, "")
	ts := newTestServer(t, srv)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monsender","requested_scopes":["nonsense"]}`)))
	_, page := get(t, ts, "/settings")
	if strings.Contains(page, "nonsense") {
		t.Error("the first render shows what was asked, not what the approval grants")
	}
	if !strings.Contains(page, `<td data-label="Scopes">`+config.ScopeRead+"</td>") {
		t.Errorf("the pending row should name the read-only grant, got %q", truncate(page))
	}
}
