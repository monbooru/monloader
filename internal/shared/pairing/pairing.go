// Package pairing holds the pairing requests a peer polls until the
// operator approves or denies them. Nothing outlives the process.
package pairing

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"sync"
	"time"
)

const (
	pendingTTL = 5 * time.Minute
	// MaxPending caps the waiting requests; the endpoint is unauthenticated.
	MaxPending = 16
)

// State is the status word a peer reads back while it polls.
type State string

const (
	Pending  State = "pending"
	Approved State = "approved"
	Denied   State = "denied"
)

// Nothing is issued until the claim, so an approval the peer never
// collects mints no token.
type Request struct {
	ID        string
	App       string
	Source    string
	Scopes    []string
	State     State
	Claimed   bool
	CreatedAt time.Time
}

func (r *Request) request() *Request { return r }

type requestPtr[T any] interface {
	*T
	request() *Request
}

// Store holds requests of type T: a Request, or a struct embedding one to
// carry what an app's offer adds.
type Store[T any, P requestPtr[T]] struct {
	mu sync.Mutex
	m  map[string]P
}

func NewStore[T any, P requestPtr[T]]() *Store[T, P] {
	return &Store[T, P]{m: map[string]P{}}
}

func pairID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store[T, P]) sweepLocked() {
	cutoff := time.Now().Add(-pendingTTL)
	maps.DeleteFunc(s.m, func(_ string, p P) bool {
		return p.request().CreatedAt.Before(cutoff)
	})
}

func (s *Store[T, P]) dropPendingLocked(app string) {
	maps.DeleteFunc(s.m, func(_ string, p P) bool {
		r := p.request()
		return r.State == Pending && r.App == app
	})
}

// A second request from the same app replaces its pending one, under a new
// id: one noisy peer would otherwise fill the cap for the TTL, and two rows
// would approve into two tokens under one name.
func (s *Store[T, P]) Create(req T) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	p := P(&req)
	r := p.request()
	s.dropPendingLocked(r.App)
	pending := 0
	for _, q := range s.m {
		if q.request().State == Pending {
			pending++
		}
	}
	if pending >= MaxPending {
		return "", false
	}
	r.ID, r.State, r.CreatedAt = pairID(), Pending, time.Now()
	s.m[r.ID] = p
	return r.ID, true
}

func (s *Store[T, P]) DropPending(app string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPendingLocked(app)
}

func (s *Store[T, P]) ListPending() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	var out []T
	for _, p := range s.m {
		if p.request().State == Pending {
			out = append(out, *p)
		}
	}
	return out
}

func (s *Store[T, P]) Get(id string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.m[id]; ok {
		return *p, true
	}
	var zero T
	return zero, false
}

// SetState moves a pending request only, so a decision is final.
func (s *Store[T, P]) SetState(id string, st State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok || p.request().State != Pending {
		return false
	}
	p.request().State = st
	return true
}

// Claim wins once per approval, so two polls cannot both collect a token.
func (s *Store[T, P]) Claim(id string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	var zero T
	if !ok {
		return zero, false
	}
	r := p.request()
	if r.State != Approved || r.Claimed {
		return zero, false
	}
	r.Claimed = true
	return *p, true
}

// Unclaim lets a later poll retry a claim whose token could not be minted.
func (s *Store[T, P]) Unclaim(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.m[id]; ok {
		p.request().Claimed = false
	}
}

func (s *Store[T, P]) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}
