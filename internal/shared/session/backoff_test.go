package session

import (
	"testing"
	"time"
)

func seedAttempt(b *Backoff, ip string, count int, lastFail time.Time) {
	b.mu.Lock()
	b.failures[ip] = attempt{count: count, lastFail: lastFail}
	b.mu.Unlock()
}

func TestBackoffAllowsAFreshClient(t *testing.T) {
	if NewBackoff().Wait("10.0.0.1") != 0 {
		t.Error("a fresh IP must be allowed")
	}
}

func TestBackoffDoublesToItsCap(t *testing.T) {
	for count, delay := range map[int]time.Duration{
		1:  time.Second,
		2:  2 * time.Second,
		3:  4 * time.Second,
		4:  8 * time.Second,
		5:  16 * time.Second,
		6:  30 * time.Second,
		20: 30 * time.Second,
	} {
		b := NewBackoff()
		const ip = "10.0.0.2"
		seedAttempt(b, ip, count, time.Now().Add(-(delay - 500*time.Millisecond)))
		if w := b.Wait(ip); w <= 0 || w > 500*time.Millisecond {
			t.Errorf("count=%d: wait %s inside the %s window, want the 500ms left", count, w, delay)
		}
		seedAttempt(b, ip, count, time.Now().Add(-(delay + 500*time.Millisecond)))
		if w := b.Wait(ip); w != 0 {
			t.Errorf("count=%d: wait %s past the %s window", count, w, delay)
		}
	}
}

func TestBackoffCountsFailuresAndForgetsOnSuccess(t *testing.T) {
	b := NewBackoff()
	const ip = "10.0.0.4"
	b.Fail(ip)
	b.Fail(ip)
	b.Fail(ip)
	if got := b.failures[ip].count; got != 3 {
		t.Errorf("count = %d after three failures, want 3", got)
	}
	b.Succeed(ip)
	if _, ok := b.failures[ip]; ok || b.Wait(ip) != 0 {
		t.Error("a success must drop the entry and unblock the IP")
	}
}

func TestBackoffSweepDropsOnlyStaleEntries(t *testing.T) {
	b := NewBackoff()
	seedAttempt(b, "old", 1, time.Now().Add(-10*time.Minute))
	seedAttempt(b, "new", 1, time.Now())
	b.Sweep()
	if _, ok := b.failures["old"]; ok {
		t.Error("the sweep kept a 10-minute-old entry")
	}
	if _, ok := b.failures["new"]; !ok {
		t.Error("the sweep dropped a fresh entry")
	}
}

func TestBackoffIsPerClient(t *testing.T) {
	b := NewBackoff()
	b.Fail("1.1.1.1")
	if b.Wait("2.2.2.2") != 0 {
		t.Error("another IP was blocked")
	}
}
