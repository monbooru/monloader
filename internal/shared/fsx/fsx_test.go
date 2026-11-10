package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomicLeavesNothingBehindOnFailure(t *testing.T) {
	boom := errors.New("write refused")
	cases := []struct {
		name  string
		write func(*os.File) error
		want  error
	}{
		{"write fails", func(*os.File) error { return boom }, boom},
		{"close fails", func(f *os.File) error { return f.Close() }, os.ErrClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "monbooru.toml")
			if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := WriteAtomic(target, "monbooru-*.tmp", tc.write); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "original" {
				t.Errorf("target = %q, want it untouched", got)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("directory holds %d files, want only the target", len(entries))
			}
		})
	}
}

func TestWriteAtomicReplacesTheTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.toml")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteAtomic(target, "state-*.tmp", func(f *os.File) error {
		_, writeErr := f.WriteString("new")
		return writeErr
	})
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("target = %q, want new", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want only the target", len(entries))
	}
}

func TestWriteAtomicReportsAnUncreatableTemp(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here", "state.toml")
	called := false
	err := WriteAtomic(missing, "state-*.tmp", func(*os.File) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("writing into a missing directory returned no error")
	}
	if called {
		t.Error("the write callback ran with no temp file to write into")
	}
}

func TestSyncDirIgnoresAMissingDirectory(t *testing.T) {
	SyncDir(filepath.Join(t.TempDir(), "not-here"))
}

func TestWriteFileCreatesTheFolderAndReplacesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "app.toml")
	for _, body := range []string{"first", "second"} {
		if err := WriteFile(path, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != body {
			t.Fatalf("read %q, %v; want %q", got, err, body)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("the folder holds %d entries, want the file alone", len(entries))
	}
}

func TestAbsInAndRelInRoundTripInsideTheBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "install")
	if got := AbsIn("data/x", base); got != filepath.Join(base, "data", "x") {
		t.Errorf("AbsIn = %q", got)
	}
	if got := RelIn(filepath.Join(base, "data", "x"), base); got != "data/x" {
		t.Errorf("RelIn = %q", got)
	}
	outside := filepath.Join(filepath.Dir(base), "elsewhere")
	if got := RelIn(outside, base); got != outside {
		t.Errorf("RelIn moved a path outside the base: %q", got)
	}
	for _, p := range []string{"", "/abs"} {
		if AbsIn(p, base) != p || RelIn(p, "") != p {
			t.Errorf("an empty or absolute path %q changed", p)
		}
	}
}

func TestSanitizeFilenameFoldsWindowsSetOnEveryOS(t *testing.T) {
	cases := map[string]string{
		`a<b>c:d"e/f\g|h?i*j`: "a_b_c_d_e_f_g_h_i_j",
		"line\r\nbreak\x7f":   "line__break_",
		" Vol. 2. ":           "Vol. 2",
		"..":                  "",
		"日本語 タイトル":            "日本語 タイトル",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
