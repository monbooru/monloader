package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/api"
	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/session"
)

type ptrEngine interface {
	api.PTRService

	Enable() error
	Disable()
	Pause()
	Resume()
	Retry()
	Delete() error
	AccessKey() string
	SetAccessKey(key string)
	DataPath() string
	SetDataPath(path string) bool
	SetAddress(addr string) bool
	ContribAccount(ctx context.Context) (*ptr.Account, error)
}

type ptrView struct {
	Status   ptr.Status
	Address  string
	DataPath string
	// Typically a docker run without the volume mounted; an index built
	// there dies with the container.
	PathMissing bool
	HasIndex    bool
	FreeBytes   int64
	MinFreeGB   int
	BelowFloor  bool
	Percent     int
	CSRFToken   string
}

func (s *Server) ptrData(r *http.Request) ptrView {
	cfg := s.cfg.Current().PTR
	st := s.ptr.Status()
	// From the engine, not the config: a path saved but not yet restarted
	// into is not the index the state, size and delete act on.
	path := s.ptr.DataPath()
	v := ptrView{
		Status:    st,
		Address:   cfg.Address,
		DataPath:  path,
		HasIndex:  ptr.IndexExists(path),
		FreeBytes: ptr.FreeBytes(path),
		MinFreeGB: cfg.MinFreeGB,
		CSRFToken: s.sessions.CSRFToken(session.FromContext(r.Context())),
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		v.PathMissing = true
	}
	v.BelowFloor = !v.HasIndex && v.MinFreeGB > 0 && v.FreeBytes > 0 && v.FreeBytes < int64(v.MinFreeGB)<<30
	// Blobs weight updates by volume; the update count is the fallback
	// without a blob census.
	if st.Progress.BlobsTotal > 0 {
		v.Percent = int(float64(st.Progress.BlobsDone) / float64(st.Progress.BlobsTotal) * 100)
	} else if st.Progress.UpdateCount > 0 {
		v.Percent = int(float64(st.Progress.UpdateIndex) / float64(st.Progress.UpdateCount) * 100)
	}
	return v
}

func (s *Server) ptrScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "ptr", "Public Tag Repository - "+s.titleName())
	data["PTR"] = s.ptrData(r)
	if msg := r.URL.Query().Get("msg"); msg != "" {
		data["Flash"] = msg
		data["FlashKind"] = cmp.Or(r.URL.Query().Get("kind"), "ok")
		data["CanonicalURL"] = "/ptr"
	}
	s.render(w, "ptr", data)
}

func (s *Server) ptrStatusFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "ptr_body", map[string]any{"PTR": s.ptrData(r)})
}

func (s *Server) ptrEnable(w http.ResponseWriter, r *http.Request) {
	if err := s.applyPTREnabled(true); err != nil {
		s.ptrRedirect(w, r, "err", err.Error())
		return
	}
	s.ptrRedirect(w, r, "ok", "ptr sync started")
}

// A persist failure is only logged: the engine did change.
func (s *Server) applyPTREnabled(on bool) error {
	if on == s.ptr.Enabled() {
		return nil
	}
	// Turning the sync on asks for it to run, so a held pause goes.
	resume := on && s.cfg.Current().PTR.Paused
	if on {
		if resume {
			s.ptr.Resume()
		}
		if err := s.ptr.Enable(); err != nil {
			return err
		}
	} else {
		s.ptr.Disable()
	}
	err := s.updateConfig(func(c *config.Config) error {
		c.PTR.Enabled = on
		if resume {
			c.PTR.Paused = false
		}
		return nil
	})
	if err != nil {
		logx.Warnf("ptr: could not persist the enabled flag: %v", err)
	}
	return nil
}

func (s *Server) ptrPause(w http.ResponseWriter, r *http.Request) {
	s.ptr.Pause()
	s.persistPTRPaused(true)
	s.ptrRedirect(w, r, "ok", "ptr sync paused")
}

func (s *Server) ptrResume(w http.ResponseWriter, r *http.Request) {
	s.ptr.Resume()
	s.persistPTRPaused(false)
	s.ptrRedirect(w, r, "ok", "ptr sync resumed")
}

func (s *Server) persistPTRPaused(paused bool) {
	if err := s.updateConfig(func(c *config.Config) error { c.PTR.Paused = paused; return nil }); err != nil {
		logx.Warnf("ptr: could not persist the pause: %v", err)
	}
}

func (s *Server) ptrRetry(w http.ResponseWriter, r *http.Request) {
	s.ptr.Retry()
	s.ptrRedirect(w, r, "ok", "retrying ptr sync")
}

// The engine snapshots its config at boot; only the key, the enabled box
// and, while no index is open, the data path and the address apply at once.
func (s *Server) savePTR(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "Bad form data.")
		return
	}
	path := strings.TrimSpace(r.FormValue("data_path"))
	if path == "" {
		s.refuseSave(w, r, "Data path required.")
		return
	}
	// A running sync is not repointed, so the engine never checks the new
	// path; unchecked, it would break the lookup backend at the next restart.
	enabled := r.FormValue("enabled") == "1"
	if enabled {
		if err := ptr.CheckDataPath(path); err != nil {
			s.refuseSave(w, r, err.Error())
			return
		}
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
		s.refuseSave(w, r, "Could not save: "+err.Error())
		return
	}
	s.ptr.SetAccessKey(s.cfg.Current().PTR.AccessKey)
	// Repointed before the enable, which would otherwise sync from the
	// address the engine booted with.
	var later []string
	if !s.ptr.SetDataPath(path) {
		later = append(later, "data path")
	}
	if !s.ptr.SetAddress(s.cfg.Current().PTR.Address) {
		later = append(later, "address")
	}
	if err := s.applyPTREnabled(enabled); err != nil {
		s.redirectFlash(w, r, "err", "Saved, but the sync could not start: "+err.Error())
		return
	}
	if len(later) > 0 {
		s.redirectFlash(w, r, "ok", "Saved. The new "+strings.Join(later, " and ")+" applies after a restart, since the sync is running.")
		return
	}
	s.redirectFlash(w, r, "ok", "Saved.")
}

func (s *Server) ptrClearKey(w http.ResponseWriter, r *http.Request) {
	if err := s.updateConfig(func(c *config.Config) error { c.PTR.AccessKey = ""; return nil }); err != nil {
		s.redirectFlash(w, r, "err", "Could not save: "+err.Error())
		return
	}
	s.ptr.SetAccessKey("")
	s.redirectFlash(w, r, "ok", "PTR access key cleared.")
}

// Posted from settings, so the flash returns there, not to the ptr page.
func (s *Server) ptrDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.ptr.Delete(); err != nil {
		s.redirectFlash(w, r, "err", "Could not delete: "+err.Error())
		return
	}
	if err := s.updateConfig(func(c *config.Config) error { c.PTR.Enabled = false; return nil }); err != nil {
		logx.Warnf("ptr: deleted but could not persist the flag: %v", err)
	}
	s.redirectFlash(w, r, "ok", "PTR index deleted.")
}

func (s *Server) ptrRedirect(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.Redirect(w, r, "/ptr?kind="+kind+"&msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func humanDate(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).In(time.Local).Format("2006-01-02")
}

func humanAgo(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return humanSince(time.Unix(unix, 0))
}

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

type ptrAccountView struct {
	HasKey      bool
	Acc         *ptr.Account
	Permissions string
	FetchErr    string
	CreateErr   string
}

func permissionWords(acc *ptr.Account) string {
	if acc == nil {
		return ""
	}
	var parts []string
	if acc.Type.CanCreateMappings() {
		parts = append(parts, "add tags")
	}
	if acc.Type.CanPetition() {
		parts = append(parts, "suggest / petition aliases and implications")
	}
	if len(parts) == 0 {
		return "read only"
	}
	return strings.Join(parts, "; ")
}

func (s *Server) ptrAccountData(r *http.Request, createErr string) ptrAccountView {
	v := ptrAccountView{
		HasKey:    s.ptr.HasPersonalKey(),
		CreateErr: createErr,
	}
	if !v.HasKey {
		return v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	acc, err := s.ptr.ContribAccount(ctx)
	if err != nil {
		v.FetchErr = err.Error()
		return v
	}
	v.Acc = acc
	v.Permissions = permissionWords(acc)
	return v
}

// Lazy and apart from the 2s status poll, which must never reach the
// repository.
func (s *Server) ptrAccountFragment(w http.ResponseWriter, r *http.Request) {
	if !s.ptr.Enabled() {
		return
	}
	s.render(w, "ptr_account", s.ptrAccountData(r, ""))
}

func (s *Server) ptrAccountCreate(w http.ResponseWriter, r *http.Request) {
	if !s.ptr.Enabled() {
		http.Error(w, "PTR disabled.", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	key, err := s.ptr.CreateContribAccount(ctx)
	if err != nil {
		s.render(w, "ptr_account", s.ptrAccountData(r, createAccountMessage(err)))
		return
	}
	if err := s.updateConfig(func(c *config.Config) error {
		c.PTR.AccessKey = key
		return nil
	}); err != nil {
		s.render(w, "ptr_account", s.ptrAccountData(r,
			"account created but saving the key failed: "+err.Error()+" - reveal and back it up before restarting"))
		return
	}
	s.render(w, "ptr_account", s.ptrAccountData(r, ""))
	// The contributions card gates on this account, so it is refreshed
	// out of band now.
	contrib := s.ptrContribData(r, "")
	contrib.OOB = true
	s.render(w, "ptr_contrib", contrib)
}

// A 400 is the repository's velocity refusal.
func createAccountMessage(err error) string {
	var serr *ptr.ServerError
	if errors.As(err, &serr) && serr.Status == 400 {
		return "The repository is not taking new accounts right now, try again later."
	}
	return err.Error()
}

// The one place the personal key renders; settings shows only set / not set.
func (s *Server) ptrRevealKey(w http.ResponseWriter, r *http.Request) {
	key := s.ptr.AccessKey()
	if key == "" {
		http.Error(w, "No personal key.", http.StatusNotFound)
		return
	}
	s.render(w, "ptr_account_key", key)
}

type ptrContribItemView struct {
	ID      int64
	Kind    string
	Label   string
	Link    string
	Origin  string
	Error   string
	Sent    string
	Outcome string
	// Only an applied mapping add has an honest withdraw: a removal petition.
	Rescindable bool
}

type ptrContribView struct {
	OOB         bool
	NoAccount   bool
	Syncing     bool
	NotReady    bool
	SendRunning bool
	Activity    ptrActivityView
	Unsent      []ptrContribItemView
	HasFailed   bool
	History     []ptrContribItemView
	Page        int
	TotalPages  int
	RetryErr    string
}

type ptrActivityCell struct {
	Level int
	Title string
}

type ptrActivityWeek struct {
	Month string
	Days  []ptrActivityCell
}

type ptrActivityView struct {
	Total int
	Weeks []ptrActivityWeek
}

// Cell depth is relative to the year's busiest day, so a light
// contributor still gets a readable ramp.
func ptrActivity(daily map[string]int, now time.Time) ptrActivityView {
	start := now.AddDate(0, 0, -364)
	start = start.AddDate(0, 0, -int(start.Weekday()))
	var v ptrActivityView
	peak := 0
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		n := daily[d.Format("2006-01-02")]
		v.Total += n
		peak = max(peak, n)
	}
	prev := time.Month(0)
	for d := start; !d.After(now); {
		var wk ptrActivityWeek
		if m := d.Month(); m != prev && d.Day() <= 7 {
			wk.Month = strings.ToLower(m.String()[:3])
		}
		prev = d.Month()
		for i := 0; i < 7 && !d.After(now); i++ {
			day := d.Format("2006-01-02")
			n := daily[day]
			cell := ptrActivityCell{Title: fmt.Sprintf("%s - %d contribution", day, n)}
			if n != 1 {
				cell.Title += "s"
			}
			if n > 0 {
				cell.Level = (4*n + peak - 1) / peak
			}
			wk.Days = append(wk.Days, cell)
			d = d.AddDate(0, 0, 1)
		}
		v.Weeks = append(v.Weeks, wk)
	}
	return v
}

var contribKindLabels = map[string]string{
	ptr.ContribMappingAdd:      "add",
	ptr.ContribMappingPetition: "petition",
	ptr.ContribSibling:         "sibling",
	ptr.ContribParent:          "parent",
	ptr.ContribSiblingPetition: "sibling petition",
	ptr.ContribParentPetition:  "parent petition",
}

func contribKindLabel(kind string) string {
	return cmp.Or(contribKindLabels[kind], kind)
}

// A denial is invisible in the protocol, so a long pending age is all
// anyone can know.
func contribOutcomeLabel(outcome string, committedAt int64) string {
	if outcome != "" {
		return outcome
	}
	days := int(time.Since(time.Unix(committedAt, 0)).Hours() / 24)
	return fmt.Sprintf("pending %dd", days)
}

func contribItemView(mbBase string, id int64, kind, tag, tag2 string, hash []byte) ptrContribItemView {
	return ptrContribItemView{
		ID:    id,
		Kind:  contribKindLabel(kind),
		Label: ptr.ContribItemLabel(kind, tag, tag2, hash),
		Link:  ptr.ContribItemLink(mbBase, kind, tag, hash),
	}
}

func (s *Server) ptrContribData(r *http.Request, retryErr string) ptrContribView {
	v := ptrContribView{
		NoAccount: !s.ptr.HasPersonalKey(),
		Syncing:   s.ptr.Syncing(),
		NotReady:  !s.ptr.CaughtUp(),
		RetryErr:  retryErr,
	}
	if v.NoAccount {
		return v
	}
	v.SendRunning = s.queue.ContribSendLive()
	store := s.ptr.Contrib()
	if store == nil {
		return v
	}
	now := time.Now()
	// 371 covers the 364-day window plus the roll back to its Sunday.
	if daily, err := store.LogDaily(now.AddDate(0, 0, -371).Unix()); err == nil {
		v.Activity = ptrActivity(daily, now)
	}
	mbBase := s.monbooruWebBase()
	if unsent, err := store.Unsent(); err == nil {
		for _, it := range unsent {
			row := contribItemView(mbBase, it.ID, it.Kind, it.Tag, it.Tag2, it.Hash)
			row.Origin, row.Error = it.Origin, it.Error
			v.Unsent = append(v.Unsent, row)
			if it.Status == "failed" {
				v.HasFailed = true
			}
		}
	}
	total, _ := store.LogCount()
	page, totalPages := pageWindow(r, total)
	v.Page, v.TotalPages = page, totalPages
	if rows, err := store.LogPage(pageSize, (page-1)*pageSize); err == nil {
		for _, row := range rows {
			h := contribItemView(mbBase, row.ID, row.Kind, row.Tag, row.Tag2, row.Hash)
			h.Sent = time.Unix(row.CommittedAt, 0).In(time.Local).Format("01-02 15:04")
			h.Outcome = contribOutcomeLabel(row.Outcome, row.CommittedAt)
			h.Rescindable = row.Kind == ptr.ContribMappingAdd && row.Outcome == ptr.OutcomeApplied
			v.History = append(v.History, h)
		}
	}
	return v
}

func (s *Server) ptrContribFragment(w http.ResponseWriter, r *http.Request) {
	if !s.ptr.Enabled() {
		return
	}
	s.render(w, "ptr_contrib", s.ptrContribData(r, ""))
}

func (s *Server) ptrContribRetry(w http.ResponseWriter, r *http.Request) {
	if refusal := api.ContribSendRefusal(r.Context(), s.ptr); refusal != nil {
		s.render(w, "ptr_contrib", s.ptrContribData(r, refusal.Msg))
		return
	}
	if s.queue.ContribSendLive() {
		s.render(w, "ptr_contrib", s.ptrContribData(r, "a send is already running"))
		return
	}
	s.queue.EnqueueContrib(nil, true)
	s.render(w, "ptr_contrib", s.ptrContribData(r, ""))
}

func (s *Server) ptrContribRescindUnsent(w http.ResponseWriter, r *http.Request) {
	store := s.ptr.Contrib()
	if store == nil {
		http.Error(w, "PTR disabled.", http.StatusConflict)
		return
	}
	msg := ""
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		switch ok, rerr := store.Rescind(id); {
		case rerr != nil:
			msg = "Could not rescind: " + rerr.Error()
		case !ok:
			// The row moved to sending since the render.
			msg = "That contribution is already on its way out."
		}
	}
	s.render(w, "ptr_contrib", s.ptrContribData(r, msg))
}

func (s *Server) ptrContribLogRescind(w http.ResponseWriter, r *http.Request) {
	store := s.ptr.Contrib()
	if store == nil {
		http.Error(w, "PTR disabled.", http.StatusConflict)
		return
	}
	retryErr := ""
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		retryErr = s.rescindCommittedAdd(r.Context(), store, id)
	}
	s.render(w, "ptr_contrib", s.ptrContribData(r, retryErr))
}

func (s *Server) rescindCommittedAdd(ctx context.Context, store *ptr.ContribStore, logID int64) string {
	if refusal := api.ContribSendRefusal(ctx, s.ptr); refusal != nil {
		return refusal.Msg
	}
	itemID, rerr := api.RescindCommittedAdd(s.ptr, store, logID)
	if rerr != nil {
		return rerr.Msg
	}
	s.queue.EnqueueContrib([]int64{itemID}, false)
	return ""
}
