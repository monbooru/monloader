package web

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/desktop"
	"github.com/monbooru/monloader/internal/logx"
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

// The desktop flag comes from the launch, never a request, and loopback keeps
// LAN viewers out. A forwarded hop is refused: behind a same-host proxy every
// peer is loopback, and a proxy that adds none of these headers defeats it. So is a Host other than a loopback name: a page rebound
// to 127.0.0.1 under its own name sends one. The bind address is
// deliberately no condition: gating on it would take Restart away just when
// the wizard's network choice needs one.
func (s *Server) desktopLocal(r *http.Request) bool {
	if !s.desktop || !desktop.IsLoopbackAddr(r.Host) {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "Via"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Past this a listing is unreadable anyway; the operator types the path.
const maxBrowseEntries = 500

// The id lands in an attribute the picker's script reads, so it must stay
// an identifier.
var browseIntoRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type browseEntry struct {
	Name string
	Path string
}

type browseData struct {
	Into      string
	Path      string
	Parent    string
	Entries   []browseEntry
	Roots     []browseEntry
	Sandboxed bool
	Err       string
	Truncated bool
	Limit     int
}

func (s *Server) browseDirs(w http.ResponseWriter, r *http.Request) {
	into := r.URL.Query().Get("into")
	if !browseIntoRe.MatchString(into) {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	data := browseData{Into: into, Sandboxed: desktop.Sandboxed(), Limit: maxBrowseEntries}
	for _, root := range desktop.Roots() {
		data.Roots = append(data.Roots, browseEntry{Name: root, Path: root})
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	if path == "" {
		path = string(filepath.Separator)
	}
	data.Path = filepath.Clean(path)
	if parent := filepath.Dir(data.Path); parent != data.Path {
		data.Parent = parent
	}

	entries, err := os.ReadDir(data.Path)
	if err != nil {
		data.Err = "cannot read this folder"
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(data.Path, e.Name())
		// Stat, not the entry's type: a symlinked folder reports as a link.
		fi, err := os.Stat(full)
		if err != nil || !fi.IsDir() {
			continue
		}
		if len(data.Entries) == maxBrowseEntries {
			data.Truncated = true
			break
		}
		data.Entries = append(data.Entries, browseEntry{Name: e.Name(), Path: full})
	}
	// ReadDir's byte order puts every capitalised name first.
	slices.SortFunc(data.Entries, func(a, b browseEntry) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	s.render(w, "dir_picker", data)
}

// The request names a kind, never a path, so no operator string reaches
// the launcher.
var openFolderKinds = []string{"config", "logs", "cookies", "ptr"}

func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	kind := r.FormValue("kind")
	if !slices.Contains(openFolderKinds, kind) {
		s.redirectFlash(w, r, "err", "unknown folder")
		return
	}
	dir, ours := s.desktopFolder(kind)
	if dir == "" {
		s.redirectFlash(w, r, "err", "that folder is not configured")
		return
	}
	if ours {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.redirectFlash(w, r, "err", "could not create "+dir+": "+err.Error())
			return
		}
	}
	if err := desktop.OpenFolder(dir); err != nil {
		logx.Warnf("open-folder %s: %v", kind, err)
		s.redirectFlash(w, r, "err", "could not open "+dir)
		return
	}
	s.redirectFlash(w, r, "ok", "opened "+dir)
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
		App:     "monloader",
		Name:    s.titleName(),
		Comment: "Downloader for monbooru",
		Icon:    icon,
	}
}

// Read back from disk on every load, so a file removed by hand shows as off.
type desktopIntegration struct {
	MenuSupported      bool
	MenuEnabled        bool
	AutostartSupported bool
	AutostartEnabled   bool
	TraySupported      bool
	TrayEnabled        bool
}

func (d desktopIntegration) Supported() bool { return d.MenuSupported || d.AutostartSupported }

func (s *Server) desktopIntegration() desktopIntegration {
	h := s.DesktopHook()
	return desktopIntegration{
		MenuSupported:      h.MenuSupported(),
		MenuEnabled:        h.MenuEnabled(),
		AutostartSupported: h.AutostartSupported(),
		AutostartEnabled:   h.AutostartEnabled(),
		TraySupported:      desktop.TrayAvailable(),
		TrayEnabled:        s.TrayEnabled(),
	}
}

func (s *Server) TrayEnabled() bool { return s.cfg.Current().Desktop.Tray }

func (s *Server) settingsDesktopPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	h := s.DesktopHook()
	var failures []string
	if h.MenuSupported() {
		if err := h.SetMenu(r.FormValue("menu_entry") == "on"); err != nil {
			failures = append(failures, "menu entry: "+err.Error())
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(r.FormValue("start_at_login") == "on"); err != nil {
			failures = append(failures, "start at login: "+err.Error())
		}
	}
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
	s.redirectFlash(w, r, "ok", "saved")
}

// Long enough for the response to reach the tab over loopback, short
// enough to feel immediate.
const quitDelay = 300 * time.Millisecond

func (s *Server) stopWithPage(w http.ResponseWriter, r *http.Request, page, verb string, act func()) {
	running, queued := s.queue.InFlight()
	s.render(w, page, map[string]any{
		"BooruName": s.titleName(),
		"InFlight":  running + queued,
	})
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
	s.stopWithPage(w, r, "stopped", "quit", s.RequestQuit)
}

func (s *Server) settingsRestart(w http.ResponseWriter, r *http.Request) {
	s.stopWithPage(w, r, "restarting", "restart", s.RequestRestart)
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
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"QuitPrompt":    s.desktopQuitPrompt(),
		"RestartPrompt": s.desktopRestartPrompt(),
	})
}

// config.Default is the container's service name; a desktop pair talks
// over loopback.
func (s *Server) monbooruAPIHint() string {
	if s.desktop {
		return "http://127.0.0.1:8455"
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
