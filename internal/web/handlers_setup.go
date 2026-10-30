package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/api"
	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/desktop"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/session"
)

// A loopback probe: the process answers at once or is not there.
const monbooruProbeTimeout = 800 * time.Millisecond

type setupData struct {
	BooruName string
	TitleName string
	CSRFToken string
	// The rest of what webui/head reads.
	Title        string
	BooruFavicon string
	Theme        bool
	Mascot       string
	Err          string
	APIURL       string
	Found        bool
	FoundVersion string
	Paired       bool
	Pair         bool
	LAN          bool
	Menu         bool
	Autostart    bool
	Port         string
	Integration  desktop.Integration
}

func (s *Server) setupPending() bool {
	return s.desktop && !s.cfg.Current().SetupDone
}

func (s *Server) SetupMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.setupPending() || setupExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// API clients get a JSON refusal they can show; a 303 to the
		// wizard tells them nothing. A preflight goes through, or the
		// extension could not read the refusal.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			api.SetCORS(w, r)
			pairError(w, http.StatusConflict, api.ErrSetupRequired, "monloader has not been set up yet - finish the first-run setup in its window")
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/setup")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
}

func setupExempt(path string) bool {
	switch path {
	// /health must always answer (single-instance check, container
	// healthcheck), /login would bounce against the session gate, and a
	// refused client needs the API reference.
	case "/health", "/setup", "/login", "/logout", "/api/v1/openapi.json", "/api/v1/docs", "/theme.css", "/theme.logo", "/theme.favicon":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/theme.mascot/")
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.setupPending() {
		s.notFound(w, r)
		return
	}
	cfg := s.cfg.Current()
	s.renderSetup(w, r, setupData{
		APIURL:    cfg.Monbooru.APIURL,
		Pair:      true,
		LAN:       !desktop.IsLoopbackAddr(cfg.Server.BindAddress),
		Menu:      true,
		Autostart: true,
	})
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, d setupData) {
	_, port, err := net.SplitHostPort(s.cfg.Current().Server.BindAddress)
	if err != nil {
		port = contract.PortMonloader
	}
	d.Port = port
	d.BooruName = s.booruName()
	d.TitleName = s.titleName()
	d.CSRFToken = s.sessions.CSRFToken(session.FromContext(r.Context()))
	d.Paired = s.hasPairedToken(contract.AppMonbooru)
	d.Integration = s.desktopIntegration()
	if d.APIURL != "" {
		if inst, found := desktop.Probe(desktop.LoopbackAddr(hostPort(d.APIURL)), monbooruProbeTimeout); found && inst.App == contract.AppMonbooru {
			d.Found = true
			d.FoundVersion = inst.Version
		}
	}
	theme := s.activeTheme()
	d.Title = d.TitleName + " setup"
	d.BooruFavicon = themeFaviconURL(theme)
	d.Theme = theme.Path != ""
	face := "sleep"
	if d.Found {
		face = "sparkle"
	}
	d.Mascot = themeMascots(theme)[face]
	s.render(w, "setup", d)
}

func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// Every input is checked before anything is written, so a refused page
// has changed nothing.
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	if !s.setupPending() || !s.desktopLocal(r) {
		s.notFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderSetup(w, r, setupData{Err: "Bad form data."})
		return
	}
	form := setupData{
		APIURL:    strings.TrimSpace(r.FormValue("api_url")),
		Pair:      r.FormValue("pair") == "on",
		LAN:       r.FormValue("reach") == "lan",
		Menu:      r.FormValue("menu_entry") == "on",
		Autostart: r.FormValue("start_at_login") == "on",
	}
	if form.APIURL != "" && !config.IsHTTPURL(form.APIURL) {
		form.Err = "That is not an http(s) address."
		s.renderSetup(w, r, form)
		return
	}

	err := s.updateConfig(func(c *config.Config) error {
		c.Monbooru.APIURL = form.APIURL
		_, port, splitErr := net.SplitHostPort(c.Server.BindAddress)
		if splitErr != nil {
			return splitErr
		}
		host := "127.0.0.1"
		if form.LAN {
			host = "0.0.0.0"
		}
		c.Server.BindAddress = net.JoinHostPort(host, port)
		c.SetupDone = true
		return nil
	})
	if err != nil {
		form.Err = "Could not save: " + err.Error()
		s.renderSetup(w, r, form)
		return
	}

	for _, f := range s.DesktopHook().Apply(form.Menu, form.Autostart) {
		logx.Warnf("setup: could not write %s", f)
	}
	// The setup is done either way, so a failed pairing request reports
	// from Settings, beside the control that re-offers it; re-rendering
	// the wizard would call a finished setup a refusal.
	done := "/"
	if form.Pair && form.APIURL != "" && !s.hasPairedToken(contract.AppMonbooru) {
		if err := s.startMonbooruPairing(r.Context()); err != nil {
			logx.Warnf("setup: could not ask monbooru to pair: %v", err)
			done = "/settings?kind=err&section=monbooru&msg=" +
				url.QueryEscape("Setup finished, but the pairing request failed: "+err.Error()) + "#pairing"
		}
	}
	logx.Infof("setup: done, reachable from %s", map[bool]string{true: "the network", false: "this computer"}[form.LAN])
	http.Redirect(w, r, done, http.StatusSeeOther)
}
