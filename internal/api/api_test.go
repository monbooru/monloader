package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/sitestate"
)

// fakeRunner satisfies gdl.Runner for the probe and version paths.
type fakeRunner struct{ probe gdl.ProbeResult }

func (f fakeRunner) Resolve(context.Context, string, string, bool) (gdl.ResolveResult, error) {
	return gdl.ResolveResult{}, nil
}
func (f fakeRunner) FetchMeta(context.Context, string) (map[string]any, error) { return nil, nil }
func (f fakeRunner) Download(context.Context, string, string, string, bool, func(int, gdl.Downloaded), bool) ([]gdl.Downloaded, error) {
	return nil, nil
}
func (f fakeRunner) ListExtractors(context.Context) ([]gdl.Extractor, error) { return nil, nil }
func (f fakeRunner) Probe(context.Context, string) (gdl.ProbeResult, error)  { return f.probe, nil }
func (f fakeRunner) Version(context.Context) string                          { return "1.32.1" }

// fakeProc produces an outcome keyed off the job URL so tests can drive the
// whole vocabulary deterministically.
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
			it.Outcome = queue.OutcomeCreated
			it.MonbooruID = 99
		})
		// Cap only the first window so a fetch-all chain terminates.
		if snap.Offset == 0 {
			job.SetCapped(1)
		}
	case strings.Contains(url, "dup"):
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
		job.UpdateItem(0, func(it *queue.Item) {
			it.Status = queue.ItemSkipped
			it.Outcome = queue.OutcomeDuplicate
			it.MonbooruID = 7
		})
	case strings.Contains(url, "fail"):
		job.UpdateItem(0, func(it *queue.Item) {
			it.Status = queue.ItemFailed
			it.Outcome = queue.OutcomeFailed
			it.ErrorCode = queue.ErrCodeMonbooruRejected
		})
	default:
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
		job.UpdateItem(0, func(it *queue.Item) {
			it.Status = queue.ItemDone
			it.Outcome = queue.OutcomeCreated
			it.MonbooruID = 42
		})
	}
	return nil
}

const apiTestToken = "test-token"

func serveCfg(t *testing.T, cfg *config.Config) *httptest.Server {
	srv, _ := serveCfgTracked(t, cfg)
	return srv
}

// serveCfgTracked also returns the sitestate tracker, for tests asserting what
// a probe recorded.
func serveCfgTracked(t *testing.T, cfg *config.Config) (*httptest.Server, *sitestate.Tracker) {
	t.Helper()
	q := queue.New(fakeProc{}, 1, 100)
	q.Start()
	mapper, err := mapping.New(config.NewProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	extractors := []gdl.Extractor{
		{Category: "danbooru", Subcategory: "post", Example: "https://example.com/posts/1"},
		{Category: "gelbooru", Subcategory: "post", Example: "https://example.com/index.php?id=1"},
		{Category: "weirdsite", Subcategory: "post", Example: "https://weird.example/1"},
	}
	siteState := sitestate.New()
	h := New(q, fakeRunner{probe: gdl.ProbeResult{Status: gdl.ProbeOK}}, mapper, config.NewProvider(cfg), extractors, "v1.2.3", "1.32.1", siteState)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); q.Close() })
	return srv, siteState
}

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Server.BaseURL = "http://localhost:8081"
	if token == "" {
		token = apiTestToken
	}
	cfg.Auth.Tokens = []config.Token{{ID: "test", Name: "test", TokenHash: config.HashToken(token), Scopes: config.AllScopes}}
	return serveCfg(t, cfg)
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

func TestHealthNoAuth(t *testing.T) {
	srv := newTestServer(t, "secret") // token set, but /health is open
	resp, body := doJSON(t, "GET", srv.URL+"/health", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["status"] != "ok" || body["version"] != "v1.2.3" || body["gallerydl_version"] != "1.32.1" {
		t.Errorf("health body = %v", body)
	}
}

// TestHealthCORS pins that /health reflects the request Origin like the rest of
// the API, even though it bypasses the auth gate that sets CORS elsewhere; the
// browser extension reads it cross-origin.
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
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"http://danbooru/posts/1"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if _, ok := body["job_id"]; !ok {
		t.Errorf("expected job_id, got %v", body)
	}
}

func TestEnqueueMissingURL(t *testing.T) {
	srv := newTestServer(t, "")
	resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEnqueueMaxItemsValidation(t *testing.T) {
	srv := newTestServer(t, "")
	// A negative max_items is rejected.
	resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"http://x","options":{"max_items":-5}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("max_items=-5: status = %d, want 400", resp.StatusCode)
	}
	// Zero is accepted (indistinguishable from omitted; treated as no override).
	resp, _ = doJSON(t, "POST", srv.URL+"/api/v1/queue", "", `{"url":"http://x","options":{"max_items":0}}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("max_items=0: status = %d, want 202", resp.StatusCode)
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

func TestMetadataEnqueues(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/metadata", "", `{"image_id":42,"gallery":"art","url":"http://danbooru/posts/1"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	id, ok := body["job_id"].(float64)
	if !ok {
		t.Fatalf("expected job_id, got %v", body)
	}
	resp, job := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+strconv.FormatInt(int64(id), 10), "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("get job status = %d", resp.StatusCode)
	}
	if job["kind"] != "metadata" || job["image_id"] != float64(42) || job["gallery"] != "art" {
		t.Errorf("job = %v, want kind=metadata image_id=42 gallery=art", job)
	}
}

func TestMetadataValidation(t *testing.T) {
	srv := newTestServer(t, "")
	for _, tc := range []struct {
		name, body string
	}{
		{"missing image_id", `{"url":"http://danbooru/posts/1"}`},
		{"non-http url", `{"image_id":1,"url":"ftp://h/x"}`},
		{"missing url", `{"image_id":1}`},
		{"invalid json", `{`},
	} {
		resp, body := doJSON(t, "POST", srv.URL+"/api/v1/metadata", "", tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
		if body["code"] != "invalid_request" {
			t.Errorf("%s: code = %v, want invalid_request", tc.name, body["code"])
		}
	}
}

func TestListRejectsUnknownStatus(t *testing.T) {
	srv := newTestServer(t, "")
	resp, _ := doJSON(t, "GET", srv.URL+"/api/v1/queue?status=bogus", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=bogus: status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", srv.URL+"/api/v1/queue?status=failed", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=failed: status = %d, want 200", resp.StatusCode)
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
		{"http://danbooru/posts/fail", "failed", "failed", 0, queue.ErrCodeMonbooruRejected},
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

func TestListGetRetryDelete(t *testing.T) {
	srv := newTestServer(t, "")
	// Enqueue a failing job and wait for it to settle.
	_, body := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/fail"}`)
	id := int64(body["id"].(float64))

	// List
	resp, list := doJSON(t, "GET", srv.URL+"/api/v1/queue", "", "")
	if resp.StatusCode != 200 || list["total"].(float64) < 1 {
		t.Errorf("list total = %v", list["total"])
	}
	// Get
	resp, got := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", "")
	if resp.StatusCode != 200 || int64(got["id"].(float64)) != id {
		t.Errorf("get id = %v, want %d", got["id"], id)
	}
	// Get unknown
	resp, _ = doJSON(t, "GET", srv.URL+"/api/v1/queue/99999", "", "")
	if resp.StatusCode != 404 {
		t.Errorf("get unknown status = %d, want 404", resp.StatusCode)
	}
	// Retry
	resp, _ = doJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(id)+"/retry", "", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("retry status = %d, want 202", resp.StatusCode)
	}
	// Wait for the retried job to settle, then delete (remove) it.
	doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", "")
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/v1/queue/"+itoa(id), "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", "")
	if resp.StatusCode != 404 {
		t.Errorf("after delete, get status = %d, want 404", resp.StatusCode)
	}
}

// TestDeleteClearsSeries checks DELETE on a series member drops every window,
// matching the web route, so a caller need not remove each window separately.
func TestDeleteClearsSeries(t *testing.T) {
	srv := newTestServer(t, "")

	_, job := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/cap"}`)
	root := int64(job["id"].(float64))
	_, cont := doJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(root)+"/continue", "", "")
	child := int64(cont["job_id"].(float64))

	// Let the continuation finish so the delete removes it rather than canceling a
	// still-running window (which would leave it in history).
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

// TestRetryForce checks that ?force=1 on the retry endpoint re-queues the job
// with the archive-bypass flag set, surfaced as "force" on the job.
func TestRetryForce(t *testing.T) {
	srv := newTestServer(t, "")

	// wait so the job is finished (retryable) before the retry call.
	resp, job := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://danbooru/posts/1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("enqueue wait status = %d, want 200", resp.StatusCode)
	}
	id := int64(job["id"].(float64))

	resp, _ = doJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(id)+"/retry?force=1", "", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("force retry status = %d, want 202", resp.StatusCode)
	}
	if _, got := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(id), "", ""); got["force"] != true {
		t.Errorf("job force = %v, want true", got["force"])
	}
}

// TestContinueJob checks the continue endpoint queues a follow-up for the next
// window of a capped job and rejects a job that was never capped.
// TestContinueEndpoints checks continue and continue-all each queue the next
// window of a capped job and reject a job that was never capped.
func TestContinueEndpoints(t *testing.T) {
	for _, action := range []string{"continue", "continue-all"} {
		t.Run(action, func(t *testing.T) {
			srv := newTestServer(t, "")

			_, job := doJSON(t, "POST", srv.URL+"/api/v1/queue?wait=5", "", `{"url":"http://x/cap"}`)
			id := int64(job["id"].(float64))
			resp, body := doJSON(t, "POST", srv.URL+"/api/v1/queue/"+itoa(id)+"/"+action, "", "")
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("%s status = %d, want 202", action, resp.StatusCode)
			}
			newID := int64(body["job_id"].(float64))
			if newID == id {
				t.Errorf("%s should return a new job id, got the source %d", action, id)
			}
			if r, _ := doJSON(t, "GET", srv.URL+"/api/v1/queue/"+itoa(newID), "", ""); r.StatusCode != 200 {
				t.Errorf("follow-up job %d should exist, get status = %d", newID, r.StatusCode)
			}

			// A non-capped job has no next window (409); an unknown id is 404.
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

// TestJobExposesSeriesRoot checks the queue JSON carries the series root so an
// API consumer can collapse a capped search and its continuations.
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
	cfg.Auth.Tokens = []config.Token{{ID: "ro", Name: "ro", TokenHash: config.HashToken("ro-token"), Scopes: []string{config.ScopeRead}}}
	srv := serveCfg(t, cfg)
	resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue/pause", "ro-token", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("pause with read-only token = %d, want 403", resp.StatusCode)
	}
}

func TestBearerAuth(t *testing.T) {
	srv := newTestServer(t, "secret")
	// Missing header -> 401 on a guarded endpoint.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/queue", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing-header status = %d, want 401", resp.StatusCode)
	}
	// Wrong token -> 401.
	resp, _ = doJSON(t, "GET", srv.URL+"/api/v1/queue", "wrong", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token status = %d, want 401", resp.StatusCode)
	}
	// Right token -> 200.
	resp, _ = doJSON(t, "GET", srv.URL+"/api/v1/queue", "secret", "")
	if resp.StatusCode != 200 {
		t.Errorf("good-token status = %d, want 200", resp.StatusCode)
	}
	// /health stays open.
	resp, _ = doJSON(t, "GET", srv.URL+"/health", "", "")
	if resp.StatusCode != 200 {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
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
	cfg.Auth.Tokens = []config.Token{{ID: "ro", Name: "ro", TokenHash: config.HashToken("ro-token"), Scopes: []string{config.ScopeRead}}}
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

func TestListSites(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "GET", srv.URL+"/api/v1/sites", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	sites := body["sites"].([]any)
	// Curated sites (danbooru, gelbooru) sort ahead of the uncurated weirdsite.
	first := sites[0].(map[string]any)
	if first["curated"] != true {
		t.Errorf("first site should be curated, got %v", first)
	}
	// A curated profile for a multi-instance family is surfaced under its own
	// host even though gallery-dl's --list-extractors only names one instance:
	// aibooru is not in the extractor fixture, but its profile must appear so a
	// client recognizes aibooru.online.
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
	// Filter narrows the list, and a curated profile already named as an
	// extractor is not duplicated (gelbooru is in the extractor fixture).
	_, filtered := doJSON(t, "GET", srv.URL+"/api/v1/sites?q=gel", "", "")
	fs := filtered["sites"].([]any)
	if len(fs) != 1 || fs[0].(map[string]any)["category"] != "gelbooru" {
		t.Errorf("q=gel should return only gelbooru, got %v", fs)
	}
}

func TestSiteProbe(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/sites/danbooru/test", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body["status"] != string(gdl.ProbeOK) {
		t.Errorf("probe status = %v, want ok", body["status"])
	}
	// Unknown site with no url -> 404.
	resp, _ = doJSON(t, "POST", srv.URL+"/api/v1/sites/nosuchsite/test", "", "")
	if resp.StatusCode != 404 {
		t.Errorf("unknown site status = %d, want 404", resp.StatusCode)
	}
}

func TestSiteProbeRecordsReached(t *testing.T) {
	// A successful API probe must record "last reached" like the web probe, so
	// the settings indicator stays fresh whichever surface ran the test.
	cfg := config.Default()
	cfg.Server.BaseURL = "http://localhost:8081"
	cfg.Auth.Tokens = []config.Token{{ID: "test", Name: "test", TokenHash: config.HashToken(apiTestToken), Scopes: config.AllScopes}}
	srv, tracker := serveCfgTracked(t, cfg)
	resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/sites/danbooru/test", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
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

	// Every $ref must resolve to a defined schema.
	var refs []string
	collectRefs(spec, &refs)
	for _, ref := range refs {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if _, ok := schemas[name]; !ok {
			t.Errorf("dangling $ref %q", ref)
		}
	}
	// Every operation must declare responses, a summary, and an operationId.
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
	// The declarations drive the mux, so a documented path is a mounted route;
	// pin the metadata endpoint that had been mounted without documentation.
	if _, ok := paths["/api/v1/metadata"]; !ok {
		t.Error("spec is missing /api/v1/metadata")
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
}

// TestDocEndpointsCORS pins that the unauthenticated self-doc endpoints reflect
// the request Origin too; like /health they bypass the auth gate that sets CORS
// on the rest of the API.
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
