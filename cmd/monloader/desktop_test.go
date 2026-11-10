package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/desktop"
)

func TestDesktopSeedReplacesTheContainerPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the seed asserts POSIX home layout")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	layout, err := desktop.Resolve(appName, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cfg := config.Default()
	desktopSeed(layout)(cfg)

	if cfg.Server.BindAddress != "127.0.0.1:"+contract.PortMonloader {
		t.Errorf("bind_address = %q", cfg.Server.BindAddress)
	}
	if cfg.Monbooru.APIURL != "http://127.0.0.1:"+contract.PortMonbooru {
		t.Errorf("monbooru.api_url = %q", cfg.Monbooru.APIURL)
	}
	for name, got := range map[string]string{
		"config_path":  cfg.GalleryDL.ConfigPath,
		"archive_path": cfg.GalleryDL.ArchivePath,
		"cookies_dir":  cfg.GalleryDL.CookiesDir,
	} {
		if filepath.Dir(got) != layout.DataDir {
			t.Errorf("gallerydl.%s = %q, want it under %q", name, got, layout.DataDir)
		}
	}
	if cfg.PTR.DataPath != "" || cfg.PTR.Enabled {
		t.Errorf("ptr should start unset and off: path=%q enabled=%v", cfg.PTR.DataPath, cfg.PTR.Enabled)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want info", cfg.Log.Level)
	}
}

func TestDesktopSeedLeavesThePortableLayoutAlone(t *testing.T) {
	dir := t.TempDir()
	layout := desktop.Layout{
		ConfigPath: filepath.Join(dir, "monloader.toml"),
		ConfigDir:  dir,
		DataDir:    filepath.Join(dir, "data"),
		Portable:   true,
	}
	cfg := config.Default()
	before := cfg.GalleryDL.ConfigPath
	desktopSeed(layout)(cfg)

	if cfg.GalleryDL.ConfigPath != before {
		t.Errorf("config_path = %q, want the config package's own %q", cfg.GalleryDL.ConfigPath, before)
	}
	if cfg.Server.BindAddress != "127.0.0.1:"+contract.PortMonloader {
		t.Errorf("bind_address = %q, want loopback", cfg.Server.BindAddress)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want info", cfg.Log.Level)
	}
}

func TestHostSeedPutsEverythingBesideTheConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	hostSeed(filepath.Join(dir, "monloader.toml"))(cfg)

	for name, got := range map[string]string{
		"config_path":   cfg.GalleryDL.ConfigPath,
		"archive_path":  cfg.GalleryDL.ArchivePath,
		"cookies_dir":   cfg.GalleryDL.CookiesDir,
		"ptr.data_path": cfg.PTR.DataPath,
	} {
		if filepath.Dir(got) != dir {
			t.Errorf("%s = %q, want it beside the config in %q", name, got, dir)
		}
	}
	if cfg.Monbooru.APIURL != "http://127.0.0.1:"+contract.PortMonbooru {
		t.Errorf("monbooru.api_url = %q, want this machine", cfg.Monbooru.APIURL)
	}
	if cfg.Server.BindAddress != "127.0.0.1:"+contract.PortMonloader {
		t.Errorf("bind_address = %q, want loopback", cfg.Server.BindAddress)
	}
	if _, err := os.Stat(cfg.PTR.DataPath); !os.IsNotExist(err) {
		t.Errorf("the ptr folder should not be created: %v", err)
	}
}

func TestInContainerReadsTheImagesOwnVariable(t *testing.T) {
	t.Setenv("MONLOADER_CONTAINER", "")
	if inContainer() {
		t.Error("inContainer = true with the variable unset")
	}
	t.Setenv("MONLOADER_CONTAINER", "1")
	if !inContainer() {
		t.Error("inContainer = false with the variable set")
	}
}

func TestPTRFolderRecordsARelativePathWhenPortable(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "monloader.toml")
	if err := os.WriteFile(configPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath, filepath.Dir(configPath), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	data := filepath.Join(dir, "data")
	provider := config.NewProvider(cfg)
	provider.Persist(configPath, dir)
	ptrFolder(provider, configPath, data)

	if want := filepath.Join(data, "ptr"); provider.Current().PTR.DataPath != want {
		t.Errorf("ptr.data_path = %q, want the absolute %q", provider.Current().PTR.DataPath, want)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data_path = "data/ptr"`, `config_path = "data/gallery-dl.json"`,
		`archive_path = "data/gallery-dl-archive.sqlite"`, `cookies_dir = "data/cookies"`} {
		if !strings.Contains(string(written), want) {
			t.Errorf("the file should carry %s:\n%s", want, written)
		}
	}
}

func TestPTRFolderDefaultsUnderTheDataDir(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "monloader.toml")
	cfg := config.Default()
	cfg.PTR.DataPath = ""
	if err := config.Save(cfg, configPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data := filepath.Join(dir, "data")
	provider := config.NewProvider(cfg)
	provider.Persist(configPath, "")
	ptrFolder(provider, configPath, data)

	want := filepath.Join(data, "ptr")
	if provider.Current().PTR.DataPath != want {
		t.Errorf("ptr.data_path = %q, want %q", provider.Current().PTR.DataPath, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("the ptr folder should exist: %v", err)
	}
	saved, err := config.LoadFromFileIn(configPath, "")
	if err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
	}
	if saved.PTR.DataPath != want {
		t.Errorf("saved ptr.data_path = %q, want %q", saved.PTR.DataPath, want)
	}
}

func TestPTRFolderKeepsAConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "monloader.toml")
	cfg := config.Default()
	cfg.PTR.DataPath = filepath.Join(dir, "elsewhere")
	if err := config.Save(cfg, configPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	provider := config.NewProvider(cfg)
	provider.Persist(configPath, "")
	ptrFolder(provider, configPath, filepath.Join(dir, "data"))

	if want := filepath.Join(dir, "elsewhere"); provider.Current().PTR.DataPath != want {
		t.Errorf("ptr.data_path = %q, want %q", provider.Current().PTR.DataPath, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "ptr")); !os.IsNotExist(err) {
		t.Errorf("no folder should be created beside the data: %v", err)
	}
}

func TestWorkRootIsPerUserOnADesktop(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no user cache directory here")
	}
	if got, want := resolveWorkRoot(true), filepath.Join(cache, "monloader", "work"); got != want {
		t.Errorf("resolveWorkRoot(true) = %q, want %q", got, want)
	}
}

func TestClearWorkRootRemovesOnlyItsOwnEntries(t *testing.T) {
	root := t.TempDir()
	debug := filepath.Join(root, "debug-123")
	if err := os.MkdirAll(debug, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "job-1")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "p1.jpg"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	clearWorkRoot(root)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("the root itself was removed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "loose.tmp" {
		t.Errorf("root holds %v, want only the file this app did not make", entries)
	}
	clearWorkRoot(filepath.Join(root, "absent"))
}

func TestWorkRootDoesNotCreateTheMount(t *testing.T) {
	mount := filepath.Join(t.TempDir(), "work")
	old := workMount
	workMount = mount
	t.Cleanup(func() { workMount = old })
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no user cache directory here")
	}

	t.Setenv("MONLOADER_CONTAINER", "")
	if got, want := resolveWorkRoot(false), filepath.Join(cache, "monloader", "work"); got != want {
		t.Errorf("on a bare host with no mount, resolveWorkRoot(false) = %q, want %q", got, want)
	}
	t.Setenv("MONLOADER_CONTAINER", "1")
	if got, want := resolveWorkRoot(false), filepath.Join(os.TempDir(), "monloader-work"); got != want {
		t.Errorf("in the image with no mount, resolveWorkRoot(false) = %q, want %q", got, want)
	}
	if _, err := os.Stat(mount); !os.IsNotExist(err) {
		t.Errorf("the probe created %s", mount)
	}

	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveWorkRoot(false); got != mount {
		t.Errorf("with the mount provisioned, resolveWorkRoot(false) = %q, want %q", got, mount)
	}
	t.Setenv("MONLOADER_CONTAINER", "")
	if got, want := resolveWorkRoot(false), filepath.Join(cache, "monloader", "work"); got != want {
		t.Errorf("a bare host's writable /work is not this app's: resolveWorkRoot(false) = %q, want %q", got, want)
	}
}
