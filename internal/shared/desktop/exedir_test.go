package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExeDirIsTheRunningBinarysDirectory(t *testing.T) {
	got := ExeDir()
	if got == "" {
		t.Fatal("ExeDir() is empty")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if want := filepath.Dir(exe); got != want {
		t.Errorf("ExeDir() = %q, want %q", got, want)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Errorf("ExeDir() = %q, which is not a directory (%v)", got, err)
	}
}

func TestIsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want bool
	}{
		"a regular file": {file, true},
		"a directory":    {dir, false},
		"nothing there":  {filepath.Join(dir, "gone"), false},
	} {
		if got := IsFile(tc.path); got != tc.want {
			t.Errorf("%s: IsFile(%q) = %v, want %v", name, tc.path, got, tc.want)
		}
	}
}

func TestBundledToolTakesOnlyARunnableCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode-bit check is POSIX-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the exec bit")
	}
	const name = "bundled-tool-probe"
	beside := filepath.Join(ExeDir(), name)
	if _, err := os.Stat(beside); err == nil {
		t.Skip("something is already there")
	}
	if err := os.WriteFile(beside, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Skipf("cannot write beside the executable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(beside) })
	if got := BundledTool(name); got != "" {
		t.Errorf("BundledTool took the non-executable %q", got)
	}
	if err := os.Chmod(beside, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := BundledTool(name); got != beside {
		t.Errorf("BundledTool = %q, want %q", got, beside)
	}
}

const printExeDir = "DESKTOP_PRINT_EXEDIR"

func TestMain(m *testing.M) {
	if os.Getenv(printExeDir) == "1" {
		fmt.Println(ExeDir())
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExeDirResolvesThroughASymlink(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("this platform does not name the running binary")
	}
	link := filepath.Join(t.TempDir(), "app")
	if err := os.Symlink(self, link); err != nil {
		t.Skip("symlinks unavailable here")
	}
	cmd := exec.Command(link)
	cmd.Env = append(os.Environ(), printExeDir+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running through the link: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(out)), filepath.Dir(resolved); got != want {
		t.Errorf("ExeDir through a link = %q, want %q", got, want)
	}
}
