package gdl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/config"
)

func fakeManagedBinary(t *testing.T, root string) string {
	t.Helper()
	venv := filepath.Join(root, "venv")
	if err := os.MkdirAll(filepath.Dir(venvExe(venv, "python3")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(venvExe(venv, "python3"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := venvExe(venv, "gallery-dl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 9.9.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallManagedRejectsBadVersion(t *testing.T) {
	for _, v := range []string{"", "latest", "1.2.3; rm -rf /", "v1.32.9", "1..2", "--pre"} {
		dir := t.TempDir()
		if err := InstallManaged(context.Background(), dir, v, nil); err == nil {
			t.Errorf("version %q: expected an error", v)
		}
		if _, err := os.Stat(ManagedRoot(dir)); !os.IsNotExist(err) {
			t.Errorf("version %q: managed root was created", v)
		}
	}
}

func TestInstallManagedRefusesARootABundledBinaryHolds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ManagedRoot(dir), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := InstallManaged(context.Background(), dir, "1.32.9", nil)
	if err == nil || !strings.Contains(err.Error(), "bundled gallery-dl") {
		t.Errorf("err = %v, want one naming the collision", err)
	}
}

func TestAnInstallOnADeadContextKeepsTheWorkingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	state := t.TempDir()
	root := ManagedRoot(state)
	want := fakeManagedBinary(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := InstallManaged(ctx, state, "1.32.19", nil); err == nil {
		t.Fatal("expected the canceled install to fail")
	}
	if got := ManagedBinary(root); got != want {
		t.Errorf("binary = %q, want the working install kept at %q", got, want)
	}
}

func TestRemoveIfUnrunnableDropsABrokenBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	root := filepath.Join(t.TempDir(), "managed")
	p := fakeManagedBinary(t, root)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	removeIfUnrunnable(root)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("a broken install stayed: %v", err)
	}
}

func TestRunPipSurvivesALineTooLongToScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake pip is a shell script")
	}
	venv := filepath.Join(t.TempDir(), "venv")
	if err := os.MkdirAll(filepath.Dir(venvExe(venv, "pip")), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nhead -c 70000 /dev/zero | tr '\\0' x\necho\nhead -c 1000000 /dev/zero | tr '\\0' y\necho\n"
	if err := os.WriteFile(venvExe(venv, "pip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := runPip(ctx, venv, "1.32.19", func(string, int) {}); err != nil {
		t.Errorf("runPip = %v, want the run to finish past the long line", err)
	}
	if ctx.Err() != nil {
		t.Error("pip blocked on a full pipe until the deadline")
	}
}

func TestManagedBinaryDetection(t *testing.T) {
	root := t.TempDir()
	if got := ManagedBinary(root); got != "" {
		t.Fatalf("empty root: got %q", got)
	}
	if got := ManagedBinary(""); got != "" {
		t.Fatalf("blank root: got %q", got)
	}
	want := fakeManagedBinary(t, root)
	if got := ManagedBinary(root); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if err := os.Remove(venvExe(filepath.Join(root, "venv"), "python3")); err != nil {
		t.Fatal(err)
	}
	if got := ManagedBinary(root); got != "" {
		t.Errorf("venv without its interpreter: got %q, want none", got)
	}
	if !ManagedInstalled(root) {
		t.Error("expected the broken install to still count as installed")
	}
	if ManagedInstalled(t.TempDir()) || ManagedInstalled("") {
		t.Error("expected no install where no venv exists")
	}
}

func TestEffectiveBinaryPrecedence(t *testing.T) {
	root := t.TempDir()
	managed := fakeManagedBinary(t, root)

	cfg := config.Default()
	if got := EffectiveBinary(cfg, root); got != managed {
		t.Errorf("default path with managed install: got %q, want %q", got, managed)
	}
	if got := EffectiveBinary(cfg, ""); got != cfg.GalleryDL.BinaryPath {
		t.Errorf("no managed root: got %q", got)
	}
	cfg.GalleryDL.BinaryPath = "/opt/custom/gallery-dl"
	if got := EffectiveBinary(cfg, root); got != "/opt/custom/gallery-dl" {
		t.Errorf("explicit path: got %q", got)
	}

	cfg = config.Default()
	if !ManagedActive(cfg, root) {
		t.Error("expected the managed install active on a default config")
	}
	cfg.GalleryDL.BinaryPath = "/opt/custom/gallery-dl"
	if ManagedActive(cfg, root) {
		t.Error("explicit binary path must deactivate the managed install")
	}
}

func TestEffectiveSupportedSitesPath(t *testing.T) {
	root := t.TempDir()
	fakeManagedBinary(t, root)
	cfg := config.Default()

	if got := EffectiveSupportedSitesPath(cfg, root); got != cfg.GalleryDL.SupportedSitesPath {
		t.Errorf("no managed copy: got %q", got)
	}
	managedCopy := filepath.Join(root, "supportedsites.md")
	if err := os.WriteFile(managedCopy, []byte("<table></table>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := EffectiveSupportedSitesPath(cfg, root); got != managedCopy {
		t.Errorf("managed copy present: got %q, want %q", got, managedCopy)
	}
	cfg.GalleryDL.SupportedSitesPath = "/elsewhere/supportedsites.md"
	if got := EffectiveSupportedSitesPath(cfg, root); got != "/elsewhere/supportedsites.md" {
		t.Errorf("explicit path: got %q", got)
	}
}

func TestLatestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"version":"1.99.0"}}`))
	}))
	defer srv.Close()
	old := pypiJSONURL
	pypiJSONURL = srv.URL
	defer func() { pypiJSONURL = old }()

	v, err := LatestVersion(context.Background())
	if err != nil || v != "1.99.0" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestLatestVersionRejectsOddAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"version":"1.99.0; echo owned"}}`))
	}))
	defer srv.Close()
	old := pypiJSONURL
	pypiJSONURL = srv.URL
	defer func() { pypiJSONURL = old }()

	if _, err := LatestVersion(context.Background()); err == nil {
		t.Fatal("expected an error for a non-release version string")
	}
}

func TestVenvExeFollowsThePlatformLayout(t *testing.T) {
	got := venvExe(filepath.Join("root", "venv"), "gallery-dl")
	want := filepath.Join("root", "venv", "bin", "gallery-dl")
	if runtime.GOOS == "windows" {
		want = filepath.Join("root", "venv", "Scripts", "gallery-dl.exe")
	}
	if got != want {
		t.Errorf("venvExe = %q, want %q", got, want)
	}
	if runtime.GOOS == "windows" {
		if want := filepath.Join("root", "venv", "Scripts", "python.exe"); venvExe(filepath.Join("root", "venv"), "python3") != want {
			t.Errorf("interpreter = %q, want %q", venvExe(filepath.Join("root", "venv"), "python3"), want)
		}
	}
}

func TestOutputTail(t *testing.T) {
	cases := []struct {
		name, out, want string
	}{
		{"the last non-empty line", "resolving\nERROR: nope\n\n", "ERROR: nope"},
		{"nothing at all", "  \n\n", "no output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outputTail([]byte(tc.out)); got != tc.want {
				t.Errorf("outputTail = %q, want %q", got, tc.want)
			}
		})
	}

	long := outputTail([]byte(strings.Repeat("é", 400)))
	if !strings.HasSuffix(long, "...") {
		t.Errorf("a long line should be truncated, got %q", long)
	}
	if !utf8.ValidString(long) {
		t.Errorf("truncation split a rune: %q", long)
	}
}
