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

// The GitHub releases are mirrors with no assets; the standalone builds
// are only on Codeberg.
var releaseBase = "https://codeberg.org/mikf/gallery-dl/releases/download/v"

// Only this falls back to the venv; a download that starts and then fails
// never does.
var errNoAsset = errors.New("no published build for this platform")

// Not releaseMetaClient: its timeout covers the whole body, which a 25 MB
// transfer over a slow link would exceed.
var standaloneClient = &http.Client{Timeout: 10 * time.Minute}

// Upstream's asset names; any other platform falls back to the venv.
type standaloneAsset struct {
	Remote string
	Local  string
}

var standaloneAssets = map[string]standaloneAsset{
	"linux/amd64":   {Remote: "gallery-dl.bin", Local: "gallery-dl"},
	"windows/amd64": {Remote: "gallery-dl.exe", Local: "gallery-dl.exe"},
	"windows/386":   {Remote: "gallery-dl_x86.exe", Local: "gallery-dl.exe"},
}

func standaloneForHost() (standaloneAsset, bool) {
	a, ok := standaloneAssets[runtime.GOOS+"/"+runtime.GOARCH]
	return a, ok
}

// The real binary is about 25 MB; the cap keeps a replaced URL from
// filling the volume that holds the queue and every credential.
const maxStandaloneBytes int64 = 128 << 20

// Checked against the release's SHA256SUMS rather than trusted for
// arriving over HTTPS.
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

func publishedSHA256(ctx context.Context, version, name string) (string, error) {
	body, err := fetch(ctx, releaseBase+version+"/SHA256SUMS", 1<<20)
	if err != nil {
		// A release too old for Codeberg has no checksum file, which
		// means no build for us.
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

// Its percentages match the venv path's, so the progress bar behaves the
// same either way.
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

var errNotFound = errors.New("not found")

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
