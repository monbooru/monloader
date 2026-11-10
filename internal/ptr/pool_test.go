package ptr

import (
	"database/sql"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

func waitForIdleDrain(t *testing.T, name string, stats func() int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if stats() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s still holds %d open connection(s) past the idle window; its page cache stays resident", name, stats())
}

func TestStoreReleasesIdleConnections(t *testing.T) {
	orig := idleConnWindow
	idleConnWindow = 50 * time.Millisecond
	defer func() { idleConnWindow = orig }()

	s := openTestStore(t)
	if _, _, err := s.Cursor(); err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	mustReplay(t, s, 1, 0, wireBlobs(t, &update{}))
	if s.db.Stats().OpenConnections == 0 || s.replay.Stats().OpenConnections == 0 {
		t.Fatal("no connection was opened, so the drain below would prove nothing")
	}
	waitForIdleDrain(t, "the query pool", func() int { return s.db.Stats().OpenConnections })
	waitForIdleDrain(t, "the replay handle", func() int { return s.replay.Stats().OpenConnections })
}

func TestReplayCacheStaysOffTheQueryPool(t *testing.T) {
	s := openTestStore(t)

	cachePages := func(db *sql.DB) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`PRAGMA cache_size`).Scan(&n); err != nil {
			t.Fatalf("reading cache_size: %v", err)
		}
		return n
	}
	if got := cachePages(s.db); got != -queryCacheKB {
		t.Errorf("query pool cache_size = %d, want %d", got, -queryCacheKB)
	}
	if got := cachePages(s.replay); got != -replayCacheKB {
		t.Errorf("replay cache_size = %d, want %d", got, -replayCacheKB)
	}
	if queryCacheKB >= replayCacheKB {
		t.Errorf("query cache %d KB should stay well under the replay's %d KB", queryCacheKB, replayCacheKB)
	}
	if got := s.replay.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("replay handle allows %d connections, want 1", got)
	}
}

func TestIdleConnWindowUndercutsReadyPoll(t *testing.T) {
	readyPoll := NewEngine(config.PTRConfig{}).readyPoll
	if idleConnWindow >= readyPoll {
		t.Errorf("idleConnWindow = %v, must stay under the engine's caught-up poll of %v", idleConnWindow, readyPoll)
	}
	if idleConnWindow <= 30*time.Second {
		t.Errorf("idleConnWindow = %v, too short to span the gap between two sync slices", idleConnWindow)
	}
}
