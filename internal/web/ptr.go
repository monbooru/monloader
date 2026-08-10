package web

import (
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
	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/ptr"
)

// ptrEngine is the PTR surface the web layer drives: status for the page, the
// tag graph and hash lookup for the mounted API, and the lifecycle controls
// the page's forms invoke. *ptr.Engine satisfies it; tests inject a stub.
type ptrEngine interface {
	Enabled() bool
	Status() ptr.Status
	TagGraph(names []string) (map[string]ptr.TagInfo, error)
	TagsForHash(hashHex string) (tags []string, ok bool, err error)
	Enable() error
	Disable()
	Pause()
	Resume()
	Retry()
	Delete() error
	HasPersonalKey() bool
	AccessKey() string
	SetAccessKey(key string)
	CreateContribAccount(ctx context.Context) (string, error)
	ContribAccount(ctx context.Context) (*ptr.Account, error)
	RefreshAccount(ctx context.Context) (*ptr.Account, error)
	Syncing() bool
	Provisional() bool
	CaughtUp() bool
	Contrib() *ptr.ContribStore
	TagFilterCached() *ptr.TagFilter
	HashHasIdeal(hashHex, tag string) (bool, error)
	HashHasIdeals(hashHex string, tags []string) (map[string]bool, error)
	HashHasRaw(hashHex, tag string) (bool, error)
	RawTagsForHash(hashHex string) ([]string, error)
	IdealTag(tag string) (string, bool, error)
	SiblingCurrent(bad, good string) (bool, error)
	ParentCurrent(child, parent string) (bool, error)
	ParentEdgeCovered(child, parent string) (bool, error)
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
	// HasIndex tells a disabled state that was turned off from settings (the
	// files are still there, so enabling resumes) from one that never synced.
	HasIndex  bool
	FreeBytes int64
	MinFreeGB int
	Percent   int
	CSRFToken string
}

// ptrData builds the current view of the PTR engine and its config.
func (s *Server) ptrData(r *http.Request) ptrView {
	cfg := s.cfg.Current().PTR
	st := s.ptr.Status()
	v := ptrView{
		Status:    st,
		Address:   cfg.Address,
		DataPath:  cfg.DataPath,
		HasIndex:  ptr.IndexExists(cfg.DataPath),
		FreeBytes: ptr.FreeBytes(cfg.DataPath),
		MinFreeGB: cfg.MinFreeGB,
		CSRFToken: s.csrfToken(sessionFromContext(r.Context())),
	}
	if _, err := os.Stat(cfg.DataPath); os.IsNotExist(err) {
		v.PathMissing = true
	}
	// The blob fraction weights updates by their published volume; the update
	// fraction is the fallback for a status without the blob census.
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
	if err := s.applyPTREnabled(true); err != nil {
		s.ptrRedirect(w, r, "err", err.Error())
		return
	}
	s.ptrRedirect(w, r, "ok", "ptr sync started")
}

// applyPTREnabled brings the engine to the requested state and persists the
// flag so a restart matches. Turning it off stops the sync and closes the index
// but keeps the files: only the danger-zone delete reclaims the disk. An error
// is a refused start (below the free-space floor); a persist failure is logged
// rather than reported, since the engine did change.
func (s *Server) applyPTREnabled(on bool) error {
	if on == s.ptr.Enabled() {
		return nil
	}
	if on {
		if err := s.ptr.Enable(); err != nil {
			return err
		}
	} else {
		s.ptr.Disable()
	}
	if err := s.updateConfig(func(c *config.Config) error { c.PTR.Enabled = on; return nil }); err != nil {
		logx.Warnf("ptr: could not persist the enabled flag: %v", err)
	}
	return nil
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
// value. The enabled box is the exception: it takes effect at once, starting or
// stopping the sync like the ptr page's own controls.
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
	// Re-point the live client so a key change (set or cleared back to
	// the public one) applies with no restart.
	s.ptr.SetAccessKey(s.cfg.Current().PTR.AccessKey)
	if err := s.applyPTREnabled(r.FormValue("enabled") == "1"); err != nil {
		s.redirectFlash(w, r, "err", "settings saved, but the sync could not start: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "ptr settings saved")
}

// ptrClearKey forgets the personal contribution account (or a private
// repository key) and reverts to the public read-only one. It sits in the
// danger block because there is no recovery without a backup of the key.
func (s *Server) ptrClearKey(w http.ResponseWriter, r *http.Request) {
	if err := s.updateConfig(func(c *config.Config) error { c.PTR.AccessKey = ""; return nil }); err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.ptr.SetAccessKey("")
	s.redirectFlash(w, r, "ok", "ptr access key cleared")
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

// humanDate formats a unix time as a calendar date in the process timezone,
// or "" when unset.
func humanDate(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).In(time.Local).Format("2006-01-02")
}

// humanAgo formats a unix time as a short "4m ago" note, or "" when unset.
func humanAgo(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return humanSince(time.Unix(unix, 0))
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

// ptrAccountView backs the PTR page's account card.
type ptrAccountView struct {
	HasKey bool
	// Acc is the fetched account state; nil with FetchErr set when the
	// repository could not answer.
	Acc         *ptr.Account
	Permissions string
	FetchErr    string
	CreateErr   string
}

// permissionWords renders an account type's permission map in plain
// words for the card.
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

// ptrAccountData assembles the card view, fetching the account state
// when a personal key is set. The fetch is bounded so a slow repository
// delays only this lazy fragment, never the page.
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

// ptrAccountFragment lazily renders the account card once per page
// visit, so the 2s status poll never touches the repository.
func (s *Server) ptrAccountFragment(w http.ResponseWriter, r *http.Request) {
	if !s.ptr.Enabled() {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.render(w, "ptr_account", s.ptrAccountData(r, ""))
}

// ptrAccountCreate runs the repository's open auto-creation, persists
// the new key, and re-renders the card.
func (s *Server) ptrAccountCreate(w http.ResponseWriter, r *http.Request) {
	if !s.ptr.Enabled() {
		http.Error(w, "ptr disabled", http.StatusConflict)
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
	// The card below gates on the account this just created, so it ships with
	// the response rather than waiting for a reload to stop asking for one.
	contrib := s.ptrContribData(r, "")
	contrib.OOB = true
	s.render(w, "ptr_contrib", contrib)
}

// createAccountMessage folds the velocity refusal into the retry-later
// wording; everything else surfaces verbatim.
func createAccountMessage(err error) string {
	var serr *ptr.ServerError
	if errors.As(err, &serr) && serr.Status == 400 {
		return "the repository is not taking new accounts right now, try again later"
	}
	return err.Error()
}

// ptrRevealKey answers the card's explicit reveal control - the one
// place the personal key renders. The settings page keeps showing only
// set / not set.
func (s *Server) ptrRevealKey(w http.ResponseWriter, r *http.Request) {
	key := s.ptr.AccessKey()
	if key == "" {
		http.Error(w, "no personal key", http.StatusNotFound)
		return
	}
	s.render(w, "ptr_account_key", key)
}

// ptrContribItemView is one unsent or history row shaped for the card.
type ptrContribItemView struct {
	ID      int64
	Kind    string
	Label   string // "blonde hair @ a1b2c3.." or "blond hair => blonde hair"
	Link    string // monbooru detail URL for the item, when resolvable
	Origin  string
	Error   string
	Sent    string // committed_at for history rows
	Outcome string // outcome, or "pending Nd"
	// Rescindable marks an applied mapping add, the one committed kind
	// with an honest withdraw (a removal petition).
	Rescindable bool
}

// ptrContribView backs the contributions card. OOB marks a render that rides
// another card's response, so htmx swaps it by id instead of into the target.
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

// ptrActivityCell is one day square of the card's activity panel.
type ptrActivityCell struct {
	Level int
	Title string
}

// ptrActivityWeek is one column of the panel: seven day cells and the
// month label the column opens, when it opens one.
type ptrActivityWeek struct {
	Month string
	Days  []ptrActivityCell
}

// ptrActivityView backs the card's per-day activity panel.
type ptrActivityView struct {
	Total int
	Weeks []ptrActivityWeek
}

// ptrActivity buckets a year of ledger days into week columns starting
// Sunday. Cell depth is the day's share of the year's busiest day, so a
// light contributor still gets a readable ramp.
func ptrActivity(daily map[string]int, now time.Time) ptrActivityView {
	start := now.AddDate(0, 0, -364)
	start = start.AddDate(0, 0, -int(start.Weekday()))
	var v ptrActivityView
	peak := 0
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		n := daily[d.Format("2006-01-02")]
		v.Total += n
		if n > peak {
			peak = n
		}
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

// contribKindLabels renders each kind for the card's row.
var contribKindLabels = map[string]string{
	ptr.ContribMappingAdd:      "add",
	ptr.ContribMappingPetition: "petition",
	ptr.ContribSibling:         "sibling",
	ptr.ContribParent:          "parent",
	ptr.ContribSiblingPetition: "sibling petition",
	ptr.ContribParentPetition:  "parent petition",
}

func contribKindLabel(kind string) string {
	if label, ok := contribKindLabels[kind]; ok {
		return label
	}
	return kind
}

// contribOutcomeLabel renders a history row's outcome, or its pending
// age - a denial is invisible in the protocol, so a long-pending row is
// all anyone can know.
func contribOutcomeLabel(outcome string, committedAt int64) string {
	if outcome != "" {
		return outcome
	}
	days := int(time.Since(time.Unix(committedAt, 0)).Hours() / 24)
	return fmt.Sprintf("pending %dd", days)
}

// contribItemView fills the id/kind/label/link quartet the unsent and
// history rows share; the caller sets its side's remaining fields.
func contribItemView(mbBase string, id int64, kind, tag, tag2 string, hash []byte) ptrContribItemView {
	return ptrContribItemView{
		ID:    id,
		Kind:  contribKindLabel(kind),
		Label: ptr.ContribItemLabel(kind, tag, tag2, hash),
		Link:  ptr.ContribItemLink(mbBase, kind, tag, hash),
	}
}

// ptrContribData assembles the contributions card.
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

// ptrContribFragment lazily renders the contributions card.
func (s *Server) ptrContribFragment(w http.ResponseWriter, r *http.Request) {
	if !s.ptr.Enabled() {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.render(w, "ptr_contrib", s.ptrContribData(r, ""))
}

// ptrContribRetry queues the backlog send - the failed rows' [retry].
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

// ptrContribRescindUnsent deletes one unsent row. It never left the
// machine, so there is nothing to rescind upstream.
func (s *Server) ptrContribRescindUnsent(w http.ResponseWriter, r *http.Request) {
	store := s.ptr.Contrib()
	if store == nil {
		http.Error(w, "ptr disabled", http.StatusConflict)
		return
	}
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		_, _ = store.Rescind(id)
	}
	s.render(w, "ptr_contrib", s.ptrContribData(r, ""))
}

// ptrContribLogRescind petitions a committed mapping add back off under
// the fixed reason and marks the ledger row - the "(petition)" in the
// action label is the honest wording: it asks, it does not undo.
func (s *Server) ptrContribLogRescind(w http.ResponseWriter, r *http.Request) {
	store := s.ptr.Contrib()
	if store == nil {
		http.Error(w, "ptr disabled", http.StatusConflict)
		return
	}
	retryErr := ""
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		retryErr = s.rescindCommittedAdd(r.Context(), store, id)
	}
	s.render(w, "ptr_contrib", s.ptrContribData(r, retryErr))
}

// rescindCommittedAdd stages and sends the fixed-reason removal
// petition for one applied mapping add; returns a message when the send
// is refused or the row is not rescindable.
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
