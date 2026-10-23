package session

import (
	"sync"
	"time"
)

type Backoff struct {
	mu       sync.Mutex
	failures map[string]attempt
}

type attempt struct {
	count    int
	lastFail time.Time
}

func NewBackoff() *Backoff {
	return &Backoff{failures: map[string]attempt{}}
}

// Wait returns how long ip must still wait before its next attempt; 0 lets it in.
func (b *Backoff) Wait(ip string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.failures[ip]
	if !ok {
		return 0
	}
	// The shift is clamped at 0 because a negative shift panics.
	shift := min(max(a.count-1, 0), 5)
	delay := min(time.Duration(1<<shift)*time.Second, 30*time.Second)
	return max(delay-time.Since(a.lastFail), 0)
}

func (b *Backoff) Fail(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.failures[ip]
	a.count++
	a.lastFail = time.Now()
	b.failures[ip] = a
}

func (b *Backoff) Succeed(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, ip)
}

func (b *Backoff) Sweep() {
	cutoff := time.Now().Add(-5 * time.Minute)
	b.mu.Lock()
	defer b.mu.Unlock()
	for ip, a := range b.failures {
		if a.lastFail.Before(cutoff) {
			delete(b.failures, ip)
		}
	}
}
