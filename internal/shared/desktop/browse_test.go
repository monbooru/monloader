package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func names(dirs []Dir) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Name
	}
	return out
}

func TestListDirsListsSortedSubdirectoriesOnly(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"beta", "Alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a-file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs, truncated, err := ListDirs(root, 10)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if got := fmt.Sprint(names(dirs)); got != "[Alpha beta]" {
		t.Errorf("dirs = %s", got)
	}
	if dirs[0].Path != filepath.Join(root, "Alpha") {
		t.Errorf("path = %q", dirs[0].Path)
	}
}

func TestListDirsTruncatesTheAlphabeticalHead(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"Zebra", "a1", "a2", "a3"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dirs, truncated, _ := ListDirs(root, 3)
	if got := fmt.Sprint(names(dirs)); !truncated || got != "[a1 a2 a3]" {
		t.Errorf("dirs = %s, truncated = %v", got, truncated)
	}
}

func TestListDirsFollowsASymlinkedFolder(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(target, "nothing"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	dirs, _, _ := ListDirs(root, 10)
	if got := fmt.Sprint(names(dirs)); got != "[linked]" {
		t.Errorf("dirs = %s", got)
	}
}

func TestListDirsReportsAnUnreadableFolder(t *testing.T) {
	dirs, _, err := ListDirs(filepath.Join(t.TempDir(), "not-here"), 10)
	if err == nil || len(dirs) != 0 {
		t.Errorf("dirs = %v, err = %v", dirs, err)
	}
}

func TestBrowseNamesTheFolderAndItsParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, ok := Browse("gallery_path", root+string(filepath.Separator), 10)
	if !ok || l.Into != "gallery_path" || l.Path != root || l.Parent != filepath.Dir(root) || l.Limit != 10 {
		t.Fatalf("listing = %+v, ok %v", l, ok)
	}
	if len(l.Entries) != 1 || l.Entries[0].Name != "sub" || l.Err != "" {
		t.Errorf("entries = %+v, err %q", l.Entries, l.Err)
	}
	if missing, _ := Browse("x", filepath.Join(root, "gone"), 10); missing.Err == "" {
		t.Error("an unreadable folder carried no error")
	}
}

func TestBrowseStartsAtHomeAndRefusesABadTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if l, _ := Browse("x", "  ", 10); l.Path != home {
		t.Errorf("path = %q, want home %q", l.Path, home)
	}
	for _, into := range []string{"", `" onload="x`, "a/b"} {
		if _, ok := Browse(into, home, 10); ok {
			t.Errorf("into %q was accepted", into)
		}
	}
}
