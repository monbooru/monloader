//go:build live

package gdl

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestInstallManagedLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live install skipped in -short")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if out, err := exec.Command("python3", "-c", "import ensurepip").CombinedOutput(); err != nil {
		t.Skipf("python3 venv module unavailable: %s", strings.TrimSpace(string(out)))
	}
	skipUnlessReachable(t, "https://codeberg.org", "https://pypi.org/simple/")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := t.TempDir()
	const version = "1.30.2"
	var steps []int
	if err := InstallManaged(ctx, dir, version, func(_ string, percent int) {
		steps = append(steps, percent)
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(steps) < 2 || !slices.IsSorted(steps) {
		t.Errorf("progress reported %v, want at least two steps that do not go backwards", steps)
	}
	bin := ManagedBinary(ManagedRoot(dir))
	if bin == "" {
		t.Fatal("no managed binary after install")
	}
	if got := BinaryVersion(ctx, bin); got != version {
		t.Fatalf("installed version %q, want %q", got, version)
	}
	if err := RevertManaged(dir); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if ManagedBinary(ManagedRoot(dir)) != "" {
		t.Fatal("managed binary still present after revert")
	}
}

func TestInstallNightlyLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live install skipped in -short")
	}
	if _, err := nightlyAsset(); err != nil {
		t.Skip(err)
	}
	skipUnlessReachable(t, "https://api.github.com", "https://codeberg.org")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tag, err := LatestNightly(ctx)
	if err != nil {
		t.Fatalf("latest nightly: %v", err)
	}
	dir := t.TempDir()
	if err := InstallManaged(ctx, dir, tag, nil); err != nil {
		t.Fatalf("install %s: %v", tag, err)
	}
	bin := ManagedBinary(ManagedRoot(dir))
	if bin == "" {
		t.Fatal("no managed binary after install")
	}
	if got := BinaryVersion(ctx, bin); !strings.Contains(got, tag) {
		t.Errorf("installed version %q, want the build of %s", got, tag)
	}
	if _, err := os.Stat(filepath.Join(ManagedRoot(dir), "supportedsites.md")); err != nil {
		t.Errorf("no site list beside the nightly, so its release notes no longer link a commit: %v", err)
	}
	if err := RevertManaged(dir); err != nil {
		t.Fatalf("revert: %v", err)
	}
}

func skipUnlessReachable(t *testing.T, urls ...string) {
	t.Helper()
	for _, u := range urls {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			t.Skipf("%s unreachable: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			t.Skipf("%s answered %s", u, resp.Status)
		}
	}
}
