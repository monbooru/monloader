package web

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/desktop"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/session"
)

// Every route that reaches the filesystem or the process goes through
// this. A 404, not a 403, so the endpoints do not advertise themselves
// where they are off.
func (s *Server) localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.desktopLocal(r) {
			s.notFound(w, r)
			return
		}
		h(w, r)
	}
}

// The desktop flag comes from the launch, never a request. The bind address
// is deliberately no condition: gating on it would take Restart away just
// when the wizard's network choice needs one.
func (s *Server) desktopLocal(r *http.Request) bool {
	return s.desktop && desktop.LocalRequest(r)
}

// Past this a listing is unreadable anyway; the operator types the path.
const maxBrowseEntries = 500

func (s *Server) browseDirs(w http.ResponseWriter, r *http.Request) {
	l, ok := desktop.Browse(r.URL.Query().Get("into"), r.URL.Query().Get("path"), maxBrowseEntries)
	if !ok {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	s.render(w, "webui/dir_picker", struct {
		desktop.Listing
		Route string
	}{l, "/internal/browse"})
}

// The request names a kind, never a path, so no operator string reaches
// the launcher.
var openFolderKinds = []string{"config", "logs", "cookies", "ptr"}

func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "Bad form data.")
		return
	}
	kind := r.FormValue("kind")
	if !slices.Contains(openFolderKinds, kind) {
		s.redirectFlash(w, r, "err", "Unknown folder.")
		return
	}
	dir, ours := s.desktopFolder(kind)
	if dir == "" {
		s.redirectFlash(w, r, "err", "That folder is not configured.")
		return
	}
	if ours {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.redirectFlash(w, r, "err", "Could not create "+dir+": "+err.Error())
			return
		}
	}
	if err := desktop.OpenFolder(dir); err != nil {
		logx.Warnf("open-folder %s: %v", kind, err)
		s.redirectFlash(w, r, "err", "Could not open "+dir+".")
		return
	}
	s.redirectFlash(w, r, "ok", "Opened "+dir+".")
}

// The bool is whether monloader owns the folder and may create it; the
// PTR index is the operator's.
func (s *Server) desktopFolder(kind string) (string, bool) {
	cfg := s.cfg.Current()
	switch kind {
	case "config":
		return filepath.Dir(s.configPath), true
	case "logs":
		return s.logDir, true
	case "cookies":
		return cfg.GalleryDL.CookiesDir, true
	case "ptr":
		if cfg.PTR.DataPath != "" {
			return cfg.PTR.DataPath, false
		}
	}
	return "", false
}

// appicon.png, not the logo: it is square at a size the icon theme
// declares, and its plate tells the two apps apart.
func (s *Server) DesktopHook() desktop.Hook {
	icon, _ := fs.ReadFile(s.staticFS, "appicon.png")
	return desktop.Hook{
		App:        s.desktopApp,
		Name:       s.titleName(),
		Comment:    "Downloader for monbooru",
		Categories: "Graphics;Photography;",
		Icon:       icon,
	}
}

func (s *Server) desktopIntegration() desktop.Integration {
	return s.DesktopHook().Integration(s.TrayEnabled())
}

func (s *Server) TrayEnabled() bool { return s.cfg.Current().Desktop.Tray }

func (s *Server) settingsDesktopPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "Bad form data.")
		return
	}
	failures := s.DesktopHook().Apply(r.FormValue("menu_entry") == "on", r.FormValue("start_at_login") == "on")
	if desktop.TrayAvailable() {
		tray := r.FormValue("tray") == "on"
		if err := s.updateConfig(func(c *config.Config) error {
			c.Desktop.Tray = tray
			return nil
		}); err != nil {
			failures = append(failures, "tray icon: "+err.Error())
		}
	}
	if len(failures) > 0 {
		s.redirectFlash(w, r, "err", strings.Join(failures, "; "))
		return
	}
	s.redirectFlash(w, r, "ok", "Saved.")
}

// Long enough for the response to reach the tab over loopback, short
// enough to feel immediate.
const quitDelay = 300 * time.Millisecond

func (s *Server) stopWithPage(w http.ResponseWriter, r *http.Request, verb string, act func()) {
	running, queued := s.queue.InFlight()
	inFlight := running + queued
	page := map[string]any{"App": "monloader", "Poll": verb == "restart"}
	if verb == "restart" {
		page["Heading"] = s.titleName() + " is restarting"
		page["Hint"] = "This page will reload automatically."
		if inFlight > 0 {
			page["Hint"] = fmt.Sprintf("This page will reload automatically. %d job(s) were running or waiting. The restart cancelled them; retry them from the queue once it is back.", inFlight)
		}
	} else {
		page["Heading"] = s.titleName() + " has stopped"
		page["Hint"] = "You can close this tab. Start it again from the applications menu."
		if inFlight > 0 {
			page["Hint"] = fmt.Sprintf("%d job(s) were running or waiting. Stopping cancelled them; retry them from the queue next time you start it.", inFlight)
		}
	}
	theme := s.activeTheme()
	page["Title"], page["BooruFavicon"], page["Theme"] = page["Heading"], themeFaviconURL(theme), theme.Path != ""
	s.render(w, "webui/message_page", page)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	logx.Infof("%s requested from the UI (%d running, %d queued)", verb, running, queued)
	go func() {
		time.Sleep(quitDelay)
		act()
	}()
}

func (s *Server) settingsQuit(w http.ResponseWriter, r *http.Request) {
	s.stopWithPage(w, r, "quit", s.RequestQuit)
}

func (s *Server) settingsRestart(w http.ResponseWriter, r *http.Request) {
	s.stopWithPage(w, r, "restart", s.RequestRestart)
}

// Restart carries one too: it ends the queue exactly as a stop does.
func (s *Server) desktopStopPrompt(verb, gerund string) string {
	running, queued := s.queue.InFlight()
	total := running + queued
	if total == 0 {
		return verb + " monloader?"
	}
	return fmt.Sprintf("%s monloader? %d job(s) are running or waiting; %s cancels them, and the queue keeps them for a retry.",
		verb, total, gerund)
}

func (s *Server) desktopQuitPrompt() string { return s.desktopStopPrompt("Stop", "stopping") }

func (s *Server) desktopRestartPrompt() string { return s.desktopStopPrompt("Restart", "restarting") }

func (s *Server) desktopActionsFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "desktop_actions", map[string]any{
		"CSRFToken":     s.sessions.CSRFToken(session.FromContext(r.Context())),
		"QuitPrompt":    s.desktopQuitPrompt(),
		"RestartPrompt": s.desktopRestartPrompt(),
	})
}

// config.Default is the container's service name; a desktop pair talks
// over loopback.
func (s *Server) monbooruAPIHint() string {
	if s.desktop {
		return "http://127.0.0.1:" + contract.PortMonbooru
	}
	return config.Default().Monbooru.APIURL
}

func (s *Server) availableFolders() []string {
	out := make([]string, 0, len(openFolderKinds))
	for _, kind := range openFolderKinds {
		if dir, _ := s.desktopFolder(kind); dir != "" {
			out = append(out, kind)
		}
	}
	return out
}
