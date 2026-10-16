// Package session holds a process's browser sessions and the CSRF tokens
// bound to them. Nothing outlives the process: a restart signs everyone out.
package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"maps"
	"sync"
	"time"
)

type Store struct {
	mu      sync.Mutex
	expires map[string]time.Time
	secret  []byte
}

func New() *Store {
	return &Store{expires: map[string]time.Time{}, secret: randomBytes(32)}
}

// crypto/rand.Read never returns an error; it crashes the process instead.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (s *Store) Create(lifetimeDays int) string {
	id := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	s.mu.Lock()
	s.expires[id] = time.Now().Add(time.Duration(lifetimeDays) * 24 * time.Hour)
	s.mu.Unlock()
	return id
}

func (s *Store) Valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.expires[id]
	if ok && time.Now().After(exp) {
		delete(s.expires, id)
		return false
	}
	return ok
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	delete(s.expires, id)
	s.mu.Unlock()
}

func (s *Store) Clear() { s.ClearExcept("") }

func (s *Store) ClearExcept(keep string) {
	s.mu.Lock()
	maps.DeleteFunc(s.expires, func(id string, _ time.Time) bool { return id != keep })
	s.mu.Unlock()
}

func (s *Store) SweepExpired() {
	now := time.Now()
	s.mu.Lock()
	maps.DeleteFunc(s.expires, func(_ string, exp time.Time) bool { return now.After(exp) })
	s.mu.Unlock()
}

// The token is derived, not stored, so any session id - "anon" included -
// has one, and it validates only against the store that issued it.
func (s *Store) CSRFToken(id string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Store) ValidCSRF(id, token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRFToken(id))) == 1
}
