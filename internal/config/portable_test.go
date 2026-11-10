package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func portableDir(t *testing.T, marker string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "monloader.toml")
	if err := os.WriteFile(path, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestPortableEmptyMarkerSeedsBesideTheProgram(t *testing.T) {
	dir, path := portableDir(t, "\n  \n")

	cfg, err := Load(path, filepath.Dir(path), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "data", "gallery-dl.json"); cfg.GalleryDL.ConfigPath != want {
		t.Errorf("config_path = %q, want %q", cfg.GalleryDL.ConfigPath, want)
	}
	if want := filepath.Join(dir, "data", "cookies"); cfg.GalleryDL.CookiesDir != want {
		t.Errorf("cookies_dir = %q, want %q", cfg.GalleryDL.CookiesDir, want)
	}
	if cfg.PTR.DataPath != "" {
		t.Errorf("ptr.data_path = %q, want it unset", cfg.PTR.DataPath)
	}
	if cfg.SetupDone {
		t.Error("SetupDone = true, want false on a seeded marker")
	}
	if cfg.GalleryDL.BinaryPath != "gallery-dl" {
		t.Errorf("binary_path = %q, want it left alone", cfg.GalleryDL.BinaryPath)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `config_path = "data/gallery-dl.json"`) {
		t.Errorf("seeded config should carry a relative path:\n%s", written)
	}
}

func TestPortableFileTakesTheLayoutForWhatItOmits(t *testing.T) {
	dir, path := portableDir(t, "[server]\nbind_address = \"127.0.0.1:9100\"\n")

	cfg, err := Load(path, filepath.Dir(path), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.BindAddress != "127.0.0.1:9100" {
		t.Errorf("bind_address = %q, want the one the file names", cfg.Server.BindAddress)
	}
	if want := filepath.Join(dir, "data", "gallery-dl-archive.sqlite"); cfg.GalleryDL.ArchivePath != want {
		t.Errorf("archive_path = %q, want %q (not the container volume)", cfg.GalleryDL.ArchivePath, want)
	}
	if !cfg.SetupDone {
		t.Error("SetupDone = false, want true for a config already on disk")
	}
}

func TestPortableResolvesTheRelativePathsItNames(t *testing.T) {
	dir, path := portableDir(t, `
[gallerydl]
config_path = "state/gdl.json"
cookies_dir = "state/cookies"

[ptr]
data_path = "state/ptr"
`)

	cfg, err := Load(path, filepath.Dir(path), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "state", "gdl.json"); cfg.GalleryDL.ConfigPath != want {
		t.Errorf("config_path = %q, want %q", cfg.GalleryDL.ConfigPath, want)
	}
	if want := filepath.Join(dir, "state", "ptr"); cfg.PTR.DataPath != want {
		t.Errorf("ptr.data_path = %q, want %q", cfg.PTR.DataPath, want)
	}
}

func TestPortableSaveKeepsTheStoredPathsAsWritten(t *testing.T) {
	dir, path := portableDir(t, "")
	if _, err := Load(path, filepath.Dir(path), nil); err != nil {
		t.Fatalf("Load: %v", err)
	}

	persist, err := LoadFromFileIn(path, dir)
	if err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
	}
	persist.Downloader.Concurrency = 2
	RelativizeIn(persist, dir)
	mustSave(t, persist, path)
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `config_path = "data/gallery-dl.json"`) {
		t.Errorf("a save absolutized the layout:\n%s", written)
	}
}

func TestPortablePersistKeepsTheLayoutForWhatTheFileOmits(t *testing.T) {
	dir, path := portableDir(t, "[server]\nbind_address = \"127.0.0.1:9100\"\n")

	persist, err := LoadFromFileIn(path, dir)
	if err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
	}
	if want := filepath.Join(dir, "data", "cookies"); persist.GalleryDL.CookiesDir != want {
		t.Errorf("cookies_dir = %q, want %q", persist.GalleryDL.CookiesDir, want)
	}
	persist.Downloader.Concurrency = 2
	RelativizeIn(persist, dir)
	mustSave(t, persist, path)
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "/config") {
		t.Errorf("a save wrote the container volume into a portable config:\n%s", written)
	}
	if !strings.Contains(string(written), `cookies_dir = "data/cookies"`) {
		t.Errorf("a save should record the layout relative:\n%s", written)
	}
}

func TestEmptyConfigKeepsTheDefaultsWithoutPortable(t *testing.T) {
	_, path := portableDir(t, "\n")
	cfg, err := Load(path, "", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GalleryDL.ConfigPath != "/config/gallery-dl.json" {
		t.Errorf("config_path = %q, want the container volume", cfg.GalleryDL.ConfigPath)
	}
}

func TestPortableSiteCookiesFollowTheFolder(t *testing.T) {
	dir, path := portableDir(t, "")
	persist, err := LoadFromFileIn(path, dir)
	if err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
	}
	persist.Sites = []Site{
		{Name: "pixiv", Cookies: filepath.Join(dir, "data", "cookies", "pixiv.txt")},
		{Name: "other", Cookies: "/elsewhere/other.txt"},
	}
	RelativizeIn(persist, dir)
	mustSave(t, persist, path)

	moved := filepath.Join(filepath.Dir(dir), "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(moved, filepath.Base(path)), moved, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(moved, "data", "cookies", "pixiv.txt"); cfg.FindSite("pixiv").Cookies != want {
		t.Errorf("cookies = %q, want %q", cfg.FindSite("pixiv").Cookies, want)
	}
	if got := cfg.FindSite("other").Cookies; got != "/elsewhere/other.txt" {
		t.Errorf("a path outside the folder = %q, want it left", got)
	}
}
