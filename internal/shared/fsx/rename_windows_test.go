package fsx

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRenameWaitsOutAHandleOnTheTarget(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	name, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	release := time.AfterFunc(100*time.Millisecond, func() { _ = windows.CloseHandle(h) })
	defer release.Stop()

	if err := rename(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if body, _ := os.ReadFile(dst); string(body) != src {
		t.Errorf("dst = %q, want the source's bytes", body)
	}
}
