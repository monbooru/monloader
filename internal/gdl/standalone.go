package gdl

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Upstream moved to Codeberg; the GitHub releases are mirrors that carry no
// assets at all, so the standalone builds are only there. A var so a test
// can point it at a stub, like the PyPI URL in managed.go.
var releaseBase = "https://codeberg.org/mikf/gallery-dl/releases/download/v"

// errNoAsset says this release publishes nothing for this platform, which is
// the case the venv fallback exists for. A download that starts and then
// fails is a different thing and is never fallen back from.
var errNoAsset = errors.New("no published build for this platform")

// standaloneClient bounds the binary download. The metadata client's short
// timeout covers a whole request including its body, which a 25 MB transfer
// over a slow link would blow through.
var standaloneClient = &http.Client{Timeout: 10 * time.Minute}

// standaloneAsset is the published one-file build for one platform, and the
// name it lands under here. The table is upstream's, not ours: read from the
// v1.32.9 release, which publishes a Linux amd64 binary and two Windows
// ones. There is no linux/arm64 and no macOS build, so those platforms fall
// back to the venv (installVenv in managed.go) or to a gallery-dl on PATH.
type standaloneAsset struct {
	Remote string
	Local  string
}

var standaloneAssets = map[string]standaloneAsset{
	"linux/amd64":   {Remote: "gallery-dl.bin", Local: "gallery-dl"},
	"windows/amd64": {Remote: "gallery-dl.exe", Local: "gallery-dl.exe"},
	"windows/386":   {Remote: "gallery-dl_x86.exe", Local: "gallery-dl.exe"},
}

// standaloneForHost returns the asset for the running platform, if any.
func standaloneForHost() (standaloneAsset, bool) {
	a, ok := standaloneAssets[runtime.GOOS+"/"+runtime.GOARCH]
	return a, ok
}

// maxStandaloneBytes caps the download. The real binary is about 25 MB; the
// cap is what keeps a redirected or replaced URL from filling the volume
// that also holds the queue and every credential.
const maxStandaloneBytes int64 = 128 << 20

// installStandalone downloads the published one-file build for version into
// root and returns the path it landed at. The release publishes a
// SHA256SUMS beside the assets, so the download is checked against it rather
// than trusted for arriving over HTTPS.
func installStandalone(ctx context.Context, root, version string, asset standaloneAsset, report InstallProgress) (string, error) {
	want, err := publishedSHA256(ctx, version, asset.Remote)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(root, asset.Local)
	report("downloading gallery-dl "+version, 20)
	if err := downloadVerified(ctx, releaseBase+version+"/"+asset.Remote, dst, want, report); err != nil {
		return "", err
	}
	report("installing", 70)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dst, 0o755); err != nil {
			return "", err
		}
	}
	return dst, nil
}

// publishedSHA256 reads the release's checksum file and returns the digest
// for one asset.
func publishedSHA256(ctx context.Context, version, name string) (string, error) {
	body, err := fetch(ctx, releaseBase+version+"/SHA256SUMS", 1<<20)
	if err != nil {
		// A release too old to have been published here has no checksum
		// file at all, which is the same answer as no build for us.
		if errors.Is(err, errNotFound) {
			return "", errNoAsset
		}
		return "", fmt.Errorf("reading the release checksums: %w", err)
	}
	scan := bufio.NewScanner(strings.NewReader(string(body)))
	for scan.Scan() {
		digest, file, ok := strings.Cut(strings.TrimSpace(scan.Text()), "  ")
		if ok && file == name {
			return digest, nil
		}
	}
	return "", errNoAsset
}

// downloadVerified streams url into dst through a temp file, refusing to
// publish it unless the bytes hash to want. The percentages match what the
// venv path emitted, so the panel's progress bar behaves the same either way.
func downloadVerified(ctx context.Context, url, dst, want string, report InstallProgress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := standaloneClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".gallery-dl-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, sum), &progressReader{
		r:      io.LimitReader(resp.Body, maxStandaloneBytes+1),
		total:  resp.ContentLength,
		report: report,
		label:  "downloading gallery-dl",
		from:   20,
		to:     60,
	})
	closeErr := tmp.Close()
	switch {
	case err != nil:
		return err
	case closeErr != nil:
		return closeErr
	case written > maxStandaloneBytes:
		return errors.New("the download is larger than expected")
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("the download does not match the published checksum")
	}
	report("installing", 70)
	return os.Rename(tmpName, dst)
}

// progressReader reports how far a download has got, mapped onto the
// percentage band the caller reserved for it. A response with no
// Content-Length reports nothing rather than guessing.
type progressReader struct {
	r        io.Reader
	total    int64
	read     int64
	report   InstallProgress
	label    string
	from, to int
	last     int
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.total > 0 && p.report != nil {
		pct := p.from + int(int64(p.to-p.from)*p.read/p.total)
		if pct > p.last {
			p.last = pct
			p.report(p.label, pct)
		}
	}
	return n, err
}

// errNotFound distinguishes an absent document from a failed request, so a
// caller can tell "there is nothing here" from "the fetch broke".
var errNotFound = errors.New("not found")

// fetch reads a small document whole, bounded.
func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := releaseMetaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
