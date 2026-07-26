package web

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

// outboundPair is an in-flight "connect to monbooru" attempt. The token is the
// credential monbooru will use to call back; it is committed only once monbooru
// approves and returns its own token, so a denied attempt leaves nothing behind.
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

// takePairAttempt clears the attempt only if it is still att, so two concurrent
// polls cannot both commit the same approval.
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
	for _, t := range s.cfg.Current().Auth.Tokens {
		if t.Paired == peer {
			return true
		}
	}
	return false
}

func (s *Server) monbooruPairData(r *http.Request, msg string) map[string]any {
	return map[string]any{
		"Paired":    s.hasPairedToken("monbooru"),
		"Waiting":   s.getPairAttempt() != nil,
		"Message":   msg,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}

// pairSelfURL is the address monloader advertises to monbooru at pairing: the
// port it actually binds. base_url is a display value that can point at a proxy
// or carry a stale port, so it is not used here - monbooru rewrites the host to
// the source the request came from and keeps this port to reach back for the
// connectivity light and teardown. Falls back to base_url if the bind address
// has no parseable port.
func pairSelfURL(cfg *config.Config) string {
	if _, port, err := net.SplitHostPort(strings.TrimSpace(cfg.Server.BindAddress)); err == nil && port != "" {
		return "http://localhost:" + port
	}
	return cfg.Server.BaseURL
}

// monbooruPairConnect offers a pairing to monbooru. monloader mints the token
// monbooru will carry, sends its own URL plus that token, and starts waiting
// for the operator to approve in monbooru.
func (s *Server) monbooruPairConnect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "bad form data"))
		return
	}
	if s.hasPairedToken("monbooru") {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "already paired; remove the pairing first to re-pair"))
		return
	}
	cfg := s.cfg.Current()
	if strings.TrimSpace(cfg.Monbooru.APIURL) == "" {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "set the monbooru api url above first"))
		return
	}
	peerSecret := config.GenerateSecret()
	tok := config.TokenFromSecret("monbooru (paired)", peerSecret, config.AllScopes)
	tok.Paired = "monbooru"
	tok.PeerURL = cfg.Monbooru.APIURL
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	requestID, err := s.client.PairRequest(ctx, peerSecret, pairSelfURL(cfg), []string{config.ScopeRead, config.ScopeWrite})
	if err != nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "could not reach monbooru: "+err.Error()))
		return
	}
	s.setPairAttempt(&outboundPair{requestID: requestID, token: tok})
	logx.Infof("pairing: requested pairing with monbooru")
	s.render(w, "monbooru_pair", s.monbooruPairData(r, "waiting"))
}

// monbooruPairPoll checks the pending attempt; on approval it commits the
// minted token and stores monbooru's returned token.
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
			// A concurrent poll already committed this approval.
			s.render(w, "monbooru_pair", s.monbooruPairData(r, ""))
			return
		}
		tok := att.token
		if err := s.updateConfig(func(c *config.Config) error {
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

// monbooruPairCancel abandons the pending attempt. Nothing was committed on
// either side yet, so dropping it locally is enough; a stale approval in
// monbooru ages out of its pending store on its own.
func (s *Server) monbooruPairCancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "monbooru_pair", s.monbooruPairData(r, "bad form data"))
		return
	}
	s.clearPairAttempt()
	logx.Infof("pairing: canceled the pending monbooru attempt")
	s.render(w, "monbooru_pair", s.monbooruPairData(r, ""))
}

// monbooruPairRemove tears down this side: it drops the token monbooru carries
// and the token monloader uses to push, so a re-pair starts clean.
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

// notifyMonbooruTeardown asks monbooru to drop its side of the pairing and
// returns an error when it could not be reached, so the caller can tell the
// operator to remove the far end by hand.
func (s *Server) notifyMonbooruTeardown(token string) error {
	if token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return s.client.PairTeardown(ctx, token)
}
