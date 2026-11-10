package ptr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

type fixtureRepo struct {
	*httptest.Server
	metadata    []byte
	entries     []updateEntry
	nextDue     int64
	updates     map[string][]byte
	gotKey      string
	failMeta    bool
	ignoreSince bool
	metaGate    chan struct{}
	metaHits    int
	updateHits  int
	hitsByBlob  map[string]int
	failBlob    map[string]int
	onUpdate    func()
	mu          sync.Mutex
}

func (fr *fixtureRepo) setEntries(nextDue int64, entries []updateEntry) {
	fr.mu.Lock()
	fr.entries, fr.nextDue = entries, nextDue
	fr.mu.Unlock()
}

func (fr *fixtureRepo) setFailMeta(v bool) {
	fr.mu.Lock()
	fr.failMeta = v
	fr.mu.Unlock()
}

func (fr *fixtureRepo) holdMetadata() (release func()) {
	gate := make(chan struct{})
	fr.mu.Lock()
	fr.metaGate, fr.metaHits = gate, 0
	fr.mu.Unlock()
	return sync.OnceFunc(func() {
		fr.mu.Lock()
		fr.metaGate = nil
		fr.mu.Unlock()
		close(gate)
	})
}

func (fr *fixtureRepo) waitMetaHit(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fr.mu.Lock()
		n := fr.metaHits
		fr.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no /metadata request arrived")
}

func (fr *fixtureRepo) hits() (meta, update int) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.metaHits, fr.updateHits
}

func (fr *fixtureRepo) setIgnoreSince(v bool) {
	fr.mu.Lock()
	fr.ignoreSince = v
	fr.mu.Unlock()
}

func newFixtureRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	fr := &fixtureRepo{updates: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/metadata", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.gotKey = r.Header.Get("Hydrus-Key")
		fail, static, entries, nextDue := fr.failMeta, fr.metadata, fr.entries, fr.nextDue
		ignoreSince, gate := fr.ignoreSince, fr.metaGate
		fr.metaHits++
		fr.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if static != nil {
			_, _ = w.Write(static)
			return
		}
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		if ignoreSince {
			since = 0
		}
		var filtered []updateEntry
		for _, e := range entries {
			if e.Index >= since {
				filtered = append(filtered, e)
			}
		}
		body, err := encodeMetadata(nextDue, filtered)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/update", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.updateHits++
		hash := r.URL.Query().Get("update_hash")
		if fr.hitsByBlob == nil {
			fr.hitsByBlob = map[string]int{}
		}
		fr.hitsByBlob[hash]++
		failing := fr.failBlob[hash] > 0
		if failing {
			fr.failBlob[hash]--
		}
		hook := fr.onUpdate
		fr.mu.Unlock()
		if failing {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if hook != nil {
			hook()
		}
		blob, ok := fr.updates[hash]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(blob)
	})
	fr.Server = httptest.NewServer(mux)
	t.Cleanup(fr.Close)
	return fr
}

func (fr *fixtureRepo) addUpdate(blob []byte) string {
	sum := sha256.Sum256(blob)
	h := hex.EncodeToString(sum[:])
	fr.updates[h] = blob
	return h
}

func testClient(fr *fixtureRepo) *client {
	return newClient(config.PTRConfig{Address: fr.URL, AccessKey: "testkey", FetchSleep: 0})
}

func TestClientMetadataSendsKey(t *testing.T) {
	fr := newFixtureRepo(t)
	h := "aa"
	fr.metadata = buildMetadata(t, 999, []updateEntry{{Index: 0, Hashes: []string{h}, Begin: 1, End: 2}})

	slice, err := testClient(fr).Metadata(context.Background(), 0)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if fr.gotKey != "testkey" {
		t.Errorf("Hydrus-Key = %q, want testkey", fr.gotKey)
	}
	if len(slice.Updates) != 1 || slice.NextUpdateDue != 999 {
		t.Errorf("slice = %+v", slice)
	}
}

func TestClientUpdateVerifiesHash(t *testing.T) {
	fr := newFixtureRepo(t)
	blob := buildDefinitions(t, map[uint64]string{1: "ab"}, nil)
	h := fr.addUpdate(blob)

	up, err := testClient(fr).Update(context.Background(), h)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if up.Definitions == nil || up.Definitions.Hashes[1] != "ab" {
		t.Errorf("update = %+v", up)
	}
}

func TestClientUpdateAcceptsUppercaseHash(t *testing.T) {
	fr := newFixtureRepo(t)
	blob := buildDefinitions(t, map[uint64]string{1: "ab"}, nil)
	h := fr.addUpdate(blob)
	upper := strings.ToUpper(h)
	fr.updates[upper] = blob

	if _, err := testClient(fr).Update(context.Background(), upper); err != nil {
		t.Fatalf("uppercase hash should verify: %v", err)
	}
}

func TestClientUpdateRejectsCorruptedBlob(t *testing.T) {
	fr := newFixtureRepo(t)
	blob := buildDefinitions(t, map[uint64]string{1: "ab"}, nil)
	h := fr.addUpdate(blob)
	fr.updates[h] = buildDefinitions(t, map[uint64]string{2: "cd"}, nil)

	if _, err := testClient(fr).Update(context.Background(), h); err == nil {
		t.Fatal("want an integrity error when the blob does not match its hash")
	}
}

func TestClientRetryableStatuses(t *testing.T) {
	for _, code := range []int{429, 500, 503, 509} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := newClient(config.PTRConfig{Address: srv.URL})
		_, err := c.Metadata(context.Background(), 0)
		if err == nil || !IsRetryable(err) {
			t.Errorf("status %d: err = %v, want a retryable error", code, err)
		}
		srv.Close()
	}
}

func TestClientPermanentStatusNotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := newClient(config.PTRConfig{Address: srv.URL})
	_, err := c.Metadata(context.Background(), 0)
	if err == nil || IsRetryable(err) {
		t.Errorf("403 err = %v, want a permanent (non-retryable) error", err)
	}
}

func TestClientCanceledContext(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.metadata = buildMetadata(t, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := testClient(fr).Metadata(ctx, 0)
	if err == nil {
		t.Fatal("want an error for a canceled context")
	}
	if IsRetryable(err) {
		t.Errorf("a canceled context should not read as retryable: %v", err)
	}
}

func TestPublicKeyDefault(t *testing.T) {
	c := newClient(config.PTRConfig{Address: "http://x"})
	if c.accessKey != config.PublicAccessKey {
		t.Errorf("empty access key should default to the public key, got %q", c.accessKey)
	}
}

func (c *client) Update(ctx context.Context, hashHex string) (*update, error) {
	raw, err := c.updateRaw(ctx, hashHex)
	if err != nil {
		return nil, err
	}
	return parseUpdate(raw)
}

func TestARedirectDoesNotCarryTheKey(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Hydrus-Key") != "" {
			leaked.Store(true)
		}
	}))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer first.Close()

	c := newClient(config.PTRConfig{Address: first.URL, AccessKey: "abababab"})
	if _, err := c.updateRaw(context.Background(), h32("aa")); err == nil {
		t.Error("a redirect should land in the non-200 refusals")
	}
	if leaked.Load() {
		t.Error("the Hydrus-Key followed the redirect to another host")
	}
}
