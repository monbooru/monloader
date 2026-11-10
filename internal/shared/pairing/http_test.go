package pairing

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPollHandsTheTokenToOneClaimAndRetriesAFailedMint(t *testing.T) {
	s := NewStore[Request]()
	if _, f := s.Poll("nope", nil); f == nil || f.Status != http.StatusNotFound || f.Code != CodeNotFound {
		t.Fatalf("unknown id = %+v, want 404 %s", f, CodeNotFound)
	}
	id, _ := s.Create(Request{App: "monedit"})
	mints := 0
	mint := func(Request) (string, error) {
		mints++
		if mints == 1 {
			return "", errors.New("disk full")
		}
		return "secret", nil
	}
	if body, f := s.Poll(id, mint); f != nil || body.Status != Pending || mints != 0 {
		t.Fatalf("pending poll = %+v %+v after %d mints", body, f, mints)
	}
	s.SetState(id, Approved)
	if _, f := s.Poll(id, mint); f == nil || f.Status != http.StatusInternalServerError || f.Code != CodeMintFailed || f.Msg != "disk full" {
		t.Fatalf("failed mint = %+v", f)
	}
	body, f := s.Poll(id, mint)
	if f != nil || body.Status != Approved || body.Token != "secret" {
		t.Fatalf("retried poll = %+v %+v", body, f)
	}
	if _, f := s.Poll(id, mint); f == nil || f.Status != http.StatusNotFound {
		t.Errorf("a collected request still answers %+v", f)
	}
}

func TestPollLeavesAClaimInFlightTokenless(t *testing.T) {
	s := NewStore[Request]()
	id, _ := s.Create(Request{App: "monedit"})
	s.SetState(id, Approved)
	s.Claim(id)
	if body, f := s.Poll(id, func(Request) (string, error) { return "x", nil }); f != nil || body.Status != Approved || body.Token != "" {
		t.Errorf("poll during a claim = %+v %+v, want approved with no token", body, f)
	}
}

func TestOfferRefusesPastTheCap(t *testing.T) {
	s := NewStore[Request]()
	for i := range MaxPending {
		if a, f := s.Offer(Request{App: string(rune('a' + i))}); f != nil || a.Status != Pending || a.RequestID == "" {
			t.Fatalf("offer %d = %+v %+v", i, a, f)
		}
	}
	if _, f := s.Offer(Request{App: "late"}); f == nil || f.Status != http.StatusTooManyRequests || f.Code != CodeTooMany {
		t.Errorf("offer past the cap = %+v", f)
	}
}

func TestDecodeOfferFillsAnEmbeddingBody(t *testing.T) {
	var body struct {
		RequestBody
		Contract int `json:"contract"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"app":"monedit","contract":2}`))
	if f := DecodeOffer(httptest.NewRecorder(), r, &body); f != nil || body.App != "monedit" || body.Contract != 2 {
		t.Fatalf("decode = %+v %+v", f, body)
	}
	for _, in := range []string{`{}`, `{"app":""}`, `not json`, `{"app":"x","pad":"` + strings.Repeat("a", maxOfferBody) + `"}`} {
		var b RequestBody
		r := httptest.NewRequest("POST", "/", strings.NewReader(in))
		if f := DecodeOffer(httptest.NewRecorder(), r, &b); f == nil || f.Status != http.StatusBadRequest || f.Code != CodeInvalidRequest {
			t.Errorf("%.20q = %+v, want 400 %s", in, f, CodeInvalidRequest)
		}
	}
}

func TestValidApp(t *testing.T) {
	for name, want := range map[string]bool{
		"monedit": true, "a_b-C9": true, strings.Repeat("a", 32): true,
		"": false, strings.Repeat("a", 33): false, "a b": false, "a/b": false, "é": false,
	} {
		if got := ValidApp(name); got != want {
			t.Errorf("ValidApp(%q) = %v, want %v", name, got, want)
		}
	}
}
