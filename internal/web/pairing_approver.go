package web

import (
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/api"
	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/pairing"
	"github.com/monbooru/monloader/internal/shared/session"
	"github.com/monbooru/monloader/internal/shared/webui"
)

const appMonsender = "monsender"

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

func pairRefuse(w http.ResponseWriter, f *pairing.Refusal) {
	contract.WriteError(w, f.Status, f.Code, f.Msg)
}

func (s *Server) removePairingLocal(peer string) error {
	return s.cfg.Update(func(c *config.Config) error {
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
		contract.WriteError(w, http.StatusInternalServerError, pairing.CodeRemoveFailed, err.Error())
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", tok.Paired)
	contract.WriteJSON(w, http.StatusOK, pairing.StatusBody{Status: pairing.Removed})
}

func (s *Server) extPairRequest(w http.ResponseWriter, r *http.Request) {
	setPairCORS(w, r)
	var body pairing.RequestBody
	if f := pairing.DecodeOffer(w, r, &body); f != nil {
		pairRefuse(w, f)
		return
	}
	if !pairing.ValidApp(body.App) {
		contract.WriteError(w, http.StatusBadRequest, pairing.CodeInvalidRequest, "app must match [A-Za-z0-9_-]{1,32}")
		return
	}
	// monbooru's pairing has its own flow; an extension approved under
	// the name would show a phantom connection.
	if strings.EqualFold(body.App, contract.AppMonbooru) {
		contract.WriteError(w, http.StatusBadRequest, pairing.CodeInvalidRequest, "app name \"monbooru\" is reserved")
		return
	}
	if s.hasPairedToken(body.App) {
		contract.WriteError(w, http.StatusConflict, api.ErrAlreadyPaired, "already paired with "+body.App+"; remove the existing pairing first")
		return
	}
	accepted, f := s.pairs.Offer(pairing.Request{App: body.App, Source: session.ClientIP(r), Scopes: body.RequestedScopes})
	if f != nil {
		pairRefuse(w, f)
		return
	}
	logx.Infof("pairing: request from %s", body.App)
	contract.WriteJSON(w, http.StatusOK, accepted)
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
	contract.WriteJSON(w, http.StatusOK, body)
}

func (s *Server) mintExtToken(req pairing.Request) (string, error) {
	tok, secret := apitoken.New(req.App+" (paired)", grantedScopes(req.Scopes))
	tok.Paired = req.App
	if err := s.cfg.Update(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t apitoken.Token) bool { return t.Paired == req.App })
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		return "", err
	}
	logx.Infof("pairing: issued token to %s", req.App)
	return secret, nil
}

// The operator approves what the Scopes column shows, so it names the grant,
// not the ask.
func (s *Server) pendingPairs() []pairing.Request {
	pending := s.pairs.ListPending()
	for i := range pending {
		pending[i].Scopes = grantedScopes(pending[i].Scopes)
	}
	return pending
}

func (s *Server) extPairData(r *http.Request) map[string]any {
	pending := s.pendingPairs()
	n := s.pairNotice()
	n.OOB = true
	return map[string]any{
		"Pending":  pending,
		"Awaiting": s.pairs.ListApproved(),
		"Paired":   s.hasPairedToken(appMonsender),
		"Notice":   n,
		"PairKey":  n.Key(),
	}
}

func (s *Server) pairNotice() webui.PairNotice {
	n := webui.PairNotice{Href: "/settings#pairing"}
	for _, req := range s.pairs.ListPending() {
		n.Items = append(n.Items, webui.PairItem{App: req.App, Source: req.Source, ExpiresAt: req.ExpiresAt()})
	}
	return n
}

func (s *Server) monsenderPairingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.extPairData(r)
	paired, _ := data["Paired"].(bool)
	if r.URL.Query().Get("pair") == data["PairKey"] {
		delete(data, "Notice")
	}
	s.render(w, "monsender_pairing", data)
	// The poll sends the paired state it last rendered; a flip refreshes
	// the token list too.
	if was := r.URL.Query().Get("paired"); was != "" && (was == "true") != paired {
		s.renderAuthTokensOOB(w)
	}
}

func (s *Server) monsenderPairApprove(w http.ResponseWriter, r *http.Request) {
	if s.pairs.SetState(r.PathValue("id"), pairing.Approved) {
		logx.Infof("pairing: approved extension request %s", r.PathValue("id"))
	}
	s.render(w, "monsender_pairing", s.extPairData(r))
}

func (s *Server) monsenderPairDeny(w http.ResponseWriter, r *http.Request) {
	s.pairs.SetState(r.PathValue("id"), pairing.Denied)
	s.render(w, "monsender_pairing", s.extPairData(r))
}

func (s *Server) monsenderPairRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.removePairingLocal(appMonsender); err != nil {
		logx.Errorf("pairing: remove failed: %v", err)
		data := s.extPairData(r)
		data["Message"] = "Could not save: " + err.Error()
		s.render(w, "monsender_pairing", data)
		return
	}
	s.render(w, "monsender_pairing", s.extPairData(r))
	s.renderAuthTokensOOB(w)
}

func grantedScopes(requested []string) []string {
	if sc := apitoken.Filter(requested, config.AllScopes); len(sc) > 0 {
		return sc
	}
	return []string{config.ScopeRead}
}
