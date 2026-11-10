package web

import (
	"testing"
	"time"
)

func TestTimestampHelpers_RenderInLocalZone(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("TEST-3", -3*60*60)
	defer func() { time.Local = orig }()

	reached := time.Date(2026, 7, 5, 19, 51, 3, 0, time.UTC)
	if got := stampLocal(reached); got != "2026-07-05 16:51 TEST-3" {
		t.Errorf("stampLocal = %q, want 2026-07-05 16:51 TEST-3", got)
	}

	if got := humanDate(time.Date(2026, 7, 5, 1, 30, 0, 0, time.UTC).Unix()); got != "2026-07-04" {
		t.Errorf("humanDate = %q, want 2026-07-04", got)
	}
	if got := humanDate(0); got != "" {
		t.Errorf("humanDate(unset) = %q, want empty", got)
	}
}

func TestHumanAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		unix int64
		want string
	}{
		{0, ""},
		{now.Add(-30 * time.Second).Unix(), "just now"},
		{now.Add(-5 * time.Minute).Unix(), "5m ago"},
		{now.Add(-3 * time.Hour).Unix(), "3h ago"},
		{now.Add(-2 * 24 * time.Hour).Unix(), "2d ago"},
	}
	for _, c := range cases {
		if got := humanAgo(c.unix); got != c.want {
			t.Errorf("humanAgo(%d) = %q, want %q", c.unix, got, c.want)
		}
	}
}
