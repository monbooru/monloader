package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/desktop"
	"github.com/monbooru/monloader/internal/logx"
)

// monbooruProbeTimeout bounds the "is monbooru there?" question. It is a
// loopback request to a process that either answers or is not there.
const monbooruProbeTimeout = 800 * time.Millisecond

// setupData drives the wizard. Everything the operator can change is echoed
// back when a submit has to be shown again.
type setupData struct {
	// BooruName is the lowercase name the body copy uses; TitleName is the
	// capitalized one, for the browser tab alone.
	BooruName    string
	TitleName    string
	CSRFToken    string
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
	Integration  desktopIntegration
}

// setupPending reports whether the first-run wizard still owes the operator
// a pass. Only the desktop profile has one: a container or a server install
// was configured by whoever deployed it.
func (s *Server) setupPending() bool {
	return s.desktop && !s.cfg.Current().SetupDone
}

// SetupMiddleware sends every page to the wizard until it has been through
// once. The exemptions are what the wizard itself needs plus the health
// probe, which the single-instance check and the container healthcheck both
// depend on answering at all times, and the login pages: the session gate
// exempts /login from its own redirect, so leaving it behind this one would
// bounce every request between the two and lock the operator out entirely.
// Everything under /api/ is refused in JSON instead of redirected.
func (s *Server) SetupMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.setupPending() || setupExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// The API is a client's contract, not a browser's: an extension firing
		// "connect to monloader" at a fresh install has to read why it was
		// refused, and a 303 to an HTML wizard says nothing it can show.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"monloader has not been set up yet - finish the first-run setup in its window","code":"setup_required"}`))
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
	// The API reference stays readable: a client refused by the gate needs
	// somewhere to look up what it was refused with.
	case "/health", "/setup", "/login", "/logout", "/api/v1/openapi.json", "/api/v1/docs":
		return true
	}
	return strings.HasPrefix(path, "/static/")
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.desktop {
		// A stale link or bookmark reaches this on a container install; give it
		// the styled page with a way back, like every other unknown path.
		s.notFound(w, r)
		return
	}
	cfg := s.cfg.Current()
	s.renderSetup(w, r, setupData{
		APIURL: cfg.Monbooru.APIURL,
		Pair:   true,
		LAN:    !desktop.IsLoopbackAddr(cfg.Server.BindAddress),
		// The menu entry is the offer worth making by default; the queue's
		// retries and daily budget only run while monloader does, so start at
		// login is too.
		Menu:      true,
		Autostart: true,
	})
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, d setupData) {
	_, port, err := net.SplitHostPort(s.cfg.Current().Server.BindAddress)
	if err != nil {
		port = "8456"
	}
	d.Port = port
	d.BooruName = s.booruName()
	d.TitleName = s.titleName()
	d.CSRFToken = s.csrfToken(sessionFromContext(r.Context()))
	d.Paired = s.hasPairedToken("monbooru")
	d.Integration = s.desktopIntegration()
	// The page leads with whatever is actually listening, so the common case
	// is one button rather than a typed address.
	if d.APIURL != "" {
		if inst, found := desktop.Probe(desktop.LoopbackAddr(hostPort(d.APIURL)), monbooruProbeTimeout); found && inst.App == "monbooru" {
			d.Found = true
			d.FoundVersion = inst.Version
		}
	}
	s.render(w, "setup", d)
}

// hostPort reduces a base URL to the host:port a probe dials, defaulting
// the port the way a browser would.
func hostPort(raw string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://")
	trimmed = strings.TrimSuffix(strings.SplitN(trimmed, "/", 2)[0], "/")
	if trimmed == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(trimmed); err != nil {
		if strings.HasPrefix(raw, "https://") {
			return net.JoinHostPort(trimmed, "443")
		}
		return net.JoinHostPort(trimmed, "80")
	}
	return trimmed
}

// setupPost is the wizard's only submit. Every input is checked before
// anything is written, so a refused page has changed nothing and can simply
// be shown again with what was entered. Past that check the setup is done and
// the wizard is over, whatever the optional pairing does.
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	if !s.desktop {
		s.notFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderSetup(w, r, setupData{Err: "bad form"})
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
		form.Err = "that is not an http(s) address"
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
		form.Err = "save failed: " + err.Error()
		s.renderSetup(w, r, form)
		return
	}

	h := s.DesktopHook()
	if h.MenuSupported() {
		if err := h.SetMenu(form.Menu); err != nil {
			logx.Warnf("setup: could not write the menu entry: %v", err)
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(form.Autostart); err != nil {
			logx.Warnf("setup: could not write start at login: %v", err)
		}
	}
	// The approval still happens in monbooru, which is the point of the
	// design; the offer is only shown when the probe found it, so a failure
	// here is a race rather than the ordinary case. The setup is finished
	// either way, so the failure reports from Settings, beside the control
	// that re-offers the handshake - re-rendering the wizard would call
	// a completed setup a refusal and leave the operator pressing a button
	// with nothing left to do.
	done := "/"
	if form.Pair && form.APIURL != "" && !s.hasPairedToken("monbooru") {
		if err := s.startMonbooruPairing(r.Context()); err != nil {
			logx.Warnf("setup: could not ask monbooru to pair: %v", err)
			done = "/settings?kind=err&section=monbooru&msg=" +
				url.QueryEscape("setup finished, but the pairing request failed: "+err.Error()) + "#pairing"
		}
	}
	logx.Infof("setup: done")
	http.Redirect(w, r, done, http.StatusSeeOther)
}
