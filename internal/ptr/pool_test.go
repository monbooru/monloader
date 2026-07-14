package ptr

import (
	"database/sql"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
)

// waitForIdleDrain polls until the pool has closed every connection, or fails.
// database/sql's cleaner runs on its own ticker (floored at a second), so the
// drain is not observable the instant the window elapses.
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

// An idle connection keeps its sqlite page cache, which the sync grows to the
// configured ceiling and which never shrinks. The pool must therefore let idle
// connections go, or an enabled-and-synced index pins hundreds of MB of RSS
// for the life of the process.
func TestStoreReleasesIdleConnections(t *testing.T) {
	orig := idleConnWindow
	idleConnWindow = 50 * time.Millisecond
	defer func() { idleConnWindow = orig }()

	s := openTestStore(t)
	if _, _, err := s.Cursor(); err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	if _, err := s.Replay(1, 0, []*Update{{}}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if s.db.Stats().OpenConnections == 0 || s.replay.Stats().OpenConnections == 0 {
		t.Fatal("no connection was opened, so the drain below would prove nothing")
	}
	waitForIdleDrain(t, "the query pool", func() int { return s.db.Stats().OpenConnections })
	waitForIdleDrain(t, "the replay handle", func() int { return s.replay.Stats().OpenConnections })
}

// Only the replay's randomly-landing inserts earn the large page cache. The
// budget is per connection and never shrinks, so on a shared handle the
// census scan alone would strand hundreds of MB it never reuses.
func TestReplayCacheStaysOffTheQueryPool(t *testing.T) {
	s := openTestStore(t)

	// Read the pragma back rather than the constant: this is what sqlite
	// actually applied, so a malformed DSN cannot pass.
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
	// One connection, so the large cache exists at most once.
	if got := s.replay.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("replay handle allows %d connections, want 1", got)
	}
}

// The engine re-checks-out a connection every readyPoll while caught up, which
// restarts the idle timer. A window at or above that interval would let a
// connection - and the page cache it filled during the sync - survive
// indefinitely on an index nobody is querying.
func TestIdleConnWindowUndercutsReadyPoll(t *testing.T) {
	readyPoll := NewEngine(config.PTRConfig{}).readyPoll
	if idleConnWindow >= readyPoll {
		t.Errorf("idleConnWindow = %v, must stay under the engine's caught-up poll of %v", idleConnWindow, readyPoll)
	}
	// The other bound: a sync commits a slice, fetches the next blob with
	// fetch_sleep between, then replays again. Dropping the connection in that
	// gap would restart every slice with a cold cache.
	if idleConnWindow <= 30*time.Second {
		t.Errorf("idleConnWindow = %v, too short to span the gap between two sync slices", idleConnWindow)
	}
}
