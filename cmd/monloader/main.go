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

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/logx"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/monbooru"
	"github.com/leqwin/monloader/internal/pipeline"
	"github.com/leqwin/monloader/internal/ptr"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/similarity"
	"github.com/leqwin/monloader/internal/sitestate"
	internalweb "github.com/leqwin/monloader/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "healthcheck" {
		runHealthcheck(os.Args[2:])
		return
	}

	configPath := flag.String("config", "./monloader.toml", "path to the monloader.toml config file")
	hashPassword := flag.String("hash-password", "", "print the bcrypt hash of the given password and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("monloader", internalweb.Version)
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

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("FATAL loading config: %v", err)
	}
	logx.Set(cfg.Log.Level)

	// One provider shared by every component that reads UI-mutable config, so a
	// settings save publishes a new snapshot all of them observe without a race.
	provider := config.NewProvider(cfg)

	mapper, err := mapping.New(provider)
	if err != nil {
		log.Fatalf("FATAL loading mappings: %v", err)
	}

	runner := gdl.New(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites())
	if err := gdl.WriteManagedConfig(cfg, mapper.FlatTagSites(), mapper.MetadataSites(), mapper.NotesSites()); err != nil {
		logx.Warnf("could not write the managed gallery-dl config: %v", err)
	}

	// Probe gallery-dl once at boot. A missing binary is not fatal: the UI and
	// API still run; downloads surface a clear error when attempted.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	gdlVersion := runner.Version(bootCtx)
	extractors, exErr := runner.ListExtractors(bootCtx)
	bootCancel()
	if gdlVersion == "" {
		logx.Warnf("gallery-dl not available at %q; downloads will fail until it is installed", cfg.GalleryDL.BinaryPath)
	} else {
		logx.Infof("gallery-dl %s, %d extractors", gdlVersion, len(extractors))
	}
	if exErr != nil {
		logx.Warnf("listing gallery-dl extractors: %v", exErr)
	}

	client := monbooru.New(provider)
	workRoot := resolveWorkRoot()
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
	q.Start()

	srv, err := internalweb.NewServer(provider, *configPath, q, client, runner, mapper, extractors, gdlVersion, siteState, ptrEngine, sim)
	if err != nil {
		log.Fatalf("FATAL creating web server: %v", err)
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
			log.Fatalf("FATAL HTTP server: %v", err)
		}
	}()

	<-quit
	logx.Infof("shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	q.Close()           // cancels the in-flight job and waits for workers to exit
	ptrEngine.Disable() // stops the sync goroutine and closes the index
}

// resolveWorkRoot picks the scratch directory for gallery-dl output: the
// conventional /work mount when it is writable, else a temp dir for non
// container runs. The directory must never be a host bind in production;
// that is a deployment concern, not enforced here.
func resolveWorkRoot() string {
	const mount = "/work"
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

// clearWorkRoot empties the scratch directory at startup. A job dir orphaned by
// a crash (its deferred cleanup never ran) would otherwise be re-entered by a
// later job that reuses the same id - ids restart at 1 each run - so its stale
// files could be bundled or pushed. Contents are removed, not the directory
// itself, so a /work tmpfs mount stays intact.
func clearWorkRoot(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}
