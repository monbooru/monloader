package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/logx"
)

const (
	pairPendingTTL = 5 * time.Minute
	pairMaxPending = 16
)

type pairState string

const (
	pairPending  pairState = "pending"
	pairApproved pairState = "approved"
	pairDenied   pairState = "denied"
)

// pairReq is one in-flight extension pairing, held in memory until claimed or
// aged out. Nothing is issued until the claim, so an approval the extension
// never collects mints no token.
type pairReq struct {
	ID        string
	App       string
	Source    string
	Scopes    []string
	State     pairState
	Claimed   bool
	CreatedAt time.Time
}

type pairStore struct {
	mu sync.Mutex
	m  map[string]*pairReq
}

func newPairStore() *pairStore { return &pairStore{m: map[string]*pairReq{}} }

func pairID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (ps *pairStore) sweepLocked() {
	cutoff := time.Now().Add(-pairPendingTTL)
	for id, r := range ps.m {
		if r.CreatedAt.Before(cutoff) {
			delete(ps.m, id)
		}
	}
}

func (ps *pairStore) create(app, source string, scopes []string) (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	pending := 0
	for _, r := range ps.m {
		if r.State == pairPending {
			pending++
		}
	}
	if pending >= pairMaxPending {
		return "", false
	}
	id := pairID()
	ps.m[id] = &pairReq{ID: id, App: app, Source: source, Scopes: scopes, State: pairPending, CreatedAt: time.Now()}
	return id, true
}

func (ps *pairStore) get(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		return *r, true
	}
	return pairReq{}, false
}

func (ps *pairStore) listPending() []pairReq {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	var out []pairReq
	for _, r := range ps.m {
		if r.State == pairPending {
			out = append(out, *r)
		}
	}
	return out
}

func (ps *pairStore) setState(id string, st pairState) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairPending {
		return false
	}
	r.State = st
	return true
}

func (ps *pairStore) claim(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairApproved || r.Claimed {
		return pairReq{}, false
	}
	r.Claimed = true
	return *r, true
}

func (ps *pairStore) remove(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.m, id)
}

// setPairCORS lets the browser extension (a distinct origin) reach the pairing
// endpoints; the operator approval in monloader is the real gate.
func setPairCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Vary", "Origin")
}

func pairJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// removePairingLocal drops the token issued to a peer and, for the monbooru
// pairing, the credential used to push back. It removes only locally - callers
// that want the far end gone too notify the peer separately.
func (s *Server) removePairingLocal(peer string) error {
	return s.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == peer })
		if peer == "monbooru" {
			c.Monbooru.APIToken = ""
		}
		return nil
	})
}

// extPairTeardown lets a paired peer drop the pairing on this side too, so one
// "remove pairing" tears down both ends. It authenticates with the peer's token
// and removes only locally - it never calls back, which would loop.
func (s *Server) extPairTeardown(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	cfg := s.cfg.Current()
	tok := cfg.FindTokenByHash(config.HashToken(strings.TrimPrefix(got, prefix)))
	if !strings.HasPrefix(got, prefix) || tok == nil || tok.Paired == "" {
		pairJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized", "error": "pairing token required"})
		return
	}
	if err := s.removePairingLocal(tok.Paired); err != nil {
		pairJSON(w, http.StatusInternalServerError, map[string]string{"code": "remove_failed", "error": err.Error()})
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", tok.Paired)
	pairJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// extPairRequest receives a pairing offer from the extension. It issues nothing;
// the operator approves in settings, after which the extension claims its token.
func (s *Server) extPairRequest(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	var body struct {
		App             string   `json:"app"`
		RequestedScopes []string `json:"requested_scopes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.App == "" {
		pairJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "app and a JSON body are required"})
		return
	}
	if s.hasPairedToken(body.App) {
		pairJSON(w, http.StatusConflict, map[string]string{"code": "already_paired", "error": "already paired with " + body.App + "; remove the existing pairing first"})
		return
	}
	id, ok := s.pairs.create(body.App, r.RemoteAddr, body.RequestedScopes)
	if !ok {
		pairJSON(w, http.StatusTooManyRequests, map[string]string{"code": "too_many_requests", "error": "too many pending pairing requests"})
		return
	}
	logx.Infof("pairing: request from %s", body.App)
	pairJSON(w, http.StatusOK, map[string]string{"request_id": id, "status": "pending"})
}

// extPairStatus reports a request's state; on the first poll after approval it
// mints the extension's token and returns the secret once.
func (s *Server) extPairStatus(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	req, ok := s.pairs.get(r.URL.Query().Get("id"))
	if !ok {
		pairJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "error": "unknown pairing request"})
		return
	}
	if req.State != pairApproved {
		pairJSON(w, http.StatusOK, map[string]string{"status": string(req.State)})
		return
	}
	cr, won := s.pairs.claim(req.ID)
	if !won {
		pairJSON(w, http.StatusOK, map[string]string{"status": "approved"})
		return
	}
	secret, err := s.mintExtToken(cr)
	if err != nil {
		pairJSON(w, http.StatusInternalServerError, map[string]string{"code": "mint_failed", "error": err.Error()})
		return
	}
	s.pairs.remove(cr.ID)
	w.Header().Set("Cache-Control", "no-store")
	pairJSON(w, http.StatusOK, map[string]string{"status": "approved", "token": secret})
}

func (s *Server) mintExtToken(req pairReq) (string, error) {
	scopes := filterScopes(req.Scopes)
	if len(scopes) == 0 {
		// No scopes requested: grant the least, not everything.
		scopes = []string{config.ScopeRead}
	}
	tok, secret := config.GenerateToken(req.App+" (paired)", scopes)
	tok.Paired = req.App
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		return "", err
	}
	logx.Infof("pairing: issued token to %s", req.App)
	return secret, nil
}

func (s *Server) extPairData(r *http.Request) map[string]any {
	return map[string]any{
		"Pending":   s.pairs.listPending(),
		"Paired":    s.hasPairedToken("monsender"),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}

func (s *Server) monsenderPairingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.extPairData(r)
	paired, _ := data["Paired"].(bool)
	s.render(w, "monsender_pairing", data)
	// The poll carries the browser's last-rendered paired state; when it flips
	// (the extension claimed, or the pairing was removed) refresh the token list too.
	if was := r.URL.Query().Get("paired"); was != "" && (was == "true") != paired {
		s.renderAuthTokensOOB(w, r)
	}
}

func (s *Server) monsenderPairApprove(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if s.pairs.setState(r.PathValue("id"), pairApproved) {
		logx.Infof("pairing: approved extension request %s", r.PathValue("id"))
	}
	s.render(w, "monsender_pairing", s.extPairData(r))
}

func (s *Server) monsenderPairDeny(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.pairs.setState(r.PathValue("id"), pairDenied)
	s.render(w, "monsender_pairing", s.extPairData(r))
}

func (s *Server) monsenderPairRemove(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if err := s.removePairingLocal("monsender"); err != nil {
		logx.Errorf("pairing: remove failed: %v", err)
	}
	s.render(w, "monsender_pairing", s.extPairData(r))
	s.renderAuthTokensOOB(w, r)
}
