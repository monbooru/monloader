package web

import (
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/logx"
	"github.com/leqwin/monloader/internal/ptr"
)

// ptrEngine is the PTR surface the web layer drives: status for the page, the
// tag graph for the mounted API, and the lifecycle controls the page's forms
// invoke. *ptr.Engine satisfies it; tests inject a stub.
type ptrEngine interface {
	Enabled() bool
	Status() ptr.Status
	TagGraph(names []string) (map[string]ptr.TagInfo, error)
	Enable() error
	Pause()
	Resume()
	Retry()
	Delete() error
}

// ptrView backs the ptr page and its status fragment.
type ptrView struct {
	Status   ptr.Status
	Address  string
	DataPath string
	// PathMissing flags a data path with no directory behind it - typically a
	// docker run without a volume mounted there, where an index created inside
	// the container is lost when the container is recreated.
	PathMissing bool
	FreeBytes   int64
	MinFreeGB   int
	Percent     int
	CSRFToken   string
}

// ptrData builds the current view of the PTR engine and its config.
func (s *Server) ptrData(r *http.Request) ptrView {
	cfg := s.cfg.Current().PTR
	st := s.ptr.Status()
	v := ptrView{
		Status:    st,
		Address:   cfg.Address,
		DataPath:  cfg.DataPath,
		FreeBytes: ptr.FreeBytes(cfg.DataPath),
		MinFreeGB: cfg.MinFreeGB,
		CSRFToken: s.csrfToken(sessionFromContext(r.Context())),
	}
	if _, err := os.Stat(cfg.DataPath); os.IsNotExist(err) {
		v.PathMissing = true
	}
	if st.Progress.UpdateCount > 0 {
		v.Percent = int(float64(st.Progress.UpdateIndex) / float64(st.Progress.UpdateCount) * 100)
	}
	return v
}

func (s *Server) ptrScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "ptr", "ptr - "+s.titleName())
	data["PTR"] = s.ptrData(r)
	if msg := r.URL.Query().Get("msg"); msg != "" {
		data["Flash"] = msg
		kind := r.URL.Query().Get("kind")
		if kind == "" {
			kind = "ok"
		}
		data["FlashKind"] = kind
	}
	s.render(w, "ptr", data)
}

// ptrStatusFragment is the htmx-polled body while syncing, so progress and
// counts update without a full reload.
func (s *Server) ptrStatusFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "ptr_body", map[string]any{"PTR": s.ptrData(r)})
}

// ptrEnable starts the sync and, once it has started, persists the enabled flag
// so a restart resumes. A refused start (below the free-space floor) flashes the
// reason and leaves the config off.
func (s *Server) ptrEnable(w http.ResponseWriter, r *http.Request) {
	if err := s.ptr.Enable(); err != nil {
		s.ptrRedirect(w, r, "err", err.Error())
		return
	}
	if err := s.updateConfig(func(c *config.Config) error { c.PTR.Enabled = true; return nil }); err != nil {
		logx.Warnf("ptr: enabled but could not persist the flag: %v", err)
	}
	s.ptrRedirect(w, r, "ok", "ptr sync started")
}

func (s *Server) ptrPause(w http.ResponseWriter, r *http.Request) {
	s.ptr.Pause()
	s.ptrRedirect(w, r, "ok", "ptr sync paused")
}

func (s *Server) ptrResume(w http.ResponseWriter, r *http.Request) {
	s.ptr.Resume()
	s.ptrRedirect(w, r, "ok", "ptr sync resumed")
}

func (s *Server) ptrRetry(w http.ResponseWriter, r *http.Request) {
	s.ptr.Retry()
	s.ptrRedirect(w, r, "ok", "retrying ptr sync")
}

// savePTR edits the [ptr] storage and repository settings. The engine and its
// client snapshot the config at boot, so changes apply on restart; the access
// key is a secret with the site-credential semantics: blank keeps the stored
// value.
func (s *Server) savePTR(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	path := strings.TrimSpace(r.FormValue("data_path"))
	if path == "" {
		s.redirectFlash(w, r, "err", "data path required")
		return
	}
	err := s.updateConfig(func(c *config.Config) error {
		c.PTR.DataPath = path
		if v := strings.TrimSpace(r.FormValue("address")); v != "" {
			c.PTR.Address = v
		}
		if v := strings.TrimSpace(r.FormValue("access_key")); v != "" {
			c.PTR.AccessKey = v
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("fetch_sleep")), 64); err == nil && f >= 0 {
			c.PTR.FetchSleep = f
		}
		if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("min_free_gb"))); err == nil && n >= 0 {
			c.PTR.MinFreeGB = n
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "ptr settings saved")
}

// ptrDelete removes the index and turns the sync off, persisting the flag.
// Posted from the settings ptr section, so the flash returns there.
func (s *Server) ptrDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.ptr.Delete(); err != nil {
		s.redirectFlash(w, r, "err", "delete failed: "+err.Error())
		return
	}
	if err := s.updateConfig(func(c *config.Config) error { c.PTR.Enabled = false; return nil }); err != nil {
		logx.Warnf("ptr: deleted but could not persist the flag: %v", err)
	}
	s.redirectFlash(w, r, "ok", "ptr index deleted")
}

// ptrRedirect sends the operator back to the ptr page with a flash.
func (s *Server) ptrRedirect(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.Redirect(w, r, "/ptr?kind="+kind+"&msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// humanDate formats a unix time as a UTC calendar date, or "" when unset.
func humanDate(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}

// humanDue formats a unix due time as a short "in 14h" note, or "" when unset.
func humanDue(unix int64) string {
	if unix <= 0 {
		return ""
	}
	d := time.Until(time.Unix(unix, 0))
	switch {
	case d < 0:
		return "due now"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return "in " + strconv.Itoa(int(d.Hours())) + "h"
	default:
		return "in " + strconv.Itoa(int(d.Hours())/24) + "d"
	}
}
