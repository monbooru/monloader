package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
)

type fakeMonbooru struct {
	offers, polls atomic.Int32
	fail          int32
	status        atomic.Value
}

func newFakeMonbooru(t *testing.T, fail int32, status string) (*fakeMonbooru, *httptest.Server) {
	t.Helper()
	f := &fakeMonbooru{fail: fail}
	f.status.Store(status)
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/pair/request":
			if f.offers.Add(1) <= f.fail {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"request_id":"r","status":"pending"}`))
		case "/api/v1/pair/status":
			f.polls.Add(1)
			st := f.status.Load().(string)
			if st == "not_found" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(st))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(mb.Close)
	return f, mb
}

func fastOffer(t *testing.T) {
	t.Helper()
	was := [4]time.Duration{offerRetryMin, offerRetryMax, offerPollEvery, offerRenewAfter}
	offerRetryMin, offerRetryMax, offerPollEvery, offerRenewAfter = 5*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond, time.Hour
	t.Cleanup(func() { offerRetryMin, offerRetryMax, offerPollEvery, offerRenewAfter = was[0], was[1], was[2], was[3] })
}

func unpairedServer(t *testing.T, monbooruURL string) *Server {
	t.Helper()
	srv := newWebServer(t, monbooruURL, "")
	if err := srv.cfg.Update(func(c *config.Config) error { c.Monbooru.APIToken = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	return srv
}

func runOffer(t *testing.T, srv *Server, until func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { srv.OfferAtStart(ctx); close(done) }()
	if until != nil {
		for !until() {
			if ctx.Err() != nil {
				t.Fatal("condition not reached")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("OfferAtStart did not return")
	}
}

func TestOfferAtStartPairsWithNoClickOnEitherSide(t *testing.T) {
	fastOffer(t)
	f, mb := newFakeMonbooru(t, 0, `{"status":"approved","token":"mb-issued"}`)
	srv := unpairedServer(t, mb.URL)
	runOffer(t, srv, nil)
	if f.offers.Load() != 1 {
		t.Errorf("offers = %d, want one per start", f.offers.Load())
	}
	if !srv.hasPairedToken(contract.AppMonbooru) || srv.cfg.Current().Monbooru.APIToken != "mb-issued" {
		t.Errorf("not paired: token=%q", srv.cfg.Current().Monbooru.APIToken)
	}
}

func TestOfferAtStartRetriesUntilMonbooruAnswers(t *testing.T) {
	fastOffer(t)
	f, mb := newFakeMonbooru(t, 3, `{"status":"approved","token":"mb-issued"}`)
	srv := unpairedServer(t, mb.URL)
	runOffer(t, srv, nil)
	if f.offers.Load() != 4 || srv.cfg.Current().Monbooru.APIToken != "mb-issued" {
		t.Errorf("offers = %d, token = %q; want three refusals, then a pairing", f.offers.Load(), srv.cfg.Current().Monbooru.APIToken)
	}
}

func TestOfferAtStartWaitsInTodaysStateAndRenewsTheOffer(t *testing.T) {
	fastOffer(t)
	offerRenewAfter = 30 * time.Millisecond
	f, mb := newFakeMonbooru(t, 0, `{"status":"pending"}`)
	srv := unpairedServer(t, mb.URL)
	runOffer(t, srv, func() bool {
		return f.offers.Load() >= 3 && srv.getPairAttempt() != nil
	})
	if srv.hasPairedToken(contract.AppMonbooru) {
		t.Error("paired while monbooru never approved")
	}
}

func TestOfferAtStartOffersAgainWhenMonbooruLostTheRequest(t *testing.T) {
	fastOffer(t)
	f, mb := newFakeMonbooru(t, 0, "not_found")
	srv := unpairedServer(t, mb.URL)
	runOffer(t, srv, func() bool { return f.offers.Load() >= 2 })
}

func TestOfferAtStartStopsOnADenial(t *testing.T) {
	fastOffer(t)
	f, mb := newFakeMonbooru(t, 0, `{"status":"denied"}`)
	srv := unpairedServer(t, mb.URL)
	runOffer(t, srv, nil)
	if f.offers.Load() != 1 || srv.getPairAttempt() != nil || srv.hasPairedToken(contract.AppMonbooru) {
		t.Errorf("offers = %d, attempt = %v; want one offer and nothing left after the denial", f.offers.Load(), srv.getPairAttempt())
	}
}

func TestOfferAtStartStopsWhenCancelledInSettings(t *testing.T) {
	fastOffer(t)
	f, mb := newFakeMonbooru(t, 0, `{"status":"pending"}`)
	srv := unpairedServer(t, mb.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { srv.OfferAtStart(ctx); close(done) }()
	for f.polls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	srv.monbooruPairCancel(httptest.NewRecorder(), httptest.NewRequest("POST", "/settings/monbooru/pair/cancel", nil))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancel in settings did not stop the offer")
	}
	if f.offers.Load() != 1 {
		t.Errorf("offers = %d, want no new one after the cancel", f.offers.Load())
	}
}

func TestOfferAtStartOffersNothingWithATokenOrNoAddress(t *testing.T) {
	fastOffer(t)
	f, mb := newFakeMonbooru(t, 0, `{"status":"approved","token":"x"}`)
	withToken := newWebServer(t, mb.URL, "")
	runOffer(t, withToken, nil)

	off := unpairedServer(t, mb.URL)
	if err := off.cfg.Update(func(c *config.Config) error { c.Monbooru.APIURL = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	runOffer(t, off, nil)
	if f.offers.Load() != 0 {
		t.Errorf("offers = %d, want none", f.offers.Load())
	}
}
