package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	// Embed the zoneinfo database so TZ resolves to a real location even
	// on a base image that ships no tzdata files.
	_ "time/tzdata"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/desktop"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/pipeline"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/internal/sitestate"
	internalweb "github.com/monbooru/monloader/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func main() {
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

	// Registered before every other defer, so a Restart's fresh process
	// starts only once this one has released the port, the queue store, the
	// index and the log file.
	restartOnExit := false
	defer func() {
		if restartOnExit {
			relaunch()
		}
	}()

	var seed func(*config.Config)
	var profile internalweb.Desktop
	// stateDir is where what monloader writes for itself lives - the queue
	// store and the managed gallery-dl install. The container keeps them
	// beside the config on its one volume; the desktop profile follows the
	// OS split, so the config directory holds only what the operator wrote.
	stateDir := ""
	if *desktopMode {
		dialogOnFatal = true
		layout, err := desktop.Resolve(appName, explicitFlag("config", *configPath))
		if err != nil {
			fatalf("resolving the desktop paths: %v", err)
		}
		*configPath = layout.ConfigPath
		profile = internalweb.Desktop{Active: true, LogDir: layout.LogDir}
		// Before config load, so every fatal below reaches the file too.
		if f, err := desktop.OpenLog(layout.LogDir, appName); err != nil {
			logx.Warnf("could not open a log file under %s: %v", layout.LogDir, err)
		} else {
			defer func() { _ = f.Close() }()
		}
		seed = desktopSeed(layout)
		stateDir = layout.DataDir
	}
	if stateDir == "" {
		stateDir = filepath.Dir(*configPath)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		fatalf("creating the state directory %s: %v", stateDir, err)
	}

	_, statErr := os.Stat(*configPath)
	freshConfig := os.IsNotExist(statErr)

	cfg, err := config.LoadWithDefaults(*configPath, seed)
	if err != nil {
		fatalf("loading config: %v", err)
	}
	logx.Set(cfg.Log.Level)
	if *desktopMode {
		if !claimPort(cfg.Server.BindAddress, !*noBrowser) {
			return
		}
		ptrFolder(cfg, *configPath, stateDir)
	}

	// One provider shared by every component that reads UI-mutable config, so a
	// settings save publishes a new snapshot all of them observe without a race.
	provider := config.NewProvider(cfg)

	mapper, err := mapping.New(provider, filepath.Join(filepath.Dir(*configPath), "profiles"))
	if err != nil {
		fatalf("loading mappings: %v", err)
	}

	runner := gdl.New(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites())
	managedRoot := gdl.ManagedRoot(stateDir)
	runner.SetManagedRoot(managedRoot)
	if err := gdl.WriteManagedConfig(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites(), mapper.SiteOptions()); err != nil {
		logx.Warnf("could not write the managed gallery-dl config: %v", err)
	}

	// Probe gallery-dl once at boot. A missing binary is not fatal: the UI and
	// API still run; downloads surface a clear error when attempted.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	gdlVersion := runner.Version(bootCtx)
	extractors, exErr := runner.ListExtractors(bootCtx)
	gdlManaged := gdl.ManagedActive(cfg, managedRoot)
	bundledVersion := gdlVersion
	if gdlManaged {
		bundledVersion = gdl.BinaryVersion(bootCtx, cfg.GalleryDL.BinaryPath)
	}
	bootCancel()
	switch {
	case gdlVersion == "":
		// The binary that was actually run, which is the managed install when
		// one shadows the default - naming the bundled path there would point
		// at the one binary that is fine.
		logx.Warnf("gallery-dl not available at %q; downloads will fail until it is installed", gdl.EffectiveBinary(cfg, managedRoot))
	case gdlManaged:
		logx.Infof("gallery-dl %s (managed install, bundled %s), %d extractors", gdlVersion, bundledVersion, len(extractors))
	default:
		logx.Infof("gallery-dl %s, %d extractors", gdlVersion, len(extractors))
	}
	if exErr != nil {
		logx.Warnf("listing gallery-dl extractors: %v", exErr)
	}

	// supportedsites.md seeds display names and auth kinds for sites without a
	// shipped profile - the managed install's fetched copy when one is active,
	// the bundled one otherwise; without either those just seed empty.
	supportedPath := gdl.EffectiveSupportedSitesPath(cfg, managedRoot)
	supported, supErr := gdl.ParseSupportedSites(supportedPath)
	if supErr != nil {
		logx.Infof("supportedsites data unavailable at %q: %v", supportedPath, supErr)
	}
	catalog := gdl.NewCatalog(gdlVersion, bundledVersion, gdlManaged, extractors, supported)

	client := monbooru.New(provider)
	workRoot := resolveWorkRoot(*desktopMode)
	clearWorkRoot(workRoot)
	// One tracker shared by the pipeline and the web server: both record a reach
	// (on a fetch and on a test probe), and the settings sites table reads it.
	siteState := sitestate.New()

	// The PTR thin client runs only when enabled; its context lives for the app
	// so the sync goroutine stops within the shutdown drain.
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

	srv, err := internalweb.NewServer(provider, *configPath, stateDir, q, client, runner, mapper, catalog, siteState, ptrEngine, sim, profile)
	if err != nil {
		fatalf("creating web server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:        cfg.Server.BindAddress,
		Handler:     srv.Handler(),
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is intentionally unset: a wait=N enqueue and a large
		// multipart push to monbooru can each run for many seconds.
		IdleTimeout: 120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logx.Infof("monloader listening on %s (work dir %s)", cfg.Server.BindAddress, workRoot)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			if freshConfig && !*desktopMode {
				log.Printf("monloader wrote %s with default settings meant for the docker image.", *configPath)
				log.Printf("edit server.bind_address (and monbooru.api_url once monbooru runs), then run it again. docs: %s", internalweb.DocURL)
			}
			if *desktopMode {
				// Closes the race between two launches that both probed an
				// empty port: whoever lost the bind asks again.
				claimPort(cfg.Server.BindAddress, !*noBrowser)
			}
			fatalf("HTTP server: %v", err)
		}
	}()

	if *desktopMode {
		if !*noBrowser {
			go openWhenServing(cfg.Server.BindAddress)
		}
		if srv.TrayEnabled() {
			trayCtx, stopTray := context.WithCancel(context.Background())
			defer stopTray()
			go runTray(trayCtx, srv, cfg.Server.BindAddress)
		}
	}

	select {
	case <-quit:
	case <-srv.QuitRequested():
	}
	logx.Infof("shutting down...")
	_ = drainHTTP(httpSrv, shutdownDrain)
	q.Close()           // cancels the in-flight job and waits for workers to exit
	ptrEngine.Disable() // stops the sync goroutine and closes the index
	restartOnExit = srv.RestartRequested()
}

// shutdownDrain bounds how long a stop waits on requests already in flight.
const shutdownDrain = 10 * time.Second

// drainHTTP closes the listener and lets requests already in flight finish, up
// to timeout: a `?wait=N` enqueue or a large push to monbooru runs for many
// seconds, and cutting one off mid-response would report a failure for work
// that landed. Past the timeout the remaining connections are dropped.
func drainHTTP(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

// resolveWorkRoot picks the scratch directory for gallery-dl output: the
// conventional /work mount when it is writable, else a temp dir for non
// container runs. The directory must never be a host bind in production;
// that is a deployment concern, not enforced here.
func resolveWorkRoot(desktopMode bool) string {
	const mount = "/work"
	// On a desktop the scratch belongs in the OS temp directory: /work is a
	// container convention, and a writable one on a workstation would be
	// somebody else's directory.
	if desktopMode {
		return filepath.Join(os.TempDir(), "monloader-work")
	}
	if err := os.MkdirAll(mount, 0o755); err == nil {
		if f, err := os.CreateTemp(mount, ".probe"); err == nil {
			name := f.Name()
			f.Close()
			os.Remove(name)
			return mount
		}
	}
	return filepath.Join(os.TempDir(), "monloader-work")
}

// clearWorkRoot empties the scratch directory at startup. A crashed run's job
// dir is stale whatever happens to it (its deferred cleanup never ran), and
// when the queue store is unavailable ids restart at 1, so a later job can walk
// straight into one and bundle or push its files. Contents are removed, not the
// directory itself, so a /work tmpfs mount stays intact.
func clearWorkRoot(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}
