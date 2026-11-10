package gdl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/shared/desktop"

	"github.com/monbooru/monloader/internal/config"
)

func stubRelease(t *testing.T, version string, corrupt bool) string {
	t.Helper()
	payload := []byte("#!/bin/sh\necho " + version + "\n")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	if corrupt {
		digest = strings.Repeat("0", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if !strings.HasPrefix(r.URL.Path, "/v"+version+"/") {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write([]byte(digest + "  gallery-dl.bin\n" + strings.Repeat("f", 64) + "  gallery-dl.exe\n"))
		case strings.HasSuffix(r.URL.Path, "/gallery-dl.bin"):
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releaseBase
	releaseBase = srv.URL + "/v"
	t.Cleanup(func() { releaseBase = old })
	return srv.URL
}

func TestInstallStandaloneVerifiesAndLands(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the published one-file build this exercises is linux/amd64")
	}
	const version = "1.32.9"
	stubRelease(t, version, false)

	dir := t.TempDir()
	var steps []int
	if err := InstallManaged(context.Background(), dir, version, func(_ string, pct int) {
		steps = append(steps, pct)
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	root := ManagedRoot(dir)
	bin := ManagedBinary(root)
	if bin != filepath.Join(root, "gallery-dl") {
		t.Fatalf("managed binary = %q", bin)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Error("the downloaded build is not executable")
	}
	if got := BinaryVersion(context.Background(), bin); got != version {
		t.Errorf("BinaryVersion = %q, want %q", got, version)
	}
	if len(steps) < 2 {
		t.Errorf("progress reported %v, want at least two steps", steps)
	}
	if _, err := os.Stat(filepath.Join(root, "venv")); err == nil {
		t.Error("a venv was created for a platform with a published build")
	}
}

func TestInstallStandaloneRefusesAWrongChecksum(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the published one-file build this exercises is linux/amd64")
	}
	const version = "1.32.9"
	stubRelease(t, version, true)

	dir := t.TempDir()
	err := InstallManaged(context.Background(), dir, version, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("install error = %v, want a checksum refusal", err)
	}
	if ManagedBinary(ManagedRoot(dir)) != "" {
		t.Error("a refused download left a managed binary behind")
	}
}

func TestPublishedSHA256ReportsNoAsset(t *testing.T) {
	stubRelease(t, "1.32.9", false)
	if _, err := publishedSHA256(context.Background(), "1.0.0", "gallery-dl.bin"); err != errNoAsset {
		t.Errorf("missing release: err = %v, want errNoAsset", err)
	}
	if _, err := publishedSHA256(context.Background(), "1.32.9", "gallery-dl_ppc64.bin"); err != errNoAsset {
		t.Errorf("unlisted asset: err = %v, want errNoAsset", err)
	}
}

func TestEffectiveBinaryPrefersTheBundledCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode-bit check is POSIX-only")
	}
	dir := exeDirForTest(t)
	beside := filepath.Join(dir, standaloneName())
	if _, err := os.Stat(beside); err == nil {
		t.Skip("something is already there")
	}
	if err := os.WriteFile(beside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Skipf("cannot write beside the executable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(beside) })

	cfg := defaultConfigForTest()
	if got := EffectiveBinary(cfg, ""); got != beside {
		t.Errorf("EffectiveBinary = %q, want the bundled %q", got, beside)
	}
	cfg.GalleryDL.BinaryPath = "/usr/bin/gallery-dl"
	if got := EffectiveBinary(cfg, ""); got != "/usr/bin/gallery-dl" {
		t.Errorf("EffectiveBinary = %q, want the explicit path", got)
	}
}

func TestBundledBinaryIgnoresANonExecutableFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("relies on POSIX mode bits as an unprivileged user")
	}
	dir := exeDirForTest(t)
	beside := filepath.Join(dir, standaloneName())
	if _, err := os.Stat(beside); err == nil {
		t.Skip("something is already there")
	}
	if err := os.WriteFile(beside, []byte("not executable"), 0o644); err != nil {
		t.Skipf("cannot write beside the executable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(beside) })

	cfg := defaultConfigForTest()
	if got := EffectiveBinary(cfg, ""); got != cfg.GalleryDL.BinaryPath {
		t.Errorf("EffectiveBinary = %q, want the configured %q", got, cfg.GalleryDL.BinaryPath)
	}
}

func TestParseSupportedSitesUsesTheEmbeddedCopy(t *testing.T) {
	sites, err := ParseSupportedSites("")
	if err != nil {
		t.Fatalf("ParseSupportedSites: %v", err)
	}
	if len(sites) < 100 {
		t.Fatalf("embedded table has %d rows, want the full generated one", len(sites))
	}
	if got, ok := sites["danbooru"]; !ok || got.Name == "" {
		t.Errorf("danbooru row = %+v", got)
	}
}

func TestEffectiveSupportedSitesPathDefaultsToEmbedded(t *testing.T) {
	cfg := defaultConfigForTest()
	if got := EffectiveSupportedSitesPath(cfg, ""); got != "" {
		t.Errorf("EffectiveSupportedSitesPath = %q, want the embedded copy", got)
	}
	cfg.GalleryDL.SupportedSitesPath = "/somewhere/supportedsites.md"
	if got := EffectiveSupportedSitesPath(cfg, ""); got != "/somewhere/supportedsites.md" {
		t.Errorf("an operator path must win, got %q", got)
	}
}

func exeDirForTest(t *testing.T) string {
	t.Helper()
	dir := desktop.ExeDir()
	if dir == "" {
		t.Skip("cannot locate the test executable")
	}
	return dir
}

func defaultConfigForTest() *config.Config { return config.Default() }
