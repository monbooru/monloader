package similarity

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
)

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

func iqdbCfg() *config.Config {
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "danbooru", Username: "u", APIKey: "k"}}
	return cfg
}

const iqdbFixture = `[
  {"post_id": 11742926, "score": 96.3786683999095, "hash": "x", "signature": "y", "post": {"id": 11742926}},
  {"post_id": 5551212, "score": 19.621989405411874, "post": {"id": 5551212}}
]`

func stubServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServiceURLsComeFromTheConfig(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(srv.Close)
	cfg := iqdbCfg()
	cfg.Lookup.Iqdb.URL = srv.URL + "/mirror/iqdb.json"
	cfg.Lookup.Saucenao.URL = srv.URL + "/mirror/sn.php"
	cfg.Lookup.Saucenao.APIKey = "k"
	c := newClient(cfg, nil)
	_, _ = c.Search(context.Background(), "iqdb", []byte("img"))
	_, _ = c.Search(context.Background(), "saucenao", []byte("img"))
	if !slices.Equal(paths, []string{"/mirror/iqdb.json", "/mirror/sn.php"}) {
		t.Errorf("requests went to %v, want the configured endpoints", paths)
	}
}

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

func TestIQDBAccepts201Created(t *testing.T) {
	ts := stubServer(t, http.StatusCreated, iqdbFixture)

	res, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("result = %+v, want the 201 body parsed like a 200", res)
	}
}

func TestIQDBUserAgentFollowsTheSite(t *testing.T) {
	for _, tc := range []struct {
		name, agent, want string
	}{
		{"no agent set", "", "monloader; user #123"},
		{"literal", "my agent/1.0", "my agent/1.0"},
		{"a preset only gallery-dl can expand", "+firefox", "monloader; user #123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.UserAgent()
				_, _ = w.Write([]byte(iqdbFixture))
			}))
			defer ts.Close()

			cfg := iqdbCfg()
			cfg.Sites[0].UserID = "123"
			cfg.Sites[0].Agent = tc.agent
			if _, err := newClient(cfg, ts).Search(context.Background(), "iqdb", ProbeImage()); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if got != tc.want {
				t.Errorf("User-Agent = %q, want %q", got, tc.want)
			}
		})
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
		{"auth", http.StatusForbidden, `{"success":false,"error":"InvalidAuthenticityToken"}`, outcome.ErrCodeAuthRequired},
		{"blocked", http.StatusForbidden, "<html>Just a moment... cloudflare</html>", outcome.ErrCodeBlocked},
		{"server error", http.StatusBadGateway, "boom", outcome.ErrCodeDownloadFailed},
		{"garbage body", http.StatusOK, "<html>not json</html>", outcome.ErrCodeDownloadFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := stubServer(t, tc.status, tc.body)
			_, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
			var ce *outcome.CodedError
			if !errors.As(err, &ce) || ce.Code != tc.wantCode {
				t.Errorf("err = %v, want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestAuthRefusalsCarryTheirCause(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{{
		name: "bad key",
		body: `{"success":false,"error":"Pundit::NotAuthorizedError","message":"Access Denied: create"}`,
		want: []string{"Access Denied", "403"},
	}, {
		name: "privilege refusal",
		body: `{"success":false,"error":"User::PrivilegeError","message":"Access denied"}`,
		want: []string{"Access denied", "email-verified"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ts := stubServer(t, http.StatusForbidden, tc.body)

			_, err := newClient(iqdbCfg(), ts).Search(context.Background(), "iqdb", ProbeImage())
			var ce *outcome.CodedError
			if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeAuthRequired {
				t.Fatalf("err = %v, want auth_required", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(ce.Msg, want) {
					t.Errorf("message %q does not carry %q", ce.Msg, want)
				}
			}
		})
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
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeRateLimited {
		t.Fatalf("err = %v, want rate_limited", err)
	}
	if until := c.LimitedUntil("iqdb"); !until.After(time.Now()) {
		t.Fatal("a 429 should arm the cooldown")
	}
	_, err = c.Search(context.Background(), "iqdb", ProbeImage())
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeRateLimited {
		t.Fatalf("cooldown err = %v, want rate_limited", err)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1 - a cooling-down service must not be queried", hits)
	}
	if !c.LimitedUntil("saucenao").IsZero() {
		t.Error("the saucenao cooldown should be untouched")
	}
}

func TestSauceNAOInvalidKeyReadsAsAuth(t *testing.T) {

	ts := stubServer(t, http.StatusForbidden, `{"header":{"status":-1,"message":"The anonymous account type does not permit API usage."}}`)

	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = "bad"
	c := newClient(cfg, ts)
	_, err := c.Search(context.Background(), "saucenao", ProbeImage())
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeAuthRequired || !strings.Contains(ce.Msg, "anonymous account") {
		t.Fatalf("err = %v, want auth_required with the server's message", err)
	}
	if !c.LimitedUntil("saucenao").IsZero() {
		t.Error("a refused key must not arm the rate-limit cooldown")
	}
}

func TestSauceNAODailyQuotaExhausted(t *testing.T) {
	ts := stubServer(t, http.StatusTooManyRequests,
		`{"header": {"status": 0, "long_remaining": 0, "short_remaining": 4}, "results": []}`)

	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = "sk"
	c := newClient(cfg, ts)
	_, err := c.Search(context.Background(), "saucenao", ProbeImage())
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeRateLimited || !strings.Contains(ce.Msg, "daily") {
		t.Fatalf("err = %v, want the daily-quota message", err)
	}
	if until := c.LimitedUntil("saucenao"); time.Until(until) < 30*time.Minute {
		t.Errorf("cooldown ends %v, want the hour-long daily hold", until)
	}
}

func TestSauceNAOLastQueryStillAnswers(t *testing.T) {
	ts := stubServer(t, http.StatusOK, `{
		  "header": {"status": 0, "long_remaining": 0, "short_remaining": 3},
		  "results": [{"header": {"similarity": "95.0"}, "data": {"ext_urls": ["https://danbooru.donmai.us/posts/1"]}}]}`)

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
	if label, missing := c.Missing("iqdb"); !missing || label != "danbooru login and API key" {
		t.Errorf("iqdb = %q %v, want missing danbooru login and api key", label, missing)
	}
	for _, tc := range []struct {
		site config.Site
		want string
	}{
		{config.Site{Name: "danbooru", APIKey: "k"}, "danbooru username"},
		{config.Site{Name: "danbooru", Username: "u"}, "danbooru API key"},
	} {
		half := config.Default()
		half.Sites = []config.Site{tc.site}
		if label, missing := newClient(half, nil).Missing("iqdb"); !missing || label != tc.want {
			t.Errorf("iqdb with %+v = %q %v, want %q", tc.site, label, missing, tc.want)
		}
	}
	if label, missing := c.Missing("saucenao"); !missing || label != "API key" {
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

func TestSearchDoesNotFollowARedirect(t *testing.T) {
	var elsewhere int
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "[]")
	}))
	defer away.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, away.URL+"/steal?"+r.URL.RawQuery, http.StatusFound)
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = "secret-key"
	c := newClient(cfg, ts)

	_, err := c.Search(context.Background(), "saucenao", []byte("png"))
	if err == nil {
		t.Fatal("a redirect must not be followed into a result")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("err = %v, want the 302 reported", err)
	}
	if elsewhere != 0 {
		t.Errorf("the redirect target was called %d times; the api key must not travel there", elsewhere)
	}
}
