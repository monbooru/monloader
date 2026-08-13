package web

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/logx"
)

// gdlInstallTimeout bounds one managed install: a venv creation plus a pip
// wheel download, seconds normally, minutes on a slow line.
const gdlInstallTimeout = 3 * time.Minute

// gdlProbeTimeout bounds a re-probe of the active binary: two short subprocess
// runs (--version and --list-extractors).
const gdlProbeTimeout = 30 * time.Second

// installManaged runs the install; swapped in tests, like gdl's pypiJSONURL.
var installManaged = gdl.InstallManaged

// gdlBusyReason answers every switch refused because one is already running.
const gdlBusyReason = "an install is running - wait for it to finish"

// gdlJob is a managed install in flight. It outlives the request that started
// it - the settings panel polls its progress instead of holding a connection
// open for the whole install - and keeps its outcome for the tick that
// reports it.
type gdlJob struct {
	version string
	step    string
	percent int
	running bool
	kind    string
	msg     string
}

// gdlPanel feeds the settings gallery-dl block and its poll.
type gdlPanel struct {
	ActiveVersion  string
	BundledVersion string
	Managed        bool
	// Installed marks an install present on disk whether or not runs use it,
	// so the panel that wrote one can always remove it; Inactive says why one
	// is there and not answering.
	Installed bool
	Inactive  string
	CSRFToken string
	Running   bool
	Version   string
	Step      string
	Percent   int
	MsgKind   string
	Msg       string
}

func (s *Server) gdlPanel(ctx context.Context, csrf string) gdlPanel {
	root := gdl.ManagedRoot(s.configDir())
	s.reconcileGDLCatalog(ctx, root)
	p := gdlPanel{
		ActiveVersion:  s.catalog.Version(),
		BundledVersion: s.catalog.BundledVersion(),
		Managed:        s.catalog.Managed(),
		Installed:      gdl.ManagedInstalled(root),
		CSRFToken:      csrf,
	}
	if p.Installed && !p.Managed {
		p.Inactive = gdlInactiveReason(root)
	}
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	if s.gdlJob == nil {
		return p
	}
	p.Running, p.Version, p.Step, p.Percent = s.gdlJob.running, s.gdlJob.version, s.gdlJob.step, s.gdlJob.percent
	p.MsgKind, p.Msg = s.gdlJob.kind, s.gdlJob.msg
	if !p.Running {
		// The outcome shows on the tick that reports it; a later reload reads
		// the version line rather than a stale message.
		s.gdlJob = nil
	}
	return p
}

func (s *Server) configDir() string { return filepath.Dir(s.configPath) }

// reconcileGDLCatalog re-probes when the cached inventory was built against a
// different binary than the one that would run now. The cache is filled at
// boot and after a switch, so a managed install that stopped answering since -
// its venv bound to a python the image no longer ships - would leave this
// panel and /health naming a binary that cannot start. Resolution is a stat,
// so noticing costs nothing and the probe only runs on the state change; it
// must not run against a directory an install is still writing.
func (s *Server) reconcileGDLCatalog(ctx context.Context, root string) {
	if s.gdlRunning() || s.catalog.Managed() == gdl.ManagedActive(s.cfg.Current(), root) {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, gdlProbeTimeout)
	defer cancel()
	s.refreshGDLCatalog(probeCtx)
}

// gdlInactiveReason says why an install sitting in /config is not the binary
// answering: an explicit binary_path outranks it, or its venv no longer runs.
func gdlInactiveReason(root string) string {
	if gdl.ManagedBinary(root) == "" {
		return "the installed gallery-dl no longer runs - revert to remove it"
	}
	return "the installed gallery-dl is not in use - binary_path names another one"
}

// gdlClaim takes the install slot, refusing a second install while one runs.
func (s *Server) gdlClaim(version string) bool {
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	if s.gdlJob != nil && s.gdlJob.running {
		return false
	}
	s.gdlJob = &gdlJob{version: version, step: "starting", running: true}
	return true
}

func (s *Server) gdlRunning() bool {
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	return s.gdlJob != nil && s.gdlJob.running
}

func (s *Server) gdlStep(step string, percent int) {
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	if s.gdlJob != nil {
		s.gdlJob.step, s.gdlJob.percent = step, percent
	}
}

func (s *Server) gdlDone(kind, msg string) {
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	if s.gdlJob != nil {
		s.gdlJob.running, s.gdlJob.kind, s.gdlJob.msg = false, kind, msg
	}
}

// gdlSwitchGuard refuses a binary switch while a job runs - one job must
// never resolve with one gallery-dl and download with another - or while
// another switch is mid-flight, since a revert deleting the directory an
// install is writing leaves neither. It holds the queue for the switch
// itself; done restores it unless the operator had paused it themselves, and
// a non-empty reason means the switch cannot happen.
func (s *Server) gdlSwitchGuard() (done func(), reason string) {
	if s.gdlRunning() {
		return nil, gdlBusyReason
	}
	// The pause comes first: a worker parked in nextJob and woken by an enqueue
	// between the two would start a job the count had already ruled out.
	wasPaused := s.queue.Paused()
	s.queue.Pause()
	done = func() {
		if !wasPaused {
			s.queue.Resume()
		}
	}
	if s.queueCounts().Running > 0 {
		done()
		return nil, "a download is running - wait for it to finish before switching gallery-dl"
	}
	return done, ""
}

// gdlInstall (POST /settings/gallerydl/install) starts a managed install of a
// gallery-dl release under /config and answers the panel. A blank version
// means the newest PyPI release.
func (s *Server) gdlInstall(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderGDLPanel(w, r, "err", "bad form")
		return
	}
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		v, err := gdl.LatestVersion(r.Context())
		if err != nil {
			s.renderGDLPanel(w, r, "err", "could not ask PyPI for the latest release: "+err.Error())
			return
		}
		version = v
	}
	if !gdl.ValidVersion(version) {
		s.renderGDLPanel(w, r, "err", version+" is not a release number like 1.32.9")
		return
	}
	done, reason := s.gdlSwitchGuard()
	if reason != "" {
		s.renderGDLPanel(w, r, "err", reason)
		return
	}
	if !s.gdlClaim(version) {
		// Lost the race to a request that cleared the same guard.
		done()
		s.renderGDLPanel(w, r, "err", gdlBusyReason)
		return
	}
	go s.runGDLInstall(version, done)
	s.renderGDLPanel(w, r, "", "")
}

// runGDLInstall installs the claimed release. It runs on past the request
// that started it, so it carries its own context; the queue stays held until
// the catalog knows which binary answers now.
func (s *Server) runGDLInstall(version string, done func()) {
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), gdlInstallTimeout)
	defer cancel()
	if err := installManaged(ctx, s.configDir(), version, s.gdlStep); err != nil {
		// A failed upgrade may have torn the managed install down; re-probe
		// so the panel and /health report what is actually active.
		s.refreshGDLCatalog(ctx)
		s.gdlDone("err", "install failed: "+err.Error())
		return
	}
	s.gdlStep("re-reading the extractor list", 92)
	s.refreshGDLCatalog(ctx)
	if !s.catalog.Managed() {
		// The version readout right above this message would contradict an
		// "active" the resolution does not back.
		s.gdlDone("warn", "gallery-dl "+version+" installed, but binary_path still names the binary in use")
		return
	}
	s.gdlDone("ok", "gallery-dl "+version+" installed and active")
}

// gdlStatusFragment is the htmx-polled panel while an install runs, so the
// progress bar advances without a reload.
func (s *Server) gdlStatusFragment(w http.ResponseWriter, r *http.Request) {
	s.renderGDLPanel(w, r, "", "")
}

// renderGDLPanel answers the panel. A message given here is one this request
// refuses with; the outcome of an install comes from the job itself.
func (s *Server) renderGDLPanel(w http.ResponseWriter, r *http.Request, kind, msg string) {
	panel := s.gdlPanel(r.Context(), s.csrfToken(sessionFromContext(r.Context())))
	if msg != "" {
		panel.MsgKind, panel.Msg = kind, msg
	}
	s.render(w, "gallerydl_panel", panel)
}

// gdlRevert (POST /settings/gallerydl/revert) removes the managed install;
// the bundled binary answers again from the next invocation.
func (s *Server) gdlRevert(w http.ResponseWriter, r *http.Request) {
	if !gdl.ManagedInstalled(gdl.ManagedRoot(s.configDir())) {
		s.redirectFlash(w, r, "err", "already running the bundled gallery-dl")
		return
	}
	done, reason := s.gdlSwitchGuard()
	if reason != "" {
		s.redirectFlash(w, r, "err", reason)
		return
	}
	defer done()
	if err := gdl.RevertManaged(s.configDir()); err != nil {
		s.redirectFlash(w, r, "err", "could not remove the managed install: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gdlProbeTimeout)
	defer cancel()
	s.refreshGDLCatalog(ctx)
	s.redirectFlash(w, r, "ok", "reverted to the bundled gallery-dl "+s.catalog.Version())
}

// refreshGDLCatalog re-probes the active binary and swaps the cached
// inventory wholesale. A probe that fails keeps the previous listing - an
// empty site search would read as "no sites supported", which is worse than a
// slightly stale one.
func (s *Server) refreshGDLCatalog(ctx context.Context) {
	cfg := s.cfg.Current()
	root := gdl.ManagedRoot(s.configDir())
	version := s.runner.Version(ctx)
	extractors, err := s.runner.ListExtractors(ctx)
	if err != nil {
		logx.Warnf("listing gallery-dl extractors: %v", err)
		extractors = s.catalog.Extractors()
	}
	supported, err := gdl.ParseSupportedSites(gdl.EffectiveSupportedSitesPath(cfg, root))
	if err != nil {
		logx.Infof("supportedsites data unavailable: %v", err)
		supported = s.catalog.Supported()
	}
	managed := gdl.ManagedActive(cfg, root)
	bundled := version
	if managed {
		bundled = gdl.BinaryVersion(ctx, cfg.GalleryDL.BinaryPath)
	}
	s.catalog.Replace(version, bundled, managed, extractors, supported)
}
