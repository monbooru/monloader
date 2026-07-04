package gdl

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leqwin/monloader/internal/kwdict"
	"github.com/leqwin/monloader/internal/queue"
)

// gallery-dl's directlink extractor only matches a URL whose path ends in a
// known media extension, so a bare media URL with none (a CDN avatar served as
// image/jpeg) is rejected as unsupported. When that happens the wrapper probes
// the content type and, for a media file, handles it as a directlink itself.
// This is the one fetch monloader makes outside gallery-dl; it never touches a
// monbooru gallery, so the isolation model holds.

// directlinkCategory is gallery-dl's pseudo-extractor name for a bare media URL,
// stamped on the synthesized metadata so the mapper takes its directlink path.
const directlinkCategory = "directlink"

// directFetchUserAgent is sent on the probe and the fetch: some CDNs reject a
// request without a browser-like agent, which is also why gallery-dl sends one.
const directFetchUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

// directRequest builds the UA-stamped request the probe and fetch paths share.
func directRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", directFetchUserAgent)
	return req, nil
}

// directFetchClient fetches the bare media URLs gallery-dl cannot. The generous
// ceiling bounds a stuck connection; the request context cancels the fetch on a
// job cancel or shutdown.
var directFetchClient = &http.Client{Timeout: 10 * time.Minute}

// directProbeTimeout bounds the content-type probe (the HEAD and ranged GET
// before any download) so a black-hole host on an extension-less URL cannot
// stall a worker for the full directFetchClient ceiling; the media download
// itself keeps that ceiling.
var directProbeTimeout = 15 * time.Second

// mediaExtensions maps a downloadable media content type to the file extension
// monloader saves it under. The set mirrors the image and video formats
// gallery-dl's directlink extractor recognizes by extension; a type not here
// (an HTML page, JSON) is not treated as a bare media file.
var mediaExtensions = map[string]string{
	"image/jpeg":       "jpg",
	"image/pjpeg":      "jpg",
	"image/png":        "png",
	"image/apng":       "png",
	"image/gif":        "gif",
	"image/webp":       "webp",
	"image/avif":       "avif",
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

// mediaExtension maps a Content-Type header to a file extension, reporting false
// for anything monloader will not treat as a bare media file.
func mediaExtension(contentType string) (string, bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", false
	}
	ext, ok := mediaExtensions[mt]
	return ext, ok
}

// directlinkResolve synthesizes a directlink resolve item for a media URL
// gallery-dl rejected as unsupported. A HEAD reveals the content type; an
// ingestable media type yields one item whose metadata rebuilds the submitted
// URL (extension left empty, since the URL carries none). Not handled (false)
// for a type monbooru cannot ingest or an unreachable host, so the caller keeps
// gallery-dl's unsupported error.
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

// directlinkDownload fetches a media URL gallery-dl rejected as unsupported (see
// directlinkResolve) into workDir as a single directlink file. The extension
// comes from the served content type, so the pushed file carries the real type
// even though the URL had none. Not handled (false) when the type is not media
// monbooru can ingest, so the caller keeps gallery-dl's error.
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
		return nil, true, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}

	d := Downloaded{Path: path, Meta: meta}
	if onFile != nil {
		onFile(0, d)
	}
	return []Downloaded{d}, true, nil
}

// writeFile streams r into a new file at path, cleaning up a partial file on a
// copy or close error so a truncated download is never left for the push.
func writeFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
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

// probeContentType returns the Content-Type for rawURL: a HEAD first, then a
// ranged GET as a fallback for servers that do not answer HEAD with 200.
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

// probeContentTypeGET reads the Content-Type from a ranged GET (first byte
// only); the body is closed without downloading the file.
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

// directlinkMeta builds the metadata gallery-dl's directlink extractor would
// expose for u, with the extension left empty so the mapper rebuilds the
// submitted URL rather than appending one. The download path saves the file
// under the content type's real extension instead.
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

// splitPath divides a URL path into its leading directories and final segment,
// matching how gallery-dl's directlink extractor splits domain/path/filename.
func splitPath(p string) (dir, file string) {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

// registrableSuffix returns the last two labels of a host (the directlink
// subcategory gallery-dl derives), e.g. yt3.ggpht.com -> ggpht.com.
func registrableSuffix(host string) string {
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// parseHTTPURL parses an http(s) URL with a host, rejecting anything else so the
// fallback never fetches a non-network scheme.
func parseHTTPURL(rawURL string) (*url.URL, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, false
	}
	return u, true
}
