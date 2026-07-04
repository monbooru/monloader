package monbooru

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/queue"
)

func testClient(url, token string) *Client {
	cfg := config.Default()
	cfg.Monbooru.APIURL = url
	cfg.Monbooru.APIToken = token
	return New(config.NewProvider(cfg))
}

func tempFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPushImageCreatedBareBody(t *testing.T) {
	data := []byte("the image bytes")
	var gotGallery, gotAuth, gotVia, gotSource, gotURL, gotCollection, gotOrder, gotTags, gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGallery = r.URL.Query().Get("gallery")
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotVia = r.FormValue("via")
		gotSource = r.FormValue("source")
		gotURL = r.FormValue("url")
		gotCollection = r.FormValue("collection")
		gotOrder = r.FormValue("collection_order")
		gotTags = r.FormValue("tags")
		f, fh, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
		} else {
			gotFilename = fh.Filename
			b, _ := io.ReadAll(f)
			if string(b) != string(data) {
				t.Errorf("file bytes = %q, want %q", b, data)
			}
		}
		// Bare image object (no notes): the success body has no envelope.
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "sha256": "x"})
	}))
	defer srv.Close()

	meta := PushMeta{
		Filename:        "post.jpg",
		Tags:            []string{"1girl", "artist:x", "rating:general"},
		Source:          "danbooru",
		URL:             "https://example.com/posts/1",
		Collection:      "pool name",
		CollectionOrder: 2,
		Via:             "monloader",
	}
	res, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, data), meta, "mygallery")
	if err != nil {
		t.Fatalf("PushImageFile: %v", err)
	}
	if res.Outcome != queue.OutcomeCreated {
		t.Errorf("outcome = %s, want created", res.Outcome)
	}
	if res.MonbooruID != 123 {
		t.Errorf("id = %d, want 123", res.MonbooruID)
	}
	sum := sha256.Sum256(data)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 mismatch")
	}
	if gotGallery != "mygallery" {
		t.Errorf("gallery query = %q, want mygallery", gotGallery)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
	if gotVia != "monloader" || gotSource != "danbooru" || gotURL != "https://example.com/posts/1" {
		t.Errorf("provenance fields wrong: via=%q source=%q url=%q", gotVia, gotSource, gotURL)
	}
	if gotCollection != "pool name" || gotOrder != "2" {
		t.Errorf("collection fields wrong: %q / %q", gotCollection, gotOrder)
	}
	if gotFilename != "post.jpg" {
		t.Errorf("filename = %q, want post.jpg", gotFilename)
	}
	var tags []string
	if err := json.Unmarshal([]byte(gotTags), &tags); err != nil || len(tags) != 3 {
		t.Errorf("tags field = %q, want a 3-element JSON array", gotTags)
	}
}

func TestPushImageDuplicateMergeNote(t *testing.T) {
	data := []byte("dup bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		// Duplicate sha: monbooru merged the pushed metadata and reports it.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image":       map[string]any{"id": 7, "sha256": "x"},
			"alias_added": true,
			"merge":       map[string]any{"tags_added": 5, "tags_removed": 1, "rating_filled": true, "source_added": true},
		})
	}))
	defer srv.Close()

	res, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, data), PushMeta{Filename: "p.jpg", Source: "danbooru"}, "g")
	if err != nil {
		t.Fatalf("PushImageFile: %v", err)
	}
	if res.Outcome != queue.OutcomeDuplicate {
		t.Errorf("outcome = %s, want duplicate", res.Outcome)
	}
	if res.MonbooruID != 7 {
		t.Errorf("id = %d, want 7", res.MonbooruID)
	}
	if res.MergeNote != "+5 tags, -1 tags, rating" {
		t.Errorf("merge note = %q, want %q", res.MergeNote, "+5 tags, -1 tags, rating")
	}
}

func TestEnrichImageEnriched(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"merge": map[string]any{"tags_added": 5}, "verified": true})
	}))
	defer srv.Close()

	res, err := testClient(srv.URL, "tok").EnrichImage(context.Background(), 42, "mygallery", EnrichPayload{
		Tags: []string{"1girl"}, Source: "danbooru", URL: "https://d/1", SourceMD5: "abc", Verify: true, Commentary: "hi",
	})
	if err != nil {
		t.Fatalf("EnrichImage: %v", err)
	}
	if res.Outcome != queue.OutcomeEnriched {
		t.Errorf("outcome = %s, want enriched", res.Outcome)
	}
	if res.MonbooruID != 42 {
		t.Errorf("id = %d, want 42", res.MonbooruID)
	}
	if res.MergeNote != "+5 tags" {
		t.Errorf("merge note = %q, want +5 tags", res.MergeNote)
	}
	if gotPath != "/api/v1/images/42/enrich?gallery=mygallery" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"verify":true`) || !strings.Contains(gotBody, `"commentary":"hi"`) {
		t.Errorf("body missing verify/commentary: %s", gotBody)
	}
}

func TestEnrichImageHashMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "the source no longer serves this file", "code": "hash_mismatch"})
	}))
	defer srv.Close()

	_, err := testClient(srv.URL, "tok").EnrichImage(context.Background(), 42, "g", EnrichPayload{Verify: true, SourceMD5: "abc"})
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeHashMismatch {
		t.Fatalf("err = %v, want CodedError hash_mismatch", err)
	}
}

func TestPushImageCapsLongFields(t *testing.T) {
	var gotSource, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotSource = r.FormValue("source")
		gotURL = r.FormValue("url")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer srv.Close()

	meta := PushMeta{
		Filename: "f.jpg",
		Source:   strings.Repeat("s", maxSourceLen+50),
		URL:      "https://cdn.example.com/f.jpg?sig=" + strings.Repeat("a", maxURLLen),
	}
	if _, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), meta, "g"); err != nil {
		t.Fatalf("PushImageFile: %v", err)
	}
	if len(gotSource) != maxSourceLen {
		t.Errorf("source len = %d, want %d (trimmed to monbooru's cap)", len(gotSource), maxSourceLen)
	}
	if len(gotURL) != maxURLLen {
		t.Errorf("url len = %d, want %d (trimmed to monbooru's cap)", len(gotURL), maxURLLen)
	}
}

func TestPushImageFileStreams(t *testing.T) {
	data := []byte("the cbz archive bytes")
	path := filepath.Join(t.TempDir(), "bundle.cbz")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var gotFilename, gotCollection string
	var gotFile []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotCollection = r.FormValue("collection")
		f, fh, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
		} else {
			gotFilename = fh.Filename
			gotFile, _ = io.ReadAll(f)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 55})
	}))
	defer srv.Close()

	meta := PushMeta{Filename: "My Doujin.cbz", Tags: []string{"rating:explicit"}, Source: "nhentai", Collection: "vol1"}
	res, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), path, meta, "g")
	if err != nil {
		t.Fatalf("PushImageFile: %v", err)
	}
	if res.Outcome != queue.OutcomeCreated || res.MonbooruID != 55 {
		t.Errorf("outcome/id = %s/%d, want created/55", res.Outcome, res.MonbooruID)
	}
	sum := sha256.Sum256(data)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want the file's digest", res.SHA256)
	}
	if string(gotFile) != string(data) {
		t.Errorf("streamed file = %q, want %q", gotFile, data)
	}
	if gotFilename != "My Doujin.cbz" {
		t.Errorf("filename = %q, want My Doujin.cbz", gotFilename)
	}
	if gotCollection != "vol1" {
		t.Errorf("collection field = %q, want it streamed alongside the file", gotCollection)
	}
}

func TestPushImageFileUnblocksOnBuildError(t *testing.T) {
	// A request-build error (an unparseable api_url) must not strand the goroutine
	// streaming the cbz into the pipe; it should unblock and close the file.
	cfg := config.Default()
	cfg.Monbooru.APIURL = "http://\x7f" // a control char fails url.Parse in NewRequest
	c := New(config.NewProvider(cfg))

	path := filepath.Join(t.TempDir(), "bundle.cbz")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond) // let any prior test's connections drain
	before := runtime.NumGoroutine()
	if _, err := c.PushImageFile(context.Background(), path, PushMeta{Filename: "b.cbz"}, "g"); err == nil {
		t.Fatal("PushImageFile should error on an unparseable api_url")
	}
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("writer goroutine leaked: before=%d still=%d", before, runtime.NumGoroutine())
}

func TestPushImageCreatedEnvelopedWithWarnings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image":        map[string]any{"id": 124},
			"tag_warnings": []string{"tag foo: rejected"},
		})
	}))
	defer srv.Close()
	res, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{Via: "monloader"}, "")
	if err != nil {
		t.Fatalf("PushImageFile: %v", err)
	}
	if res.Outcome != queue.OutcomeCreated || res.MonbooruID != 124 {
		t.Errorf("got %+v, want created id 124", res)
	}
	if len(res.TagWarnings) != 1 || res.TagWarnings[0] != "tag foo: rejected" {
		t.Errorf("tag warnings = %v", res.TagWarnings)
	}
}

func TestPushImageDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// monbooru returns 200 + alias_added on a known sha256.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image":       map[string]any{"id": 55},
			"alias_added": true,
		})
	}))
	defer srv.Close()
	res, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{}, "furry")
	if err != nil {
		t.Fatalf("PushImageFile: %v", err)
	}
	if res.Outcome != queue.OutcomeDuplicate || res.MonbooruID != 55 {
		t.Errorf("got %+v, want duplicate id 55", res)
	}
}

func TestPushImageErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusRequestEntityTooLarge, queue.ErrCodeFileTooLarge},
		{http.StatusInternalServerError, queue.ErrCodeMonbooruRejected},
		{http.StatusBadRequest, queue.ErrCodeMonbooruRejected},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "nope", "code": "x"})
		}))
		_, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{}, "")
		srv.Close()
		e, ok := err.(*queue.CodedError)
		if !ok || e.Code != tc.want {
			t.Errorf("status %d -> %v, want code %s", tc.status, err, tc.want)
		}
	}
}

func TestPushImageUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now
	_, err := testClient(url, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{}, "")
	e, ok := err.(*queue.CodedError)
	if !ok || e.Code != queue.ErrCodeMonbooruUnreachable {
		t.Errorf("got %v, want monbooru_unreachable", err)
	}
}

func TestPushImageCanceled(t *testing.T) {
	// A job canceled while the push is in flight must be recorded as canceled,
	// not monbooru_unreachable - the context error wins over Do's wrapped error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the request goes out
	_, err := testClient(srv.URL, "tok").PushImageFile(ctx, tempFile(t, []byte("x")), PushMeta{Filename: "x.jpg"}, "g")
	e, ok := err.(*queue.CodedError)
	if !ok || e.Code != queue.ErrCodeCanceled {
		t.Errorf("got %v, want canceled", err)
	}
}

func TestListGalleries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/galleries" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]Gallery{
			{Name: "default", Images: 10, Tags: 5, Active: true},
			{Name: "furry", Images: 2, Tags: 1},
		})
	}))
	defer srv.Close()
	gs, err := testClient(srv.URL, "tok").ListGalleries(context.Background())
	if err != nil {
		t.Fatalf("ListGalleries: %v", err)
	}
	if len(gs) != 2 || gs[0].Name != "default" || !gs[0].Active || gs[1].Name != "furry" {
		t.Errorf("galleries = %+v", gs)
	}
}

func TestConnection(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"api": "monbooru", "version": "v1.2.3"})
	}))
	defer ok.Close()
	version, err := testClient(ok.URL, "tok").TestConnection(context.Background())
	if err != nil {
		t.Errorf("TestConnection on a healthy server: %v", err)
	}
	if version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", version)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	if _, err := testClient(bad.URL, "wrong").TestConnection(context.Background()); err == nil {
		t.Error("TestConnection should fail on 401")
	}
}

// TestOpenAPIContract pins the monbooru fields ingest depends on against the
// recorded openapi.json so an upstream rename trips here instead of silently
// breaking the push.
func TestOpenAPIContract(t *testing.T) {
	path := filepath.Join("..", "testdata", "monbooru", "openapi.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read openapi fixture: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	requireProps := map[string][]string{
		"Image":                  {"id", "sha256", "source", "url", "collection", "collection_order", "tags"},
		"CreateImageResponse":    {"image", "tag_warnings"},
		"DuplicateImageResponse": {"image", "alias_added"},
		"Gallery":                {"name", "images", "tags", "active"},
	}
	for schema, props := range requireProps {
		s, ok := spec.Components.Schemas[schema]
		if !ok {
			t.Errorf("openapi schema %q missing (monbooru contract drift)", schema)
			continue
		}
		for _, p := range props {
			if _, ok := s.Properties[p]; !ok {
				t.Errorf("openapi %s.%s missing - the client depends on this field", schema, p)
			}
		}
	}
	for _, p := range []string{"/images", "/galleries"} {
		if _, ok := spec.Paths[p]; !ok {
			t.Errorf("openapi path %q missing", p)
		}
	}
}
