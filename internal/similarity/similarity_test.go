package similarity

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/queue"
)

// newClient builds a Client whose endpoints point at the test server and
// whose resolver funcs mimic the mapper: danbooru and gelbooru hosts resolve,
// everything else misses.
func newClient(cfg *config.Config, ts *httptest.Server) *Client {
	c := New(config.NewProvider(cfg), func(raw string) (string, string, bool) {
		switch {
		case strings.Contains(raw, "danbooru.donmai.us"):
			return "danbooru", raw, true
		case strings.Contains(raw, "gelbooru.com"):
			return "gelbooru", raw, true
		}
		return "", "", false
	}, func(site, id string) string {
		return "https://danbooru.donmai.us/posts/" + id
	})
	if ts != nil {
		c.iqdbURL = ts.URL + "/iqdb_queries.json"
		c.saucenaoURL = ts.URL + "/search.php"
	}
	return c
}

// iqdbCfg is a config carrying the danbooru credentials iqdb requires.
func iqdbCfg() *config.Config {
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "danbooru", Username: "u", APIKey: "k"}}
	return cfg
}

// The trimmed shape of a live iqdb answer (2026-07-07): a plain array, best
// first, scores 0-100, the runner-up far below the true match.
const iqdbFixture = `[
  {"post_id": 11742926, "score": 96.3786683999095, "hash": "x", "signature": "y", "post": {"id": 11742926}},
  {"post_id": 5551212, "score": 19.621989405411874, "post": {"id": 5551212}}
]`

func TestIQDBSearchParsesCandidates(t *testing.T) {
	var gotQuery url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("multipart: %v", err)
		}
		gotQuery = r.URL.Query()
		if r.PostFormValue("login") != "" || r.PostFormValue("api_key") != "" {
			t.Error("credentials must not ride the multipart body - danbooru's CSRF exemption cannot see them there")
		}
		if _, _, err := r.FormFile("search[file]"); err != nil {
			t.Errorf("missing search[file] part: %v", err)
		}
		_, _ = w.Write([]byte(iqdbFixture))
	}))
	defer ts.Close()

	res, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery.Get("login") != "u" || gotQuery.Get("api_key") != "k" {
		t.Errorf("query credentials = %q/%q, want the danbooru site block's", gotQuery.Get("login"), gotQuery.Get("api_key"))
	}
	if len(res.Candidates) != 2 || res.DailyRemaining != -1 {
		t.Fatalf("result = %+v, want 2 candidates and no quota", res)
	}
	best := res.Candidates[0]
	if best.Site != "danbooru" || best.URL != "https://danbooru.donmai.us/posts/11742926" || best.Similarity < 96 {
		t.Errorf("best candidate = %+v", best)
	}
}

// Danbooru's iqdb endpoint is a Rails create action: a verified account gets
// a 201 Created carrying the same JSON array a 200 does.
func TestIQDBAccepts201Created(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(iqdbFixture))
	}))
	defer ts.Close()

	res, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("result = %+v, want the 201 body parsed like a 200", res)
	}
}

func TestSauceNAOSearchParsesCandidates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("output_type") != "2" || q.Get("api_key") != "sk" || q.Get("numres") != "16" || len(q["dbs[]"]) != 0 {
			t.Errorf("query = %v, want output_type=2, the api key, numres=16, and no index restriction", q)
		}
		_, _ = w.Write([]byte(`{
		  "header": {"status": 0, "short_remaining": 3, "long_remaining": 42},
		  "results": [
		    {"header": {"similarity": "94.02", "index_id": 9},
		     "data": {"ext_urls": ["https://unknown.example/x", "https://danbooru.donmai.us/post/show/123"]}},
		    {"header": {"similarity": "93.5", "index_id": 25},
		     "data": {"ext_urls": ["https://gelbooru.com/index.php?page=post&s=view&id=456"]}},
		    {"header": {"similarity": "91.0", "index_id": 5},
		     "data": {"ext_urls": ["https://nowhere.example/y"]}}
		  ]}`))
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = "sk"
	res, err := newClient(cfg, ts).Search(context.Background(), "saucenao", ProbeImage())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.DailyRemaining != 42 {
		t.Errorf("DailyRemaining = %d, want 42", res.DailyRemaining)
	}
	// Each result maps by its first recognizable ext_url; a result with no
	// recognizable URL at all is kept as a trail-only extra.
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want 2", res.Candidates)
	}
	if res.Candidates[0].Site != "danbooru" || res.Candidates[0].Similarity != 94.02 {
		t.Errorf("first candidate = %+v", res.Candidates[0])
	}
	if res.Candidates[1].Site != "gelbooru" {
		t.Errorf("second candidate = %+v", res.Candidates[1])
	}
	if len(res.Extras) != 1 || res.Extras[0].URL != "https://nowhere.example/y" ||
		res.Extras[0].Site != "nowhere.example" || res.Extras[0].Similarity != 91.0 {
		t.Errorf("extras = %+v, want the unrecognized result kept with its score", res.Extras)
	}
}

func TestSearchClassifiesFailures(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
	}{
		{"auth", http.StatusForbidden, `{"success":false,"error":"InvalidAuthenticityToken"}`, queue.ErrCodeAuthRequired},
		{"blocked", http.StatusForbidden, "<html>Just a moment... cloudflare</html>", queue.ErrCodeBlocked},
		{"server error", http.StatusBadGateway, "boom", queue.ErrCodeDownloadFailed},
		{"garbage body", http.StatusOK, "<html>not json</html>", queue.ErrCodeDownloadFailed},
	}
	for _, tc := range cases {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
		ts.Close()
		var ce *queue.CodedError
		if !errors.As(err, &ce) || ce.Code != tc.wantCode {
			t.Errorf("%s: err = %v, want code %s", tc.name, err, tc.wantCode)
		}
	}
}

func TestAuthRefusalCarriesServerMessage(t *testing.T) {
	// danbooru names the refusal ("Invalid API key", "Access Denied") in its
	// JSON body; the trail must carry it - the fixes differ.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"Pundit::NotAuthorizedError","message":"Access Denied: create"}`))
	}))
	defer ts.Close()

	_, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeAuthRequired ||
		!strings.Contains(ce.Msg, "Access Denied") || !strings.Contains(ce.Msg, "403") {
		t.Fatalf("err = %v, want auth_required carrying the server message and status", err)
	}
}

func TestIQDBPrivilegeRefusalNamesTheCause(t *testing.T) {
	// danbooru's bare "Access denied" on a credentialed query is a privilege
	// refusal (restricted account or scoped api key), not a bad key; the
	// message must say so or the operator re-checks credentials that are fine.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"User::PrivilegeError","message":"Access denied"}`))
	}))
	defer ts.Close()

	_, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeAuthRequired ||
		!strings.Contains(ce.Msg, "Access denied") || !strings.Contains(ce.Msg, "email-verified") {
		t.Fatalf("err = %v, want auth_required naming the privilege cause", err)
	}
}

func TestRateLimitArmsCooldown(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	c := newClient(iqdbCfg(), ts)
	_, err := c.Search(context.Background(), "iqdb", ProbeImage())
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeRateLimited {
		t.Fatalf("err = %v, want rate_limited", err)
	}
	if until := c.LimitedUntil("iqdb"); !until.After(time.Now()) {
		t.Fatal("a 429 should arm the cooldown")
	}
	// The second call is refused locally, without a request.
	_, err = c.Search(context.Background(), "iqdb", ProbeImage())
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeRateLimited {
		t.Fatalf("cooldown err = %v, want rate_limited", err)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1 - a cooling-down service must not be queried", hits)
	}
	// The other service is unaffected.
	if !c.LimitedUntil("saucenao").IsZero() {
		t.Error("the saucenao cooldown should be untouched")
	}
}

func TestSauceNAOInvalidKeyReadsAsAuth(t *testing.T) {
	// The live answer for a bad or absent key: 403 with saucenao's negative
	// client-error status. It must read as an auth problem carrying the
	// server's message, not as a closed rate window, and must not arm the
	// cooldown.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"header":{"status":-1,"message":"The anonymous account type does not permit API usage."}}`))
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = "bad"
	c := newClient(cfg, ts)
	_, err := c.Search(context.Background(), "saucenao", ProbeImage())
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeAuthRequired || !strings.Contains(ce.Msg, "anonymous account") {
		t.Fatalf("err = %v, want auth_required with the server's message", err)
	}
	if !c.LimitedUntil("saucenao").IsZero() {
		t.Error("a refused key must not arm the rate-limit cooldown")
	}
}

func TestSauceNAODailyQuotaExhausted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"header": {"status": 0, "long_remaining": 0, "short_remaining": 4}, "results": []}`))
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = "sk"
	c := newClient(cfg, ts)
	_, err := c.Search(context.Background(), "saucenao", ProbeImage())
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeRateLimited || !strings.Contains(ce.Msg, "daily") {
		t.Fatalf("err = %v, want the daily-quota message", err)
	}
	// The daily cooldown is the long one.
	if until := c.LimitedUntil("saucenao"); time.Until(until) < 30*time.Minute {
		t.Errorf("cooldown ends %v, want the hour-long daily hold", until)
	}
}

func TestSauceNAOLastQueryStillAnswers(t *testing.T) {
	// long_remaining hits 0 on the answer that used the last query: its
	// results are still good, only the cooldown arms.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "header": {"status": 0, "long_remaining": 0, "short_remaining": 3},
		  "results": [{"header": {"similarity": "95.0"}, "data": {"ext_urls": ["https://danbooru.donmai.us/posts/1"]}}]}`))
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = "sk"
	c := newClient(cfg, ts)
	res, err := c.Search(context.Background(), "saucenao", ProbeImage())
	if err != nil || len(res.Candidates) != 1 {
		t.Fatalf("Search = %+v, %v; want the final answer's candidate", res, err)
	}
	if c.LimitedUntil("saucenao").IsZero() {
		t.Error("the exhausted quota should still arm the cooldown")
	}
}

func TestMissingCredentials(t *testing.T) {
	cfg := config.Default()
	cfg.Sites = nil
	c := newClient(cfg, nil)
	if label, missing := c.Missing("iqdb"); !missing || label != "danbooru api key" {
		t.Errorf("iqdb = %q %v, want missing danbooru api key", label, missing)
	}
	if label, missing := c.Missing("saucenao"); !missing || label != "api key" {
		t.Errorf("saucenao = %q %v, want missing api key", label, missing)
	}

	cfg = iqdbCfg()
	cfg.Lookup.Saucenao.APIKey = "sk"
	c = newClient(cfg, nil)
	if _, missing := c.Missing("iqdb"); missing {
		t.Error("iqdb should be ready with the danbooru credentials set")
	}
	if _, missing := c.Missing("saucenao"); missing {
		t.Error("saucenao should be ready with its key set")
	}
}

func TestProbeImageIsAPNG(t *testing.T) {
	img := ProbeImage()
	if !bytes.HasPrefix(img, []byte("\x89PNG")) {
		t.Fatal("probe image is not a PNG")
	}
	if !bytes.Equal(img, ProbeImage()) {
		t.Error("probe image must be deterministic")
	}
}
