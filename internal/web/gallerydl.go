package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/shared/session"
)

// A venv plus a pip download: seconds normally, minutes on a slow line.
const gdlInstallTimeout = 3 * time.Minute

const gdlProbeTimeout = 30 * time.Second

var installManaged = gdl.InstallManaged

const gdlBusyReason = "A gallery-dl install or revert is running - wait for it to finish."

type gdlJob struct {
	version string
	step    string
	percent int
	running bool
	kind    string
	msg     string
}

type gdlPanel struct {
	ActiveVersion  string
	BundledVersion string
	Managed        bool
	// Installed is on disk whether or not runs use it, so the panel can
	// always remove it.
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
	root := gdl.ManagedRoot(s.stateDir)
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
		// Cleared once reported, so a later reload shows the version
		// line, not a stale message.
		s.gdlJob = nil
	}
	return p
}

// The cached inventory can outlive its binary, e.g. a venv whose python
// is gone. ManagedActive is only a stat, so checking costs nothing; never
// probe while an install or a revert holds the directory.
func (s *Server) reconcileGDLCatalog(ctx context.Context, root string) {
	if s.gdlBusy() || s.catalog.Managed() == gdl.ManagedActive(s.cfg.Current(), root) {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, gdlProbeTimeout)
	defer cancel()
	s.refreshGDLCatalog(probeCtx)
}

func gdlInactiveReason(root string) string {
	if gdl.ManagedBinary(root) == "" {
		return "The installed gallery-dl no longer runs - revert to remove it."
	}
	return "The installed gallery-dl is not in use - binary_path names another one."
}

func (s *Server) gdlClaim(version string) bool {
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	if s.gdlJob != nil && s.gdlJob.running {
		return false
	}
	s.gdlJob = &gdlJob{version: version, step: "starting", running: true}
	return true
}

func (s *Server) gdlBusy() bool {
	s.gdlMu.Lock()
	defer s.gdlMu.Unlock()
	return s.gdlBusyLocked()
}

func (s *Server) gdlBusyLocked() bool {
	return s.gdlSwitching || (s.gdlJob != nil && s.gdlJob.running)
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

// A job must not resolve with one gallery-dl and download with another,
// and a revert must not delete a directory an install is writing. done
// resumes the queue unless the operator had paused it, then frees the slot.
func (s *Server) gdlSwitchGuard() (done func(), reason string) {
	s.gdlMu.Lock()
	if s.gdlBusyLocked() {
		s.gdlMu.Unlock()
		return nil, gdlBusyReason
	}
	s.gdlSwitching = true
	s.gdlMu.Unlock()
	// Pause before counting, or an enqueue in between wakes a parked
	// worker into a job the count ruled out.
	wasPaused := s.queue.Paused()
	s.queue.Pause()
	done = func() {
		if !wasPaused {
			s.queue.Resume()
		}
		s.gdlMu.Lock()
		s.gdlSwitching = false
		s.gdlMu.Unlock()
	}
	if s.queueCounts().Running > 0 {
		done()
		return nil, "a download is running - wait for it to finish before switching gallery-dl"
	}
	return done, ""
}

func (s *Server) gdlInstall(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderGDLPanel(w, r, "err", "Bad form data.")
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

// The queue stays held until the catalog knows which binary answers. It is
// resumed before the job reads as finished: a switch that clears the guard
// once it does would otherwise have this one's resume land mid-switch.
func (s *Server) runGDLInstall(version string, done func()) {
	ctx, cancel := context.WithTimeout(context.Background(), gdlInstallTimeout)
	defer cancel()
	finish := func(kind, msg string) {
		done()
		s.gdlDone(kind, msg)
	}
	if err := installManaged(ctx, s.stateDir, version, s.gdlStep); err != nil {
		// A failed upgrade may have torn the old install down.
		s.refreshGDLCatalog(ctx)
		finish("err", "Install failed: "+err.Error())
		return
	}
	s.gdlStep("re-reading the extractor list", 92)
	s.refreshGDLCatalog(ctx)
	if !s.catalog.Managed() {
		finish("warn", "gallery-dl "+version+" installed, but binary_path still names the binary in use.")
		return
	}
	finish("ok", "gallery-dl "+version+" installed and active.")
}

func (s *Server) gdlStatusFragment(w http.ResponseWriter, r *http.Request) {
	s.renderGDLPanel(w, r, "", "")
}

func (s *Server) renderGDLPanel(w http.ResponseWriter, r *http.Request, kind, msg string) {
	panel := s.gdlPanel(r.Context(), s.sessions.CSRFToken(session.FromContext(r.Context())))
	if msg != "" {
		panel.MsgKind, panel.Msg = kind, msg
	}
	s.render(w, "gallerydl_panel", panel)
}

func (s *Server) gdlRevert(w http.ResponseWriter, r *http.Request) {
	if !gdl.ManagedInstalled(gdl.ManagedRoot(s.stateDir)) {
		s.redirectFlash(w, r, "err", "Already running the bundled gallery-dl.")
		return
	}
	done, reason := s.gdlSwitchGuard()
	if reason != "" {
		s.redirectFlash(w, r, "err", reason)
		return
	}
	defer done()
	if err := gdl.RevertManaged(s.stateDir); err != nil {
		s.redirectFlash(w, r, "err", "Could not remove the managed install: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gdlProbeTimeout)
	defer cancel()
	s.refreshGDLCatalog(ctx)
	s.redirectFlash(w, r, "ok", "Reverted to the bundled gallery-dl "+s.catalog.Version()+".")
}

func (s *Server) refreshGDLCatalog(ctx context.Context) {
	s.catalog.Refresh(ctx, s.runner, s.cfg.Current(), gdl.ManagedRoot(s.stateDir))
}
