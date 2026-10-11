package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

type outboundPair struct {
	requestID string
	token     config.Token
}

func (s *Server) setPairAttempt(p *outboundPair) {
	s.pairMu.Lock()
	s.pairAttempt = p
	s.pairMu.Unlock()
}

func (s *Server) getPairAttempt() *outboundPair {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	return s.pairAttempt
}

func (s *Server) clearPairAttempt() {
	s.pairMu.Lock()
	s.pairAttempt = nil
	s.pairMu.Unlock()
}

// Compare-and-clear, so two concurrent polls cannot both commit one approval.
func (s *Server) takePairAttempt(att *outboundPair) bool {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.pairAttempt != att {
		return false
	}
	s.pairAttempt = nil
	return true
}

func (s *Server) hasPairedToken(peer string) bool {
	return slices.ContainsFunc(s.cfg.Current().Auth.Tokens, func(t config.Token) bool { return t.Paired == peer })
}

func (s *Server) monbooruPairData(r *http.Request, msg string) map[string]any {
	return map[string]any{
		"Paired":    s.hasPairedToken("monbooru"),
		"Waiting":   s.getPairAttempt() != nil,
		"Message":   msg,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}

// The bound port, not base_url, which may name a proxy or a stale port:
// monbooru swaps in the host the request came from and keeps this port.
func pairSelfURL(cfg *config.Config) string {
	if _, port, err := net.SplitHostPort(strings.TrimSpace(cfg.Server.BindAddress)); err == nil && port != "" {
		return "http://localhost:" + port
	}
	return cfg.Server.BaseURL
}

func (s *Server) monbooruPairConnect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "bad form data"))
		return
	}
	if err := s.startMonbooruPairing(r.Context()); err != nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, err.Error()))
		return
	}
	s.render(w, "monbooru_pair", s.monbooruPairData(r, "waiting"))
}

func (s *Server) startMonbooruPairing(ctx context.Context) error {
	cfg := s.cfg.Current()
	if strings.TrimSpace(cfg.Monbooru.APIURL) == "" {
		return errors.New("set the monbooru api url first")
	}
	peerSecret := config.GenerateSecret()
	tok := config.TokenFromSecret("monbooru (paired)", peerSecret, config.AllScopes)
	tok.Paired = "monbooru"
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	requestID, err := s.client.PairRequest(reqCtx, peerSecret, pairSelfURL(cfg), []string{config.ScopeRead, config.ScopeWrite})
	if err != nil {
		return errors.New("could not reach monbooru: " + err.Error())
	}
	s.setPairAttempt(&outboundPair{requestID: requestID, token: tok})
	logx.Infof("pairing: requested pairing with monbooru")
	return nil
}

func (s *Server) monbooruPairPoll(w http.ResponseWriter, r *http.Request) {
	att := s.getPairAttempt()
	if att == nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, ""))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	status, token, err := s.client.PairStatus(ctx, att.requestID)
	if err != nil {
		// Transient (monbooru briefly unreachable); keep waiting.
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "waiting"))
		return
	}
	switch status {
	case "approved":
		if token == "" {
			s.render(w, "monbooru_pair", s.monbooruPairData(r, "waiting"))
			return
		}
		if !s.takePairAttempt(att) {
			s.render(w, "monbooru_pair", s.monbooruPairData(r, ""))
			return
		}
		tok := att.token
		if err := s.updateConfig(func(c *config.Config) error {
			// Replaces the old pair only now, so a denied or abandoned
			// attempt leaves the working pairing untouched.
			c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == "monbooru" })
			c.Auth.Tokens = append(c.Auth.Tokens, tok)
			c.Monbooru.APIToken = token
			return nil
		}); err != nil {
			s.setPairAttempt(att) // restore so a later poll retries
			s.render(w, "monbooru_pair", s.monbooruPairData(r, "could not save: "+err.Error()))
			return
		}
		logx.Infof("pairing: connected to monbooru")
		s.render(w, "monbooru_pair", s.monbooruPairData(r, ""))
		s.renderAuthTokensOOB(w, r)
		s.renderDefaultGalleryOOB(w, r)
	case "denied":
		s.clearPairAttempt()
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "request denied in monbooru"))
	case "expired":
		s.clearPairAttempt()
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "request expired; try again"))
	case "not_found":
		s.clearPairAttempt()
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "monbooru no longer has this request - connect again"))
	default:
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "waiting"))
	}
}

// Nothing is committed on either side yet; monbooru's pending request
// ages out on its own.
func (s *Server) monbooruPairCancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "bad form data"))
		return
	}
	s.clearPairAttempt()
	logx.Infof("pairing: canceled the pending monbooru attempt")
	s.render(w, "monbooru_pair", s.monbooruPairData(r, ""))
}

func (s *Server) monbooruPairRemove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "bad form data"))
		return
	}
	s.clearPairAttempt()
	peerToken := s.cfg.Current().Monbooru.APIToken
	if err := s.removePairingLocal("monbooru"); err != nil {
		logx.Errorf("pairing: remove failed: %v", err)
	}
	msg := ""
	if err := s.notifyMonbooruTeardown(peerToken); err != nil {
		logx.Errorf("pairing: could not notify monbooru of teardown: %v", err)
		msg = "removed here, but could not reach monbooru - remove the pairing in monbooru too."
	}
	logx.Infof("pairing: removed monbooru pairing")
	s.render(w, "monbooru_pair", s.monbooruPairData(r, msg))
	s.renderAuthTokensOOB(w, r)
	s.renderDefaultGalleryOOB(w, r)
}

func (s *Server) notifyMonbooruTeardown(token string) error {
	if token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return s.client.PairTeardown(ctx, token)
}
