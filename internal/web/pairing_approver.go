package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/pairing"
	"github.com/monbooru/monloader/internal/shared/session"
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
	pairJSON(w, status, contract.Error{Error: msg, Code: code})
}

func pairRefuse(w http.ResponseWriter, f *pairing.Refusal) { pairError(w, f.Status, f.Code, f.Msg) }

func (s *Server) removePairingLocal(peer string) error {
	return s.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t apitoken.Token) bool { return t.Paired == peer })
		if peer == contract.AppMonbooru {
			c.Monbooru.APIToken, c.Monbooru.Paused = "", false
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
	tok := cfg.Auth.Tokens.ByHash(apitoken.Hash(strings.TrimPrefix(got, prefix)))
	if !strings.HasPrefix(got, prefix) || tok == nil || tok.Paired == "" {
		pairRefuse(w, pairing.NoPairingToken())
		return
	}
	if err := s.removePairingLocal(tok.Paired); err != nil {
		pairError(w, http.StatusInternalServerError, pairing.CodeRemoveFailed, err.Error())
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", tok.Paired)
	pairJSON(w, http.StatusOK, pairing.StatusBody{Status: pairing.Removed})
}

func (s *Server) extPairRequest(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	var body pairing.RequestBody
	if f := pairing.DecodeOffer(w, r, &body); f != nil {
		pairRefuse(w, f)
		return
	}
	if !pairing.ValidApp(body.App) {
		pairError(w, http.StatusBadRequest, pairing.CodeInvalidRequest, "app must match [A-Za-z0-9_-]{1,32}")
		return
	}
	// monbooru's pairing has its own flow; an extension approved under
	// the name would show a phantom connection.
	if strings.EqualFold(body.App, contract.AppMonbooru) {
		pairError(w, http.StatusBadRequest, pairing.CodeInvalidRequest, "app name \"monbooru\" is reserved")
		return
	}
	if s.hasPairedToken(body.App) {
		pairError(w, http.StatusConflict, "already_paired", "already paired with "+body.App+"; remove the existing pairing first")
		return
	}
	accepted, f := s.pairs.Offer(pairing.Request{App: body.App, Source: session.ClientIP(r), Scopes: body.RequestedScopes})
	if f != nil {
		pairRefuse(w, f)
		return
	}
	logx.Infof("pairing: request from %s", body.App)
	pairJSON(w, http.StatusOK, accepted)
}

func (s *Server) extPairStatus(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	body, f := s.pairs.Poll(r.URL.Query().Get("id"), s.mintExtToken)
	if f != nil {
		pairRefuse(w, f)
		return
	}
	if body.Token != "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	pairJSON(w, http.StatusOK, body)
}

func (s *Server) mintExtToken(req pairing.Request) (string, error) {
	tok, secret := apitoken.New(req.App+" (paired)", grantedScopes(req.Scopes))
	tok.Paired = req.App
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t apitoken.Token) bool { return t.Paired == req.App })
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
		"Awaiting":  s.pairs.ListApproved(),
		"Paired":    s.hasPairedToken("monsender"),
		"CSRFToken": s.sessions.CSRFToken(session.FromContext(r.Context())),
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
