package sitestate

import (
	"testing"
	"time"
)

func TestTrackerRecordsAndReads(t *testing.T) {
	tr := New()
	if !tr.LastReached("danbooru").IsZero() {
		t.Fatal("an untracked site should read as the zero time")
	}

	now := time.Now()
	tr.Reached("danbooru", now)
	if got := tr.LastReached("danbooru"); !got.Equal(now) {
		t.Errorf("LastReached = %v, want %v", got, now)
	}

	tr.Reached("danbooru", now.Add(-time.Hour))
	if got := tr.LastReached("danbooru"); !got.Equal(now) {
		t.Errorf("an older reach overwrote the newer time: got %v", got)
	}
	later := now.Add(time.Hour)
	tr.Reached("danbooru", later)
	if got := tr.LastReached("danbooru"); !got.Equal(later) {
		t.Errorf("a newer reach was not recorded: got %v, want %v", got, later)
	}

	tr.Reached("", now)
	if !tr.LastReached("").IsZero() {
		t.Error("an empty site should not be tracked")
	}
}
