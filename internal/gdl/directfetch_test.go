package gdl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/outcome"
)

func TestIngestableMedia(t *testing.T) {
	for _, tc := range []struct {
		ct   string
		want string
		ok   bool
	}{
		{"image/jpeg", "jpg", true},
		{"image/png", "png", true},
		{"image/webp", "webp", true},
		{"video/mp4", "mp4", true},
		{"image/avif", "avif", true},
		{"image/jxl", "jxl", true},
		{"image/svg+xml", "", false},
		{"video/quicktime", "", false},
		{"text/html", "", false},
	} {
		got, ok := ingestableMedia(tc.ct)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ingestableMedia(%q) = (%q, %v), want (%q, %v)", tc.ct, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDirectlinkResolveDeclinesUningestible(t *testing.T) {
	serve := func(ct string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	if _, ok := directlinkResolve(context.Background(), serve("image/svg+xml").URL+"/avatar"); ok {
		t.Error("an svg directlink should be declined (monbooru cannot ingest it)")
	}
	if _, ok := directlinkResolve(context.Background(), serve("image/png").URL+"/avatar"); !ok {
		t.Error("a png directlink should be handled")
	}
}

func TestDirectlinkMeta(t *testing.T) {
	for _, tc := range []struct {
		raw                          string
		domain, path, filename       string
		query, fragment, subcategory string
	}{
		{
			raw:    "https://yt3.ggpht.com/ytc/AIdro_avatar=s400-c-k-no-rj",
			domain: "yt3.ggpht.com", path: "ytc", filename: "AIdro_avatar=s400-c-k-no-rj",
			subcategory: "ggpht.com",
		},
		{
			raw:    "https://cdn.example.com/image",
			domain: "cdn.example.com", path: "", filename: "image",
			subcategory: "example.com",
		},
		{
			raw:    "https://host.example.com/a/b/c?size=large#frag",
			domain: "host.example.com", path: "a/b", filename: "c",
			query: "size=large", fragment: "frag", subcategory: "example.com",
		},
	} {
		u, ok := parseHTTPURL(tc.raw)
		if !ok {
			t.Fatalf("parseHTTPURL(%q) failed", tc.raw)
		}
		meta := directlinkMeta(u)
		for key, want := range map[string]string{
			"category":    "directlink",
			"domain":      tc.domain,
			"path":        tc.path,
			"filename":    tc.filename,
			"extension":   "",
			"query":       tc.query,
			"fragment":    tc.fragment,
			"subcategory": tc.subcategory,
		} {
			if got := kwdict.String(meta, key); got != want {
				t.Errorf("%s: %s = %q, want %q", tc.raw, key, got, want)
			}
		}
	}
}

func TestParseHTTPURLRejectsNonHTTP(t *testing.T) {
	for _, raw := range []string{"ftp://example.com/x", "file:///etc/passwd", "mailto:a@b.c", "/relative/path", "example.com/no-scheme"} {
		if _, ok := parseHTTPURL(raw); ok {
			t.Errorf("parseHTTPURL(%q) accepted a non-http(s) URL", raw)
		}
	}
}

func mediaServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeContentTypeFallsBackToGET(t *testing.T) {
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG"))
	}))
	defer srv.Close()

	ct, ok := probeContentType(context.Background(), srv.URL+"/avatar")
	if !ok {
		t.Fatal("probe should fall back to GET when HEAD is not 200")
	}
	if ext, _ := ingestableMedia(ct); ext != "png" {
		t.Errorf("content type = %q, want image/png", ct)
	}
	if gotRange != "bytes=0-0" {
		t.Errorf("GET probe Range = %q, want bytes=0-0 (must not pull the whole file)", gotRange)
	}
}

func TestProbeContentTypeHonorsTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	old := directProbeTimeout
	directProbeTimeout = 100 * time.Millisecond
	defer func() { directProbeTimeout = old }()

	done := make(chan bool, 1)
	go func() {
		_, ok := probeContentType(context.Background(), srv.URL+"/avatar")
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("probe of a hanging host should fail, not succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not honor directProbeTimeout; it hung past the deadline")
	}
}

func TestDirectlinkDownload(t *testing.T) {
	body := []byte("\xff\xd8\xffavatarbytes")
	srv := mediaServer(t, body)
	workDir := t.TempDir()

	dls, ok, err := directlinkDownload(context.Background(), srv.URL+"/ytc/avatar", workDir, nil)
	if err != nil || !ok {
		t.Fatalf("directlinkDownload: ok=%v err=%v", ok, err)
	}
	if len(dls) != 1 || !strings.HasSuffix(dls[0].Path, ".jpg") {
		t.Fatalf("downloaded = %+v, want one .jpg from the served type", dls)
	}
	if got, err := os.ReadFile(dls[0].Path); err != nil || !bytes.Equal(got, body) {
		t.Errorf("saved bytes = %q (err %v), want the served body", got, err)
	}

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
	}))
	t.Cleanup(html.Close)
	if _, ok, _ := directlinkDownload(context.Background(), html.URL+"/page", workDir, nil); ok {
		t.Error("a text/html response should be declined")
	}
}

func TestDirectlinkDownloadReadsATransientFailureAsAFailedFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	_, ok, err := directlinkDownload(context.Background(), srv.URL+"/ytc/avatar", t.TempDir(), nil)
	var ce *outcome.CodedError
	if !ok || !errors.As(err, &ce) || ce.Code != outcome.ErrCodeDownloadFailed {
		t.Errorf("ok=%v err=%v, want a download_failed the caller keeps", ok, err)
	}
}

func TestDirectlinkDownloadStallsOnlyOnSilence(t *testing.T) {
	old := directFetchStall
	directFetchStall = 150 * time.Millisecond
	t.Cleanup(func() { directFetchStall = old })

	trickle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		for range 6 {
			_, _ = w.Write([]byte("bytes"))
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	t.Cleanup(trickle.Close)
	if _, ok, err := directlinkDownload(context.Background(), trickle.URL+"/slow", t.TempDir(), nil); !ok || err != nil {
		t.Errorf("a body outlasting the stall failed: ok=%v err=%v", ok, err)
	}

	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("bytes"))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(silent.Close)
	t.Cleanup(func() { close(release) })
	_, ok, err := directlinkDownload(context.Background(), silent.URL+"/stuck", t.TempDir(), nil)
	var ce *outcome.CodedError
	if !ok || !errors.As(err, &ce) || ce.Code != outcome.ErrCodeDownloadFailed || ce.Msg != errDirectFetchStalled.Error() {
		t.Errorf("ok=%v err=%v, want the stall named", ok, err)
	}
}

func TestDirectlinkDownloadClipsALongName(t *testing.T) {
	srv := mediaServer(t, []byte("\xff\xd8\xffbytes"))
	long := strings.Repeat("あ", 100)
	dls, ok, err := directlinkDownload(context.Background(), srv.URL+"/"+long, t.TempDir(), nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	base := filepath.Base(dls[0].Path)
	if len(base) > maxDirectName+len(".jpg") || !utf8.ValidString(base) {
		t.Errorf("name = %q (%d bytes), want it clipped whole", base, len(base))
	}
}

func TestLiveResolveDirectlinkFallback(t *testing.T) {
	tool := liveTool(t)
	srv := mediaServer(t, []byte("\xff\xd8\xffjpeg"))
	rawURL := srv.URL + "/ytc/AIdro_avatar=s400-c-k-no-rj"

	res, err := tool.Resolve(context.Background(), rawURL, "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	items := res.Items
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 directlink item", len(items))
	}
	if items[0].Category != "directlink" {
		t.Errorf("category = %q, want directlink", items[0].Category)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if got := kwdict.String(items[0].Meta, "domain"); got != host {
		t.Errorf("domain = %q, want the file host %q", got, host)
	}
	if got := kwdict.String(items[0].Meta, "filename"); got != "AIdro_avatar=s400-c-k-no-rj" {
		t.Errorf("filename = %q, want the last path segment", got)
	}
}

func TestLiveDownloadDirectlinkFallback(t *testing.T) {
	tool := liveTool(t)
	body := []byte("\xff\xd8\xffYTAVATARBYTES")
	srv := mediaServer(t, body)
	rawURL := srv.URL + "/ytc/AIdro_avatar=s400-c-k-no-rj"
	workDir := t.TempDir()

	var streamed []Downloaded
	dls, err := tool.Download(context.Background(), rawURL, "", workDir, ArchiveRead, func(d Downloaded) {
		streamed = append(streamed, d)
	}, false)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("downloaded = %d, want 1", len(dls))
	}
	if got := kwdict.String(dls[0].Meta, "category"); got != "directlink" {
		t.Errorf("category = %q, want directlink", got)
	}
	if !strings.HasSuffix(dls[0].Path, ".jpg") {
		t.Errorf("path = %q, want a .jpg extension from the served content type", dls[0].Path)
	}
	got, err := os.ReadFile(dls[0].Path)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded bytes = %q, want the served body", got)
	}
	if len(streamed) != 1 {
		t.Errorf("onFile called %d times, want 1 (the item must advance as the file lands)", len(streamed))
	}
}

type errAfterReader struct{ r io.Reader }

func (e *errAfterReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func TestWriteFileRemovesPartialOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.jpg")
	if err := writeFile(path, &errAfterReader{r: strings.NewReader("abc")}); err == nil {
		t.Fatal("writeFile on a failing reader returned nil")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("partial file left behind after a copy error")
	}
}

func TestWriteFileEnforcesSizeCap(t *testing.T) {
	old := directFetchMaxBytes
	directFetchMaxBytes = 4
	defer func() { directFetchMaxBytes = old }()

	path := filepath.Join(t.TempDir(), "x.jpg")
	err := writeFile(path, strings.NewReader("12345678"))
	if !errors.Is(err, errDirectFetchTooLarge) {
		t.Fatalf("oversize writeFile error = %v, want errDirectFetchTooLarge", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("oversize file left behind")
	}
	if err := writeFile(path, strings.NewReader("1234")); err != nil {
		t.Fatalf("at-cap writeFile: %v", err)
	}
}
