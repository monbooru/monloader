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
		if r.FormValue("clear_access_key") == "1" {
			c.PTR.AccessKey = ""
		} else if v := strings.TrimSpace(r.FormValue("access_key")); v != "" {
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

// humanDate formats a unix time as a calendar date in the process timezone,
// or "" when unset.
func humanDate(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).In(time.Local).Format("2006-01-02")
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

// ptrContribView backs the contributions card.
type ptrContribView struct {
	NoAccount   bool
	Syncing     bool
	NotReady    bool
	SendRunning bool
	Unsent      []ptrContribItemView
	HasFailed   bool
	History     []ptrContribItemView
	Page        int
	TotalPages  int
	RetryErr    string
}

// contribKindLabel renders a kind for the card's row.
func contribKindLabel(kind string) string {
	switch kind {
	case ptr.ContribMappingAdd:
		return "add"
	case ptr.ContribMappingPetition:
		return "petition"
	case ptr.ContribSibling:
		return "sibling"
	case ptr.ContribParent:
		return "parent"
	case ptr.ContribSiblingPetition:
		return "sibling petition"
	case ptr.ContribParentPetition:
		return "parent petition"
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
