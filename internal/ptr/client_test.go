package ptr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/leqwin/monloader/internal/config"
)

// fixtureRepo is an httptest server speaking the repository protocol over the
// builder blobs, so client and engine tests run without the real PTR.
type fixtureRepo struct {
	*httptest.Server
	metadata []byte // static body for the low-level client tests
	// entries/nextDue drive the since-filtered manifest the engine tests use.
	entries []UpdateEntry
	nextDue int64
	updates map[string][]byte // sha256 hex -> blob
	gotKey  string
	// failMeta, when set true, makes /metadata answer 500, modeling a transient
	// outage a test later clears.
	failMeta bool
	// ignoreSince, when set true, serves the full manifest whatever the since
	// parameter says, modeling a repository stuck re-publishing old indexes.
	ignoreSince bool
	mu          sync.Mutex
}

// setEntries registers the manifest the engine tests sync; the /metadata
// handler filters it by the request's since parameter, like the real server.
func (fr *fixtureRepo) setEntries(nextDue int64, entries []UpdateEntry) {
	fr.mu.Lock()
	fr.entries, fr.nextDue = entries, nextDue
	fr.mu.Unlock()
}

func (fr *fixtureRepo) setFailMeta(v bool) {
	fr.mu.Lock()
	fr.failMeta = v
	fr.mu.Unlock()
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
		ignoreSince := fr.ignoreSince
		fr.mu.Unlock()
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
		var filtered []UpdateEntry
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
		blob, ok := fr.updates[r.URL.Query().Get("update_hash")]
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

// addUpdate registers a blob under its real sha256 and returns that hex hash.
func (fr *fixtureRepo) addUpdate(blob []byte) string {
	sum := sha256.Sum256(blob)
	h := hex.EncodeToString(sum[:])
	fr.updates[h] = blob
	return h
}

func testClient(fr *fixtureRepo) *Client {
	return NewClient(config.PTRConfig{Address: fr.URL, AccessKey: "testkey", FetchSleep: 0})
}

func TestClientMetadataSendsKey(t *testing.T) {
	fr := newFixtureRepo(t)
	h := "aa"
	fr.metadata = buildMetadata(t, 999, []UpdateEntry{{Index: 0, Hashes: []string{h}, Begin: 1, End: 2}})

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

func TestClientUpdateRejectsCorruptedBlob(t *testing.T) {
	fr := newFixtureRepo(t)
	blob := buildDefinitions(t, map[uint64]string{1: "ab"}, nil)
	h := fr.addUpdate(blob)
	// Serve different bytes than the hash promises.
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
		c := NewClient(config.PTRConfig{Address: srv.URL})
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
	c := NewClient(config.PTRConfig{Address: srv.URL})
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
	c := NewClient(config.PTRConfig{Address: "http://x"})
	if c.accessKey != config.PublicAccessKey {
		t.Errorf("empty access key should default to the public key, got %q", c.accessKey)
	}
}
