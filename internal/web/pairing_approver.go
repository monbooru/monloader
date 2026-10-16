package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/pairing"
)

// Any origin is allowed, the extension's included: the operator's approval
// is the real gate.
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

func pairError(w http.ResponseWriter, status int, code, msg string) {
	pairJSON(w, status, map[string]string{"code": code, "error": msg})
}

func (s *Server) removePairingLocal(peer string) error {
	return s.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == peer })
		if peer == "monbooru" {
			c.Monbooru.APIToken = ""
		}
		return nil
	})
}

// Removes only locally: calling the peer back would loop.
func (s *Server) extPairTeardown(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	cfg := s.cfg.Current()
	tok := cfg.FindTokenByHash(config.HashToken(strings.TrimPrefix(got, prefix)))
	if !strings.HasPrefix(got, prefix) || tok == nil || tok.Paired == "" {
		pairError(w, http.StatusUnauthorized, "unauthorized", "pairing token required")
		return
	}
	if err := s.removePairingLocal(tok.Paired); err != nil {
		pairError(w, http.StatusInternalServerError, "remove_failed", err.Error())
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", tok.Paired)
	pairJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) extPairRequest(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	var body struct {
		App             string   `json:"app"`
		RequestedScopes []string `json:"requested_scopes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.App == "" {
		pairError(w, http.StatusBadRequest, "invalid_request", "app and a JSON body are required")
		return
	}
	// monbooru's pairing has its own flow; an extension approved under
	// the name would show a phantom connection.
	if strings.EqualFold(body.App, "monbooru") {
		pairError(w, http.StatusBadRequest, "invalid_request", "app name \"monbooru\" is reserved")
		return
	}
	if s.hasPairedToken(body.App) {
		pairError(w, http.StatusConflict, "already_paired", "already paired with "+body.App+"; remove the existing pairing first")
		return
	}
	id, ok := s.pairs.Create(pairing.Request{App: body.App, Source: r.RemoteAddr, Scopes: body.RequestedScopes})
	if !ok {
		pairError(w, http.StatusTooManyRequests, "too_many_requests", "too many pending pairing requests")
		return
	}
	logx.Infof("pairing: request from %s", body.App)
	pairJSON(w, http.StatusOK, map[string]string{"request_id": id, "status": string(pairing.Pending)})
}

func (s *Server) extPairStatus(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	req, ok := s.pairs.Get(r.URL.Query().Get("id"))
	if !ok {
		pairError(w, http.StatusNotFound, "not_found", "unknown pairing request")
		return
	}
	if req.State != pairing.Approved {
		pairJSON(w, http.StatusOK, map[string]string{"status": string(req.State)})
		return
	}
	cr, won := s.pairs.Claim(req.ID)
	if !won {
		pairJSON(w, http.StatusOK, map[string]string{"status": string(pairing.Approved)})
		return
	}
	secret, err := s.mintExtToken(cr)
	if err != nil {
		s.pairs.Unclaim(cr.ID)
		pairError(w, http.StatusInternalServerError, "mint_failed", err.Error())
		return
	}
	s.pairs.Remove(cr.ID)
	w.Header().Set("Cache-Control", "no-store")
	pairJSON(w, http.StatusOK, map[string]string{"status": string(pairing.Approved), "token": secret})
}

func (s *Server) mintExtToken(req pairing.Request) (string, error) {
	tok, secret := config.GenerateToken(req.App+" (paired)", grantedScopes(req.Scopes))
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
	pending := s.pairs.ListPending()
	for i := range pending {
		// The operator approves what this column shows, so it names the
		// grant, not the ask.
		pending[i].Scopes = grantedScopes(pending[i].Scopes)
	}
	return map[string]any{
		"Pending":   pending,
		"Paired":    s.hasPairedToken("monsender"),
		"CSRFToken": s.sessions.CSRFToken(sessionFromContext(r.Context())),
	}
}

func (s *Server) monsenderPairingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.extPairData(r)
	paired, _ := data["Paired"].(bool)
	s.render(w, "monsender_pairing", data)
	// The poll sends the paired state it last rendered; a flip refreshes
	// the token list too.
	if was := r.URL.Query().Get("paired"); was != "" && (was == "true") != paired {
		s.renderAuthTokensOOB(w, r)
	}
}

func (s *Server) monsenderPairApprove(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if s.pairs.SetState(r.PathValue("id"), pairing.Approved) {
		logx.Infof("pairing: approved extension request %s", r.PathValue("id"))
	}
	s.render(w, "monsender_pairing", s.extPairData(r))
}

func (s *Server) monsenderPairDeny(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.pairs.SetState(r.PathValue("id"), pairing.Denied)
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
