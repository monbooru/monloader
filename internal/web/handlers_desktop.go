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

// desktopLocal gates the controls that reach the machine itself rather than
// the queue: the directory picker, the folder opener and Quit. The profile
// keeps a server install out entirely - it is a launch flag, not something a
// request can set - and the remote address keeps a LAN viewer out. The bind
// address is deliberately not a condition: the wizard offers the network as
// a choice, and gating on it would take Restart away from the operator at
// the moment the choice needs one.
//
// A refusal is a 404 rather than a 403 so the endpoints do not advertise
// themselves where they are switched off.
func (s *Server) desktopLocal(r *http.Request) bool {
	if !s.desktop {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// maxBrowseEntries bounds one listing. A directory holding tens of
// thousands of subfolders is a page nobody can read and a render nobody
// waits for; the operator types the path in that case.
const maxBrowseEntries = 500

// browseIntoRe gates the id the chosen path is written back into. It ends
// up in an attribute the picker's script reads, so it stays an identifier.
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

// browseDirs lists the subdirectories of path so a browser can hand the
// server a filesystem location, which it otherwise cannot do. Directories
// only, never files; an unreadable entry is skipped rather than failing the
// whole listing.
func (s *Server) browseDirs(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
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
		// Stat rather than trusting the entry type so a symlinked folder
		// still shows up as one.
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
	// ReadDir already sorts by byte order, which puts every capitalised
	// folder above every lowercase one; a picker reads better folded.
	slices.SortFunc(data.Entries, func(a, b browseEntry) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	s.render(w, "dir_picker", data)
}

// openFolderKinds are the folders the opener will show. The request names a
// kind, never a path: every folder worth opening is one the server can name
// itself, so putting an operator string into a launcher argument buys
// nothing.
var openFolderKinds = []string{"config", "logs", "cookies", "ptr"}

// openFolder shows one of monloader's own directories in the desktop's file
// manager, creating it when it is one monloader owns.
func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
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

// desktopFolder maps a kind to a directory and reports whether monloader
// owns it, which is what decides between creating it and only opening it.
// The PTR index is the operator's disk, not ours.
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

// DesktopHook describes this install to the platform's integration points,
// for the Settings controls and for the command, which builds the tray menu
// from the same identity. The icon is appicon.png, the mascot on monloader's
// accent plate: it is square at a size the icon theme declares, and the plate
// is what tells the two apps apart in a launcher.
func (s *Server) DesktopHook() desktop.Hook {
	icon, _ := fs.ReadFile(s.staticFS, "appicon.png")
	return desktop.Hook{
		App:     "monloader",
		Name:    s.titleName(),
		Comment: "Downloader for monbooru",
		Icon:    icon,
	}
}

// desktopIntegration is what the Settings controls render from, read back
// from disk on every load so a file removed by hand shows as off.
type desktopIntegration struct {
	MenuSupported      bool
	MenuEnabled        bool
	AutostartSupported bool
	AutostartEnabled   bool
	TraySupported      bool
	TrayEnabled        bool
}

// Supported reports whether either switch can be written here, which is
// what decides whether a section offering them is worth rendering at all.
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

// TrayEnabled is the tray switch, read by the command at startup and by the
// Settings render.
func (s *Server) TrayEnabled() bool { return s.cfg.Current().Desktop.Tray }

// settingsDesktopPost applies the integration switches. The menu entry and
// start-at-login are files (or, on Windows, a registry value) rather than
// config keys, so the form posts what it wants and the answer is re-read
// from disk.
func (s *Server) settingsDesktopPost(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
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

// quitDelay lets the response reach the browser before the listener goes
// away. Long enough for a loopback write, short enough that the click feels
// like it did something.
const quitDelay = 300 * time.Millisecond

// stopWithPage renders page, flushes it to the browser, and runs act once the
// response has landed. Both stop controls take this path: the page has to
// reach the tab before the listener goes away.
func (s *Server) stopWithPage(w http.ResponseWriter, r *http.Request, page, verb string, act func()) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
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

// settingsQuit stops the process from the UI. monloader is the half that
// genuinely wants to keep running - the queue holds retries and a daily
// budget - so the page says what the stop ends rather than stopping silently.
func (s *Server) settingsQuit(w http.ResponseWriter, r *http.Request) {
	s.stopWithPage(w, r, "stopped", "quit", s.RequestQuit)
}

// settingsRestart stops the process and has the command start it again, for
// the settings that only take effect at boot - the bind address, the tray,
// the PTR data path.
func (s *Server) settingsRestart(w http.ResponseWriter, r *http.Request) {
	s.stopWithPage(w, r, "restarting", "restart", s.RequestRestart)
}

// desktopStopPrompt is the confirmation text a stop control carries: what
// ending the queue now costs, in the words the queue can answer for. Both
// controls carry one - a restart ends the queue exactly as a stop does, and
// its name says nothing about that.
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

// desktopActionsFragment re-renders the two stop controls for their poll: the
// prompts count the queue, and a count rendered with the page is stale by the
// time the operator reaches for the button.
func (s *Server) desktopActionsFragment(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	s.render(w, "desktop_actions", map[string]any{
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"QuitPrompt":    s.desktopQuitPrompt(),
		"RestartPrompt": s.desktopRestartPrompt(),
	})
}

// monbooruAPIHint is the address a config created here would carry. The
// container reaches monbooru by service name, a desktop pair over loopback,
// so config.Default answers for one deployment only.
func (s *Server) monbooruAPIHint() string {
	if s.desktop {
		return "http://127.0.0.1:8455"
	}
	return config.Default().Monbooru.APIURL
}

// availableFolders drops the kinds this install has nowhere to point at,
// so the row never offers a button that can only refuse.
func (s *Server) availableFolders() []string {
	out := make([]string, 0, len(openFolderKinds))
	for _, kind := range openFolderKinds {
		if dir, _ := s.desktopFolder(kind); dir != "" {
			out = append(out, kind)
		}
	}
	return out
}
