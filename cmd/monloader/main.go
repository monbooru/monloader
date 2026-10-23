package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/pipeline"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/desktop"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/internal/sitestate"
	internalweb "github.com/monbooru/monloader/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	// Before anything prints: a Windows GUI build starts with no console.
	desktop.AttachConsole()

	if len(os.Args) >= 2 && os.Args[1] == "healthcheck" {
		runHealthcheck(os.Args[2:])
		return
	}

	configPath := flag.String("config", "./monloader.toml", "path to the monloader.toml config file")
	hashPassword := flag.String("hash-password", "", "print the bcrypt hash of the given password and exit")
	desktopMode := flag.Bool("desktop", defaultDesktop == "true", "desktop profile: OS-native paths, a log file, a browser at startup")
	noBrowser := flag.Bool("no-browser", false, "with -desktop, do not open a browser at startup")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionLine())
		return
	}
	if *hashPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*hashPassword), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error hashing password: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	// Runs after the defers below, so the new process finds the port and files released.
	restartOnExit := false
	defer func() {
		if restartOnExit {
			if err := desktop.Relaunch(); err != nil {
				logx.Errorf("restart: %v", err)
			}
		}
	}()

	var seed func(*config.Config)
	var profile internalweb.Desktop
	var layout desktop.Layout
	// Outside the desktop profile, state sits beside the config: the
	// container has one volume.
	stateDir := ""
	if *desktopMode {
		dialogOnFatal = true
		resolved, err := desktop.Resolve(appName, explicitFlag("config", *configPath))
		if err != nil {
			fatalf("resolving the desktop paths: %v", err)
		}
		layout = resolved
		*configPath = layout.ConfigPath
		profile = internalweb.Desktop{Active: true, LogDir: layout.LogDir}
		if layout.Portable {
			profile.PortableBase = layout.ConfigDir
		}
		// Before config load, so every fatal below reaches the file too.
		if f, err := desktop.OpenLog(layout.LogDir, appName); err != nil {
			logx.Warnf("could not open a log file under %s: %v", layout.LogDir, err)
		} else {
			defer func() { _ = f.Close() }()
		}
		seed = desktopSeed(layout)
		stateDir = layout.DataDir
	} else if !inContainer() {
		seed = hostSeed(*configPath)
	}
	if stateDir == "" {
		stateDir = filepath.Dir(*configPath)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		fatalf("creating the state directory %s: %v", stateDir, err)
	}

	_, statErr := os.Stat(*configPath)
	freshConfig := os.IsNotExist(statErr)

	load := config.LoadWithDefaults
	if layout.Portable {
		load = config.LoadPortable
	}
	cfg, err := load(*configPath, seed)
	if err != nil {
		fatalf("loading config: %v", err)
	}
	logx.Set(cfg.Log.Level)
	// Bound before the work root, the queue store or the ptr index is
	// touched: a launch that loses the race must leave the winner's alone.
	var ln net.Listener
	if *desktopMode {
		ln, err = desktop.Listen(appName, cfg.Server.BindAddress, bindWait)
		if errors.Is(err, desktop.ErrRunning) {
			if err := desktop.HandOver(appName, cfg.Server.BindAddress, !*noBrowser); err != nil {
				logx.Warnf("%v", err)
			}
			return
		}
		if err != nil {
			fatalf("%v", err)
		}
	} else if ln, err = net.Listen("tcp", cfg.Server.BindAddress); err != nil {
		if freshConfig && inContainer() {
			logx.Warnf("monloader wrote %s with default settings meant for the docker image.", *configPath)
			logx.Warnf("edit server.bind_address (and monbooru.api_url once monbooru runs), then run it again. docs: %s", internalweb.DocURL)
		}
		fatalf("HTTP server: %v", err)
	}
	if *desktopMode {
		ptrFolder(cfg, layout, *configPath, stateDir)
	}

	provider := config.NewProvider(cfg)

	mapper, err := mapping.New(provider, filepath.Join(filepath.Dir(*configPath), "profiles"))
	if err != nil {
		fatalf("loading mappings: %v", err)
	}

	runner := gdl.New(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites())
	managedRoot := gdl.ManagedRoot(stateDir)
	runner.SetManagedRoot(managedRoot)
	runner.SetSecrets(func() []config.Secret { return provider.Current().Secrets() })
	if err := gdl.WriteManagedConfig(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites(), mapper.SiteOptions()); err != nil {
		logx.Warnf("could not write the managed gallery-dl config: %v", err)
	}

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	catalog := gdl.ProbeCatalog(bootCtx, runner, cfg, managedRoot)
	bootCancel()
	switch {
	case catalog.Version() == "":
		// The binary actually run: a broken managed install shadows a
		// bundled one that works.
		logx.Warnf("gallery-dl not available at %q; downloads will fail until it is installed", gdl.EffectiveBinary(cfg, managedRoot))
	case catalog.Managed():
		logx.Infof("gallery-dl %s (managed install, bundled %s), %d extractors", catalog.Version(), catalog.BundledVersion(), len(catalog.Extractors()))
	default:
		logx.Infof("gallery-dl %s, %d extractors", catalog.Version(), len(catalog.Extractors()))
	}

	client := monbooru.New(provider)
	workRoot := resolveWorkRoot(*desktopMode)
	clearWorkRoot(workRoot)
	siteState := sitestate.New()

	ptrCtx, ptrCancel := context.WithCancel(context.Background())
	defer ptrCancel()
	ptrEngine := ptr.NewEngine(cfg.PTR)
	ptrEngine.Start(ptrCtx)

	sim := similarity.New(provider, mapper.CanonicalPostURL, mapper.PostURLFor)
	proc := pipeline.New(runner, mapper, client, provider, workRoot, siteState, ptrEngine, sim)

	q := queue.New(proc, cfg.Downloader.Concurrency, 100)
	q.SetRetention(cfg.Downloader.HistoryRetention())
	q.SetSuccessRetention(cfg.Downloader.SuccessRetention())
	q.SetLookupBudget(cfg.Lookup.ScheduledDailyBudget)
	if qs, err := queue.OpenStore(stateDir); err != nil {
		logx.Warnf("queue history store unavailable, falling back to in-memory only: %v", err)
	} else {
		q.UseStore(qs)
		defer func() { _ = qs.Close() }()
	}
	q.Start()
	watchCtx, stopWatches := context.WithCancel(context.Background())
	defer stopWatches()
	go q.RunWatches(watchCtx, func() bool {
		m := provider.Current().Monbooru
		return m.APIURL == "" || m.Paused
	})

	srv, err := internalweb.NewServer(provider, *configPath, stateDir, workRoot, q, client, runner, mapper, catalog, siteState, ptrEngine, sim, profile)
	if err != nil {
		fatalf("creating web server: %v", err)
	}

	sweepCtx, stopSweeps := context.WithCancel(context.Background())
	defer stopSweeps()
	go srv.SweepSessions(sweepCtx)

	httpSrv := &http.Server{
		Addr:    cfg.Server.BindAddress,
		Handler: srv.Handler(),
		// The whole request, unlike monbooru: no body here is larger than 8 MiB.
		ReadTimeout: 30 * time.Second,
		// No WriteTimeout: a ?wait=N enqueue or a push to monbooru runs
		// for many seconds.
		IdleTimeout: 120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	srvErr := make(chan error, 1)
	go func() {
		logx.Infof("monloader listening on %s (work dir %s)", cfg.Server.BindAddress, workRoot)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	if *desktopMode {
		if !*noBrowser {
			go func() {
				if err := desktop.OpenWhenServing(appName, cfg.Server.BindAddress, 30*time.Second); err != nil {
					logx.Warnf("%v", err)
				}
			}()
		}
		if srv.TrayEnabled() {
			trayCtx, stopTray := context.WithCancel(context.Background())
			defer stopTray()
			go runTray(trayCtx, srv, cfg.Server.BindAddress)
		}
	}

	select {
	case <-quit:
		logx.Infof("shutting down...")
	case <-srv.QuitRequested():
		logx.Infof("shutting down on request...")
	case err := <-srvErr:
		reportFatal("HTTP server: " + err.Error())
		exitCode = 1
	}
	signal.Stop(quit)
	// Before the drain: a ?wait parked on a queued job would hold it for
	// the whole grace, and a container stop kills at about that much.
	stopWatches()
	q.Stop()
	_ = drainHTTP(httpSrv, shutdownDrain)
	q.Close()
	ptrEngine.Disable()
	restartOnExit = srv.RestartRequested()
}

const shutdownDrain = 10 * time.Second

// Cutting off a request mid-response would report a failure for work that
// landed.
func drainHTTP(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

var workMount = "/work"

// Probed, never created: a root run outside the container would otherwise
// make /work at the filesystem root.
func resolveWorkRoot(desktopMode bool) string {
	// /work is a container convention; on a workstation it would be
	// somebody else's directory. So is a fixed name under a shared /tmp,
	// which another local user could pre-create and read or swap files in.
	if !desktopMode {
		if fi, err := os.Stat(workMount); err == nil && fi.IsDir() {
			if f, err := os.CreateTemp(workMount, ".probe"); err == nil {
				name := f.Name()
				f.Close()
				os.Remove(name)
				return workMount
			}
		}
		// The image's /tmp is its own writable layer, shared with no one.
		if inContainer() {
			return filepath.Join(os.TempDir(), "monloader-work")
		}
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "monloader", "work")
	}
	if dir, err := os.MkdirTemp("", "monloader-work-"); err == nil {
		return dir
	}
	return filepath.Join(os.TempDir(), "monloader-work")
}

// Without the queue store job ids restart at 1, so a crashed run's
// leftover job dir would be bundled into a new job. The contents go, not
// the directory: /work may be a tmpfs mount.
func clearWorkRoot(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}
