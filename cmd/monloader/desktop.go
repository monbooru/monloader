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
	"github.com/monbooru/monloader/internal/desktop"
	"github.com/monbooru/monloader/internal/fsx"
	"github.com/monbooru/monloader/internal/logx"
	internalweb "github.com/monbooru/monloader/internal/web"
)

// appName is the identity the desktop profile builds its directory names,
// its log filename and its single-instance answer from.
const appName = "monloader"

// defaultDesktop is stamped at link time by the desktop artifacts, so
// double-clicking one lands in the profile rather than on the container
// defaults. -desktop=false still turns it off.
var defaultDesktop string

// desktopPort is what a config created by the desktop profile binds, next
// to monbooru's 8455 so the pair is memorable.
const desktopPort = "8456"

// monbooruPort is where a desktop monbooru listens, which is what a fresh
// config points at before pairing.
const monbooruPort = "8455"

// probeTimeout bounds the single-instance question. It is a loopback
// request to a process that either answers immediately or is not there.
const probeTimeout = 500 * time.Millisecond

// desktopSeed adjusts the defaults a config file that does not exist yet is
// written with. The bind address is the one that differs in kind from the
// container's: monloader listens on 0.0.0.0 there because it has to, and on
// a desktop that would put a downloader with no password in front of the
// whole LAN. The setup wizard offers the network as the other choice.
func desktopSeed(l desktop.Layout) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Server.BindAddress = "127.0.0.1:" + desktopPort
		cfg.Server.BaseURL = "http://localhost:" + desktopPort
		cfg.Monbooru.APIURL = "http://127.0.0.1:" + monbooruPort
		// Under the config directory sits only what the operator writes -
		// the toml and the mapping profiles. Everything monloader generates
		// for itself, cookies included, belongs with the rest of its data.
		cfg.GalleryDL.ConfigPath = filepath.Join(l.DataDir, "gallery-dl.json")
		cfg.GalleryDL.ArchivePath = filepath.Join(l.DataDir, "gallery-dl-archive.sqlite")
		cfg.GalleryDL.CookiesDir = filepath.Join(l.DataDir, "cookies")
		// The container's /ptr is nowhere on a desktop; the profile settles
		// that folder at start, once the data directory is known (ptrFolder),
		// and nothing is downloaded into it until the operator asks.
		cfg.PTR.DataPath = ""
		cfg.PTR.Enabled = false
		// The log file is the only thing a desktop bug report can carry, and
		// nothing writes at the warn default while the app is healthy.
		cfg.Log.Level = "info"
	}
}

// ptrFolder settles the PTR index folder for an install that names none,
// creating it: the sync refuses a path that is not there, so an empty one
// leaves the feature unreachable. The config file gets it too, since the
// enable flag is saved against that file and a start refuses to load one
// with the sync on and no path behind it. Settings repoints it at a disk
// with more room - 70 GB rarely fits beside the rest of a user's data.
func ptrFolder(cfg *config.Config, configPath, dataDir string) {
	if cfg.PTR.DataPath != "" {
		return
	}
	dir := filepath.Join(dataDir, "ptr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warnf("could not create the ptr folder %s: %v", dir, err)
		return
	}
	persist, err := config.LoadFromFile(configPath)
	if err == nil {
		persist.PTR.DataPath = dir
		err = config.Save(persist, configPath)
	}
	if err != nil {
		logx.Warnf("could not record the ptr folder in %s: %v", configPath, err)
		return
	}
	cfg.PTR.DataPath = dir
}

// claimPort answers a second launch before anything tries to bind. It
// returns false when this process should stop, having already done
// whatever the situation called for.
func claimPort(addr string, openBrowser bool) bool {
	local := desktop.LoopbackAddr(addr)
	inst, found := desktop.Probe(local, probeTimeout)
	if !found {
		return true
	}
	if inst.App == appName {
		// stdout, not the log: a -no-browser launch from a terminal is the
		// one shape of this that has nothing else to show the operator.
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

// relaunch starts a fresh process carrying this one's arguments, for the
// Restart control. It runs after the shutdown has released the port, the
// queue store and the index, so the new process probes an empty port and
// binds it. -no-browser is added because the page that asked for the restart
// is already open and reloads itself; a second tab would be noise.
func relaunch() {
	exe, err := os.Executable()
	if err != nil {
		logx.Errorf("restart: locating this executable: %v", err)
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
	// Nothing here waits on it: this process is on its way out.
	_ = cmd.Process.Release()
}

// dialogOnFatal is set by the desktop profile: a shortcut launch on Windows
// has no console, so a startup failure has to reach a message box or it
// reaches nobody.
var dialogOnFatal bool

// fatalf reports a startup failure through the log sink - which the desktop
// profile has already pointed at a file - and exits non-zero.
func fatalf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	logx.Errorf("FATAL %s", msg)
	if dialogOnFatal {
		desktop.ShowError(appName, msg)
	}
	os.Exit(1)
}

// explicitFlag returns value only when the operator actually passed the
// flag, so the desktop profile can tell "-config was given" from the flag
// set's own default without changing what -help prints.
func explicitFlag(name, value string) string {
	passed := ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = value
		}
	})
	return passed
}

// versionLine is what -version prints: the version, plus the build stamp in
// parentheses when the artifact carries one.
func versionLine() string {
	line := appName + " " + internalweb.Version
	if label := internalweb.BuildLabel(); label != "" {
		line += " (" + label + ")"
	}
	return line
}

// openWhenServing waits for the listener to answer before opening a
// browser, so a first run that pays a cold start does not land the user on
// a connection-refused page.
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

// runTray serves the optional tray. It is never the only route to anything
// - Open, start-at-login and Quit are all in Settings - so a platform or a
// build without one is a log line, not a failure.
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

// trayIconPath is the .ico the Windows artifacts ship beside the binary.
// Elsewhere the icon comes from the desktop theme by name.
func trayIconPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir := fsx.ExeDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, appName+".ico")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
