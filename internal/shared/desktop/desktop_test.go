package desktop

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func unixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("XDG layout is linux-only")
	}
}

func xdgHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	return home
}

func TestResolveUsesTheOSDirectories(t *testing.T) {
	unixOnly(t)
	home := xdgHome(t)

	l, err := Resolve("monbooru", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(home, "cfg", "monbooru", "monbooru.toml"); l.ConfigPath != want {
		t.Errorf("ConfigPath = %q, want %q", l.ConfigPath, want)
	}
	if want := filepath.Join(home, "data", "monbooru"); l.DataDir != want {
		t.Errorf("DataDir = %q, want %q", l.DataDir, want)
	}
	if want := filepath.Join(l.DataDir, "logs"); l.LogDir != want {
		t.Errorf("LogDir = %q, want %q", l.LogDir, want)
	}
	if l.Portable {
		t.Error("Portable should be false when the config came from the OS directory")
	}
}

func TestResolveFallsBackToDotLocalShare(t *testing.T) {
	unixOnly(t)
	home := xdgHome(t)
	t.Setenv("XDG_DATA_HOME", "")

	l, err := Resolve("monbooru", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(home, ".local", "share", "monbooru"); l.DataDir != want {
		t.Errorf("DataDir = %q, want %q", l.DataDir, want)
	}
}

func TestResolvePrefersAnExplicitConfig(t *testing.T) {
	home := xdgHome(t)
	explicit := filepath.Join(home, "elsewhere", "monbooru.toml")

	l, err := Resolve("monbooru", explicit)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if l.ConfigPath != explicit {
		t.Errorf("ConfigPath = %q, want the explicit %q", l.ConfigPath, explicit)
	}
	if l.ConfigDir != filepath.Dir(explicit) {
		t.Errorf("ConfigDir = %q, want %q", l.ConfigDir, filepath.Dir(explicit))
	}
	if l.Portable {
		t.Error("an explicit -config is not portable mode")
	}
}

func TestResolveTakesAConfigBesideTheExecutable(t *testing.T) {
	unixOnly(t)
	xdgHome(t)
	t.Setenv("FLATPAK_ID", "")

	exeDir := ExeDir()
	if exeDir == "" {
		t.Skip("cannot locate the test executable")
	}
	beside := filepath.Join(exeDir, "monbooru.toml")
	if err := os.WriteFile(beside, []byte("# portable\n"), 0o644); err != nil {
		t.Skipf("cannot write beside the executable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(beside) })

	resolvesPortable(t, beside)
}

func TestResolveTakesAConfigBesideTheAppImage(t *testing.T) {
	unixOnly(t)
	xdgHome(t)
	t.Setenv("FLATPAK_ID", "")

	appDir := t.TempDir()
	t.Setenv("APPIMAGE", filepath.Join(appDir, "Monbooru.AppImage"))
	beside := filepath.Join(appDir, "monbooru.toml")
	if err := os.WriteFile(beside, []byte("# portable\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", beside, err)
	}

	resolvesPortable(t, beside)
}

func resolvesPortable(t *testing.T, beside string) {
	t.Helper()
	l, err := Resolve("monbooru", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !l.Portable {
		t.Fatalf("Portable = false, want true (config at %s)", beside)
	}
	if l.ConfigPath != beside {
		t.Errorf("ConfigPath = %q, want %q", l.ConfigPath, beside)
	}
	if want := filepath.Join(filepath.Dir(beside), "data"); l.DataDir != want {
		t.Errorf("DataDir = %q, want the portable %q", l.DataDir, want)
	}
}

func TestSandboxedReadsTheFlatpakEnvironment(t *testing.T) {
	t.Setenv("FLATPAK_ID", "io.github.monbooru.Monbooru")
	if !Sandboxed() {
		t.Error("Sandboxed() = false with FLATPAK_ID set")
	}
}

func TestPicturesDir(t *testing.T) {
	unixOnly(t)
	for _, tc := range []struct{ name, userDirs, want string }{
		{"HonoursUserDirs", "XDG_DESKTOP_DIR=\"$HOME/Bureau\"\nXDG_PICTURES_DIR=\"$HOME/Images\"\n", "Images"},
		{"FallsBackToPictures", "", "Pictures"},
		{"IgnoresAHomeEntry", "XDG_PICTURES_DIR=\"$HOME/\"\n", "Pictures"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := xdgHome(t)
			if tc.userDirs != "" {
				cfg := filepath.Join(home, "cfg")
				if err := os.MkdirAll(cfg, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg, "user-dirs.dirs"), []byte(tc.userDirs), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := PicturesDir(), filepath.Join(home, tc.want); got != want {
				t.Errorf("PicturesDir() = %q, want %q", got, want)
			}
		})
	}
}

func TestLoopbackAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8455":   "127.0.0.1:8455",
		"[::]:8455":      "127.0.0.1:8455",
		":8455":          "127.0.0.1:8455",
		"127.0.0.1:8455": "127.0.0.1:8455",
		"192.168.1.5:80": "192.168.1.5:80",
		"garbage":        "garbage",
	}
	for in, want := range cases {
		if got := LoopbackAddr(in); got != want {
			t.Errorf("LoopbackAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8455": true,
		"[::1]:8455":     true,
		"localhost:8455": true,
		"0.0.0.0:8455":   false,
		"192.168.1.5:80": false,
		"":               false,
	}
	for in, want := range cases {
		if got := IsLoopbackAddr(in); got != want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestOpenLogRotatesAtTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxLogBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenLog(dir, "monbooru")
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Errorf("current log not restarted: %v %v", fi, err)
	}
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != maxLogBytes {
		t.Errorf("previous generation not kept: %v %v", fi, err)
	}
}

func TestOpenLogWritesTheFileWhenStderrIsDead(t *testing.T) {
	dir := t.TempDir()
	dead, err := os.Create(filepath.Join(dir, "dead"))
	if err != nil {
		t.Fatal(err)
	}
	_ = dead.Close()
	oldStderr := os.Stderr
	os.Stderr = dead
	t.Cleanup(func() {
		os.Stderr = oldStderr
		log.SetOutput(os.Stderr)
	})

	f, err := OpenLog(dir, "monbooru")
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	log.Print("still recorded")
	data, err := os.ReadFile(filepath.Join(dir, "monbooru.log"))
	if err != nil || !strings.Contains(string(data), "still recorded") {
		t.Errorf("log line did not reach the file: %q %v", data, err)
	}
}

func TestOpenLogAppendsBelowTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.log")
	if err := os.WriteFile(path, []byte("earlier\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenLog(dir, "monbooru")
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("rotated a log that was under the cap")
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "earlier") {
		t.Errorf("existing log was truncated: %q %v", data, err)
	}
}
