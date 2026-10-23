package web

import (
	"cmp"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/webui"
	webFS "github.com/monbooru/monloader/web"
)

const builtinLightDir = "plugins/themes/light"

var builtinThemes = []webui.Theme{
	{Name: "dark", Builtin: true},
	{
		Name: "light", Builtin: true,
		Path:    builtinLightDir + "/theme.css",
		Logo:    builtinLightDir + "/logo.png",
		Favicon: builtinLightDir + "/favicon.png",
	},
}

func (s *Server) themesDir() string {
	if s.configPath == "" {
		return ""
	}
	dir := filepath.Join(filepath.Dir(s.configPath), "themes")
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func (s *Server) seedThemesDir() {
	logNotes(webui.SeedTheme(webFS.FS, builtinLightDir, []string{"theme.css", "logo.png", "favicon.png"}, s.themesDir(), "light"))
}

func logNotes(notes []webui.Note) {
	for _, n := range notes {
		if n.Warn {
			logx.Warnf("%s", n.Text)
		} else {
			logx.Infof("%s", n.Text)
		}
	}
}

func (s *Server) listThemes() []webui.Theme {
	themes, notes := webui.ListThemes(builtinThemes, s.themesDir())
	s.themeWarn.notesOnce(notes)
	return themes
}

func (s *Server) themeName() string {
	return strings.TrimSpace(s.cfg.Current().Server.Theme)
}

func (s *Server) activeTheme() webui.Theme {
	if s.themeName() == "" {
		return builtinThemes[0]
	}
	return s.activeThemeIn(s.listThemes())
}

func (s *Server) activeThemeIn(themes []webui.Theme) webui.Theme {
	name := s.themeName()
	t, found := webui.FindTheme(themes, name)
	if !found {
		s.themeWarn.warnOnce(name)
	}
	return t
}

type themeWarnings struct {
	mu        sync.Mutex
	last      string
	lastNotes string
}

// Every page render lists the themes, so a listing's notes are logged
// only when they differ from the last listing's.
func (t *themeWarnings) notesOnce(notes []webui.Note) {
	var key strings.Builder
	for _, n := range notes {
		key.WriteString(n.Text + "\n")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if key.String() == t.lastNotes {
		return
	}
	t.lastNotes = key.String()
	logNotes(notes)
}

func (t *themeWarnings) warnOnce(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == name {
		return
	}
	t.last = name
	logx.Warnf("server.theme %q is not an installed or built-in theme; the shipped look is used", name)
}

func (s *Server) serveThemeCSS(w http.ResponseWriter, r *http.Request) {
	t := s.activeTheme()
	s.serveThemeFile(w, r, t, t.Path)
}

func (s *Server) serveThemeLogo(w http.ResponseWriter, r *http.Request) {
	t := s.activeTheme()
	s.serveThemeFile(w, r, t, t.Logo)
}

func (s *Server) serveThemeFavicon(w http.ResponseWriter, r *http.Request) {
	t := s.activeTheme()
	s.serveThemeFile(w, r, t, t.Favicon)
}

// An archive gives every theme it unpacks one mtime, so the ETag names the theme.
func (s *Server) serveThemeFile(w http.ResponseWriter, r *http.Request, t webui.Theme, path string) {
	if path != "" && !t.Builtin {
		w.Header().Set("Cache-Control", "private, no-cache")
		if info, err := os.Stat(path); err == nil {
			w.Header().Set("ETag", `"`+url.PathEscape(t.Name)+"-"+filepath.Base(path)+"-"+strconv.FormatInt(info.ModTime().UnixNano(), 10)+`"`)
		}
	}
	webui.ServeThemeFile(w, r, t, path, webFS.FS)
}

func themeLogoURL(t webui.Theme) string {
	if t.Logo != "" {
		return "/theme.logo"
	}
	return "/static/logo.png"
}

// Versioned: Firefox does not revalidate a favicon, so a fixed URL keeps the first icon seen.
func themeFaviconURL(t webui.Theme) string {
	if t.Favicon == "" {
		return "/static/favicon.png"
	}
	v := url.QueryEscape(t.Name)
	if t.Builtin {
		v += "-" + url.QueryEscape(cmp.Or(Version, "builtin"))
	} else if info, err := os.Stat(t.Favicon); err == nil {
		v += "-" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	}
	return "/theme.favicon?v=" + v
}

type themeCluster struct {
	Names  []string
	Active string
}

func (s *Server) themeCluster() themeCluster {
	themes := s.listThemes()
	names := make([]string, 0, len(themes))
	for _, t := range themes {
		names = append(names, t.Name)
	}
	return themeCluster{Names: names, Active: cmp.Or(s.activeThemeIn(themes).Name, builtinThemes[0].Name)}
}

func (s *Server) settingsThemePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form data.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("theme"))
	if name != "" && !slices.ContainsFunc(s.listThemes(), func(t webui.Theme) bool { return t.Name == name }) {
		themeRefused(w, "No theme named "+name+".")
		return
	}
	if err := s.updateConfig(func(c *config.Config) error {
		c.Server.Theme = name
		return nil
	}); err != nil {
		themeRefused(w, "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: theme set to %q", name)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// The cluster's buttons swap nothing, and htmx drops an error status.
func themeRefused(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#theme-flash")
	w.Header().Set("HX-Reswap", "innerHTML")
	flashFragment(w, "err", msg)
}
