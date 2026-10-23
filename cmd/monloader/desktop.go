package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/desktop"
	"github.com/monbooru/monloader/internal/shared/logx"
	internalweb "github.com/monbooru/monloader/internal/web"
)

const appName = "monloader"

// Stamped "true" at link time (-X) by the desktop artifacts.
var defaultDesktop string

// The winner's gallery-dl probe alone may hold its answer back for 30 s.
const bindWait = 45 * time.Second

func desktopSeed(l desktop.Layout) func(*config.Config) {
	return func(cfg *config.Config) {
		localSeed(cfg)
		// The container's /ptr means nothing on a desktop; the folder is
		// created at start.
		cfg.PTR.DataPath = ""
		cfg.PTR.Enabled = false
		// A desktop bug report carries only the log, which warn leaves empty.
		cfg.Log.Level = "info"
		// A portable install keeps the config package's relative paths:
		// OS-native ones would pin the folder to the machine it was first
		// run on.
		if l.Portable {
			return
		}
		// The config directory holds only what the operator writes;
		// cookies go with the data.
		cfg.GalleryDL.ConfigPath = filepath.Join(l.DataDir, "gallery-dl.json")
		cfg.GalleryDL.ArchivePath = filepath.Join(l.DataDir, "gallery-dl-archive.sqlite")
		cfg.GalleryDL.CookiesDir = filepath.Join(l.DataDir, "cookies")
	}
}

// Loopback outside a container: a wildcard bind would put a downloader
// with no password in front of the whole LAN.
func localSeed(cfg *config.Config) {
	cfg.Server.BindAddress = "127.0.0.1:" + contract.PortMonloader
	cfg.Server.BaseURL = "http://localhost:" + contract.PortMonloader
	cfg.Monbooru.APIURL = "http://127.0.0.1:" + contract.PortMonbooru
}

// Absolute, so a service unit's working directory cannot move the paths
// later. The PTR folder is named, not created: the sync refusing a
// missing one is what tells an operator the disk is not mounted.
func hostSeed(configPath string) func(*config.Config) {
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return func(cfg *config.Config) {
		localSeed(cfg)
		cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
		cfg.GalleryDL.ArchivePath = filepath.Join(dir, "gallery-dl-archive.sqlite")
		cfg.GalleryDL.CookiesDir = filepath.Join(dir, "cookies")
		cfg.PTR.DataPath = filepath.Join(dir, "ptr")
	}
}

// The image bakes MONLOADER_CONTAINER and nothing else sets it.
func inContainer() bool {
	return os.Getenv("MONLOADER_CONTAINER") != ""
}

// Created, not only named: the sync refuses a missing folder. Saved too:
// a start refuses a config with the sync on and no path.
func ptrFolder(cfg *config.Config, l desktop.Layout, configPath, dataDir string) {
	if cfg.PTR.DataPath != "" {
		return
	}
	dir := filepath.Join(dataDir, "ptr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warnf("could not create the ptr folder %s: %v", dir, err)
		return
	}
	// The file's copy travels with a portable folder; the running config
	// keeps the absolute path.
	base := ""
	if l.Portable {
		base = l.ConfigDir
	}
	persist, err := config.LoadFromFileIn(configPath, base)
	if err == nil {
		persist.PTR.DataPath = dir
		// The load resolved every path against base; saved as loaded, the
		// gallery-dl ones would pin the folder where it first ran.
		config.RelativizeIn(persist, base)
		err = config.Save(persist, configPath)
	}
	if err != nil {
		logx.Warnf("could not record the ptr folder in %s: %v", configPath, err)
		return
	}
	cfg.PTR.DataPath = dir
}

// A shortcut launch on Windows has no console, so a startup failure must
// reach a message box.
var dialogOnFatal bool

func reportFatal(msg string) {
	logx.Errorf("FATAL %s", msg)
	if dialogOnFatal {
		desktop.ShowError(appName, msg)
	}
}

func fatalf(format string, a ...any) {
	reportFatal(fmt.Sprintf(format, a...))
	os.Exit(1)
}

func explicitFlag(name, value string) string {
	passed := ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = value
		}
	})
	return passed
}

func versionLine() string {
	line := appName + " " + internalweb.Version
	if label := internalweb.BuildLabel(); label != "" {
		line += " (" + label + ")"
	}
	return line
}

func runTray(ctx context.Context, srv *internalweb.Server, bindAddr string) {
	local := desktop.LoopbackAddr(bindAddr)
	hook := srv.DesktopHook()
	err := desktop.RunTray(ctx, desktop.TrayMenu{
		Title:    hook.Name,
		IconPath: desktop.TrayIcon(appName),
		Open: func() {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
		},
		Quit:      srv.RequestQuit,
		Autostart: hook,
	})
	if err != nil {
		logx.Infof("tray: %v", err)
	}
}
