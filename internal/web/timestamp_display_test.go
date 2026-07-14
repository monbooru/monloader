package web

import (
	"testing"
	"time"
)

// Timestamps are held in UTC but displayed in the process timezone so they
// match the operator's wall clock. This test pins a non-UTC time.Local and
// asserts the render helpers convert instead of echoing UTC. It must not call
// t.Parallel(): it mutates the process-global time.Local.
func TestTimestampHelpers_RenderInLocalZone(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("TEST-3", -3*60*60)
	defer func() { time.Local = orig }()

	reached := time.Date(2026, 7, 5, 19, 51, 3, 0, time.UTC)
	if got := stampLocal(reached); got != "2026-07-05 16:51 TEST-3" {
		t.Errorf("stampLocal = %q, want 2026-07-05 16:51 TEST-3", got)
	}

	// 01:30Z falls on the previous day at UTC-3, so the date must roll back.
	if got := humanDate(time.Date(2026, 7, 5, 1, 30, 0, 0, time.UTC).Unix()); got != "2026-07-04" {
		t.Errorf("humanDate = %q, want 2026-07-04", got)
	}
	if got := humanDate(0); got != "" {
		t.Errorf("humanDate(unset) = %q, want empty", got)
	}
}
