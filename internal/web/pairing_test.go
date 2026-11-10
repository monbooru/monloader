package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/webui"
)

func TestMonbooruPairConnectFlow(t *testing.T) {
	approved := false
	var gotPeerToken, gotURL string
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/pair/request":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotPeerToken, _ = body["peer_token"].(string)
			gotURL, _ = body["url"].(string)
			_, _ = w.Write([]byte(`{"request_id":"r1","status":"pending"}`))
		case "/api/v1/pair/status":
			if approved {
				_, _ = w.Write([]byte(`{"status":"approved","token":"mb-issued"}`))
			} else {
				_, _ = w.Write([]byte(`{"status":"pending"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mb.Close()

	srv := newWebServer(t, mb.URL, "")

	wr := record(srv.monbooruPairConnect, httptest.NewRequest("POST", "/settings/monbooru/pair/connect", nil))
	if srv.getPairAttempt() == nil {
		t.Fatal("no pairing attempt after connect")
	}
	if !strings.Contains(wr.Body.String(), "pair/cancel") {
		t.Error("waiting fragment should carry a cancel button")
	}
	if strings.Contains(wr.Body.String(), ">waiting<") {
		t.Error("the panel already says it is waiting; a bare word beside it adds nothing")
	}
	if gotPeerToken == "" || gotURL == "" {
		t.Errorf("request omitted peer_token/url: token=%q url=%q", gotPeerToken, gotURL)
	}

	srv.monbooruPairPoll(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/monbooru/pair/poll", nil))
	if srv.hasPairedToken("monbooru") {
		t.Fatal("paired before approval")
	}

	approved = true
	cr := record(srv.monbooruPairPoll, httptest.NewRequest("POST", "/settings/monbooru/pair/poll", nil))
	if !srv.hasPairedToken("monbooru") || srv.cfg.Current().Monbooru.APIToken != "mb-issued" {
		t.Fatalf("not connected: paired=%v token=%q", srv.hasPairedToken("monbooru"), srv.cfg.Current().Monbooru.APIToken)
	}
	if !strings.Contains(cr.Body.String(), `id="auth-tokens"`) || !strings.Contains(cr.Body.String(), "hx-swap-oob") {
		t.Errorf("connecting should OOB-refresh the token list, got %q", cr.Body.String())
	}
	if srv.cfg.Current().Auth.Tokens.ByHash(apitoken.Hash(gotPeerToken)) == nil {
		t.Error("offered peer token not stored as a monloader token")
	}

	srv.monbooruPairRemove(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/monbooru/pair/remove", nil))
	if srv.hasPairedToken("monbooru") || srv.cfg.Current().Monbooru.APIToken != "" {
		t.Error("remove did not tear down the pairing")
	}
}

func TestMonbooruRePairNeedsNoUnpair(t *testing.T) {
	issued := "mb-first"
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/pair/request":
			_, _ = w.Write([]byte(`{"request_id":"r1","status":"pending"}`))
		case "/api/v1/pair/status":
			_, _ = w.Write([]byte(`{"status":"approved","token":"` + issued + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mb.Close()

	srv := newWebServer(t, mb.URL, "")
	connect := func() {
		srv.monbooruPairConnect(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/monbooru/pair/connect", nil))
		srv.monbooruPairPoll(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/monbooru/pair/poll", nil))
	}
	paired := func() int {
		n := 0
		for _, t := range srv.cfg.Current().Auth.Tokens {
			if t.Paired == "monbooru" {
				n++
			}
		}
		return n
	}

	connect()
	if paired() != 1 || srv.cfg.Current().Monbooru.APIToken != "mb-first" {
		t.Fatalf("first pairing did not commit: tokens=%d api_token=%q", paired(), srv.cfg.Current().Monbooru.APIToken)
	}

	issued = "mb-second"
	connect()
	if paired() != 1 {
		t.Errorf("paired tokens = %d, want the re-pair to have replaced the old one", paired())
	}
	if got := srv.cfg.Current().Monbooru.APIToken; got != "mb-second" {
		t.Errorf("api token = %q, want the re-paired one", got)
	}

	w := httptest.NewRecorder()
	srv.render(w, "monbooru_pair", srv.monbooruPairData(httptest.NewRequest("GET", "/settings", nil), ""))
	if !strings.Contains(w.Body.String(), "pair/connect") {
		t.Errorf("a paired card should offer reconnect, got %q", w.Body.String())
	}
}

func TestMonbooruPairingForgetsAPausedLink(t *testing.T) {
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/pair/request":
			_, _ = w.Write([]byte(`{"request_id":"r1","status":"pending"}`))
		case "/api/v1/pair/status":
			_, _ = w.Write([]byte(`{"status":"approved","token":"mb-token"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	pause := func() {
		mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.Paused = true })
	}

	pause()
	srv.monbooruPairConnect(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/monbooru/pair/connect", nil))
	srv.monbooruPairPoll(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/monbooru/pair/poll", nil))
	if c := srv.cfg.Current().Monbooru; c.APIToken != "mb-token" || c.Paused {
		t.Errorf("after a fresh pairing token=%q paused=%v, want the new token and the hold gone", c.APIToken, c.Paused)
	}

	pause()
	if err := srv.removePairingLocal(contract.AppMonbooru); err != nil {
		t.Fatal(err)
	}
	if srv.cfg.Current().Monbooru.Paused {
		t.Error("a removed pairing kept its link hold")
	}
}

func TestAChangedMonbooruLinkForgetsItsCachedStatus(t *testing.T) {
	srv := newWebServer(t, "http://mb.test", "")
	srv.statusMu.Lock()
	srv.monbooruConn = connUnpaired
	srv.statusMu.Unlock()
	if err := srv.cfg.Update(func(c *config.Config) error { c.Monbooru.APIToken = "fresh"; return nil }); err != nil {
		t.Fatal(err)
	}
	if status, _ := srv.monbooruStatusSeed(); status != connChecking {
		t.Errorf("a new token: status %q, want %q", status, connChecking)
	}

	srv.statusMu.Lock()
	srv.monbooruConn = webui.ConnOK
	srv.statusMu.Unlock()
	if err := srv.cfg.Update(func(c *config.Config) error { c.Downloader.Concurrency = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	if status, _ := srv.monbooruStatusSeed(); status != webui.ConnOK {
		t.Errorf("an unrelated save: status %q, want it kept", status)
	}
}

func TestAProbeThatOutlivesAPairingRemovalIsNotKept(t *testing.T) {
	var srv *Server
	var once sync.Once
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			if err := srv.cfg.Update(func(c *config.Config) error { c.Monbooru.APIToken = ""; return nil }); err != nil {
				t.Error(err)
			}
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1.0.0"}`))
	}))
	defer peer.Close()
	srv = newWebServer(t, peer.URL, "tok")

	if status, _ := srv.monbooruStatusCached(context.Background()); status != connUnpaired {
		t.Errorf("answer: status %q, want %q", status, connUnpaired)
	}
	if status, _ := srv.monbooruStatusSeed(); status != connUnpaired {
		t.Errorf("next page: status %q, want %q", status, connUnpaired)
	}
}

func TestTakePairAttempt(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	att := &outboundPair{requestID: "r"}
	srv.setPairAttempt(att)
	if !srv.takePairAttempt(att) {
		t.Fatal("first take should win")
	}
	if srv.takePairAttempt(att) {
		t.Error("a second take of the same attempt must lose, so no double-commit")
	}
	if srv.getPairAttempt() != nil {
		t.Error("attempt should be cleared after a take")
	}
}

func TestMonbooruPairPollResetsWhenRequestGone(t *testing.T) {
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","error":"unknown pairing request"}`))
	}))
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	srv.setPairAttempt(&outboundPair{requestID: "gone"})

	w := record(srv.monbooruPairPoll, httptest.NewRequest("POST", "/settings/monbooru/pair/poll", nil))

	if srv.getPairAttempt() != nil {
		t.Error("attempt not dropped after monbooru forgot the request")
	}
	if !strings.Contains(w.Body.String(), "connect again") {
		t.Errorf("operator not told to reconnect, got %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Connect to monbooru") {
		t.Errorf("fragment should offer the connect button, got %q", w.Body.String())
	}
}

func TestMonbooruPairCancel(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	srv.setPairAttempt(&outboundPair{requestID: "r"})

	w := record(srv.monbooruPairCancel, httptest.NewRequest("POST", "/settings/monbooru/pair/cancel", nil))

	if srv.getPairAttempt() != nil {
		t.Error("cancel did not drop the pending attempt")
	}
	if !strings.Contains(w.Body.String(), "Connect to monbooru") {
		t.Errorf("fragment should offer the connect button after cancel, got %q", w.Body.String())
	}
}

func TestMonbooruPairRemoveWarnsWhenUnreachable(t *testing.T) {
	srv := newWebServer(t, "http://ml", "")
	pairMBWith(t, srv, "peer-secret")

	w := record(srv.monbooruPairRemove, httptest.NewRequest("POST", "/settings/monbooru/pair/remove", nil))

	if srv.hasPairedToken("monbooru") {
		t.Error("local pairing not removed")
	}
	if !strings.Contains(w.Body.String(), "could not reach monbooru") {
		t.Errorf("unreachable peer should warn the operator, got %q", w.Body.String())
	}
}

func TestMonbooruPairRemoveStopsAtAFailedSave(t *testing.T) {
	teardowns := 0
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pair/remove" {
			teardowns++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	pairMBWith(t, srv, "x")
	if err := os.WriteFile(srv.configPath, []byte("not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}

	w := record(srv.monbooruPairRemove, httptest.NewRequest("POST", "/settings/monbooru/pair/remove", nil))
	if teardowns != 0 {
		t.Errorf("the peer was torn down %d times over a save that failed", teardowns)
	}
	if !srv.hasPairedToken("monbooru") || !strings.Contains(w.Body.String(), "Could not save") {
		t.Errorf("paired = %v, body %q", srv.hasPairedToken("monbooru"), w.Body.String())
	}
}

func TestPairingPollsHoldWhileAConfirmIsOpen(t *testing.T) {
	srv := newWebServer(t, "http://mb", "")
	srv.setPairAttempt(&outboundPair{requestID: "r"})
	w := httptest.NewRecorder()
	srv.render(w, "monbooru_pair", srv.monbooruPairData(httptest.NewRequest("GET", "/", nil), ""))
	if !strings.Contains(w.Body.String(), `hx-trigger="every 2s [!confirmOpen()]"`) {
		t.Errorf("the monbooru pairing poll does not hold: %q", w.Body.String())
	}

	w = record(srv.monsenderPairingFragment, httptest.NewRequest("GET", "/internal/monsender-pairing?paired=false", nil))
	if !strings.Contains(w.Body.String(), `hx-trigger="every 3s [!confirmOpen()]"`) {
		t.Errorf("the monsender pairing poll does not hold: %q", w.Body.String())
	}
}

func TestPairSelfURLUsesBindPort(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BindAddress = "0.0.0.0:18081"
	cfg.Server.BaseURL = "http://localhost:8081"
	if got, want := pairSelfURL(cfg), "http://localhost:18081"; got != want {
		t.Errorf("pairSelfURL = %q, want %q", got, want)
	}
}
