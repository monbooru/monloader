package gdl

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/outcome"
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

// The ceiling only catches a stuck connection; a job cancel comes through
// the request context.
var directFetchClient = &http.Client{Timeout: 10 * time.Minute}

// Nothing else stops a hostile host from filling the RAM-backed /work.
// Sized to monbooru's default per-file ceiling.
var directFetchMaxBytes int64 = 2 << 30

var errDirectFetchTooLarge = errors.New("file exceeds the directlink size cap")

// Shorter than the client's ceiling, so a black-hole host cannot stall a
// worker on the probe.
var directProbeTimeout = 15 * time.Second

// The formats gallery-dl's directlink extractor knows by extension, plus
// JPEG XL, which it does not: a .jxl link falls back to this fetch.
var mediaExtensions = map[string]string{
	"image/jpeg":       "jpg",
	"image/pjpeg":      "jpg",
	"image/png":        "png",
	"image/apng":       "png",
	"image/gif":        "gif",
	"image/webp":       "webp",
	"image/avif":       "avif",
	"image/jxl":        "jxl",
	"image/bmp":        "bmp",
	"image/x-ms-bmp":   "bmp",
	"image/svg+xml":    "svg",
	"image/heic":       "heic",
	"image/heif":       "heif",
	"image/tiff":       "tiff",
	"video/mp4":        "mp4",
	"video/x-m4v":      "m4v",
	"video/quicktime":  "mov",
	"video/webm":       "webm",
	"video/x-matroska": "mkv",
	"video/ogg":        "ogv",
}

func mediaExtension(contentType string) (string, bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", false
	}
	ext, ok := mediaExtensions[mt]
	return ext, ok
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

func directlinkDownload(ctx context.Context, rawURL, workDir string, onFile func(int, Downloaded)) ([]Downloaded, bool, error) {
	u, ok := parseHTTPURL(rawURL)
	if !ok {
		return nil, false, nil
	}
	req, err := directRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, false, nil
	}
	resp, err := directFetchClient.Do(req)
	if err != nil {
		return nil, false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, nil
	}
	ext, ok := ingestableMedia(resp.Header.Get("Content-Type"))
	if !ok {
		return nil, false, nil
	}

	meta := directlinkMeta(u)
	name := kwdict.String(meta, "filename")
	if name == "" {
		name = "file"
	}
	path := filepath.Join(workDir, name+"."+ext)
	if err := writeFile(path, resp.Body); err != nil {
		code := outcome.ErrCodeDownloadFailed
		if errors.Is(err, errDirectFetchTooLarge) {
			code = outcome.ErrCodeFileTooLarge
		}
		return nil, true, &outcome.CodedError{Code: code, Msg: err.Error()}
	}

	d := Downloaded{Path: path, Meta: meta}
	if onFile != nil {
		onFile(0, d)
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
	if err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
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

// The metadata gallery-dl's directlink extractor would give u, but with
// the extension empty so the mapper rebuilds the submitted URL instead of
// appending one.
func directlinkMeta(u *url.URL) map[string]any {
	dir, file := splitPath(strings.TrimPrefix(u.EscapedPath(), "/"))
	meta := map[string]any{
		"category":    directlinkCategory,
		"subcategory": registrableSuffix(u.Host),
		"domain":      u.Host,
		"path":        dir,
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

func splitPath(p string) (dir, file string) {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
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
