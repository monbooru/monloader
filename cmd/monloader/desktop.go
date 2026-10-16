package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/desktop"
	"github.com/monbooru/monloader/internal/shared/logx"
	internalweb "github.com/monbooru/monloader/internal/web"
)

const appName = "monloader"

// Stamped "true" at link time (-X) by the desktop artifacts.
var defaultDesktop string

const desktopPort = "8456"

const monbooruPort = "8455"

// A loopback request to a process that answers at once or is not there.
const probeTimeout = 500 * time.Millisecond

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
	cfg.Server.BindAddress = "127.0.0.1:" + desktopPort
	cfg.Server.BaseURL = "http://localhost:" + desktopPort
	cfg.Monbooru.APIURL = "http://127.0.0.1:" + monbooruPort
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

func claimPort(addr string, openBrowser bool) bool {
	local := desktop.LoopbackAddr(addr)
	inst, found := desktop.Probe(local, probeTimeout)
	if !found {
		return true
	}
	if inst.App == appName {
		// stdout: a -no-browser launch from a terminal shows nothing else.
		fmt.Printf("%s is already running on http://%s\n", appName, local)
		if openBrowser {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
		}
		return false
	}
	other := inst.App
	if other == "" {
		other = "another program"
	}
	fatalf("%s is already serving %s; free the port or set server.bind_address to another one", other, local)
	return false
}

// Two launches can both probe an empty port; the one that lost the bind
// waits for the winner to answer, which its gallery-dl probe alone may hold
// back for 30 s.
func awaitWinner(addr string, openBrowser bool) bool {
	local := desktop.LoopbackAddr(addr)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := desktop.Probe(local, probeTimeout); found {
			return !claimPort(addr, openBrowser)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// -no-browser is added: the page that asked for the restart reloads itself.
func relaunch() {
	exe := desktop.Program()
	if exe == "" {
		logx.Errorf("restart: locating this executable")
		return
	}
	args := os.Args[1:]
	if !slices.Contains(args, "-no-browser") {
		args = append(slices.Clone(args), "-no-browser")
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		logx.Errorf("restart: %v", err)
		return
	}
	_ = cmd.Process.Release()
}

// A shortcut launch on Windows has no console, so a startup failure must
// reach a message box.
var dialogOnFatal bool

func fatalf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	logx.Errorf("FATAL %s", msg)
	if dialogOnFatal {
		desktop.ShowError(appName, msg)
	}
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

func openWhenServing(addr string) {
	local := desktop.LoopbackAddr(addr)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if inst, found := desktop.Probe(local, probeTimeout); found && inst.App == appName {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logx.Warnf("the server did not answer in time; open http://%s yourself", local)
}

func runTray(ctx context.Context, srv *internalweb.Server, bindAddr string) {
	local := desktop.LoopbackAddr(bindAddr)
	hook := srv.DesktopHook()
	err := desktop.RunTray(ctx, desktop.TrayMenu{
		Title:    hook.Name,
		IconPath: trayIconPath(),
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

// Windows only: elsewhere the tray names its icon from the desktop theme.
func trayIconPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir := desktop.ExeDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, appName+".ico")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
