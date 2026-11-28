package web

import (
	"context"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/shared/logx"
)

// Variables so a test can shorten them.
var (
	offerRetryMin  = 2 * time.Second
	offerRetryMax  = time.Minute
	offerPollEvery = 3 * time.Second
	// Inside the five minutes monbooru keeps a pending request.
	offerRenewAfter = 4 * time.Minute
)

// OfferAtStart makes the offer "Connect to monbooru" makes when monloader
// holds no monbooru token, and waits on it until it is claimed, denied or
// cancelled. It retries until monbooru answers and renews the offer while
// it waits on a card.
func (s *Server) OfferAtStart(ctx context.Context) {
	wait := offerRetryMin
	for s.offerWanted() {
		if err := s.startMonbooruPairing(ctx); err != nil {
			logx.Debugf("pairing: monbooru not reached yet: %v", err)
			if !sleepCtx(ctx, wait) {
				return
			}
			wait = min(wait*2, offerRetryMax)
			continue
		}
		wait = offerRetryMin
		if !s.waitOnOffer(ctx, s.getPairAttempt()) {
			return
		}
	}
}

// A blank api url is pairing turned off, and a desktop still in its wizard
// offers from the wizard's Pair box.
func (s *Server) offerWanted() bool {
	m := s.cfg.Current().Monbooru
	return strings.TrimSpace(m.APIURL) != "" && m.APIToken == "" && !s.setupPending()
}

// Claims the attempt itself, since no page may be open to poll it. True
// asks for a fresh offer.
func (s *Server) waitOnOffer(ctx context.Context, att *outboundPair) bool {
	renewAt := time.Now().Add(offerRenewAfter)
	for {
		if !sleepCtx(ctx, offerPollEvery) {
			return false
		}
		// Cancelled, collected by the footer light's poll, or replaced by
		// a click on Connect.
		if s.getPairAttempt() != att {
			return false
		}
		if time.Now().After(renewAt) {
			return true
		}
		pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, out := s.claimMonbooruOutcome(pollCtx, att)
		cancel()
		switch out {
		case pairCommitted, pairDenied:
			return false
		case pairGone:
			return true
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
