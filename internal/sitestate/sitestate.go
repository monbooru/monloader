// Package sitestate tracks when each gallery-dl site category was last reached.
package sitestate

import (
	"sync"
	"time"
)

type Tracker struct {
	mu   sync.RWMutex
	last map[string]time.Time
}

func New() *Tracker {
	return &Tracker{last: map[string]time.Time{}}
}

func (t *Tracker) Reached(site string, at time.Time) {
	if site == "" {
		return
	}
	t.mu.Lock()
	if at.After(t.last[site]) {
		t.last[site] = at
	}
	t.mu.Unlock()
}

func (t *Tracker) LastReached(site string) time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.last[site]
}
