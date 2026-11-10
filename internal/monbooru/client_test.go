package monbooru

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func testClient(url, token string) *Client {
	cfg := config.Default()
	cfg.Monbooru.APIURL = url
	cfg.Monbooru.APIToken = token
	return New(config.NewProvider(cfg))
}

func TestDropCategoriesTravelAsWithheldOnEveryWrite(t *testing.T) {
	var got []string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/enrich") {
			var body EnrichPayload
			_ = json.NewDecoder(r.Body).Decode(&body)
			got = body.WithheldCategories
		} else {
			_ = r.ParseMultipartForm(1 << 20)
			_ = json.Unmarshal([]byte(r.FormValue(contract.FieldWithheldCategories)), &got)
		}
		_, _ = w.Write([]byte(`{"id":1,"sha256":"x"}`))
	}))
	cfg := config.Default()
	cfg.Monbooru.APIURL = srv.URL
	cfg.Tags.DropCategories = []string{"general", "meta"}
	c := New(config.NewProvider(cfg))
	path := tempFile(t, []byte("bytes"))
	for name, write := range map[string]func() error{
		"push": func() error {
			_, err := c.PushImageFile(context.Background(), path, PushMeta{Filename: "a.jpg"}, "")
			return err
		},
		"replace": func() error {
			_, err := c.ReplaceImageFile(context.Background(), 1, path, PushMeta{Filename: "a.jpg"}, "")
			return err
		},
		"enrich": func() error { _, err := c.EnrichImage(context.Background(), 1, "", EnrichPayload{}); return err },
	} {
		got = nil
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(got, cfg.Tags.DropCategories) {
			t.Errorf("%s sent withheld %v, want %v", name, got, cfg.Tags.DropCategories)
		}
	}
}

func TestWithheldTagsTravelOnEveryWrite(t *testing.T) {
	var got []string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/enrich") {
			var body EnrichPayload
			_ = json.NewDecoder(r.Body).Decode(&body)
			got = body.WithheldTags
		} else {
			_ = r.ParseMultipartForm(1 << 20)
			_ = json.Unmarshal([]byte(r.FormValue(contract.FieldWithheldTags)), &got)
		}
		_, _ = w.Write([]byte(`{"id":1,"sha256":"x"}`))
	}))
	c := testClient(srv.URL, "")
	want := []string{"artist:x", "meta:highres"}
	meta := PushMeta{Filename: "a.jpg", PostFields: PostFields{WithheldTags: want}}
	path := tempFile(t, []byte("bytes"))
	for name, write := range map[string]func() error{
		"push": func() error {
			_, err := c.PushImageFile(context.Background(), path, meta, "")
			return err
		},
		"replace": func() error {
			_, err := c.ReplaceImageFile(context.Background(), 1, path, meta, "")
			return err
		},
		"enrich": func() error {
			_, err := c.EnrichImage(context.Background(), 1, "", EnrichPayload{PostFields: meta.PostFields})
			return err
		},
	} {
		got = nil
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s sent withheld tags %v, want %v", name, got, want)
		}
	}
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func pushForm(t *testing.T, meta PushMeta) url.Values {
	t.Helper()
	var form url.Values
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		form = r.Form
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	mustPush(t, srv, tempFile(t, []byte("x")), meta, "g")
	return form
}

func mustPush(t *testing.T, srv *httptest.Server, path string, meta PushMeta, gallery string) *Result {
	t.Helper()
	res, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), path, meta, gallery)
	if err != nil {
		t.Fatalf("PushImageFile: %v", err)
	}
	return res
}

func stubJSON(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	return serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
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
	var gotGallery, gotAuth, gotVia, gotSource, gotPostID, gotURL, gotOriginal, gotCollection, gotOrder, gotTags, gotFilename string
	var gotCommentary, gotCommentaryDText string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGallery = r.URL.Query().Get("gallery")
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotVia = r.FormValue("via")
		gotSource = r.FormValue("source")
		gotPostID = r.FormValue("post_id")
		gotURL = r.FormValue("url")
		gotOriginal = r.FormValue("original")
		gotCommentary = r.FormValue("commentary")
		gotCommentaryDText = r.FormValue("commentary_dtext")
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
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "sha256": "x"})
	}))

	meta := PushMeta{
		PostFields: PostFields{
			Tags:            []string{"1girl", "artist:x", "rating:general"},
			Source:          "danbooru",
			PostID:          "1",
			URL:             "https://example.com/posts/1",
			Original:        "https://www.pixiv.net/artworks/9",
			Commentary:      "plain words",
			CommentaryDText: "[b]plain[/b] words",
		},
		Filename:        "post.jpg",
		Collection:      "pool name",
		CollectionOrder: 2,
		Via:             "monloader",
	}
	res := mustPush(t, srv, tempFile(t, data), meta, "mygallery")
	if res.Outcome != outcome.Created {
		t.Errorf("outcome = %s, want created", res.Outcome)
	}
	if res.MonbooruID != 123 {
		t.Errorf("id = %d, want 123", res.MonbooruID)
	}
	if res.SHA256 != "x" {
		t.Errorf("sha256 = %q, want the one monbooru answered", res.SHA256)
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
	if gotCommentary != "plain words" || gotCommentaryDText != "[b]plain[/b] words" {
		t.Errorf("commentary = %q / %q, want both forms", gotCommentary, gotCommentaryDText)
	}
	if gotPostID != "1" {
		t.Errorf("post_id field = %q, want 1", gotPostID)
	}
	if gotOriginal != "https://www.pixiv.net/artworks/9" {
		t.Errorf("original field = %q, want the upstream source", gotOriginal)
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
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image":       map[string]any{"id": 7, "sha256": "x"},
			"alias_added": true,
			"merge":       map[string]any{"tags_added": 5, "tags_retired": 1, "rating_filled": true, "source_added": true},
		})
	}))

	res := mustPush(t, srv, tempFile(t, data), PushMeta{Filename: "p.jpg", PostFields: PostFields{Source: "danbooru"}}, "g")
	if res.Outcome != outcome.Duplicate {
		t.Errorf("outcome = %s, want duplicate", res.Outcome)
	}
	if res.MonbooruID != 7 {
		t.Errorf("id = %d, want 7", res.MonbooruID)
	}
	if res.MergeNote != "+5 tags, 1 no longer listed, rating" {
		t.Errorf("merge note = %q, want %q", res.MergeNote, "+5 tags, 1 no longer listed, rating")
	}
}

func TestEnrichImageEnriched(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"merge": map[string]any{"tags_added": 5}, "verified": true})
	}))

	res, err := testClient(srv.URL, "tok").EnrichImage(context.Background(), 42, "mygallery", EnrichPayload{
		PostFields: PostFields{
			Tags: []string{"1girl"}, Source: "danbooru", PostID: "1", URL: "https://d/1", Commentary: "hi",
			CommentaryTranslated: "hello", CommentaryDText: "[b]hi[/b]", Original: "https://www.pixiv.net/artworks/9",
		},
		SourceMD5: "abc", Verify: true,
	})
	if err != nil {
		t.Fatalf("EnrichImage: %v", err)
	}
	if res.Outcome != outcome.Enriched {
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
	if !strings.Contains(gotBody, `"commentary_translated":"hello"`) {
		t.Errorf("body missing the commentary translation: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"commentary_dtext":"[b]hi[/b]"`) {
		t.Errorf("body missing the commentary's own markup: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"original":"https://www.pixiv.net/artworks/9"`) {
		t.Errorf("body missing original: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"post_id":"1"`) {
		t.Errorf("body missing post_id: %s", gotBody)
	}
}

func TestEnrichImageTrimsLikePush(t *testing.T) {
	var gotBody string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))

	long := strings.Repeat("x", contract.MaxCommentary+100)
	longDText := strings.Repeat("x", contract.MaxDText+100)
	_, err := testClient(srv.URL, "tok").EnrichImage(context.Background(), 1, "", EnrichPayload{PostFields: PostFields{
		Source:                    strings.Repeat("s", contract.MaxSourceLabel+10),
		PostID:                    strings.Repeat("i", contract.MaxPostID+10),
		URL:                       strings.Repeat("u", contract.MaxURL+10),
		Commentary:                long,
		CommentaryTranslated:      long,
		CommentaryDText:           longDText,
		CommentaryTranslatedDText: longDText,
		ParentURL:                 strings.Repeat("p", contract.MaxURL+10),
	}})
	if err != nil {
		t.Fatalf("EnrichImage: %v", err)
	}
	var got EnrichPayload
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, f := range []struct {
		name string
		len  int
		max  int
	}{
		{"source", len(got.Source), contract.MaxSourceLabel},
		{"post_id", len(got.PostID), contract.MaxPostID},
		{"url", len(got.URL), contract.MaxURL},
		{"commentary", len(got.Commentary), contract.MaxCommentary},
		{"commentary_translated", len(got.CommentaryTranslated), contract.MaxCommentary},
		{"commentary_dtext", len(got.CommentaryDText), contract.MaxDText},
		{"commentary_translated_dtext", len(got.CommentaryTranslatedDText), contract.MaxDText},
		{"parent_url", len(got.ParentURL), contract.MaxURL},
	} {
		if f.len != f.max {
			t.Errorf("%s length = %d, want trimmed to %d", f.name, f.len, f.max)
		}
	}
}

func TestEnrichImageCarriesVerifyFields(t *testing.T) {
	var gotBody string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))

	c := testClient(srv.URL, "tok")
	if _, err := c.EnrichImage(context.Background(), 1, "", EnrichPayload{SourceMD5: "d41d8cd98f00b204e9800998ecf8427e", Verify: true}); err != nil {
		t.Fatalf("EnrichImage: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got["source_md5"] != "d41d8cd98f00b204e9800998ecf8427e" || got["verify"] != true {
		t.Errorf("verify fields = md5 %v verify %v, want the submitted pair", got["source_md5"], got["verify"])
	}

	if _, err := c.EnrichImage(context.Background(), 1, "", EnrichPayload{Verify: false}); err != nil {
		t.Fatalf("EnrichImage: %v", err)
	}
	got = nil
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got["verify"] != false {
		t.Errorf("verify = %v, want false on a source-only enrich", got["verify"])
	}
	if _, ok := got["source_md5"]; ok {
		t.Error("source_md5 sent empty; it must be omitted when unset")
	}
}

func TestFetchThumbnail(t *testing.T) {
	var gotPath, gotAuth string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("jpegbytes"))
	}))

	got, err := testClient(srv.URL, "tok").FetchThumbnail(context.Background(), 42, "mygallery")
	if err != nil {
		t.Fatalf("FetchThumbnail: %v", err)
	}
	if string(got) != "jpegbytes" {
		t.Errorf("body = %q", got)
	}
	if gotPath != "/api/v1/images/42/thumbnail?gallery=mygallery" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestFetchThumbnailMissing(t *testing.T) {
	srv := stubJSON(t, http.StatusNotFound, map[string]any{"error": "image or thumbnail not found"})

	_, err := testClient(srv.URL, "tok").FetchThumbnail(context.Background(), 42, "")
	wantCode(t, err, outcome.ErrCodeMonbooruRejected)
}

func TestEnrichImageConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"hash mismatch", map[string]any{"error": "the source no longer serves this file", "code": "hash_mismatch"}, outcome.ErrCodeHashMismatch},
		{"bare 409", map[string]any{"error": "md5 differs"}, outcome.ErrCodeHashMismatch},
		{"setup required", map[string]any{"error": "monbooru has not been set up yet", "code": "setup_required"}, outcome.ErrCodeMonbooruRejected},
	} {
		srv := stubJSON(t, http.StatusConflict, tc.body)
		_, err := testClient(srv.URL, "tok").EnrichImage(context.Background(), 42, "g", EnrichPayload{Verify: true, SourceMD5: "abc"})
		var ce *outcome.CodedError
		if !errors.As(err, &ce) || ce.Code != tc.want {
			t.Errorf("%s: err = %v, want CodedError %s", tc.name, err, tc.want)
		}
	}
}

func TestPushImageCapsLongFields(t *testing.T) {
	got := pushForm(t, PushMeta{
		Filename: "f.jpg",
		PostFields: PostFields{
			Source: strings.Repeat("s", contract.MaxSourceLabel+50),
			URL:    "https://cdn.example.com/f.jpg?sig=" + strings.Repeat("a", contract.MaxURL),
		},
	})
	if n := len(got.Get("source")); n != contract.MaxSourceLabel {
		t.Errorf("source len = %d, want %d (trimmed to monbooru's cap)", n, contract.MaxSourceLabel)
	}
	if n := len(got.Get("url")); n != contract.MaxURL {
		t.Errorf("url len = %d, want %d (trimmed to monbooru's cap)", n, contract.MaxURL)
	}
}

func TestPushImageTrimsByCharacters(t *testing.T) {
	got := pushForm(t, PushMeta{
		Filename: "f.jpg",
		PostFields: PostFields{
			Commentary:           strings.Repeat("\u5b57", 6000),
			CommentaryTranslated: strings.Repeat("\u5b57", contract.MaxCommentary+5),
			CommentaryDText:      strings.Repeat("d", 20000),
		},
	})
	for _, f := range []struct {
		name string
		want int
	}{
		{"commentary", 6000},
		{"commentary_translated", contract.MaxCommentary},
		{"commentary_dtext", 20000},
	} {
		if n := utf8.RuneCountInString(got.Get(f.name)); n != f.want {
			t.Errorf("%s = %d characters, want %d", f.name, n, f.want)
		}
	}
}

func TestPushImageKeepsCollectionAndFileClaimsInBounds(t *testing.T) {
	got := pushForm(t, PushMeta{
		Filename:   "f.jpg",
		Collection: strings.Repeat("\u5b57", contract.MaxCollection+30),
		MD5:        strings.Repeat("a", contract.MaxMD5+1),
		PostFields: PostFields{PostExt: strings.Repeat("x", contract.MaxPostExt+1)},
	})
	if n := utf8.RuneCountInString(got.Get("collection")); n != contract.MaxCollection {
		t.Errorf("collection = %d characters, want %d", n, contract.MaxCollection)
	}
	if got.Get("post_ext") != "" || got.Get("md5") != "" {
		t.Errorf("post_ext %q, md5 %q: an over-long claim was sent", got.Get("post_ext"), got.Get("md5"))
	}
}

func TestEnrichImageDropsOverLongFileClaims(t *testing.T) {
	var got EnrichPayload
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte("{}"))
	}))
	_, err := testClient(srv.URL, "tok").EnrichImage(context.Background(), 1, "", EnrichPayload{
		PostFields: PostFields{PostExt: strings.Repeat("x", contract.MaxPostExt+1)},
		SourceMD5:  strings.Repeat("a", contract.MaxMD5+1),
	})
	if err != nil {
		t.Fatalf("EnrichImage: %v", err)
	}
	if got.PostExt != "" || got.SourceMD5 != "" {
		t.Errorf("post_ext %q, source_md5 %q: an over-long claim was sent", got.PostExt, got.SourceMD5)
	}
}

func TestPushImageSendsSourceMD5(t *testing.T) {
	meta := PushMeta{Filename: "f.jpg", MD5: "0123456789abcdef0123456789abcdef"}
	if got := pushForm(t, meta).Get("md5"); got != meta.MD5 {
		t.Errorf("md5 field = %q, want %q", got, meta.MD5)
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
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	meta := PushMeta{Filename: "My Doujin.cbz", Collection: "vol1", PostFields: PostFields{Tags: []string{"rating:explicit"}, Source: "nhentai"}}
	res := mustPush(t, srv, path, meta, "g")
	if res.Outcome != outcome.Created || res.MonbooruID != 55 {
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

func TestPushImageFileMissingFileIsLocal(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	_, err := testClient(srv.URL, "tok").PushImageFile(context.Background(),
		filepath.Join(t.TempDir(), "gone.jpg"), PushMeta{Filename: "gone.jpg"}, "g")
	wantCode(t, err, outcome.ErrCodeDownloadFailed)
}

func TestPushImageFileUnblocksOnBuildError(t *testing.T) {
	cfg := config.Default()
	cfg.Monbooru.APIURL = "http://\x7f"
	c := New(config.NewProvider(cfg))

	path := filepath.Join(t.TempDir(), "bundle.cbz")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
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
	srv := stubJSON(t, http.StatusCreated, map[string]any{
		"image":        map[string]any{"id": 124},
		"tag_warnings": []string{"tag foo: rejected"},
	})
	res := mustPush(t, srv, tempFile(t, []byte("x")), PushMeta{Via: "monloader"}, "")
	if res.Outcome != outcome.Created || res.MonbooruID != 124 {
		t.Errorf("got %+v, want created id 124", res)
	}
	if len(res.TagWarnings) != 1 || res.TagWarnings[0] != "tag foo: rejected" {
		t.Errorf("tag warnings = %v", res.TagWarnings)
	}
}

func TestPushImageTakesTheHashMonbooruStored(t *testing.T) {
	rescued := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name   string
		status int
		body   map[string]any
	}{
		{"bare", http.StatusCreated, map[string]any{"id": 1, "sha256": rescued}},
		{"envelope", http.StatusCreated, map[string]any{"image": map[string]any{"id": 1, "sha256": rescued}, "tag_warnings": []string{"w"}}},
		{"duplicate", http.StatusOK, map[string]any{"image": map[string]any{"id": 1, "sha256": rescued}, "alias_added": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseMultipartForm(32 << 20)
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			res := mustPush(t, srv, tempFile(t, []byte("undecodable jpeg")), PushMeta{}, "")
			if res.SHA256 != rescued {
				t.Errorf("sha256 = %q, want monbooru's %q, not the sent bytes' digest", res.SHA256, rescued)
			}
		})
	}
}

func TestPushImageDuplicate(t *testing.T) {
	srv := stubJSON(t, http.StatusOK, map[string]any{
		"image":       map[string]any{"id": 55},
		"alias_added": true,
	})
	res := mustPush(t, srv, tempFile(t, []byte("x")), PushMeta{}, "furry")
	if res.Outcome != outcome.Duplicate || res.MonbooruID != 55 {
		t.Errorf("got %+v, want duplicate id 55", res)
	}
}

func TestPushImageErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusRequestEntityTooLarge, outcome.ErrCodeFileTooLarge},
		{http.StatusInternalServerError, outcome.ErrCodeMonbooruRejected},
		{http.StatusBadRequest, outcome.ErrCodeMonbooruRejected},
	}
	for _, tc := range cases {
		srv := stubJSON(t, tc.status, map[string]string{"error": "nope", "code": "x"})
		_, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{}, "")
		e, ok := err.(*outcome.CodedError)
		if !ok || e.Code != tc.want {
			t.Errorf("status %d -> %v, want code %s", tc.status, err, tc.want)
		}
	}
}

func TestPushImageUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, err := testClient(url, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{}, "")
	e, ok := err.(*outcome.CodedError)
	if !ok || e.Code != outcome.ErrCodeMonbooruUnreachable {
		t.Errorf("got %v, want monbooru_unreachable", err)
	}
}

func TestPushImageCanceled(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := testClient(srv.URL, "tok").PushImageFile(ctx, tempFile(t, []byte("x")), PushMeta{Filename: "x.jpg"}, "g")
	e, ok := err.(*outcome.CodedError)
	if !ok || e.Code != outcome.ErrCodeCanceled {
		t.Errorf("got %v, want canceled", err)
	}
}

func TestPushImageTruncatedSuccessBodyFails(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)

	_, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{Filename: "x.jpg"}, "g")
	e, ok := err.(*outcome.CodedError)
	if !ok || e.Code != outcome.ErrCodeMonbooruRejected {
		t.Errorf("got %v, want monbooru_rejected", err)
	}
}

func TestPushImageSuccessWithNoIDFails(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("<html>captive portal</html>"))
		}))
		_, err := testClient(srv.URL, "tok").PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{Filename: "x.jpg"}, "g")
		e, ok := err.(*outcome.CodedError)
		if !ok || e.Code != outcome.ErrCodeMonbooruRejected {
			t.Errorf("status %d: got %v, want monbooru_rejected", status, err)
		}
	}
}

func TestAReplaceAnsweredByAPortalFails(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		_, _ = w.Write([]byte("<html>captive portal</html>"))
	}))
	_, err := testClient(srv.URL, "tok").ReplaceImageFile(context.Background(), 7, tempFile(t, []byte("x")), PushMeta{Filename: "x.png"}, "")
	wantCode(t, err, outcome.ErrCodeMonbooruRejected)
}

func TestAnEnrichAnsweredByAPortalFails(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>captive portal</html>"))
	}))
	_, err := testClient(srv.URL, "tok").EnrichImage(context.Background(), 7, "g", EnrichPayload{})
	wantCode(t, err, outcome.ErrCodeMonbooruRejected)
}

func TestAPushOutlastsTheBackstopTimeout(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	c := testClient(srv.URL, "tok")
	c.http.Timeout = 20 * time.Millisecond
	res, err := c.PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{Filename: "x.jpg"}, "g")
	if err != nil || res.MonbooruID != 7 {
		t.Errorf("push = %+v, %v, want it through past the backstop", res, err)
	}
}

func TestAPushMonbooruNeverAnswersFails(t *testing.T) {
	release := make(chan struct{})
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-release
	}))
	defer close(release)
	c := testClient(srv.URL, "tok")
	c.upload.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
	_, err := c.PushImageFile(context.Background(), tempFile(t, []byte("x")), PushMeta{Filename: "x.jpg"}, "g")
	e, ok := err.(*outcome.CodedError)
	if !ok || e.Code != outcome.ErrCodeMonbooruUnreachable {
		t.Errorf("got %v, want monbooru_unreachable", err)
	}
}

func TestListGalleries(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/galleries" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]Gallery{
			{Name: "default", Images: 10, Tags: 5, Active: true},
			{Name: "furry", Images: 2, Tags: 1},
		})
	}))
	gs, err := testClient(srv.URL, "tok").ListGalleries(context.Background())
	if err != nil {
		t.Fatalf("ListGalleries: %v", err)
	}
	if len(gs) != 2 || gs[0].Name != "default" || !gs[0].Active || gs[1].Name != "furry" {
		t.Errorf("galleries = %+v", gs)
	}
}

func TestCustomCategories(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/galleries":
			_ = json.NewEncoder(w).Encode([]Gallery{{Name: "default"}, {Name: "my art"}})
		case "/api/v1/categories":
			cats := []map[string]any{{"name": "general", "is_builtin": true}, {"name": "va"}}
			if r.URL.Query().Get("gallery") == "my art" {
				cats = append(cats, map[string]any{"name": "event"})
			}
			_ = json.NewEncoder(w).Encode(cats)
		default:
			t.Errorf("path = %q", r.URL.Path)
		}
	}))
	got, complete, err := testClient(srv.URL, "tok").CustomCategories(context.Background())
	if err != nil {
		t.Fatalf("CustomCategories: %v", err)
	}
	if !complete || !slices.Equal(got, []string{"event", "va"}) {
		t.Errorf("categories = %v complete=%v, want every gallery's custom ones, once", got, complete)
	}
}

func TestCustomCategoriesSurviveABrokenGallery(t *testing.T) {
	broken := map[string]bool{"broken": true}
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/galleries":
			_ = json.NewEncoder(w).Encode([]Gallery{{Name: "broken"}, {Name: "fine"}})
		case broken[r.URL.Query().Get("gallery")]:
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "va"}})
		}
	}))
	got, complete, err := testClient(srv.URL, "tok").CustomCategories(context.Background())
	if err != nil || complete || !slices.Equal(got, []string{"va"}) {
		t.Errorf("one broken gallery = %v complete=%v err=%v, want the other's list, marked partial", got, complete, err)
	}
	broken["fine"] = true
	if _, _, err := testClient(srv.URL, "tok").CustomCategories(context.Background()); err == nil {
		t.Error("no gallery answering should be an error")
	}
}

func TestResolveTagsOnAMonbooruWithoutTheRoute(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		srv := stubJSON(t, status, map[string]string{"error": "endpoint not found: POST /api/v1/tags/resolve", "code": "not_found"})
		_, err := testClient(srv.URL, "tok").ResolveTags(context.Background(), "", []string{"cat"})
		wantCode(t, err, outcome.ErrCodeMonbooruRejected)
		if !strings.Contains(err.Error(), "monbooru v1.24.0 or newer") {
			t.Errorf("%d: err = %v, want it to name the monbooru the gallery column needs", status, err)
		}
	}
}

func TestConnection(t *testing.T) {
	ok := stubJSON(t, http.StatusOK, map[string]string{"api": "monbooru", "version": "v1.2.3"})
	version, err := testClient(ok.URL, "tok").TestConnection(context.Background())
	if err != nil {
		t.Errorf("TestConnection on a healthy server: %v", err)
	}
	if version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", version)
	}

	bad := stubJSON(t, http.StatusUnauthorized, map[string]string{})
	if _, err := testClient(bad.URL, "wrong").TestConnection(context.Background()); err == nil {
		t.Error("TestConnection should fail on 401")
	}
}

func TestOpenAPIContract(t *testing.T) {
	path := cmp.Or(os.Getenv("MONLOADER_TEST_MONBOORU_OPENAPI"), filepath.Join("..", "testdata", "monbooru", "openapi.json"))
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
		"CreateImageResponse":    {"image", "tag_warnings", "rejected_tags"},
		"DuplicateImageResponse": {"image", "alias_added", "merge", "rejected_tags"},
		"EnrichResponse":         {"merge", "tag_warnings"},
		"ReplaceFileResponse":    {"replaced", "merge", "tag_warnings"},
		"MergeSummary":           {"tags_added", "tags_retired", "rating_filled", "rejected_tags"},
		"Gallery":                {"name", "images", "tags", "active"},
		"APIInfo":                {"version"},
		"Health":                 {"app", "version", "api", "min_peer_api"},
		"PairAccepted":           {"request_id"},
		"PairStatus":             {"status", "token"},
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
	for _, p := range []string{
		"/", "/images", "/galleries", "/images/{id}", "/images/{id}/thumbnail",
		"/images/{id}/enrich", "/images/{id}/fetch-status", "/images/{id}/file",
		"/health", "/pair/request", "/pair/status", "/pair/remove", "/tags/resolve", "/categories",
	} {
		if _, ok := spec.Paths[p]; !ok {
			t.Errorf("openapi path %q missing", p)
		}
	}
	var enrich struct {
		Post struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"post"`
	}
	if err := json.Unmarshal(spec.Paths["/images/{id}/enrich"], &enrich); err != nil {
		t.Fatalf("decode the enrich path: %v", err)
	}
	body := enrich.Post.RequestBody.Content["application/json"].Schema.Properties
	for _, name := range jsonNames(reflect.TypeFor[EnrichPayload]()) {
		if _, ok := body[name]; !ok {
			t.Errorf("openapi enrich body lacks %q - EnrichImage sends it", name)
		}
	}

	var push struct {
		Post struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"post"`
	}
	if err := json.Unmarshal(spec.Paths["/images"], &push); err != nil {
		t.Fatalf("decode the push path: %v", err)
	}
	form := push.Post.RequestBody.Content["multipart/form-data"].Schema.Properties
	for _, name := range []string{
		contract.FieldFile, contract.FieldTags, contract.FieldVia, contract.FieldFolder, contract.FieldSource,
		contract.FieldPostID, contract.FieldURL, contract.FieldCommentary, contract.FieldCommentaryTranslated,
		contract.FieldCommentaryDText, contract.FieldCommentaryTranslatedDText, contract.FieldOriginal,
		contract.FieldParentURL, contract.FieldNotes, contract.FieldCollection, contract.FieldCollectionOrder,
		contract.FieldPostWidth, contract.FieldPostHeight, contract.FieldPostSize, contract.FieldPostExt, contract.FieldMD5,
		contract.FieldClientTags, contract.FieldWithheldTags, contract.FieldWithheldCategories,
	} {
		if _, ok := form[name]; !ok {
			t.Errorf("openapi push form lacks %q - PushImageFile sends it", name)
		}
	}

	var resolve struct {
		Post struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Properties struct {
							Results struct {
								Items struct {
									Properties map[string]json.RawMessage `json:"properties"`
								} `json:"items"`
							} `json:"results"`
						} `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"post"`
	}
	if err := json.Unmarshal(spec.Paths["/tags/resolve"], &resolve); err != nil {
		t.Fatalf("decode the resolve path: %v", err)
	}
	row := resolve.Post.Responses["200"].Content["application/json"].Schema.Properties.Results.Items.Properties
	for _, name := range jsonNames(reflect.TypeFor[ResolvedTag]()) {
		if _, ok := row[name]; !ok {
			t.Errorf("openapi resolve result lacks %q - ResolveTags reads it", name)
		}
	}
}

func jsonNames(typ reflect.Type) []string {
	var names []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous {
			names = append(names, jsonNames(f.Type)...)
			continue
		}
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

func TestReplaceImageFile(t *testing.T) {
	data := []byte("the better bytes")
	var gotPath, gotAuth, gotFilename string
	var gotUpload []byte
	stored := strings.Repeat("cd", 32)
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/images/42" && r.URL.Query().Get("gallery") == "mygallery" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "sha256": stored})
			return
		}
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		if f, fh, err := r.FormFile("file"); err == nil {
			gotFilename = fh.Filename
			gotUpload, _ = io.ReadAll(f)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"replaced": true,
			"merge":    map[string]any{"tags_added": 3},
		})
	}))

	res, err := testClient(srv.URL, "tok").ReplaceImageFile(context.Background(), 42,
		tempFile(t, data), PushMeta{Filename: "upgrade.png", PostFields: PostFields{Source: "danbooru"}}, "mygallery")
	if err != nil {
		t.Fatalf("ReplaceImageFile: %v", err)
	}
	if res.Outcome != outcome.Replaced || res.MonbooruID != 42 {
		t.Errorf("result = %+v, want replaced on image 42", res)
	}
	if res.MergeNote != "+3 tags" {
		t.Errorf("merge note = %q, want +3 tags", res.MergeNote)
	}
	if res.SHA256 != stored {
		t.Errorf("sha256 = %q, want the hash monbooru stored after the replace", res.SHA256)
	}
	if gotPath != "/api/v1/images/42/file?gallery=mygallery" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" || gotFilename != "upgrade.png" || string(gotUpload) != string(data) {
		t.Errorf("request wrong: auth=%q filename=%q bytes=%q", gotAuth, gotFilename, gotUpload)
	}
}

func TestReplaceImageFileFallsBackToTheSentDigest(t *testing.T) {
	data := []byte("the better bytes")
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = r.ParseMultipartForm(32 << 20)
		_, _ = w.Write([]byte(`{"replaced":true}`))
	}))
	res, err := testClient(srv.URL, "tok").ReplaceImageFile(context.Background(), 42, tempFile(t, data), PushMeta{}, "")
	if err != nil {
		t.Fatalf("ReplaceImageFile: %v", err)
	}
	sum := sha256.Sum256(data)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want the sent file's digest when the image read fails", res.SHA256)
	}
}

func TestReplaceImageFileAlreadyMatched(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"no merge", `{"replaced":false}`, "file already matched"},
		{"with merge", `{"replaced":false,"merge":{"tags_added":2}}`, "file already matched; +2 tags"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseMultipartForm(32 << 20)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			res, err := testClient(srv.URL, "tok").ReplaceImageFile(context.Background(), 7,
				tempFile(t, []byte("x")), PushMeta{Filename: "x.png"}, "")
			if err != nil {
				t.Fatalf("ReplaceImageFile: %v", err)
			}
			if res.Outcome != outcome.Replaced || res.MergeNote != tc.want {
				t.Errorf("outcome=%s note=%q, want replaced / %q", res.Outcome, res.MergeNote, tc.want)
			}
		})
	}
}

func TestReplaceImageFileRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"already exists", `{"code":"already_exists","error":"image 9 already holds it"}`, http.StatusConflict, outcome.ErrCodeAlreadyExists},
		{"wrong type", `{"code":"wrong_type","error":"target is an archive"}`, http.StatusConflict, outcome.ErrCodeWrongType},
		{"other conflict", `{"code":"something_else","error":"nope"}`, http.StatusConflict, outcome.ErrCodeMonbooruRejected},
		{"too large", `{"error":"too big"}`, http.StatusRequestEntityTooLarge, outcome.ErrCodeFileTooLarge},
		{"server error", `{"error":"boom"}`, http.StatusInternalServerError, outcome.ErrCodeMonbooruRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseMultipartForm(32 << 20)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			_, err := testClient(srv.URL, "tok").ReplaceImageFile(context.Background(), 7,
				tempFile(t, []byte("x")), PushMeta{Filename: "x.png"}, "")
			wantCode(t, err, tc.want)
		})
	}
}

func TestReplaceImageFileMissingFileIsLocal(t *testing.T) {
	_, err := testClient("http://127.0.0.1:1", "tok").ReplaceImageFile(context.Background(), 1,
		filepath.Join(t.TempDir(), "gone"), PushMeta{}, "")
	wantCode(t, err, outcome.ErrCodeDownloadFailed)
}

func TestReportFetchOutcome(t *testing.T) {
	var gotPath, gotBody, gotAuth string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Query().Get("gallery") == "bad" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	c := testClient(srv.URL, "tok")
	if err := c.ReportFetchOutcome(context.Background(), 5, "art", "lookup", "ptr", "failed", "auth required"); err != nil {
		t.Fatalf("ReportFetchOutcome: %v", err)
	}
	if gotPath != "/api/v1/images/5/fetch-status?gallery=art" || gotAuth != "Bearer tok" {
		t.Errorf("path = %q auth = %q", gotPath, gotAuth)
	}
	if !strings.Contains(gotBody, `"state":"failed"`) || !strings.Contains(gotBody, `"message":"auth required"`) ||
		!strings.Contains(gotBody, `"backend":"ptr"`) || !strings.Contains(gotBody, `"kind":"lookup"`) {
		t.Errorf("body = %q", gotBody)
	}
	if err := c.ReportFetchOutcome(context.Background(), 5, "art", "metadata", "", "failed", ""); err != nil || strings.Contains(gotBody, "backend") {
		t.Errorf("a report outside a lookup = %q (%v), want no backend", gotBody, err)
	}
	if err := c.ReportFetchOutcome(context.Background(), 5, "bad", "metadata", "", "failed", ""); err == nil {
		t.Error("a non-200 should be reported to the caller")
	}
}

func TestImageSHA256(t *testing.T) {
	var gotPath string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		switch r.URL.Query().Get("gallery") {
		case "missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such image","code":"not_found"}`))
		case "garbage":
			_, _ = w.Write([]byte(`not json`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "sha256": "abc123"})
		}
	}))

	c := testClient(srv.URL, "tok")
	sum, err := c.ImageSHA256(context.Background(), 5, "art")
	if err != nil || sum != "abc123" {
		t.Fatalf("ImageSHA256 = %q, %v", sum, err)
	}
	if gotPath != "/api/v1/images/5?gallery=art" {
		t.Errorf("path = %q", gotPath)
	}
	if _, err := c.ImageSHA256(context.Background(), 5, "missing"); err == nil {
		t.Error("a 404 should be an error")
	}
	if _, err := c.ImageSHA256(context.Background(), 5, "garbage"); err == nil {
		t.Error("an undecodable body should be an error")
	}

	_, err = testClient("http://127.0.0.1:1", "tok").ImageSHA256(context.Background(), 5, "")
	wantCode(t, err, outcome.ErrCodeMonbooruUnreachable)
}

func TestPairTeardown(t *testing.T) {
	var gotPath, gotAuth string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Bearer issued" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	c := testClient(srv.URL, "stored")
	if err := c.PairTeardown(context.Background(), "issued"); err != nil {
		t.Fatalf("PairTeardown: %v", err)
	}
	if gotPath != "/api/v1/pair/remove" || gotAuth != "Bearer issued" {
		t.Errorf("path = %q auth = %q", gotPath, gotAuth)
	}
	if err := c.PairTeardown(context.Background(), "wrong"); err == nil {
		t.Error("a refused teardown should be reported")
	}
	if err := testClient("http://127.0.0.1:1", "tok").PairTeardown(context.Background(), "t"); err == nil {
		t.Error("an unreachable monbooru should be reported")
	}
}

func TestReplaceRefusesARedirect(t *testing.T) {
	var gets int
	target := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusMovedPermanently)
	}))

	_, err := testClient(srv.URL, "tok").ReplaceImageFile(context.Background(), 5, tempFile(t, []byte("bytes")), PushMeta{}, "")
	wantCode(t, err, outcome.ErrCodeMonbooruRejected)
	if gets != 0 {
		t.Errorf("the redirect was followed %d times", gets)
	}
}

func TestParseImageResultReadsTheRejectedNames(t *testing.T) {
	id, _, warnings, rejected, merge := parseImageResult([]byte(`{"image":{"id":9},"tag_warnings":["tag x: bad"],"rejected_tags":["watermark"],"merge":{"tags_added":2}}`))
	if id != 9 || len(warnings) != 1 || len(rejected) != 1 || rejected[0] != "watermark" || merge == "" {
		t.Errorf("parsed %d %v %v %q", id, warnings, rejected, merge)
	}
	if _, _, rejected := enrichResult([]byte(`{"merge":{"tags_added":0,"rejected_tags":["lowres"]}}`)); len(rejected) != 1 || rejected[0] != "lowres" {
		t.Errorf("enrich rejected = %v, want [lowres]", rejected)
	}
}
