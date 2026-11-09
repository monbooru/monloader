package gdl

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/fsx"
)

// gallery-dl's directlink extractor needs a known media extension in the
// path, so a bare media URL without one comes back unsupported and is
// fetched here instead.

const directlinkCategory = "directlink"

// Some CDNs reject a request without a browser-like agent.
const directFetchUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

func directRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", directFetchUserAgent)
	return req, nil
}

// No whole-request timeout: it would cut a large file on a slow link. A stuck
// connection is directFetchStall's, and a job cancel comes through the request context.
var directFetchClient = &http.Client{}

var directFetchStall = time.Minute

var errDirectFetchStalled = errors.New("the file's host stopped sending")

// A resolve accepted this URL before the download pass got here, so a failure is
// the fetch's, not the URL's.
func directFetchFailed(ctx context.Context, msg string) *outcome.CodedError {
	if errors.Is(context.Cause(ctx), errDirectFetchStalled) {
		msg = errDirectFetchStalled.Error()
	}
	return &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: msg}
}

type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s stallReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 {
		s.timer.Reset(directFetchStall)
	}
	return n, err
}

// Past this an over-long CDN segment would fail the create, at the 255-byte
// limit most filesystems set.
const maxDirectName = 200

func directFileName(meta map[string]any) string {
	name := cmp.Or(fsx.SanitizeFilename(kwdict.String(meta, "filename")), "file")
	if len(name) > maxDirectName {
		name = strings.ToValidUTF8(name[:maxDirectName], "")
	}
	return name
}

// Nothing else stops a hostile host from filling the RAM-backed /work.
// Sized to monbooru's default per-file ceiling.
var directFetchMaxBytes int64 = 2 << 30

var errDirectFetchTooLarge = errors.New("file exceeds the directlink size cap")

// So a black-hole host cannot stall a worker on the probe.
var directProbeTimeout = 15 * time.Second

var mediaExtensions = map[string]string{
	"image/jpeg":  "jpg",
	"image/pjpeg": "jpg",
	"image/png":   "png",
	"image/apng":  "png",
	"image/gif":   "gif",
	"image/webp":  "webp",
	"image/avif":  "avif",
	"image/jxl":   "jxl",
	"video/mp4":   "mp4",
	"video/webm":  "webm",
}

func directlinkResolve(ctx context.Context, rawURL string) ([]Item, bool) {
	u, ok := parseHTTPURL(rawURL)
	if !ok {
		return nil, false
	}
	ct, ok := probeContentType(ctx, rawURL)
	if !ok {
		return nil, false
	}
	if _, ok := ingestableMedia(ct); !ok {
		return nil, false
	}
	return []Item{itemFromMeta(directlinkMeta(u))}, true
}

func directlinkDownload(ctx context.Context, rawURL, workDir string, onFile func(Downloaded)) ([]Downloaded, bool, error) {
	u, ok := parseHTTPURL(rawURL)
	if !ok {
		return nil, false, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(directFetchStall, func() { cancel(errDirectFetchStalled) })
	defer stall.Stop()
	req, err := directRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, false, nil
	}
	resp, err := directFetchClient.Do(req)
	if err != nil {
		return nil, true, directFetchFailed(ctx, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, true, directFetchFailed(ctx, "the file's host answered "+resp.Status)
	}
	ext, ok := ingestableMedia(resp.Header.Get("Content-Type"))
	if !ok {
		return nil, false, nil
	}

	meta := directlinkMeta(u)
	path := filepath.Join(workDir, directFileName(meta)+"."+ext)
	if err := writeFile(path, stallReader{resp.Body, stall}); err != nil {
		if errors.Is(err, errDirectFetchTooLarge) {
			return nil, true, &outcome.CodedError{Code: outcome.ErrCodeFileTooLarge, Msg: err.Error()}
		}
		return nil, true, directFetchFailed(ctx, err.Error())
	}

	d := Downloaded{Path: path, Meta: meta}
	if onFile != nil {
		onFile(d)
	}
	return []Downloaded{d}, true, nil
}

func writeFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, directFetchMaxBytes+1))
	if err == nil && n > directFetchMaxBytes {
		err = errDirectFetchTooLarge
	}
	if err = cmp.Or(err, f.Close()); err != nil {
		os.Remove(path)
	}
	return err
}

func probeContentType(ctx context.Context, rawURL string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, directProbeTimeout)
	defer cancel()
	req, err := directRequest(ctx, http.MethodHead, rawURL)
	if err != nil {
		return "", false
	}
	resp, err := directFetchClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.Header.Get("Content-Type"), true
		}
	}
	return probeContentTypeGET(ctx, rawURL)
}

func probeContentTypeGET(ctx context.Context, rawURL string) (string, bool) {
	req, err := directRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return "", false
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := directFetchClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return "", false
	}
	return resp.Header.Get("Content-Type"), true
}

// The metadata gallery-dl's directlink extractor would give u, with the whole
// name in filename.
func directlinkMeta(u *url.URL) map[string]any {
	dir, file := path.Split(strings.TrimPrefix(u.EscapedPath(), "/"))
	meta := map[string]any{
		"category":    directlinkCategory,
		"subcategory": registrableSuffix(u.Host),
		"domain":      u.Host,
		"path":        strings.TrimSuffix(dir, "/"),
		"filename":    file,
		"extension":   "",
	}
	if u.RawQuery != "" {
		meta["query"] = u.RawQuery
	}
	if frag := u.EscapedFragment(); frag != "" {
		meta["fragment"] = frag
	}
	return meta
}

// The last two labels, as gallery-dl derives the directlink subcategory.
func registrableSuffix(host string) string {
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

func parseHTTPURL(rawURL string) (*url.URL, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, false
	}
	return u, true
}
