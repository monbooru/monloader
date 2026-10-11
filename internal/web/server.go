package web

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monloader/internal/api"
	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/desktop"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/internal/sitestate"
	webFS "github.com/monbooru/monloader/web"
)

// Set at build time with -ldflags -X; DocURL is read from DOC.md.
var (
	Version = "dev"
	RepoURL = "https://github.com/monbooru/monloader"
	DocURL  = "https://monbooru.github.io/mondocs/addons/monloader/index.html"
)

// Package names the artifact kind (docker, flatpak, ...), also set with
// -ldflags -X.
var Package = "source"

func BuildLabel() string {
	if Package == "" || Package == "source" {
		return ""
	}
	return Package
}

// LogDir comes from the command, which resolves the layout before the
// config loads.
type Desktop struct {
	Active       bool
	LogDir       string
	PortableBase string
}

type Server struct {
	cfg        *config.Provider
	configPath string
	// Not derived from configPath: the desktop profile keeps state apart
	// from the config.
	stateDir string
	workRoot string
	cfgMu    sync.Mutex

	pairMu      sync.Mutex
	pairAttempt *outboundPair
	pairs       *pairStore

	queue     *queue.Queue
	client    *monbooru.Client
	runner    gdl.Runner
	mapper    *mapping.Mapper
	catalog   *gdl.Catalog
	siteState *sitestate.Tracker
	ptr       ptrEngine
	sim       simService

	gdlMu  sync.Mutex
	gdlJob *gdlJob

	statusMu          sync.Mutex
	monbooruConn      string
	monbooruVersion   string
	monbooruCheckedAt time.Time

	sessions   *SessionStore
	csrfSecret []byte
	tmpl       *template.Template
	staticFS   fs.FS

	desktop      bool
	logDir       string
	portableBase string
	// Closed so a UI stop takes the same shutdown path as SIGTERM.
	quit     chan struct{}
	quitOnce sync.Once
	restart  atomic.Bool
}

func (s *Server) QuitRequested() <-chan struct{} { return s.quit }

func (s *Server) RequestQuit() {
	s.quitOnce.Do(func() { close(s.quit) })
}

func (s *Server) RestartRequested() bool { return s.restart.Load() }

func (s *Server) RequestRestart() {
	s.restart.Store(true)
	s.RequestQuit()
}

func NewServer(cfg *config.Provider, configPath, stateDir, workRoot string, q *queue.Queue, client *monbooru.Client, runner gdl.Runner, mapper *mapping.Mapper, catalog *gdl.Catalog, siteState *sitestate.Tracker, ptrEngine *ptr.Engine, sim *similarity.Client, dk Desktop) (*Server, error) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(webFS.FS, "templates/*.html", "templates/partials/*.html")
	if err != nil {
		return nil, err
	}
	staticFS, err := fs.Sub(webFS.FS, "static")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:          cfg,
		configPath:   configPath,
		stateDir:     stateDir,
		workRoot:     workRoot,
		queue:        q,
		client:       client,
		pairs:        newPairStore(),
		runner:       runner,
		mapper:       mapper,
		catalog:      catalog,
		siteState:    siteState,
		ptr:          ptrEngine,
		sim:          sim,
		sessions:     NewSessionStore(),
		csrfSecret:   mustRandBytes(32),
		tmpl:         tmpl,
		staticFS:     staticFS,
		desktop:      dk.Active,
		portableBase: dk.PortableBase,
		logDir:       dk.LogDir,
		quit:         make(chan struct{}),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))
	mux.HandleFunc("GET /custom.css", s.serveCustomCSS)
	mux.HandleFunc("GET /custom.logo", s.serveCustomLogo)
	// Browsers request /favicon.ico whatever the page links.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.booruFaviconURL(), http.StatusMovedPermanently)
	})

	mux.HandleFunc("GET /{$}", s.addScreen)
	mux.HandleFunc("POST /{$}", s.enqueueForm)
	mux.HandleFunc("GET /queue", s.queueScreen)
	mux.HandleFunc("GET /internal/queue-rows", s.queueRows)
	mux.HandleFunc("GET /internal/queue-rows/{root}/items", s.queueRowItems)
	mux.HandleFunc("POST /queue/{id}/retry", s.retryJob)
	mux.HandleFunc("POST /queue/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST /queue/{id}/continue", s.continueJob)
	mux.HandleFunc("POST /queue/{id}/continue-all", s.continueAllJob)
	mux.HandleFunc("POST /queue/clear", s.then(s.queue.Clear, s.queueRows))
	mux.HandleFunc("POST /queue/clear-succeeded", s.then(s.queue.ClearSucceeded, s.queueRows))
	mux.HandleFunc("POST /queue/cancel-pending", s.then(s.queue.CancelPending, s.queueRows))
	mux.HandleFunc("POST /queue/pause", s.then(s.queue.Pause, s.renderPauseToggle))
	mux.HandleFunc("POST /queue/resume", s.then(s.queue.Resume, s.renderPauseToggle))
	mux.HandleFunc("DELETE /queue/{id}", s.deleteJob)
	mux.HandleFunc("POST /queue/{id}/watch", s.watchJob)
	mux.HandleFunc("GET /watches", s.watchesScreen)
	mux.HandleFunc("GET /internal/watch-rows", s.watchRows)
	mux.HandleFunc("POST /watches", s.addWatch)
	mux.HandleFunc("POST /watches/{id}/interval", s.setWatchInterval)
	mux.HandleFunc("POST /watches/{id}/items", s.setWatchItems)
	mux.HandleFunc("POST /watches/{id}/check", s.checkWatch)
	mux.HandleFunc("DELETE /watches/{id}", s.removeWatch)
	mux.HandleFunc("GET /internal/monbooru-status", s.monbooruStatus)
	mux.HandleFunc("POST /internal/monbooru/pause", s.monbooruPause)
	mux.HandleFunc("POST /internal/monbooru/resume", s.monbooruResume)
	mux.HandleFunc("GET /internal/item-debug", s.itemDebug)
	mux.HandleFunc("POST /internal/item-debug", s.itemDebug)
	mux.HandleFunc("GET /internal/queue-pause", s.renderPauseToggle)
	mux.HandleFunc("GET /internal/queue-actions", s.queueActionsFragment)

	mux.HandleFunc("GET /ptr", s.ptrScreen)
	mux.HandleFunc("GET /internal/ptr-status", s.ptrStatusFragment)
	mux.HandleFunc("POST /ptr/enable", s.ptrEnable)
	mux.HandleFunc("POST /ptr/pause", s.ptrPause)
	mux.HandleFunc("POST /ptr/resume", s.ptrResume)
	mux.HandleFunc("POST /ptr/retry", s.ptrRetry)
	mux.HandleFunc("POST /ptr/account", s.ptrAccountCreate)
	mux.HandleFunc("GET /internal/ptr-account", s.ptrAccountFragment)
	mux.HandleFunc("POST /internal/ptr-reveal-key", s.ptrRevealKey)
	mux.HandleFunc("GET /internal/ptr-contrib", s.ptrContribFragment)
	mux.HandleFunc("POST /ptr/contrib/retry", s.ptrContribRetry)
	mux.HandleFunc("POST /ptr/contrib/{id}/rescind", s.ptrContribRescindUnsent)
	mux.HandleFunc("POST /ptr/contrib/log/{id}/rescind", s.ptrContribLogRescind)

	mux.HandleFunc("GET /settings", s.settingsScreen)
	mux.HandleFunc("POST /settings/monbooru", s.saveMonbooru)
	mux.HandleFunc("POST /settings/monbooru/test", s.testMonbooru)
	mux.HandleFunc("POST /settings/downloader", s.saveDownloader)
	mux.HandleFunc("POST /settings/lookup", s.saveLookup)
	mux.HandleFunc("POST /settings/lookup/chain", s.saveLookupChain)
	mux.HandleFunc("POST /settings/lookup/test/{source}", s.testLookupSource)
	mux.HandleFunc("GET /settings/sites/search", s.searchSites)
	mux.HandleFunc("GET /settings/sites/{name}/dialog", s.siteDialog)
	mux.HandleFunc("GET /settings/sites/{name}/options", s.siteOptions)
	mux.HandleFunc("POST /settings/sites/{name}/profile/reset", s.resetSiteProfile)
	mux.HandleFunc("POST /settings/sites/{name}", s.saveSite)
	mux.HandleFunc("POST /settings/sites/{name}/reset", s.resetSite)
	mux.HandleFunc("POST /settings/sites/{name}/test", s.testSite)
	mux.HandleFunc("POST /settings/host-labels", s.saveHostLabels)
	mux.HandleFunc("POST /settings/gallerydl/install", s.gdlInstall)
	mux.HandleFunc("POST /settings/gallerydl/revert", s.gdlRevert)
	mux.HandleFunc("GET /internal/gallerydl-status", s.gdlStatusFragment)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setupPost)
	mux.HandleFunc("GET /internal/browse", s.localOnly(s.browseDirs))
	mux.HandleFunc("POST /internal/open-folder", s.localOnly(s.openFolder))
	mux.HandleFunc("GET /internal/desktop-actions", s.localOnly(s.desktopActionsFragment))
	mux.HandleFunc("POST /settings/desktop", s.localOnly(s.settingsDesktopPost))
	mux.HandleFunc("POST /settings/quit", s.localOnly(s.settingsQuit))
	mux.HandleFunc("POST /settings/restart", s.localOnly(s.settingsRestart))
	mux.HandleFunc("POST /settings/raw", s.saveRaw)
	mux.HandleFunc("POST /settings/ptr", s.savePTR)
	mux.HandleFunc("POST /settings/ptr/delete", s.ptrDelete)
	mux.HandleFunc("POST /settings/ptr/clear-key", s.ptrClearKey)

	mux.HandleFunc("POST /settings/auth/password", s.settingsPasswordPost)
	mux.HandleFunc("POST /settings/auth/remove-password", s.settingsRemovePasswordPost)
	mux.HandleFunc("POST /settings/auth/tokens", s.settingsTokenCreate)
	mux.HandleFunc("DELETE /settings/auth/tokens/{id}", s.settingsTokenRevoke)
	mux.HandleFunc("GET /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesGet)
	mux.HandleFunc("POST /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesPost)
	mux.HandleFunc("POST /settings/monbooru/pair/connect", s.monbooruPairConnect)
	mux.HandleFunc("POST /settings/monbooru/pair/poll", s.monbooruPairPoll)
	mux.HandleFunc("POST /settings/monbooru/pair/cancel", s.monbooruPairCancel)
	mux.HandleFunc("POST /settings/monbooru/pair/remove", s.monbooruPairRemove)
	mux.HandleFunc("GET /internal/monsender-pairing", s.monsenderPairingFragment)
	mux.HandleFunc("POST /settings/auth/pair/{id}/approve", s.monsenderPairApprove)
	mux.HandleFunc("POST /settings/auth/pair/{id}/deny", s.monsenderPairDeny)
	mux.HandleFunc("POST /settings/auth/pair/remove", s.monsenderPairRemove)

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("POST /logout", s.logoutPost)

	mux.HandleFunc("GET /", s.notFound)

	host := api.Host{
		Pair:       api.PairHandlers{Request: s.extPairRequest, Status: s.extPairStatus, Teardown: s.extPairTeardown},
		SaveConfig: s.updateConfig,
		Queryable:  s.lookupQueryable,
	}
	api.New(s.queue, s.runner, s.mapper, s.cfg, s.catalog, Version, s.siteState, s.ptr, host).Mount(mux)

	var h http.Handler = mux
	h = s.CSRFMiddleware(h)
	h = s.SetupMiddleware(h)
	h = s.SessionMiddleware(h)
	h = s.hostMiddleware(h)
	h = loggingMiddleware(h)
	return h
}

// On a loopback bind the only callers are this machine's, and they name it
// by a loopback address. A page rebound to 127.0.0.1 under its own name
// sends that name instead, and would otherwise reach every route a session
// with no password can.
func (s *Server) hostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Current()
		if desktop.IsLoopbackAddr(cfg.Server.BindAddress) && !desktop.IsLoopbackAddr(r.Host) && !sameHost(r.Host, cfg.Server.BaseURL) {
			http.Error(w, "monloader answers only to a loopback address or its base_url host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(host, baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return strings.EqualFold(strings.Trim(h, "[]"), u.Hostname())
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				key, _ := pairs[i].(string)
				m[key] = pairs[i+1]
			}
			return m
		},
		"browseURL":   browseURL,
		"humanBytes":  humanBytes,
		"humanSince":  humanSince,
		"humanDue":    humanDue,
		"humanAgo":    humanAgo,
		"humanDate":   humanDate,
		"stampLocal":  stampLocal,
		"join":        strings.Join,
		"itemCap":     func() int { return maxQueueItems },
		"moreSummary": moreSummary,
		"itemView":    itemView,
		"clientMeta":  clientMeta,
		"docPage":     docPage,
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
	}
}

// hx-get is not a URL attribute to html/template, which only HTML-escapes
// it: "R&D" or "a+b" would reach the handler mangled.
func browseURL(into, path string) string {
	return "/internal/browse?" + url.Values{"into": {into}, "path": {path}}.Encode()
}

// Derived from DocURL so a build-time override moves the deep links too.
func docPage(page string) string {
	return strings.TrimSuffix(DocURL, "index.html") + page
}

// Every word moreBucket can return must be listed here, or its count is
// dropped.
var moreOrder = []string{"downloading", "created", "duplicate", "enriched",
	"replaced", "skipped", "failed", "canceled"}

func moreBucket(it queue.Item) string {
	switch {
	case it.ErrorCode == outcome.ErrCodeCanceled:
		return "canceled"
	case it.Outcome == outcome.Created:
		return "created"
	case it.Outcome == outcome.Duplicate:
		return "duplicate"
	case it.Outcome == outcome.Enriched:
		return "enriched"
	case it.Outcome == outcome.Replaced:
		return "replaced"
	case it.Outcome == outcome.SkippedArchive, it.Outcome == outcome.SkippedUnsupported:
		return "skipped"
	case it.Outcome == outcome.Failed:
		return "failed"
	default:
		return "downloading"
	}
}

func moreSummary(items []queue.Item) string {
	n := map[string]int{}
	for _, it := range items {
		n[moreBucket(it)]++
	}
	parts := make([]string, 0, len(moreOrder))
	for _, label := range moreOrder {
		if n[label] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[label], label))
		}
	}
	return strings.Join(parts, ", ")
}

const maxQueueItems = 20

const pageSize = 20

func humanSince(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

func stampLocal(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02 15:04 MST")
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/queue-rows" || r.URL.Path == "/internal/queue-actions" || r.URL.Path == "/internal/monbooru-status" || r.URL.Path == "/internal/queue-pause" || r.URL.Path == "/internal/ptr-status" || r.URL.Path == "/health" {
			logx.Debugf("%s %s", r.Method, r.URL.Path)
		} else {
			logx.Infof("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) base(r *http.Request, nav, title string) map[string]any {
	conn, connVer := s.monbooruStatusSeed()
	return map[string]any{
		"Title":              title,
		"ActiveNav":          nav,
		"CSRFToken":          s.csrfToken(sessionFromContext(r.Context())),
		"AuthEnabled":        s.cfg.Current().Auth.EnablePassword,
		"Version":            Version,
		"Build":              BuildLabel(),
		"GalleryDLVersion":   s.catalog.Version(),
		"RepoURL":            RepoURL,
		"DocURL":             DocURL,
		"CustomCSS":          s.cfg.Current().Server.CustomCSS != "",
		"BooruName":          s.booruName(),
		"BooruLogo":          s.booruLogoURL(),
		"BooruFavicon":       s.booruFaviconURL(),
		"Conn":               conn,
		"MonbooruVersion":    connVer,
		"Paused":             s.queue.Paused(),
		"MonbooruPaired":     s.hasPairedToken("monbooru"),
		"MonbooruConfigured": s.monbooruConfigured(),
		"MonbooruWebURL":     s.monbooruWebLink(),
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		logx.Errorf("template %q: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteTo(w)
}

func serveConfigured(w http.ResponseWriter, r *http.Request, path string) {
	if path == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

func (s *Server) serveCustomCSS(w http.ResponseWriter, r *http.Request) {
	serveConfigured(w, r, s.cfg.Current().Server.CustomCSS)
}

func (s *Server) serveCustomLogo(w http.ResponseWriter, r *http.Request) {
	serveConfigured(w, r, s.cfg.Current().Server.BooruLogo)
}

func (s *Server) booruName() string {
	return cmp.Or(s.cfg.Current().Server.BooruName, "monloader")
}

// The stock name is capitalized only in the browser tab; a configured
// name is verbatim.
func (s *Server) titleName() string {
	return cmp.Or(s.cfg.Current().Server.BooruName, "Monloader")
}

func (s *Server) configuredAsset(fallback string) string {
	if s.cfg.Current().Server.BooruLogo != "" {
		return "/custom.logo"
	}
	return fallback
}

func (s *Server) booruLogoURL() string { return s.configuredAsset("/static/logo.png") }

func (s *Server) booruFaviconURL() string { return s.configuredAsset("/static/favicon.png") }

// fn runs twice, on a copy of the runtime config and on the file layer, so it
// must be idempotent. The file layer is reloaded without MONLOADER_*
// overrides, so an env value is never baked into monloader.toml.
func (s *Server) updateConfig(fn func(*config.Config) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	next := s.cfg.Current().Clone()
	if err := fn(next); err != nil {
		return err
	}
	persist, err := config.LoadFromFileIn(s.configPath, s.portableBase)
	if err != nil {
		return err
	}
	if err := fn(persist); err != nil {
		return err
	}
	// The file keeps paths under a portable folder relative, so the
	// folder can move.
	config.RelativizeIn(persist, s.portableBase)
	if err := config.Save(persist, s.configPath); err != nil {
		return err
	}
	s.cfg.Store(next)
	return nil
}

func (s *Server) rewriteGDLConfig() {
	if err := gdl.WriteManagedConfig(s.cfg.Current(), s.mapper.FlatTagSites(), s.mapper.MetadataSites(), s.mapper.NotesSites(), s.mapper.SiteOptions()); err != nil {
		logx.Warnf("rewriting managed gallery-dl config: %v", err)
	}
}

func (s *Server) monbooruConfigured() bool {
	return s.cfg.Current().Monbooru.APIURL != ""
}

// unpaired and rejected stay apart from each other and from down: a first
// run must not read as a refused token, nor a refusal as an outage.
func (s *Server) checkMonbooru(ctx context.Context) (status, version string) {
	if !s.monbooruConfigured() {
		return "unconfigured", ""
	}
	if s.cfg.Current().Monbooru.APIToken == "" {
		return "unpaired", ""
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	version, err := s.client.TestConnection(cctx)
	if err == nil {
		return "ok", version
	}
	var ce *outcome.CodedError
	if errors.As(err, &ce) && ce.Code == outcome.ErrCodeMonbooruRejected {
		return "rejected", ""
	}
	return "down", ""
}

// Under the light's 15s poll, so an open page still refreshes on schedule.
const monbooruStatusTTL = 10 * time.Second

func (s *Server) monbooruPaused() bool {
	return s.cfg.Current().Monbooru.Paused
}

func (s *Server) monbooruStatusSeed() (status, version string) {
	if !s.monbooruConfigured() {
		return "unconfigured", ""
	}
	if s.monbooruPaused() {
		return "paused", ""
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.monbooruConn == "" {
		return "checking", ""
	}
	return s.monbooruConn, s.monbooruVersion
}

// The probe runs unlocked so a slow monbooru never serializes page renders.
func (s *Server) monbooruStatusCached(ctx context.Context) (status, version string) {
	// A paused link is never probed: that is the kill switch.
	if s.monbooruPaused() {
		return "paused", ""
	}
	s.statusMu.Lock()
	if s.monbooruConn != "" && time.Since(s.monbooruCheckedAt) < monbooruStatusTTL {
		status, version = s.monbooruConn, s.monbooruVersion
		s.statusMu.Unlock()
		return status, version
	}
	s.statusMu.Unlock()

	status, version = s.checkMonbooru(ctx)

	s.statusMu.Lock()
	s.monbooruConn, s.monbooruVersion, s.monbooruCheckedAt = status, version, time.Now()
	s.statusMu.Unlock()
	return status, version
}
