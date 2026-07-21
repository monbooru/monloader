// Package sitestate tracks, per gallery-dl site category, the last time the
// site was reached successfully - via the settings test probe or a download
// job's resolve pass. It is in-memory only (no application DB), so the
// indicator resets on restart, like the queue's recent-history ring.
package sitestate

import (
	"sync"
	"time"
)

// Tracker records the most recent successful reach per site category. The
// settings page reads it to show "last reached" beside each site; the web
// test handler and the pipeline write it.
type Tracker struct {
	mu   sync.RWMutex
	last map[string]time.Time
}

// New returns an empty Tracker.
func New() *Tracker {
	return &Tracker{last: map[string]time.Time{}}
}

// Reached records that site was reached successfully at t, keeping the most
// recent time so an older fetch never overwrites a newer one.
func (t *Tracker) Reached(site string, at time.Time) {
	if site == "" {
		return
	}
	t.mu.Lock()
	if t.last == nil {
		// A zero-value Tracker still works; New is just the usual path.
		t.last = map[string]time.Time{}
	}
	if at.After(t.last[site]) {
		t.last[site] = at
	}
	t.mu.Unlock()
}

// LastReached returns the most recent successful reach for site, or the zero
// time if it has never been reached.
func (t *Tracker) LastReached(site string) time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.last[site]
}
