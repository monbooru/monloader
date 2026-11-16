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
	"github.com/monbooru/monloader/internal/shared/webui"
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
	kind := r.FormValue("kind")
	if !slices.Contains(openFolderKinds, kind) {
		writeInlineFlash(w, "err", "Unknown folder.")
		return
	}
	dir, ours := s.desktopFolder(kind)
	if dir == "" {
		writeInlineFlash(w, "err", "That folder is not configured.")
		return
	}
	if ours {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeInlineFlash(w, "err", "Could not create "+dir+": "+err.Error())
			return
		}
	}
	if err := desktop.OpenFolder(dir); err != nil {
		logx.Warnf("open-folder %s: %v", kind, err)
		writeInlineFlash(w, "err", "Could not open "+dir+".")
		return
	}
	writeInlineFlash(w, "ok", "Opened "+dir+".")
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
	failures := s.DesktopHook().Apply(r.FormValue("menu_entry") == "on", r.FormValue("start_at_login") == "on")
	if desktop.TrayAvailable() {
		tray := r.FormValue("tray") == "on"
		if err := s.cfg.Update(func(c *config.Config) error {
			c.Desktop.Tray = tray
			return nil
		}); err != nil {
			failures = append(failures, "tray icon: "+err.Error())
		}
	}
	if len(failures) > 0 {
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
		return
	}
	writeInlineFlash(w, "ok", "Saved.")
}

// Long enough for the response to reach the tab over loopback, short
// enough to feel immediate.
const quitDelay = 300 * time.Millisecond

func (s *Server) stopWithPage(w http.ResponseWriter, r *http.Request, verb string, act func()) {
	running, queued := s.queue.InFlight()
	inFlight := running + queued
	were, them := fmt.Sprintf("%d jobs were", inFlight), "them"
	if inFlight == 1 {
		were, them = "1 job was", "it"
	}
	page := map[string]any{"App": "monloader", "Poll": verb == "restart"}
	if verb == "restart" {
		page["Heading"] = s.titleName() + " is restarting"
		page["Hint"] = "This page will reload automatically."
		if inFlight > 0 {
			page["Hint"] = fmt.Sprintf("This page will reload automatically. %s running or waiting. The restart canceled %s; retry from the queue once it is back.", were, them)
		}
	} else {
		page["Heading"] = s.titleName() + " has stopped"
		page["Hint"] = "You can close this tab. Start it again from the applications menu."
		if inFlight > 0 {
			page["Hint"] = fmt.Sprintf("%s running or waiting. Stopping canceled %s; retry from the queue next time you start it.", were, them)
		}
	}
	theme := s.activeTheme()
	page["Title"], page["BooruFavicon"], page["Theme"] = page["Heading"], themeFaviconURL(theme), theme.Path != ""
	page["ColorScheme"] = themeScheme(theme)
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
func (s *Server) desktopStopWarning(gerund string) string {
	running, queued := s.queue.InFlight()
	switch total := running + queued; total {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("1 job is running or waiting; %s cancels it, and the queue keeps it for a retry.", gerund)
	default:
		return fmt.Sprintf("%d jobs are running or waiting; %s cancels them, and the queue keeps them for a retry.", total, gerund)
	}
}

// Polled because both warnings count the queue, and a count rendered with
// the page is wrong by the time the operator clicks.
func (s *Server) desktopPower(r *http.Request) webui.DesktopPower {
	return webui.DesktopPower{
		Name:           "monloader",
		PollURL:        "/internal/desktop-power",
		RestartWarning: s.desktopStopWarning("restarting"),
		QuitWarning:    s.desktopStopWarning("stopping"),
		CSRFToken:      s.csrf(r),
	}
}

func (s *Server) desktopPowerFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "webui/desktop_power", s.desktopPower(r))
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
