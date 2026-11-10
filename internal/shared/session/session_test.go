package session

import "testing"

func TestAnExpiredSessionIsInvalidAndEvicted(t *testing.T) {
	s := New()
	id := s.Create(-1)
	if s.Valid(id) {
		t.Fatal("an expired session validated")
	}
	if n := len(s.expires); n != 0 {
		t.Errorf("expired sessions still held: %d", n)
	}
}

func TestAHugeLifetimeStaysValid(t *testing.T) {
	s := New()
	if id := s.Create(999999); !s.Valid(id) {
		t.Error("a session with a lifetime past time.Duration's range expired at once")
	}
}

func TestSweepKeepsALiveSession(t *testing.T) {
	s := New()
	dead := s.Create(0)
	live := s.Create(1)
	s.SweepExpired()
	if _, ok := s.expires[dead]; ok {
		t.Error("the sweep kept an expired session")
	}
	if !s.Valid(live) {
		t.Error("the sweep dropped a live session")
	}
}

func TestDeleteAndClear(t *testing.T) {
	s := New()
	a, b, c := s.Create(1), s.Create(1), s.Create(1)
	s.Delete(a)
	if s.Valid(a) {
		t.Error("a deleted session validated")
	}
	s.ClearExcept(b)
	if !s.Valid(b) || s.Valid(c) {
		t.Errorf("ClearExcept: kept=%v other=%v", s.Valid(b), s.Valid(c))
	}
	s.Clear()
	if s.Valid(b) {
		t.Error("Clear kept a session")
	}
}

func TestSessionIDsAreDistinct(t *testing.T) {
	s := New()
	if a, b := s.Create(1), s.Create(1); a == b || len(a) != 43 {
		t.Errorf("ids %q %q", a, b)
	}
}

func TestCSRFTokenIsBoundToTheSessionAndTheStore(t *testing.T) {
	s := New()
	tok := s.CSRFToken("sess")
	if !s.ValidCSRF("sess", tok) {
		t.Error("a store's own token was refused")
	}
	if s.ValidCSRF("sess", "wrong-token") || s.ValidCSRF("sess", "") {
		t.Error("a wrong token was accepted")
	}
	if s.ValidCSRF("other", tok) {
		t.Error("a token validated for another session")
	}
	if New().ValidCSRF("sess", tok) {
		t.Error("a token validated against another store")
	}
}
