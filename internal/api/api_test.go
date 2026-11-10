package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/sitestate"
)

type fakeRunner struct{ probe gdl.ProbeResult }

func (f fakeRunner) Resolve(context.Context, string, string, bool) (gdl.ResolveResult, error) {
	return gdl.ResolveResult{}, nil
}
func (f fakeRunner) FetchMeta(context.Context, string) (map[string]any, error) { return nil, nil }
func (f fakeRunner) FirstPost(context.Context, string) (map[string]any, error) { return nil, nil }
func (f fakeRunner) Download(context.Context, string, string, string, gdl.Archive, func(gdl.Downloaded), bool) ([]gdl.Downloaded, error) {
	return nil, nil
}
func (f fakeRunner) ListExtractors(context.Context) ([]gdl.Extractor, error) { return nil, nil }

func (f fakeRunner) Probe(_ context.Context, url string) (gdl.ProbeResult, error) {
	if strings.Contains(url, "refused") {
		return gdl.ProbeResult{Status: gdl.ProbeFailed, Detail: "404 not found"}, nil
	}
	return f.probe, nil
}
func (f fakeRunner) DebugLog(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (f fakeRunner) Version(context.Context) string { return "1.32.1" }

func (f fakeRunner) SetSiteLists(gdl.SiteLists) {}

func (f fakeRunner) ExtractorInfo(context.Context, string) (string, string) { return "", "" }

type fakeProc struct{}

func (fakeProc) Process(ctx context.Context, job *queue.Job) error {
	snap := job.Snapshot()
	url := snap.URL
	job.SetItems([]queue.Item{{PostID: "1"}})
	switch {
	case strings.Contains(url, "cap"):
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
		job.UpdateItem(0, func(it *queue.Item) {
			it.Status = queue.ItemDone
			it.Outcome = outcome.Created
			it.MonbooruID = 99
		})

		if snap.Offset == 0 {
			job.SetCapped(1)
		}
	case strings.Contains(url, "dup"):
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
		job.UpdateItem(0, func(it *queue.Item) {
			it.Status = queue.ItemSkipped
			it.Outcome = outcome.Duplicate
			it.MonbooruID = 7
		})
	case strings.Contains(url, "fail"):
		job.UpdateItem(0, func(it *queue.Item) {
			it.Status = queue.ItemFailed
			it.Outcome = outcome.Failed
			it.ErrorCode = outcome.ErrCodeMonbooruRejected
		})
	default:
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
		job.UpdateItem(0, func(it *queue.Item) {
			it.Status = queue.ItemDone
			it.Outcome = outcome.Created
			it.MonbooruID = 42
		})
	}
	return nil
}

const apiTestToken = "test-token"

func serveCfg(t *testing.T, cfg *config.Config) *httptest.Server {
	srv, _ := serveCfgTracked(t, cfg, nil)
	return srv
}

func newPTRServer(t *testing.T, token string, ptr PTRService) *httptest.Server {
	t.Helper()
	srv, _ := serveCfgTracked(t, testCfg(token), ptr)
	return srv
}

func serveCfgTracked(t *testing.T, cfg *config.Config, ptrService PTRService) (*httptest.Server, *sitestate.Tracker) {
	t.Helper()
	return serveProc(t, cfg, ptrService, fakeProc{})
}

func noSimMissing(string) (string, bool) { return "", false }

func serveProc(t *testing.T, cfg *config.Config, ptrService PTRService, proc queue.Processor) (*httptest.Server, *sitestate.Tracker) {
	t.Helper()
	if ptrService == nil {
		ptrService = disabledPTR()
	}
	q := queue.New(proc, 1, 100)
	q.SetLookupBudget(cfg.Lookup.ScheduledDailyBudget)
	q.Start()
	mapper, err := mapping.New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatal(err)
	}
	extractors := []gdl.Extractor{
		{Category: "danbooru", Subcategory: "post", Example: "https://example.com/posts/1"},

		{Category: "", Subcategory: "artist", Example: "https://danbooru.donmai.us/artists/12345"},
		{Category: "gelbooru", Subcategory: "post", Example: "https://example.com/index.php?id=1"},
		{Category: "weirdsite", Subcategory: "post", Example: "https://weird.example/1"},
	}
	siteState := sitestate.New()
	supported := map[string]gdl.SupportedSite{"weirdsite": {Category: "weirdsite", Name: "Weird Site", URL: "https://weird.example/", Auth: "Supported"}}

	pair := PairHandlers{
		Request: func(w http.ResponseWriter, r *http.Request) {
			contract.WriteJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		},
		Status: func(w http.ResponseWriter, r *http.Request) {
			contract.WriteJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		},
		Teardown: func(w http.ResponseWriter, r *http.Request) {
			contract.WriteJSON(w, http.StatusOK, map[string]string{"status": "removed"})
		},
	}
	h := New(q, fakeRunner{probe: gdl.ProbeResult{Status: gdl.ProbeOK}}, mapper, config.NewProvider(cfg), gdl.NewCatalog("1.32.1", "1.32.1", false, extractors, supported), "v1.2.3", siteState, ptrService, Host{SimMissing: noSimMissing, Pair: pair})
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); q.Close() })
	return srv, siteState
}

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return serveCfg(t, testCfg(token))
}

func testCfg(token string) *config.Config {
	cfg := config.Default()
	cfg.Server.BaseURL = "http://localhost:8081"
	if token == "" {
		token = apiTestToken
	}
	cfg.Auth.Tokens = []apitoken.Token{{ID: "test", Name: "test", TokenHash: apitoken.Hash(token), Scopes: config.AllScopes}}
	cfg.Monbooru.APIToken = "tok"
	return cfg
}

func doJSON(t *testing.T, method, url, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		token = apiTestToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(data, &out)
	return resp, out
}

func wantJSON(t *testing.T, method, url, body string, want int) map[string]any {
	t.Helper()
	resp, out := doJSON(t, method, url, "", body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %v", method, url, resp.StatusCode, want, out)
	}
	return out
}

func wantCode(t *testing.T, method, url, body string, status int, code string) map[string]any {
	t.Helper()
	resp, out := doJSON(t, method, url, "", body)
	if resp.StatusCode != status || (code != "" && out["code"] != code) {
		t.Errorf("%s %s = %d %v, want %d %s", method, url, resp.StatusCode, out, status, code)
	}
	return out
}

func postOK(t *testing.T, url, body, what string) map[string]any {
	t.Helper()
	resp, out := doJSON(t, "POST", url, "", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s = %d %v", what, resp.StatusCode, out)
	}
	return out
}

func serveSavingConfig(t *testing.T, cfg *config.Config, ptrSvc PTRService) (*httptest.Server, *config.Provider) {
	t.Helper()
	provider := config.NewProvider(cfg)
	h := New(queue.New(fakeProc{}, 1, 100), nil, nil, provider, nil, "", nil, ptrSvc, Host{
		Pair: PairHandlers{Request: http.NotFound, Status: http.NotFound, Teardown: http.NotFound},
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, provider
}

func TestGalleryRenameMovesTheTargets(t *testing.T) {
	cfg := testCfg("")
	cfg.Monbooru.DefaultGallery = "anime"
	cfg.Sites = []config.Site{{Name: "danbooru", Gallery: "anime"}, {Name: "gelbooru", Gallery: "stock"}}
	srv, provider := serveSavingConfig(t, cfg, nil)

	out := postOK(t, srv.URL+"/api/v1/galleries/rename", `{"from":"anime","to":"art"}`, "rename")
	if out["changed"] != float64(2) {
		t.Errorf("changed = %v, want 2", out["changed"])
	}
	got := provider.Current()
	if got.Monbooru.DefaultGallery != "art" || got.Sites[0].Gallery != "art" || got.Sites[1].Gallery != "stock" {
		t.Errorf("default = %q, sites = %q %q", got.Monbooru.DefaultGallery, got.Sites[0].Gallery, got.Sites[1].Gallery)
	}

	wantCode(t, "POST", srv.URL+"/api/v1/galleries/rename", `{"from":"art"}`, http.StatusBadRequest, errInvalidRequest)
}

func TestHealthNoAuth(t *testing.T) {
	srv := newTestServer(t, "secret")
	body := wantJSON(t, "GET", srv.URL+"/health", "", 200)
	if body["status"] != "ok" || body["version"] != "v1.2.3" || body["gallerydl_version"] != "1.32.1" ||
		body["api"] != float64(APIVersion) || body["min_peer_api"] != float64(MinPeerAPI) {
		t.Errorf("health body = %v", body)
	}
}

func TestPeerMismatch(t *testing.T) {
	for _, c := range []struct {
		api, min int
		mismatch bool
	}{
		{0, 0, false},
		{APIVersion, MinPeerAPI, false},
		{APIVersion + 1, APIVersion, false},
		{APIVersion + 1, APIVersion + 1, true},
	} {
		if got := PeerMismatch(contract.Health{API: c.api, MinPeerAPI: c.min}) != ""; got != c.mismatch {
			t.Errorf("PeerMismatch(%d, %d): mismatch %v, want %v", c.api, c.min, got, c.mismatch)
		}
	}
}

func TestHealthCORS(t *testing.T) {
	srv := newTestServer(t, "secret")
	req, err := http.NewRequest("GET", srv.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://booru.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://booru.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the reflected origin", got)
	}
}

func TestEnqueueAsync(t *testing.T) {
	srv := newTestServer(t, "")
	body := wantJSON(t, "POST", srv.URL+"/api/v1/queue", `{"url":"http://danbooru/posts/1"}`, http.StatusAccepted)
	if _, ok := body["job_id"]; !ok {
		t.Errorf("expected job_id, got %v", body)
	}
}

func TestQueuedJobOmitsStampsItHasNot(t *testing.T) {
	srv := newTestServer(t, "")
	doJSON(t, "POST", srv.URL+"/api/v1/queue/pause", "", "")
	_, body := doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"http://danbooru/posts/1"}`)
	id := int64(body["job_id"].(float64))

	_, got := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", "")
	if got["status"] != "queued" {
		t.Fatalf("status = %v, want queued", got["status"])
	}
	for _, field := range []string{"started_at", "finished_at"} {
		if v, ok := got[field]; ok {
			t.Errorf("%s = %v on a queued job, want it absent", field, v)
		}
	}
}

func TestEnqueueRefusedWhileMonbooruPaused(t *testing.T) {
	cfg := testCfg("")
	cfg.Monbooru.Paused = true
	srv := serveCfg(t, cfg)

	wantCode(t, "POST", srv.URL+"/api/v1/queue", `{"url":"http://danbooru/posts/1"}`, http.StatusConflict, "monbooru_paused")

	for _, path := range []string{"/api/v1/queue/1/retry", "/api/v1/queue/1/continue", "/api/v1/queue/1/continue-all"} {
		wantCode(t, "POST", srv.URL+path, "", http.StatusConflict, "monbooru_paused")
	}

	wantCode(t, "POST", srv.URL+"/api/v1/metadata", `{"image_id":1,"url":"http://danbooru/posts/1"}`, http.StatusAccepted, "")
}

func TestEnqueueRefusedWithoutMonbooru(t *testing.T) {
	cfg := testCfg("")
	cfg.Monbooru.APIURL = ""
	srv := serveCfg(t, cfg)

	wantCode(t, "POST", srv.URL+"/api/v1/queue", `{"url":"http://danbooru/posts/1"}`, http.StatusConflict, "monbooru_unconfigured")
}

func TestEnqueueRefusedWhileUnpaired(t *testing.T) {
	cfg := testCfg("")
	cfg.Monbooru.APIToken = ""
	srv := serveCfg(t, cfg)

	wantCode(t, "POST", srv.URL+"/api/v1/queue", `{"url":"http://danbooru/posts/1"}`, http.StatusConflict, "monbooru_unconfigured")
}

func TestEnqueueMissingURL(t *testing.T) {
	srv := newTestServer(t, "")
	wantCode(t, "POST", srv.URL+"/api/v1/queue", `{}`, http.StatusBadRequest, "")
}

func TestOversizedJSONBodyIs413(t *testing.T) {
	srv := newTestServer(t, "")
	body := `{"url":"https://danbooru.donmai.us/posts/1","options":{"commentary":"` + strings.Repeat("a", maxRequestBody) + `"}}`
	wantCode(t, "POST", srv.URL+"/api/v1/queue", body, http.StatusRequestEntityTooLarge, contract.CodeFileTooLarge)
	wantCode(t, "POST", srv.URL+"/api/v1/sites/danbooru/test", `{"url":"`+strings.Repeat("a", maxRequestBody)+`"}`, http.StatusRequestEntityTooLarge, contract.CodeFileTooLarge)
	wantCode(t, "POST", srv.URL+"/api/v1/queue", `{"url":`, http.StatusBadRequest, errInvalidRequest)
}

func TestRequestValidation(t *testing.T) {
	srv := newTestServer(t, "")
	tagList := func(n int) string {
		return `{"url":"http://x","options":{"tags":[` +
			strings.TrimSuffix(strings.Repeat(`"t",`, n), ",") + `]}}`
	}
	longOption := func(field string, n int) string {
		return `{"url":"http://x","options":{"` + field + `":"` + strings.Repeat("a", n) + `"}}`
	}
	cjkOption := func(field string, n int) string {
		return `{"url":"http://x","options":{"` + field + `":"` + strings.Repeat("\u5b57", n) + `"}}`
	}
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"negative max_items", "POST", "/api/v1/queue", `{"url":"http://x","options":{"max_items":-5}}`, http.StatusBadRequest},
		{"zero max_items", "POST", "/api/v1/queue", `{"url":"http://x","options":{"max_items":0}}`, http.StatusAccepted},
		{"non-http page_url", "POST", "/api/v1/queue", `{"url":"http://x","options":{"page_url":"notaurl"}}`, http.StatusBadRequest},
		{"escaping folder", "POST", "/api/v1/queue", `{"url":"http://x","options":{"folder":"../out"}}`, http.StatusBadRequest},
		{"absolute folder", "POST", "/api/v1/queue", `{"url":"http://x","options":{"folder":"/etc"}}`, http.StatusBadRequest},
		{"relative folder", "POST", "/api/v1/queue", `{"url":"http://x","options":{"folder":"art/2026"}}`, http.StatusAccepted},
		{"http page_url", "POST", "/api/v1/queue", `{"url":"http://x","options":{"page_url":"https://site.example/page"}}`, http.StatusAccepted},
		{"too many tags", "POST", "/api/v1/queue", tagList(maxOptionTags + 1), http.StatusBadRequest},
		{"tags at the cap", "POST", "/api/v1/queue", tagList(maxOptionTags), http.StatusAccepted},
		{"overlong commentary", "POST", "/api/v1/queue", longOption("commentary", contract.MaxCommentary+1), http.StatusBadRequest},
		{"overlong source", "POST", "/api/v1/queue", longOption("source", contract.MaxSourceLabel+1), http.StatusBadRequest},
		{"CJK commentary at the cap", "POST", "/api/v1/queue", cjkOption("commentary", contract.MaxCommentary), http.StatusAccepted},
		{"CJK commentary over the cap", "POST", "/api/v1/queue", cjkOption("commentary", contract.MaxCommentary+1), http.StatusBadRequest},
		{"CJK source at the cap", "POST", "/api/v1/queue", cjkOption("source", contract.MaxSourceLabel), http.StatusAccepted},
		{"CJK via at the cap", "POST", "/api/v1/queue", cjkOption("via", 200), http.StatusAccepted},
		{"via with whitespace", "POST", "/api/v1/queue", `{"url":"http://x","options":{"via":"my sender"}}`, http.StatusBadRequest},
		{"via with a quote", "POST", "/api/v1/queue", `{"url":"http://x","options":{"via":"my\"sender"}}`, http.StatusBadRequest},
		{"plain via", "POST", "/api/v1/queue", `{"url":"http://x","options":{"via":"my-sender"}}`, http.StatusAccepted},
		{"unknown status filter", "GET", "/api/v1/queue?status=bogus", "", http.StatusBadRequest},
		{"known status filter", "GET", "/api/v1/queue?status=failed", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := doJSON(t, tc.method, srv.URL+tc.path, "", tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestEnqueueRejectsNonHTTPURL(t *testing.T) {
	srv := newTestServer(t, "")
	for _, u := range []string{"ftp://h/x", "file:///etc/passwd", "notaurl", "/posts/1"} {
		resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"`+u+`"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("url %q: status = %d, want 400", u, resp.StatusCode)
		}
	}
}

var imageJobEndpoints = []struct{ path, kind string }{
	{"/api/v1/metadata", "metadata"},
	{"/api/v1/replace", "replace"},
}

func TestImageJobEnqueues(t *testing.T) {
	srv := newTestServer(t, "")
	for _, ep := range imageJobEndpoints {
		t.Run(ep.kind, func(t *testing.T) {
			body := wantJSON(t, "POST", srv.URL+ep.path, `{"image_id":42,"gallery":"art","url":"http://danbooru/posts/1"}`, http.StatusAccepted)
			id, ok := body["job_id"].(float64)
			if !ok {
				t.Fatalf("expected job_id, got %v", body)
			}
			job := wantJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(int64(id), 10), "", 200)
			if job["kind"] != ep.kind || job["image_id"] != float64(42) || job["gallery"] != "art" {
				t.Errorf("job = %v, want kind=%s image_id=42 gallery=art", job, ep.kind)
			}
		})
	}
}

func TestImageJobValidation(t *testing.T) {
	srv := newTestServer(t, "")
	for _, ep := range imageJobEndpoints {
		t.Run(ep.kind, func(t *testing.T) {
			for _, tc := range []struct {
				name, body string
			}{
				{"missing image_id", `{"url":"http://danbooru/posts/1"}`},
				{"non-http url", `{"image_id":1,"url":"ftp://h/x"}`},
				{"missing url", `{"image_id":1}`},
				{"invalid json", `{`},
			} {
				resp, body := doJSON(t, "POST", srv.URL+ep.path, "", tc.body)
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
				}
				if body["code"] != "invalid_request" {
					t.Errorf("%s: code = %v, want invalid_request", tc.name, body["code"])
				}
			}
		})
	}
}

func TestEnqueueWaitResolvesOutcomes(t *testing.T) {
	srv := newTestServer(t, "")
	cases := []struct {
		url           string
		wantStatus    string
		wantOutcome   string
		wantMonbooru  float64
		wantErrorCode string
	}{
		{"http://danbooru/posts/1", "succeeded", "created", 42, ""},
		{"http://danbooru/posts/dup", "succeeded", "duplicate", 7, ""},
		{"http://danbooru/posts/fail", "failed", "failed", 0, outcome.ErrCodeMonbooruRejected},
	}
	for _, c := range cases {
		resp, body := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"`+c.url+`"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (resolved)", c.url, resp.StatusCode)
		}
		if body["status"] != c.wantStatus {
			t.Errorf("%s: job status = %v, want %s", c.url, body["status"], c.wantStatus)
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("%s: items = %v", c.url, body["items"])
		}
		it := items[0].(map[string]any)
		if it["outcome"] != c.wantOutcome {
			t.Errorf("%s: outcome = %v, want %s", c.url, it["outcome"], c.wantOutcome)
		}
		if c.wantMonbooru != 0 && it["monbooru_id"] != c.wantMonbooru {
			t.Errorf("%s: monbooru_id = %v, want %v", c.url, it["monbooru_id"], c.wantMonbooru)
		}
		if c.wantErrorCode != "" && it["error_code"] != c.wantErrorCode {
			t.Errorf("%s: error_code = %v, want %s", c.url, it["error_code"], c.wantErrorCode)
		}
	}
}

type gatedProc struct {
	started chan struct{}
	gate    chan struct{}
	mu      sync.Mutex
	order   []string
}

func (p *gatedProc) Process(ctx context.Context, job *queue.Job) error {
	url := job.Snapshot().URL
	p.mu.Lock()
	p.order = append(p.order, url)
	first := len(p.order) == 1
	p.mu.Unlock()
	if first {
		close(p.started)
		<-p.gate
	}
	return fakeProc{}.Process(ctx, job)
}

func TestWaitEnqueueRunsAheadOfQueuedBulk(t *testing.T) {
	proc := &gatedProc{started: make(chan struct{}), gate: make(chan struct{})}
	srv, _ := serveProc(t, testCfg(""), nil, proc)

	doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"http://x/bulk1"}`)
	<-proc.started
	doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"http://x/bulk2"}`)
	waited := make(chan *http.Response, 1)
	go func() {
		resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/single"}`)
		waited <- resp
	}()
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, list := doJSON(t, "GET", srv.URL+"/api/v1/queue", "", ""); list["total"] == float64(3) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the wait enqueue never reached the queue")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(proc.gate)

	if resp := <-waited; resp.StatusCode != http.StatusOK {
		t.Errorf("wait status = %d, want 200 (resolved)", resp.StatusCode)
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if want := []string{"http://x/bulk1", "http://x/single"}; len(proc.order) < 2 || !slices.Equal(proc.order[:2], want) {
		t.Errorf("run order = %v, want the wait job right after the running bulk one", proc.order)
	}
}

func TestListGetRetryDelete(t *testing.T) {
	srv := newTestServer(t, "")
	_, body := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/fail"}`)
	id := int64(body["id"].(float64))

	resp, list := doJSON(t, "GET", srv.URL+"/api/v1/queue", "", "")
	if resp.StatusCode != 200 || list["total"].(float64) < 1 {
		t.Errorf("list total = %v", list["total"])
	}
	resp, got := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", "")
	if resp.StatusCode != 200 || int64(got["id"].(float64)) != id {
		t.Errorf("get id = %v, want %d", got["id"], id)
	}
	wantCode(t, "GET", srv.URL+"/api/v1/queue/99999", "", 404, "")
	wantCode(t, "POST", srv.URL+"/api/v1/queue/"+itoa(id)+"/retry", "", http.StatusAccepted, "")
	doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", "")
	wantCode(t, "DELETE", srv.URL+"/api/v1/queue/"+itoa(id), "", http.StatusNoContent, "")
	wantCode(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", 404, "")
}

func TestDeleteClearsSeries(t *testing.T) {
	srv := newTestServer(t, "")

	_, job := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/cap"}`)
	root := int64(job["id"].(float64))
	_, cont := doJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(root)+"/continue", "", "")
	child := int64(cont["job_id"].(float64))

	for i := 0; i < 200; i++ {
		_, cj := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(child), "", "")
		if s, _ := cj["status"].(string); s != "queued" && s != "running" {
			break
		}
	}

	if resp, _ := doJSON(t, "DELETE", srv.URL+"/api/v1/queue/"+itoa(root), "", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	for _, id := range []int64{root, child} {
		if r, _ := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", ""); r.StatusCode != http.StatusNotFound {
			t.Errorf("series member %d present after delete, get = %d, want 404", id, r.StatusCode)
		}
	}
}

func TestRetryForce(t *testing.T) {
	srv := newTestServer(t, "")

	job := wantJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", `{"url":"http://danbooru/posts/1"}`, 200)
	id := int64(job["id"].(float64))

	wantJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(id)+"/retry?force=1", "", http.StatusAccepted)
	if _, got := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", ""); got["force"] != true {
		t.Errorf("job force = %v, want true", got["force"])
	}
}

func TestContinueEndpoints(t *testing.T) {
	for _, action := range []string{"continue", "continue-all"} {
		t.Run(action, func(t *testing.T) {
			srv := newTestServer(t, "")

			_, job := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/cap"}`)
			id := int64(job["id"].(float64))
			body := wantJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(id)+"/"+action, "", http.StatusAccepted)
			newID := int64(body["job_id"].(float64))
			if newID == id {
				t.Errorf("%s should return a new job id, got the source %d", action, id)
			}
			if r, _ := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(newID), "", ""); r.StatusCode != 200 {
				t.Errorf("follow-up job %d should exist, get status = %d", newID, r.StatusCode)
			}

			_, plain := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/ok"}`)
			pid := int64(plain["id"].(float64))
			if r, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(pid)+"/"+action, "", ""); r.StatusCode != http.StatusConflict {
				t.Errorf("%s on a non-capped job = %d, want 409", action, r.StatusCode)
			}
			if r, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue/99999/"+action, "", ""); r.StatusCode != http.StatusNotFound {
				t.Errorf("%s on unknown id = %d, want 404", action, r.StatusCode)
			}
		})
	}
}

func TestJobExposesSeriesRoot(t *testing.T) {
	srv := newTestServer(t, "")

	_, job := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/cap"}`)
	id := int64(job["id"].(float64))
	if root, ok := job["root"].(float64); !ok || int64(root) != id {
		t.Fatalf("a fresh job should expose root == id: id=%d root=%v", id, job["root"])
	}

	_, body := doJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(id)+"/continue", "", "")
	nid := int64(body["job_id"].(float64))
	_, cont := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(nid), "", "")
	if root, ok := cont["root"].(float64); !ok || int64(root) != id {
		t.Errorf("a continuation should carry the originating root %d, got %v", id, cont["root"])
	}
}

func TestPauseResume(t *testing.T) {
	srv := newTestServer(t, "")

	_, list := doJSON(t, "GET", srv.URL+"/api/v1/queue", "", "")
	if list["paused"] != false {
		t.Errorf("initial list paused = %v, want false", list["paused"])
	}

	resp, out := doJSON(t, "POST", srv.URL+"/api/v1/queue/pause", "", "")
	if resp.StatusCode != http.StatusOK || out["paused"] != true {
		t.Fatalf("pause: status=%d paused=%v", resp.StatusCode, out["paused"])
	}
	if _, list = doJSON(t, "GET", srv.URL+"/api/v1/queue", "", ""); list["paused"] != true {
		t.Errorf("after pause, list paused = %v, want true", list["paused"])
	}

	resp, out = doJSON(t, "POST", srv.URL+"/api/v1/queue/resume", "", "")
	if resp.StatusCode != http.StatusOK || out["paused"] != false {
		t.Fatalf("resume: status=%d paused=%v", resp.StatusCode, out["paused"])
	}
	if _, list = doJSON(t, "GET", srv.URL+"/api/v1/queue", "", ""); list["paused"] != false {
		t.Errorf("after resume, list paused = %v, want false", list["paused"])
	}
}

func TestPauseRequiresWriteScope(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BaseURL = "http://localhost:8081"
	cfg.Auth.Tokens = []apitoken.Token{{ID: "ro", Name: "ro", TokenHash: apitoken.Hash("ro-token"), Scopes: []string{config.ScopeRead}}}
	srv := serveCfg(t, cfg)
	resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue/pause", "ro-token", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("pause with read-only token = %d, want 403", resp.StatusCode)
	}
}

func TestBearerAuth(t *testing.T) {
	srv := newTestServer(t, "secret")
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/queue", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing-header status = %d, want 401", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", srv.URL+"/api/v1/queue", "wrong", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token status = %d, want 401", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", srv.URL+"/api/v1/queue", "secret", "")
	if resp.StatusCode != 200 {
		t.Errorf("good-token status = %d, want 200", resp.StatusCode)
	}
	wantCode(t, "GET", srv.URL+"/health", "", 200, "")
}

func TestAPIDisabledWithoutTokens(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BaseURL = "http://localhost:8081"
	srv := serveCfg(t, cfg)
	resp, body := doJSON(t, "GET", srv.URL+"/api/v1/queue", "x", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body["code"] != "api_disabled" {
		t.Errorf("code = %v, want api_disabled", body["code"])
	}
}

func TestScopeEnforcement(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BaseURL = "http://localhost:8081"
	cfg.Auth.Tokens = []apitoken.Token{{ID: "ro", Name: "ro", TokenHash: apitoken.Hash("ro-token"), Scopes: []string{config.ScopeRead}}}
	srv := serveCfg(t, cfg)
	resp, _ := doJSON(t, "GET", srv.URL+"/api/v1/queue", "ro-token", "")
	if resp.StatusCode != 200 {
		t.Errorf("read GET status = %d, want 200", resp.StatusCode)
	}
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/queue", "ro-token", `{"url":"http://danbooru/posts/1"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("read POST status = %d, want 403", resp.StatusCode)
	}
	if body["code"] != "insufficient_scope" {
		t.Errorf("code = %v, want insufficient_scope", body["code"])
	}
}

func TestEveryEndpointIsGatedAsDeclared(t *testing.T) {
	cfg := testCfg("")
	cfg.Auth.Tokens = append(cfg.Auth.Tokens,
		apitoken.Token{ID: "ro", Name: "ro", TokenHash: apitoken.Hash("ro-token"), Scopes: []string{config.ScopeRead}})
	srv, _ := serveCfgTracked(t, cfg, &stubPTR{enabled: true})

	h := &Handler{}
	var noAuth []string
	for _, e := range h.endpoints() {
		url := srv.URL + strings.ReplaceAll(strings.ReplaceAll(e.Path, "{id}", "1"), "{name}", "danbooru")
		if e.NoAuth {
			noAuth = append(noAuth, e.Method+" "+e.Path)
			resp, _ := doJSON(t, e.Method, url, "not-a-token", "")
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				t.Errorf("%s %s: declared NoAuth but answered 401", e.Method, e.Path)
			}
			continue
		}
		t.Run(e.Method+" "+e.Path, func(t *testing.T) {
			req, err := http.NewRequest(e.Method, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("no header = %d, want 401", resp.StatusCode)
			}
			want := http.StatusForbidden
			if scopeForMethod(e.Method) == config.ScopeRead || e.ReadScope {
				want = 0
			}
			resp, body := doJSON(t, e.Method, url, "ro-token", "{}")
			resp.Body.Close()
			if want == http.StatusForbidden && (resp.StatusCode != want || body["code"] != "insufficient_scope") {
				t.Errorf("read-only token = %d %v, want 403 insufficient_scope", resp.StatusCode, body["code"])
			}
			if want == 0 && resp.StatusCode == http.StatusForbidden {
				t.Errorf("read-only token = 403 on an endpoint declared readable")
			}
		})
	}
	slices.Sort(noAuth)
	want := []string{
		"GET /api/v1/docs", "GET /api/v1/openapi.json", "GET /health",
		"GET /api/v1/pair/status", "POST /api/v1/pair/remove", "POST /api/v1/pair/request",
	}
	slices.Sort(want)
	if !slices.Equal(noAuth, want) {
		t.Errorf("ungated endpoints = %v, want %v", noAuth, want)
	}
}

func TestPublishedSchemasMatchTheStructsTheySerialize(t *testing.T) {
	declared := map[string][]string{}
	for _, s := range apiSchemas {
		names := make([]string, 0, len(s.Props))
		for _, p := range s.Props {
			names = append(names, p.Name)
		}
		slices.Sort(names)
		declared[s.Name] = names
	}
	for _, tc := range []struct {
		schema string
		value  any
	}{
		{"Summary", queue.Summary{}},
		{"Item", queue.Item{}},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			var tags []string
			rt := reflect.TypeOf(tc.value)
			for i := range rt.NumField() {
				name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
				if name != "" && name != "-" {
					tags = append(tags, name)
				}
			}
			slices.Sort(tags)
			if !slices.Equal(tags, declared[tc.schema]) {
				t.Errorf("%s: struct tags %v, declared schema %v", tc.schema, tags, declared[tc.schema])
			}
		})
	}
}

func TestWaitSecondsClamps(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 0}, {"abc", 0}, {"-1", 0}, {"5", 5}, {"60", 60}, {"100000", maxWaitSeconds},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/queue?wait="+tc.raw, nil)
			if got := waitSeconds(r); got != tc.want {
				t.Errorf("waitSeconds(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestListSites(t *testing.T) {
	srv := newTestServer(t, "")
	body := wantJSON(t, "GET", srv.URL+"/api/v1/sites", "", 200)
	sites := body["sites"].([]any)

	first := sites[0].(map[string]any)
	if first["curated"] != true {
		t.Errorf("first site should be curated, got %v", first)
	}

	var aibooru map[string]any
	for _, s := range sites {
		if e := s.(map[string]any); e["category"] == "aibooru" {
			aibooru = e
			break
		}
	}
	if aibooru == nil {
		t.Fatal("aibooru not listed; a multi-instance curated profile must be surfaced")
	}
	if aibooru["curated"] != true {
		t.Errorf("aibooru should be curated, got %v", aibooru)
	}
	if ex, _ := aibooru["example"].(string); !strings.Contains(ex, "aibooru.online") {
		t.Errorf("aibooru example = %q, want the aibooru.online host so a client can match it", ex)
	}
	var danbooru map[string]any
	for _, s := range sites {
		if e := s.(map[string]any); e["category"] == "danbooru" {
			danbooru = e
			break
		}
	}
	if danbooru == nil {
		t.Fatal("danbooru not listed")
	}
	hosts, _ := danbooru["hosts"].([]any)
	if !slices.Contains(hosts, any("donmai.us")) {
		t.Errorf("danbooru hosts = %v, want donmai.us among the aliases", hosts)
	}
	for _, s := range sites {
		if e := s.(map[string]any); e["category"] == "" {
			t.Fatalf("a blank-category entry reached the API: %v", e)
		}
	}

	_, filtered := doJSON(t, "GET", srv.URL+"/api/v1/sites?q=gel", "", "")
	fs := filtered["sites"].([]any)
	if len(fs) != 1 || fs[0].(map[string]any)["category"] != "gelbooru" {
		t.Errorf("q=gel should return only gelbooru, got %v", fs)
	}
}

func TestSiteProbe(t *testing.T) {
	srv := newTestServer(t, "")
	body := wantJSON(t, "POST", srv.URL+"/api/v1/sites/danbooru/test", "", 200)
	if body["status"] != string(gdl.ProbeOK) {
		t.Errorf("probe status = %v, want ok", body["status"])
	}
	wantCode(t, "POST", srv.URL+"/api/v1/sites/nosuchsite/test", "", 404, "")
}

func TestSiteProbeURLOverride(t *testing.T) {
	srv := newTestServer(t, "")
	for _, tc := range []struct {
		name, path, body string
	}{
		{"option-shaped query url", "/api/v1/sites/danbooru/test?url=-o%20extractor.x=1", ""},
		{"non-http query url", "/api/v1/sites/danbooru/test?url=ftp://h/x", ""},
		{"option-shaped body url", "/api/v1/sites/danbooru/test", `{"url":"--write-log /tmp/x"}`},
		{"unparseable body", "/api/v1/sites/danbooru/test", `{"url":`},
	} {
		resp, body := doJSON(t, "POST", srv.URL+tc.path, "", tc.body)
		if resp.StatusCode != http.StatusBadRequest || body["code"] != "invalid_request" {
			t.Errorf("%s: status = %d code = %v, want 400 invalid_request", tc.name, resp.StatusCode, body["code"])
		}
	}
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/sites/danbooru/test", "",
		`{"url":"https://danbooru.donmai.us/posts/1"}`)
	if resp.StatusCode != 200 || body["status"] != string(gdl.ProbeOK) {
		t.Errorf("a valid override = %d %v, want 200 ok", resp.StatusCode, body["status"])
	}
}

func TestSiteProbeRecordsReachedOnlyForAKnownSite(t *testing.T) {
	srv, tracker := serveCfgTracked(t, testCfg(""), nil)
	wantJSON(t, "POST", srv.URL+"/api/v1/sites/nosuchsite/test", `{"url":"https://danbooru.donmai.us/posts/1"}`, 200)
	if !tracker.LastReached("nosuchsite").IsZero() {
		t.Error("a url probed under an unknown name recorded that name as reached")
	}
}

func TestImageJobsNormalizeTheirURL(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/metadata", "", `{"image_id":1,"url":" Https://Danbooru.donmai.us/posts/1 "}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%v)", resp.StatusCode, body)
	}
	_, job := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(int64(body["job_id"].(float64)), 10), "", "")
	if job["url"] != "https://danbooru.donmai.us/posts/1" {
		t.Errorf("stored url = %v, want it trimmed with a lower-case scheme and host", job["url"])
	}
}

func TestSiteProbeNamesTheMissingCredential(t *testing.T) {
	srv := newTestServer(t, "")
	const refused = "?url=https://example.com/refused"
	_, body := doJSON(t, "POST", srv.URL+"/api/v1/sites/gelbooru/test"+refused, "", "")
	if body["status"] != string(gdl.ProbeFailed) || body["missing_credential"] != "api key" {
		t.Errorf("gelbooru probe = %v, want a failure naming the api key it requires", body)
	}
	_, body = doJSON(t, "POST", srv.URL+"/api/v1/sites/danbooru/test"+refused, "", "")
	if _, ok := body["missing_credential"]; ok {
		t.Errorf("danbooru probe = %v, want no credential named for an optional key", body)
	}
	_, body = doJSON(t, "POST", srv.URL+"/api/v1/sites/gelbooru/test", "", "")
	if _, ok := body["missing_credential"]; ok {
		t.Errorf("successful probe = %v, want no credential named", body)
	}
}

func TestSiteProbeRecordsReached(t *testing.T) {
	cfg := testCfg("")
	srv, tracker := serveCfgTracked(t, cfg, nil)
	wantJSON(t, "POST", srv.URL+"/api/v1/sites/danbooru/test", "", 200)
	if tracker.LastReached("danbooru").IsZero() {
		t.Error("a successful probe left the site's last-reached time unset")
	}
}

func TestOpenAPIStructure(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(srv.URL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("openapi is not valid JSON: %v", err)
	}
	if spec["openapi"] != "3.0.3" {
		t.Errorf("openapi version = %v, want 3.0.3", spec["openapi"])
	}
	info := spec["info"].(map[string]any)
	if info["title"] == "" || info["version"] == "" {
		t.Error("info.title/version must be set")
	}
	paths := spec["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("no paths")
	}
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)

	var refs []string
	collectRefs(spec, &refs)
	for _, ref := range refs {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if _, ok := schemas[name]; !ok {
			t.Errorf("dangling $ref %q", ref)
		}
	}
	for p, item := range paths {
		ops := item.(map[string]any)
		for m, op := range ops {
			o := op.(map[string]any)
			if _, ok := o["responses"]; !ok {
				t.Errorf("%s %s has no responses", m, p)
			}
			if s, _ := o["summary"].(string); s == "" {
				t.Errorf("%s %s has no summary", m, p)
			}
			if id, _ := o["operationId"].(string); id == "" {
				t.Errorf("%s %s has no operationId", m, p)
			}
		}
	}
	for _, p := range []string{"/api/v1/metadata", "/api/v1/pair/request", "/api/v1/pair/status", "/api/v1/pair/remove"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("spec is missing %s", p)
		}
	}
}

func TestDocsRenders(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(srv.URL + "/api/v1/docs")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if !bytes.Contains(body, []byte("/api/v1/queue")) {
		t.Error("docs should list the queue endpoint")
	}
	for _, want := range []string{
		"<code>options.gallery</code>",
		"Run the repository&#39;s open auto-creation",
		"<code>created</code> boolean",
		"<code>progress.update_index</code>",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("docs should render %s", want)
		}
	}
}

func TestOpenAPIMatchesTheHandlers(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(srv.URL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths      map[string]map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	err = json.NewDecoder(resp.Body).Decode(&spec)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec.Components.Schemas["Health"]["properties"].(map[string]any)["app"]; !ok {
		t.Error("Health omits app")
	}
	for path, methods := range spec.Paths {
		if !strings.Contains(path, "/queue/{id}") && !strings.Contains(path, "/ptr/contrib/") {
			continue
		}
		for method, op := range methods {
			params, _ := op["parameters"].([]any)
			for _, raw := range params {
				param := raw.(map[string]any)
				if param["name"] == "id" && param["schema"].(map[string]any)["type"] != "integer" {
					t.Errorf("%s %s types its id %v, want integer", method, path, param["schema"])
				}
			}
		}
	}
	account := spec.Paths["/api/v1/ptr/account"]["post"]["responses"].(map[string]any)
	for _, code := range []string{"500", "502"} {
		if _, ok := account[code]; !ok {
			t.Errorf("POST /api/v1/ptr/account omits its %s", code)
		}
	}
	if _, ok := spec.Paths["/api/v1/queue"]["post"]["responses"].(map[string]any)["413"]; !ok {
		t.Error("POST /api/v1/queue omits its 413")
	}
	if _, ok := spec.Paths["/api/v1/pair/request"]["post"]["responses"].(map[string]any)["413"]; ok {
		t.Error("POST /api/v1/pair/request declares a 413 its handler never answers")
	}
	schemaOf := func(path, method, status string) map[string]any {
		rs, _ := spec.Paths[path][method]["responses"].(map[string]any)
		r, _ := rs[status].(map[string]any)
		content, _ := r["content"].(map[string]any)
		c, _ := content["application/json"].(map[string]any)
		s, _ := c["schema"].(map[string]any)
		return s
	}
	ledger, _ := schemaOf("/api/v1/ptr/contrib", "get", "200")["properties"].(map[string]any)
	for _, field := range []string{"unsent", "counts", "log"} {
		if _, ok := ledger[field]; !ok {
			t.Errorf("GET /api/v1/ptr/contrib 200 omits %s", field)
		}
	}
	if props, _ := schemaOf("/api/v1/ptr/contrib", "delete", "200")["properties"].(map[string]any); props["rescinded"] == nil {
		t.Error("DELETE /api/v1/ptr/contrib 200 omits rescinded")
	}
	for _, p := range []string{"/api/v1/ptr/contrib/commit", "/api/v1/ptr/contrib/log/{id}/rescind"} {
		if ref := schemaOf(p, "post", "202")["$ref"]; ref != "#/components/schemas/EnqueueResponse" {
			t.Errorf("POST %s 202 = %v, want the job id schema", p, ref)
		}
	}
	for path, ops := range spec.Paths {
		for method, op := range ops {
			_, open := op["security"]
			responses, _ := op["responses"].(map[string]any)
			for _, code := range []string{"401", "403", "503"} {
				if _, ok := responses[code]; !ok && !open {
					t.Errorf("%s %s omits the bearer gate's %s", method, path, code)
				}
			}
			if _, ok := responses["503"]; ok && open {
				t.Errorf("%s %s skips the bearer gate but declares api_disabled", method, path)
			}
		}
	}
	if body := spec.Paths["/api/v1/sites/{name}/test"]["post"]["requestBody"].(map[string]any); body["required"] != false {
		t.Errorf("testSite request body = %v, want an optional body", body)
	}
	for _, p := range spec.Paths["/api/v1/queue"]["post"]["parameters"].([]any) {
		if p := p.(map[string]any); p["name"] == "wait" && p["schema"].(map[string]any)["type"] != "integer" {
			t.Errorf("wait is typed %v, want integer", p["schema"])
		}
	}
}

func TestDocEndpointsCORS(t *testing.T) {
	srv := newTestServer(t, "secret")
	for _, path := range []string{"/api/v1/openapi.json", "/api/v1/docs"} {
		req, err := http.NewRequest("GET", srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://booru.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://booru.example" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want the reflected origin", path, got)
		}
	}
}

func collectRefs(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				if s, ok := val.(string); ok {
					*out = append(*out, s)
				}
			} else {
				collectRefs(val, out)
			}
		}
	case []any:
		for _, e := range t {
			collectRefs(e, out)
		}
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestLookupEnqueues(t *testing.T) {
	srv := newTestServer(t, "")
	md5 := "0123456789ABCDEF0123456789abcdef"
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":42,"backend":"booru","md5":"`+md5+`","gallery":"art"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %v", resp.StatusCode, body)
	}
	id, ok := body["job_id"].(float64)
	if !ok {
		t.Fatalf("expected job_id, got %v", body)
	}
	job := wantJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(int64(id), 10), "", 200)
	if job["kind"] != "lookup" || job["backend"] != "booru" || job["image_id"] != float64(42) ||
		job["md5"] != strings.ToLower(md5) {
		t.Errorf("job = %v, want a booru lookup keyed on the md5", job)
	}

	resp, body = doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":42,"backend":"all","md5":"`+md5+`","sha256":"`+strings.Repeat("a", 64)+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("all backend status = %d, want 202: %v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":42,"backend":"all","md5":"`+md5+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("all backend md5-only status = %d, want 202: %v", resp.StatusCode, body)
	}
}

func TestLookupBudgetRefusesOnlyBudgeted(t *testing.T) {
	md5 := "0123456789abcdef0123456789abcdef"
	cfg := config.Default()
	cfg.Auth.Tokens = []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash(apiTestToken), Scopes: config.AllScopes}}
	cfg.Lookup.ScheduledDailyBudget = 1
	srv := serveCfg(t, cfg)

	post := func(extra string) (int, map[string]any) {
		resp, body := doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
			`{"image_id":1,"backend":"booru","md5":"`+md5+`"`+extra+`}`)
		return resp.StatusCode, body
	}
	if status, body := post(`,"background":true,"budgeted":true}`); status != http.StatusAccepted {
		t.Fatalf("first budgeted lookup = %d %v, want 202", status, body)
	}
	status, body := post(`,"background":true,"budgeted":true}`)
	if status != http.StatusTooManyRequests || body["code"] != "budget_exhausted" {
		t.Errorf("second budgeted lookup = %d %v, want 429 budget_exhausted", status, body)
	}
	if status, body := post(`,"background":true}`); status != http.StatusAccepted {
		t.Errorf("bulk lookup past the cap = %d %v, want 202", status, body)
	}
	if status, body := post(`}`); status != http.StatusAccepted {
		t.Errorf("plain lookup past the cap = %d %v, want 202", status, body)
	}

	st := wantJSON(t, "GET", srv.URL+"/api/v1/lookup/status", "", http.StatusOK)
	if st["daily_budget"] != float64(1) || st["left_today"] != float64(0) {
		t.Errorf("lookup status = %v, want a cap of 1 with none left", st)
	}
	if chain, _ := st["chain"].([]any); len(chain) == 0 {
		t.Errorf("chain = %v, want the default walk's sources", st["chain"])
	}
}

func TestLookupAllRequiresSHA256WhenPTREnabled(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true})
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":1,"backend":"all","md5":"0123456789abcdef0123456789abcdef"}`)
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "invalid_request" {
		t.Errorf("all without sha256 (ptr on) = %d %v, want 400 invalid_request", resp.StatusCode, body)
	}
}

func TestLookupValidation(t *testing.T) {
	srv := newTestServer(t, "")
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"missing image_id", `{"backend":"booru","md5":"0123456789abcdef0123456789abcdef"}`, http.StatusBadRequest, "invalid_request"},
		{"unknown backend", `{"image_id":1,"backend":"hydrus","md5":"0123456789abcdef0123456789abcdef"}`, http.StatusBadRequest, "invalid_request"},
		{"missing backend", `{"image_id":1,"md5":"0123456789abcdef0123456789abcdef"}`, http.StatusBadRequest, "invalid_request"},
		{"short md5", `{"image_id":1,"backend":"booru","md5":"abc"}`, http.StatusBadRequest, "invalid_request"},
		{"non-hex md5", `{"image_id":1,"backend":"booru","md5":"zz23456789abcdef0123456789abcdef"}`, http.StatusBadRequest, "invalid_request"},
		{"booru with only sha256", `{"image_id":1,"backend":"booru","sha256":"` + strings.Repeat("a", 64) + `"}`, http.StatusBadRequest, "invalid_request"},
		{"ptr bad sha256", `{"image_id":1,"backend":"ptr","sha256":"abc"}`, http.StatusBadRequest, "invalid_request"},
		{"ptr disabled", `{"image_id":1,"backend":"ptr","sha256":"` + strings.Repeat("a", 64) + `"}`, http.StatusConflict, "ptr_unavailable"},
		{"all with only sha256", `{"image_id":1,"backend":"all","sha256":"` + strings.Repeat("a", 64) + `"}`, http.StatusBadRequest, "invalid_request"},
		{"all with bad sha256", `{"image_id":1,"backend":"all","md5":"0123456789abcdef0123456789abcdef","sha256":"abc"}`, http.StatusBadRequest, "invalid_request"},
		{"invalid json", `{`, http.StatusBadRequest, "invalid_request"},
	} {
		resp, body := doJSON(t, "POST", srv.URL+"/api/v1/lookup", "", tc.body)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.status)
		}
		if body["code"] != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.name, body["code"], tc.code)
		}
	}
}

func TestEnqueueMD5FormImports(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/queue", "",
		`{"url":"md5:0123456789abcdef0123456789abcdef","options":{"gallery":"art"}}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %v", resp.StatusCode, body)
	}
	id := int64(body["job_id"].(float64))
	_, job := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(id, 10), "", "")
	if job["kind"] != "hash_import" || job["md5"] != "0123456789abcdef0123456789abcdef" || job["gallery"] != "art" {
		t.Errorf("job = %v, want a hash import keyed on the md5", job)
	}
	wantCode(t, "POST", srv.URL+"/api/v1/queue", `{"url":"md5:xyz"}`, http.StatusBadRequest, "")
	wantCode(t, "POST", srv.URL+"/api/v1/queue", `{"url":"0123456789abcdef0123456789abcdef"}`, http.StatusBadRequest, "")
	resp, body = doJSON(t, "POST", srv.URL+"/api/v1/queue", "",
		`{"url":"  MD5:0123456789ABCDEF0123456789ABCDEF  "}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("padded uppercase md5 = %d, want 202: %v", resp.StatusCode, body)
	}
	id = int64(body["job_id"].(float64))
	_, job = doJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(id, 10), "", "")
	if job["md5"] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("job md5 = %v, want the lower-cased hash", job["md5"])
	}
}

func TestEnqueueTrimsAPaddedURL(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/queue", "",
		`{"url":"  https://danbooru.donmai.us/posts/135\n"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %v", resp.StatusCode, body)
	}
	id := int64(body["job_id"].(float64))
	_, job := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(id, 10), "", "")
	if job["url"] != "https://danbooru.donmai.us/posts/135" {
		t.Errorf("stored url = %v, want the trimmed one", job["url"])
	}
}

func TestEnqueueLowercasesACapitalizedSchemeAndHost(t *testing.T) {
	srv := newTestServer(t, "")
	_, body := doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"Https://DANBOORU.donmai.us/posts/135#Note"}`)
	id := int64(body["job_id"].(float64))
	_, job := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(id, 10), "", "")
	if job["url"] != "https://danbooru.donmai.us/posts/135#Note" {
		t.Errorf("stored url = %v, want a lower-case scheme and host, the rest as sent", job["url"])
	}
}

func TestListSitesFields(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Tokens = []apitoken.Token{{ID: "test", Name: "test", TokenHash: apitoken.Hash(apiTestToken), Scopes: config.AllScopes}}
	cfg.Sites = append(cfg.Sites, config.Site{Name: "weirdsite", Gallery: "art"})
	srv, _ := serveCfgTracked(t, cfg, nil)
	_, body := doJSON(t, "GET", srv.URL+"/api/v1/sites?q=weird", "", "")
	sites := body["sites"].([]any)
	if len(sites) != 1 {
		t.Fatalf("q=weird should return one site, got %v", sites)
	}
	e := sites[0].(map[string]any)
	if e["name"] != "Weird Site" || e["auth"] != "username_password" || e["kind"] != "other" {
		t.Errorf("supportedsites seed wrong: %v", e)
	}
	if e["curated"] != false || e["configured"] != true || e["custom_profile"] != false {
		t.Errorf("state flags wrong: %v", e)
	}
	if _, body := doJSON(t, "GET", srv.URL+"/api/v1/sites?q=Weird+Site", "", ""); len(body["sites"].([]any)) != 1 {
		t.Error("the display name should match the filter")
	}
	_, body = doJSON(t, "GET", srv.URL+"/api/v1/sites?q=danbooru", "", "")
	for _, s := range body["sites"].([]any) {
		if e := s.(map[string]any); e["category"] == "danbooru" && e["configured"] != false {
			t.Errorf("a bare seeded block should not read configured: %v", e)
		}
	}
}

func TestLookupStatusChainListsOnlyQueryableSources(t *testing.T) {
	cfg := testCfg("")
	q := queue.New(fakeProc{}, 1, 100)
	t.Cleanup(q.Close)
	mapper, err := mapping.New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatal(err)
	}
	h := New(q, nil, mapper, config.NewProvider(cfg), nil, "", nil, disabledPTR(), Host{
		SimMissing: func(string) (string, bool) { return "a key", true },
	})
	w := httptest.NewRecorder()
	h.lookupStatus(w, httptest.NewRequest("GET", "/api/v1/lookup/status", nil))
	var st struct {
		Chain []string `json:"chain"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Chain) == 0 || slices.Contains(st.Chain, "iqdb") || slices.Contains(st.Chain, "saucenao") {
		t.Errorf("chain = %v, want the sources the walk would query, without the skipped services", st.Chain)
	}
}

func TestDeleteRemovesAQueuedJobAndAFinishedSeries(t *testing.T) {
	srv := newTestServer(t, "")
	_, _ = doJSON(t, "POST", srv.URL+"/api/v1/queue/pause", "", "")
	_, body := doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"https://danbooru.donmai.us/posts/1"}`)
	id := strconv.FormatInt(int64(body["job_id"].(float64)), 10)
	wantCode(t, "DELETE", srv.URL+"/api/v1/queue/"+id, "", http.StatusNoContent, "")
	wantCode(t, "GET", srv.URL+"/api/v1/queue/"+id, "", http.StatusNotFound, "")
}

func TestTagRulesListsTheConfiguredSitesThenThePTR(t *testing.T) {
	cfg := testCfg("")
	cfg.Sites = []config.Site{{Name: "gelbooru", Blacklist: []string{"watermark"}}, {Name: "danbooru", UserID: "1"}}
	cfg.Tags.DropCategories = []string{"meta"}
	srv := serveCfg(t, cfg)
	body := wantJSON(t, "GET", srv.URL+"/api/v1/rules?tag=watermark", "", 200)
	if body["category"] != "general" || body["category_kept"] != true {
		t.Errorf("body = %v, want general kept", body)
	}
	sites := body["sites"].([]any)
	var names, kinds, blacklists []string
	for _, s := range sites {
		row := s.(map[string]any)
		names = append(names, row["site"].(string))
		kinds = append(kinds, row["kind"].(string))
		b, _ := row["blacklist"].(string)
		blacklists = append(blacklists, b)
	}
	if !slices.Equal(names, []string{"danbooru", "gelbooru", "ptr"}) || !slices.Equal(kinds, []string{"booru", "booru", "lookup"}) {
		t.Errorf("sites = %v %v, want the configured sites sorted then the PTR as lookup", names, kinds)
	}
	if !slices.Equal(blacklists, []string{"", "site", ""}) {
		t.Errorf("blacklists = %v, want gelbooru's own list alone", blacklists)
	}
	if body := wantJSON(t, "GET", srv.URL+"/api/v1/rules?tag=highres&category=meta", "", 200); body["category_kept"] != false {
		t.Errorf("meta:highres = %v, want category_kept false", body)
	}
	wantJSON(t, "GET", srv.URL+"/api/v1/rules", "", 400)
}

func TestTagRulesListsAUsedSiteAsBuiltin(t *testing.T) {
	cfg := testCfg("")
	cfg.Sites = []config.Site{{Name: "danbooru", UserID: "1"}}
	q := queue.New(fakeProc{}, 1, 100)
	t.Cleanup(q.Close)
	q.Enqueue("https://www.zerochan.net/1", queue.Options{Site: "zerochan"})
	q.Enqueue("https://danbooru.donmai.us/posts/1", queue.Options{Site: "danbooru"})
	mapper, err := mapping.New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatal(err)
	}
	h := New(q, nil, mapper, config.NewProvider(cfg), gdl.NewCatalog("1.32.1", "1.32.1", false, nil, nil), "", nil, disabledPTR(), Host{SimMissing: noSimMissing})
	w := httptest.NewRecorder()
	h.tagRules(w, httptest.NewRequest("GET", "/api/v1/rules?tag=smile", nil))
	var body struct {
		Sites []struct {
			Site    string `json:"site"`
			Builtin bool   `json:"builtin"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range body.Sites {
		got = append(got, s.Site+":"+strconv.FormatBool(s.Builtin))
	}
	if !slices.Equal(got, []string{"danbooru:false", "zerochan:true", "ptr:false"}) {
		t.Errorf("sites = %v, want the configured site, the used one as built-in, then the PTR", got)
	}
}

func TestTagRulesBatchAnswersEachNameInOrder(t *testing.T) {
	cfg := testCfg("")
	cfg.Sites = []config.Site{{Name: "gelbooru", Blacklist: []string{"watermark"}}}
	cfg.Tags.DropCategories = []string{"meta"}
	srv := serveCfg(t, cfg)
	body := wantJSON(t, "POST", srv.URL+"/api/v1/rules", `{"names":[{"tag":"watermark"},{"tag":"highres","category":"meta"}]}`, 200)
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v, want two answers", results)
	}
	first, second := results[0].(map[string]any), results[1].(map[string]any)
	if first["tag"] != "watermark" || first["category"] != "general" {
		t.Errorf("first = %v, want general:watermark", first)
	}
	if b, _ := first["sites"].([]any)[0].(map[string]any)["blacklist"].(string); b != "site" {
		t.Errorf("first sites = %v, want gelbooru's own blacklist", first["sites"])
	}
	if second["tag"] != "highres" || second["category_kept"] != false {
		t.Errorf("second = %v, want meta:highres not kept", second)
	}
	wantCode(t, "POST", srv.URL+"/api/v1/rules", `{"names":[]}`, 400, "invalid_request")
	wantCode(t, "POST", srv.URL+"/api/v1/rules", `{"names":[{"tag":" "}]}`, 400, "invalid_request")
	many := strings.Repeat(`{"tag":"a"},`, 50) + `{"tag":"a"}`
	wantCode(t, "POST", srv.URL+"/api/v1/rules", `{"names":[`+many+`]}`, 400, "invalid_request")
}
