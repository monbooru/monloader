package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
)

func (s *Server) addScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "add", s.titleName())
	data["Lookup"] = s.lookupStatus()
	s.render(w, "add", data)
}

// notFound serves a themed page for unmatched browser routes and a JSON error
// for unmatched API routes, in place of net/http's plain-text default.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	s.render(w, "notfound", s.base(r, "", "not found"))
}

// enqueueForm handles the add bar (POST /). On success it sends the operator
// to the queue screen (HX-Redirect) so they can follow the job; a bad request
// stays put with an inline flash fragment swapped into #add-flash.
func (s *Server) enqueueForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "bad form data")
		return
	}
	if msg := s.monbooruBlocked(); msg != "" {
		flashFragment(w, "err", msg)
		return
	}
	target := strings.TrimSpace(r.FormValue("url"))
	if target == "" {
		flashFragment(w, "err", "enter a URL or an md5 hash")
		return
	}
	// A pasted md5 (bare or prefixed) imports the post carrying that hash via
	// the lookup walk. A sha256 has no file to import here: it keys a PTR tag
	// lookup, which runs from monbooru against an image it already holds.
	if md5, ok := hashImportTarget(target); ok {
		if len(s.mapper.LookupSites()) == 0 {
			flashFragment(w, "err", "md5 lookup is off - give a site a chain position in the settings lookup section")
			return
		}
		s.queue.EnqueueHashImport(md5, queue.Options{})
	} else if looksLikeSHA256(target) {
		flashFragment(w, "err", sha256AddHint(s.ptr.Enabled(), s.ptr.CaughtUp()))
		return
	} else if !config.IsHTTPURL(target) {
		flashFragment(w, "err", "enter a valid http(s) URL or an md5 hash")
		return
	} else {
		s.queue.Enqueue(target, queue.Options{})
	}
	// Refresh the rows in place when adding from the queue screen; redirecting
	// would reload the page and drop the operator's expand/collapse state.
	if onQueueScreen(r) {
		w.Header().Set("HX-Retarget", "#queue-rows")
		w.Header().Set("HX-Reswap", "innerHTML")
		s.queueRows(w, r)
		return
	}
	w.Header().Set("HX-Redirect", "/queue")
}

// monbooruBlocked says why new work cannot be accepted, or "" when it can:
// with no monbooru to push to, or the link held from the footer light, a
// fetched post could only fail at the push step, so every surface that starts
// work - the add bar and the queue rows' retry / continue - refuses it here so
// the operator fixes the connection first. The controls are disabled
// client-side too, so this guards a stale page.
func (s *Server) monbooruBlocked() string {
	if !s.monbooruConfigured() {
		return "monbooru is not configured - set its connection in settings"
	}
	if s.monbooruPaused() {
		return "the monbooru link is paused - resume it from the light in the footer"
	}
	return ""
}

// hashImportTarget recognizes an md5 pasted into the add bar - bare or with an
// "md5:" prefix - returning the lower-cased hash. The prefix lets an operator
// force the hash reading of an ambiguous string, but a bare 32-hex string is
// treated as a hash too, since nothing else looks like one.
func hashImportTarget(s string) (string, bool) {
	s = strings.TrimPrefix(s, "md5:")
	if config.IsHexHash(s, 32) {
		return strings.ToLower(s), true
	}
	return "", false
}

// looksLikeSHA256 reports whether s is a bare 64-hex string, so the add bar can
// explain that a sha256 cannot be imported rather than failing it as a bad URL.
func looksLikeSHA256(s string) bool {
	return config.IsHexHash(strings.TrimPrefix(s, "sha256:"), 64)
}

// sha256AddHint explains what a sha256 does at the add bar: it is not an import
// key but a PTR tag-lookup key, used from monbooru. The wording depends on
// whether the PTR can answer that lookup - off, still building, or ready.
func sha256AddHint(ptrEnabled, ptrCaughtUp bool) string {
	switch {
	case !ptrEnabled:
		return "a sha256 looks tags up from the PTR, which is off - enable it on the ptr page"
	case !ptrCaughtUp:
		return "a sha256 looks tags up from the PTR, which answers once it has finished syncing - follow it on the ptr page"
	default:
		return "a sha256 is for looking tags up from the PTR (via monbooru), not for importing here - paste an md5 or a URL"
	}
}

// lookupStatusView backs the add-bar status line: which chain sources a booru
// lookup queries, and whether the PTR (sha256) backend is on.
type lookupStatusView struct {
	BooruSources []string
	PTREnabled   bool
	PTRSyncing   bool
}

// lookupStatus snapshots the two lookup backends for the add-bar status line.
// BooruSources lists the chain entries that would actually be queried - a
// source missing its credential is left out, like the chain leaves it out -
// with the similarity services marked.
func (s *Server) lookupStatus() lookupStatusView {
	st := s.ptr.Status()
	view := lookupStatusView{
		PTREnabled: st.Enabled,
		PTRSyncing: st.Enabled && st.State != ptr.StateReady,
	}
	for _, src := range s.mapper.LookupChain() {
		if src.Similarity {
			if _, missing := s.sim.Missing(src.Name); !missing {
				view.BooruSources = append(view.BooruSources, src.Name+" (similarity)")
			}
		} else if _, needs := s.siteNeedsCredential(src.Name); !needs {
			view.BooruSources = append(view.BooruSources, src.Name)
		}
	}
	return view
}

// onQueueScreen reports whether the htmx request was issued from /queue.
func onQueueScreen(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	return err == nil && u.Path == "/queue"
}

func flashFragment(w http.ResponseWriter, kind, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="flash-%s">%s</span>`, kind, html.EscapeString(msg))
}

func (s *Server) queueScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "queue", "Queue - "+s.titleName())
	s.fillQueue(r, data)
	data["Actions"] = s.queueActions()
	s.render(w, "queue", data)
}

// queueActionsView gates the add bar's bulk buttons: each shows only while the
// queue holds what it acts on, so the url field takes the rest of the row.
type queueActionsView struct {
	HasPending  bool
	HasFinished bool
}

func (s *Server) queueActions() queueActionsView {
	counts := s.queueCounts()
	return queueActionsView{HasPending: counts.Queued > 0, HasFinished: counts.Finished > 0}
}

// queueCounts tallies the tracked jobs by state. The items are left behind:
// every caller wants counters, and a snapshot with items deep-copies each
// job's whole slice.
func (s *Server) queueCounts() queueStats {
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	var st queueStats
	for _, j := range jobs {
		switch j.Status {
		case queue.JobQueued:
			st.Queued++
		case queue.JobRunning:
			st.Running++
		default:
			// neither in the FIFO nor still working: it is history
			st.Finished++
		}
	}
	return st
}

// queueActionsFragment re-renders the bulk buttons for their poll, so a job
// finishing or a FIFO draining shows up without a reload.
func (s *Server) queueActionsFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "queue_actions", s.queueActions())
}

func (s *Server) queueRows(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"CSRFToken": s.csrfToken(sessionFromContext(r.Context()))}
	s.fillQueue(r, data)
	s.render(w, "queue_rows", data)
}

// queueRowItems renders one group's items, fetched when a finished job's row is
// expanded so the poll does not carry every item of every job.
func (s *Server) queueRowItems(w http.ResponseWriter, r *http.Request) {
	root, err := strconv.ParseInt(r.PathValue("root"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	for _, g := range groupJobs(jobs) {
		if g.Root == root {
			s.fillItems(&g)
			s.render(w, "queue_items_capped", map[string]any{"Items": g.Items, "MonbooruURL": s.monbooruWebBase(), "Site": g.Lead.Site, "ImageID": g.Lead.ImageID, "Kind": g.Lead.Kind})
			return
		}
	}
	http.NotFound(w, r)
}

// jobGroup collapses a continue-series (a capped search and its continuations)
// into one queue row: the newest window leads, with counts and items summed
// across the windows.
type jobGroup struct {
	Root    int64
	Lead    *queue.Job
	Summary queue.Summary
	// IDs are the jobs behind the row, oldest window first; Items is filled
	// from them for the rows that actually render.
	IDs   []int64
	Items []queue.Item
	// NextWindow is how many posts the continue action would fetch, filled by
	// fillQueue since it depends on the current config.
	NextWindow int
}

// Windows is how many jobs the row collapses, so the remove confirm can say
// how many one click drops.
func (g jobGroup) Windows() int { return len(g.IDs) }

// ArchiveSkips counts the items gallery-dl passed over because its archive
// already held them - the only ones a force download can fetch again. A file
// monbooru cannot ingest is skipped too, but re-downloading it changes nothing.
func (g jobGroup) ArchiveSkips() int {
	n := 0
	for _, it := range g.Items {
		if it.Outcome == queue.OutcomeSkippedArchive {
			n++
		}
	}
	return n
}

// Phase labels a group's progress next to the row's item count: "downloading"
// while any item is still waiting to be fetched, "pushing" once the files are
// down and going to monbooru, "finished" once the series has stopped running.
func (g jobGroup) Phase() string {
	if g.Lead == nil {
		return ""
	}
	if g.Lead.Status != queue.JobRunning && g.Lead.Status != queue.JobQueued {
		return "finished"
	}
	for _, it := range g.Items {
		if it.Status == queue.ItemPending {
			return "downloading"
		}
	}
	for _, it := range g.Items {
		if it.Status == queue.ItemDownloaded || it.Status == queue.ItemUploaded {
			return "pushing"
		}
	}
	return "downloading"
}

// groupJobs buckets a newest-first job list by series, keeping newest-first
// order between groups and oldest-first windows within each. The jobs are
// snapshots the caller owns, so a single-window group adopts its item slice
// rather than copying it again; a list taken with OmitItems carries none and
// the rendered rows fill theirs through fillItems.
func groupJobs(jobs []*queue.Job) []jobGroup {
	groups := make([]jobGroup, 0, len(jobs))
	at := map[int64]int{}
	for _, j := range jobs {
		root := j.Root
		if root == 0 {
			root = j.ID
		}
		if i, ok := at[root]; ok {
			g := &groups[i]
			g.IDs = append([]int64{j.ID}, g.IDs...)
			g.Items = slices.Concat(j.Items, g.Items)
			g.Summary = g.Summary.Add(j.Summary)
			continue
		}
		at[root] = len(groups)
		groups = append(groups, jobGroup{
			Root:    root,
			Lead:    j,
			Summary: j.Summary,
			IDs:     []int64{j.ID},
			Items:   j.Items,
		})
	}
	return groups
}

// fillItems materializes a row's items from the jobs behind it, oldest window
// first. Only the rows about to render call it, so the 2 s poll no longer
// copies the items of every tracked job to show twenty of them.
func (s *Server) fillItems(g *jobGroup) {
	for _, id := range g.IDs {
		if j, err := s.queue.Get(id); err == nil {
			g.Items = append(g.Items, j.Items...)
		}
	}
}

// fillQueue adds the grouped job list and the monbooru web base (for image
// links) to the template data.
func (s *Server) fillQueue(r *http.Request, data map[string]any) {
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	groups := groupJobs(jobs)
	page, totalPages := pageWindow(r, len(groups))
	lo := (page - 1) * pageSize
	shown := groups[lo:min(lo+pageSize, len(groups))]
	configured := s.cfg.Current().Downloader.MaxItemsPerJob
	for i := range shown {
		shown[i].NextWindow = nextWindow(shown[i].Lead.Cap, configured)
		s.fillItems(&shown[i])
	}
	data["Groups"] = shown
	data["Page"] = page
	data["TotalPages"] = totalPages
	data["MonbooruURL"] = s.monbooruWebBase()
	data["Lookup"] = s.lookupStatus()
}

// nextWindow is how many posts a continue on a capped row would actually
// fetch. The follow-up job carries the series' cap but the download takes the
// smaller of that and the configured one, so a cap lowered since would leave
// the button promising more than it delivers.
func nextWindow(seriesCap, configured int) int {
	if configured > 0 && configured < seriesCap {
		return configured
	}
	return seriesCap
}

// pageWindow reads the ?page= param and clamps it to [1, totalPages] for a
// list of n rows split into pageSize-row pages.
func pageWindow(r *http.Request, n int) (page, totalPages int) {
	totalPages = max((n+pageSize-1)/pageSize, 1)
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	return min(max(page, 1), totalPages), totalPages
}

// monbooruWebBase is the browser-facing monbooru base for image links.
func (s *Server) monbooruWebBase() string {
	return s.cfg.Current().Monbooru.WebBase()
}

// monbooruWebLink is the base for the footer "connected to monbooru" link, or
// "" when no web_url is set: unlike the image links it never falls back to
// api_url, which is an internal address that would not resolve from a browser.
func (s *Server) monbooruWebLink() string {
	return strings.TrimRight(s.cfg.Current().Monbooru.WebURL, "/")
}

// jobAction parses the row's {id}, runs one queue action on it, and re-renders
// the rows. A row that is no longer tracked says so in the add bar, like the
// API's 404; any other refusal just re-renders (the poll reports the state).
func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, action func(id int64) error) {
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		if errors.Is(action(id), queue.ErrNotFound) {
			addBarFlash(w, "that job is no longer in the queue")
			return
		}
	}
	s.queueRows(w, r)
}

// addBarFlash answers a refused row action in the add bar's flash slot instead
// of swapping the unchanged rows back in as if the click had worked.
func addBarFlash(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#add-flash")
	w.Header().Set("HX-Reswap", "innerHTML")
	flashFragment(w, "err", msg)
}

// retryJob re-queues a finished job. With ?force=1 the re-run bypasses the
// download-archive so a post already fetched (e.g. since deleted in monbooru)
// is downloaded again; that button is offered on the collapsed row's archive
// skips, which are the whole series', so it re-runs every window. A plain retry
// stays on the window whose run did not finish cleanly.
func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	if msg := s.monbooruBlocked(); msg != "" {
		addBarFlash(w, msg)
		return
	}
	s.jobAction(w, r, func(id int64) error {
		if r.URL.Query().Get("force") == "1" {
			return s.queue.RetrySeries(id, true)
		}
		return s.queue.Retry(id, false)
	})
}

// continueJob enqueues a follow-up job for the next window of a capped job, so
// the user can keep pulling a truncated search past the per-job cap.
func (s *Server) continueJob(w http.ResponseWriter, r *http.Request) {
	s.continueAction(w, r, s.queue.Continue)
}

// continueAllJob starts a fetch-all chain: the queue keeps pulling the next
// window until the capped search runs short, instead of one click per window.
func (s *Server) continueAllJob(w http.ResponseWriter, r *http.Request) {
	s.continueAction(w, r, s.queue.ContinueAll)
}

// continueAction runs one continue variant and re-renders the rows. A series
// that ran out between the render and the click is reported in the add bar's
// flash, like the API's 409, instead of swapping the unchanged row back in as
// if the click had queued something.
func (s *Server) continueAction(w http.ResponseWriter, r *http.Request, run func(id int64) (int64, error)) {
	if msg := s.monbooruBlocked(); msg != "" {
		addBarFlash(w, msg)
		return
	}
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		switch _, err := run(id); {
		case errors.Is(err, queue.ErrNotCapped):
			addBarFlash(w, "this search has no more items to fetch")
			return
		case errors.Is(err, queue.ErrNotFound):
			addBarFlash(w, "that job is no longer in the queue")
			return
		}
	}
	s.queueRows(w, r)
}

// cancelJob stops a queued or running row and its series. It refuses a job that
// has already finished, unlike deleteJob: the row's cancel and remove labels
// differ only by the state the last poll saw, so a cancel clicked on a row that
// finished in the 2 s gap must not delete its history instead.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, s.queue.CancelLive)
}

// deleteJob removes a queue row, cancelling it first when it is still live. The
// row collapses a continue-series, so it clears every window in the series, not
// just the one clicked.
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, s.queue.CancelSeries)
}

// clearQueue drops the finished-job history; running and pending jobs stay.
func (s *Server) clearQueue(w http.ResponseWriter, r *http.Request) {
	s.queue.Clear()
	s.queueRows(w, r)
}

// cancelPendingJobs empties the FIFO of jobs that have not started;
// running jobs and history stay.
func (s *Server) cancelPendingJobs(w http.ResponseWriter, r *http.Request) {
	s.queue.CancelPending()
	s.queueRows(w, r)
}

// pauseDownloads holds the queue and re-renders the topbar control.
func (s *Server) pauseDownloads(w http.ResponseWriter, r *http.Request) {
	s.queue.Pause()
	s.renderPauseToggle(w, r)
}

// resumeDownloads lifts the hold and re-renders the topbar control.
func (s *Server) resumeDownloads(w http.ResponseWriter, r *http.Request) {
	s.queue.Resume()
	s.renderPauseToggle(w, r)
}

// queuePauseToggle re-renders the topbar control for its poll, so a pause set
// elsewhere (e.g. from monsender) shows here without a reload.
func (s *Server) queuePauseToggle(w http.ResponseWriter, r *http.Request) {
	s.renderPauseToggle(w, r)
}

func (s *Server) renderPauseToggle(w http.ResponseWriter, r *http.Request) {
	s.render(w, "pause_toggle", map[string]any{
		"Paused":    s.queue.Paused(),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

// monbooruStatus renders the footer connectivity light from a cached probe.
func (s *Server) monbooruStatus(w http.ResponseWriter, r *http.Request) {
	status, version := s.monbooruStatusCached(r.Context())
	s.renderConnLight(w, status, version)
}

// monbooruPause holds the link from the footer light's kill switch: the probe
// stops and the add bar refuses new work, while the pairing stays on disk so
// resuming needs no re-pair.
func (s *Server) monbooruPause(w http.ResponseWriter, r *http.Request) {
	s.setMonbooruPaused(w, true)
}

// monbooruResume lifts the hold. The light renders "checking" and its own poll
// probes monbooru within the second.
func (s *Server) monbooruResume(w http.ResponseWriter, r *http.Request) {
	s.setMonbooruPaused(w, false)
}

func (s *Server) setMonbooruPaused(w http.ResponseWriter, paused bool) {
	if err := s.updateConfig(func(c *config.Config) error { c.Monbooru.Paused = paused; return nil }); err != nil {
		logx.Errorf("monbooru: could not persist the link hold: %v", err)
	}
	status := ""
	if paused {
		status = "paused"
	}
	s.renderConnLight(w, status, "")
}

func (s *Server) renderConnLight(w http.ResponseWriter, status, version string) {
	s.render(w, "conn_light", map[string]any{
		"Conn":            status,
		"MonbooruWebURL":  s.monbooruWebLink(),
		"MonbooruVersion": version,
		"MonbooruPaired":  s.hasPairedToken("monbooru"),
	})
}

// siteRow is one configured site as the settings table shows it. CSRFToken
// rides along so the shared row partial can post the test probe and the edit
// dialog. LastReached is the most recent successful test or fetch, shown in
// the state cell (zero = never reached this run). CustomProfile marks a row
// whose user profile file shadows (or adds to) the shipped set.
type siteRow struct {
	Category      string
	Login         string
	Auth          string
	NeedsCred     bool
	CustomProfile bool
	// CookieCount is the configured cookies file's cookie-line count - the
	// only thing the UI reveals about the file's contents.
	CookieCount int
	// AuthSet names the credentials the block holds ("username, api key
	// set"), never their values.
	AuthSet     string
	Site        *config.Site
	CSRFToken   string
	LastReached time.Time
}

// siteRows builds the settings table rows for a list of categories.
func (s *Server) siteRows(cats []string, csrf string) []siteRow {
	rows := make([]siteRow, 0, len(cats))
	for _, cat := range cats {
		// The effective auth kind: the profile's, or for a profile-less site
		// the supportedsites seed, so the row and dialog know what it needs.
		auth := s.effectiveAuth(cat)
		site := s.cfg.Current().FindSite(cat)
		label, needs := loginInfo(auth, site)
		row := siteRow{
			Category: cat, Login: label, Auth: auth, NeedsCred: needs,
			CustomProfile: s.mapper.CustomProfile(cat), AuthSet: authSet(site),
			Site: site, CSRFToken: csrf,
			LastReached: s.siteState.LastReached(cat),
		}
		if site != nil && site.Cookies != "" {
			row.CookieCount = countCookies(site.Cookies)
		}
		rows = append(rows, row)
	}
	return rows
}

// authSet names the credentials a site block holds - never their values -
// for the sites table's auth column.
func authSet(site *config.Site) string {
	if site == nil {
		return "-"
	}
	var parts []string
	for _, c := range []struct{ value, label string }{
		{site.Username, "username"}, {site.Password, "password"},
		{site.APIKey, "api key"}, {site.UserID, "user id"}, {site.Cookies, "cookies"},
	} {
		if c.value != "" {
			parts = append(parts, c.label)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ") + " set"
}

// visibleSites returns the categories the sites tables show, grouped by
// effective kind: the sites whose [[sites]] block carries user-set data plus
// those with a differing user profile. A bare or lookup-order-only block (the
// seeded chain) is not a customization and surfaces no row; a category
// without any profile is grouped with the other sites.
func (s *Server) visibleSites() (boorus, manga, other []string) {
	names := map[string]bool{}
	for _, site := range s.cfg.Current().Sites {
		if site.HasUserData() {
			names[site.Name] = true
		}
	}
	for _, cat := range s.mapper.CustomCategories() {
		names[cat] = true
	}
	cats := slices.Sorted(maps.Keys(names))
	for _, cat := range cats {
		p, ok := s.mapper.Lookup(cat)
		switch {
		case ok && p.Kind == mapping.KindManga:
			manga = append(manga, cat)
		case !ok || p.Kind == mapping.KindOther:
			other = append(other, cat)
		default:
			boorus = append(boorus, cat)
		}
	}
	return boorus, manga, other
}

func (s *Server) settingsScreen(w http.ResponseWriter, r *http.Request) {
	data := s.settingsData(r)
	if msg := r.URL.Query().Get("msg"); msg != "" {
		kind := r.URL.Query().Get("kind")
		if kind == "" {
			kind = "ok"
		}
		setFlash(data, kind, r.URL.Query().Get("section"), msg)
	}
	s.render(w, "settings", data)
}

// setFlash puts one flash on a settings render, scoped to the section whose box
// shows it.
func setFlash(data map[string]any, kind, section, msg string) {
	data["Flash"] = msg
	data["FlashKind"] = kind
	data["FlashSection"] = section
}

// settingsData assembles the settings page's view. Split from the screen so a
// save that has to refuse can re-render the page with the operator's submitted
// value instead of redirecting back to the stored one.
func (s *Server) settingsData(r *http.Request) map[string]any {
	data := s.base(r, "settings", "Settings - "+s.titleName())
	data["Cfg"] = s.cfg.Current()

	if galleries, ok := s.galleries(r); ok {
		data["Galleries"] = galleries
		if warn := defaultGalleryWarning(s.cfg.Current().Monbooru.DefaultGallery, galleries); warn != "" {
			data["GalleryWarn"] = warn
		}
	}

	csrf := s.csrfToken(sessionFromContext(r.Context()))
	boorus, manga, other := s.visibleSites()
	data["BooruSites"] = s.siteRows(boorus, csrf)
	data["MangaSites"] = s.siteRows(manga, csrf)
	data["OtherSites"] = s.siteRows(other, csrf)
	data["LookupPanel"] = s.lookupPanel(csrf)
	ptrStatus := s.ptr.Status()
	data["PTRDiskBytes"] = ptrStatus.DiskBytes
	data["PTREnabled"] = ptrStatus.Enabled
	data["Stats"] = s.gatherStats()
	data["MonbooruPaired"] = s.hasPairedToken("monbooru")
	data["MonbooruPairWaiting"] = s.getPairAttempt() != nil
	data["MonsenderPending"] = s.pairs.listPending()
	data["MonsenderPaired"] = s.hasPairedToken("monsender")
	return data
}

// galleries lists monbooru's galleries for the settings dropdown and the site
// dialog, under one short budget: the page must render even when monbooru is
// slow or down, and ok=false says only that the list is unavailable - a
// monbooru with no galleries at all is a successful empty answer.
func (s *Server) galleries(r *http.Request) ([]monbooru.Gallery, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	galleries, err := s.client.ListGalleries(ctx)
	return galleries, err == nil
}

// defaultGalleryWarning flags a default gallery pushes will not reach: unset
// (downloads fall back to monbooru's active gallery) or a name monbooru lacks.
func defaultGalleryWarning(name string, galleries []monbooru.Gallery) string {
	if name == "" {
		return "no default gallery set - downloads use monbooru's active gallery; pick one to set a fixed target"
	}
	for _, g := range galleries {
		if g.Name == name {
			return ""
		}
	}
	return "gallery \"" + name + "\" is not in monbooru - pushes will be rejected until it exists"
}

// renderDefaultGalleryOOB re-renders the default-gallery field out of band so it
// appears the moment a monbooru pairing completes, without a page reload.
func (s *Server) renderDefaultGalleryOOB(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"OOB":            true,
		"Paired":         s.hasPairedToken("monbooru"),
		"DefaultGallery": s.cfg.Current().Monbooru.DefaultGallery,
	}
	if galleries, ok := s.galleries(r); ok {
		data["Galleries"] = galleries
		if warn := defaultGalleryWarning(s.cfg.Current().Monbooru.DefaultGallery, galleries); warn != "" {
			data["GalleryWarn"] = warn
		}
	}
	s.render(w, "monbooru_gallery", data)
}

// statsData backs the settings Stats section: process memory, the bundled
// gallery-dl, and the in-memory queue.
type statsData struct {
	Mem        memStats
	GDLVersion string
	Extractors int
	Queue      queueStats
}

// memStats is the process memory view. RSS is the resident set (what is
// actually in use, and what drops after a job frees its buffers); Sys is the
// runtime's reserved address space (a high-water mark that never shrinks), kept
// only as a fallback where RSS is unavailable.
type memStats struct {
	RSS        int64
	Sys        int64
	HeapAlloc  int64
	Goroutines int
}

// readRSS returns the process resident set size from /proc, or 0 when it is
// unavailable (non-Linux).
func readRSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				kb, _ := strconv.ParseInt(fields[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

type queueStats struct {
	Workers  int
	Queued   int
	Running  int
	Finished int
}

// gatherStats snapshots runtime memory, gallery-dl, and queue counts for the
// Stats section.
func (s *Server) gatherStats() statsData {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := statsData{
		Mem:        memStats{RSS: readRSS(), Sys: int64(ms.Sys), HeapAlloc: int64(ms.HeapAlloc), Goroutines: runtime.NumGoroutine()},
		GDLVersion: s.gdlVersion,
		Extractors: len(s.extractors),
		Queue:      s.queueCounts(),
	}
	// The running worker count, not the saved setting: concurrency takes
	// effect only on restart, so report what is actually running.
	st.Queue.Workers = s.queue.Workers()
	return st
}

// loginInfo maps a profile auth kind to a settings label and whether a
// required credential is missing (per the shared rule the lookup walk also
// gates on).
func loginInfo(auth string, site *config.Site) (string, bool) {
	switch auth {
	case mapping.AuthAPIOptional:
		return "api (opt)", false
	case mapping.AuthAPIRequired, mapping.AuthCookies:
		return mapping.RequiredCredential(auth, site)
	case mapping.AuthUsernamePassword:
		return "user/pass", false
	case mapping.AuthOAuth:
		return "oauth", false
	default:
		return "none", false
	}
}

// redirectFlash sends the operator back to settings with a flash. The section
// is derived from the form's path so the message renders at the top of that
// section's box (and the #anchor scrolls to it), not at the top of the page.
func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	section := sectionForPath(r.URL.Path)
	loc := "/settings?kind=" + kind + "&section=" + section + "&msg=" + url.QueryEscape(msg)
	if section != "" {
		loc += "#" + section
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// sectionForPath maps a settings form's path to its section id.
func sectionForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/settings/monbooru"):
		return "monbooru"
	case strings.HasPrefix(path, "/settings/downloader"):
		return "downloads"
	case strings.HasPrefix(path, "/settings/lookup"):
		return "lookup"
	case strings.HasPrefix(path, "/settings/ptr"):
		return "ptr"
	case strings.HasPrefix(path, "/settings/sites"), strings.HasPrefix(path, "/settings/host-labels"):
		return "sites"
	case strings.HasPrefix(path, "/settings/raw"):
		return "advanced"
	}
	return ""
}

func (s *Server) saveMonbooru(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	apiURL := strings.TrimSpace(r.FormValue("api_url"))
	webURL := strings.TrimSpace(r.FormValue("web_url"))
	// Either may be blank - an empty api url is "no monbooru configured" - but a
	// value that is not an http(s) URL breaks every push, and reporting that as
	// saved sends the operator hunting through queue error codes instead of at
	// the field they just edited.
	var bad []string
	for _, f := range []struct{ label, value string }{{"api url", apiURL}, {"web url", webURL}} {
		if f.value != "" && !config.IsHTTPURL(f.value) {
			bad = append(bad, f.label+" must be an http(s) URL")
		}
	}
	if len(bad) > 0 {
		s.redirectFlash(w, r, "err", strings.Join(bad, "; ")+" - nothing was saved")
		return
	}
	err := s.updateConfig(func(c *config.Config) error {
		c.Monbooru.APIURL = apiURL
		c.Monbooru.WebURL = webURL
		c.Monbooru.DefaultGallery = strings.TrimSpace(r.FormValue("default_gallery"))
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "monbooru settings saved")
}

func (s *Server) testMonbooru(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	tmp := *s.cfg.Current()
	if v := strings.TrimSpace(r.FormValue("api_url")); v != "" {
		tmp.Monbooru.APIURL = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := monbooru.New(config.NewProvider(&tmp)).TestConnection(ctx); err != nil {
		flashFragment(w, "err", "connection failed: "+err.Error())
		return
	}
	// An htmx swap into the result slot, not a redirect, so the form's unsaved
	// values survive for a following save rather than being blanked by a reload.
	flashFragment(w, "ok", "monbooru reachable - save to keep these settings")
}

func (s *Server) saveDownloader(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	// A field the form did not post keeps its stored value; one that was posted
	// and does not parse - cleared included, since these knobs have no "unset" -
	// is refused by name, so the flash cannot report success while the old value
	// quietly stands.
	var bad []string
	num := func(name, label string, floor int) (int, bool) {
		if !r.Form.Has(name) {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
		if err != nil || n < floor {
			bad = append(bad, fmt.Sprintf("%s must be a whole number of %d or more", label, floor))
			return 0, false
		}
		return n, true
	}
	concurrency, setConcurrency := num("concurrency", "concurrency", 1)
	maxItems, setMaxItems := num("max_items_per_job", "max items / job", 1)
	// Zero is meaningful here (keep history until the ring evicts it), so
	// unlike the caps above it is accepted rather than treated as unset.
	retention, setRetention := num("history_retention_days", "clear history after (days)", 0)
	// monbooru refuses an absolute folder or one carrying a .. segment, so such
	// a value reports "saved" and then fails every push with no pointer back to
	// the field; refuse it here instead.
	folder, setFolder := "", false
	if r.Form.Has("default_folder") {
		folder = strings.TrimSpace(r.FormValue("default_folder"))
		if strings.HasPrefix(folder, "/") || slices.Contains(strings.Split(folder, "/"), "..") {
			bad = append(bad, "default folder must be a relative path with no .. segment")
		} else {
			setFolder = true
		}
	}
	sleep, setSleep := 0.0, false
	if r.Form.Has("sleep_request") {
		if f, ferr := strconv.ParseFloat(strings.TrimSpace(r.FormValue("sleep_request")), 64); ferr == nil && f >= 0 {
			sleep, setSleep = f, true
		} else {
			bad = append(bad, "sleep / request must be a number of 0 or more")
		}
	}
	if len(bad) > 0 {
		s.redirectFlash(w, r, "err", strings.Join(bad, "; ")+" - nothing was saved")
		return
	}
	err := s.updateConfig(func(c *config.Config) error {
		if setConcurrency {
			c.Downloader.Concurrency = concurrency
		}
		if setSleep {
			c.GalleryDL.SleepRequest = sleep
		}
		if setMaxItems {
			c.Downloader.MaxItemsPerJob = maxItems
		}
		if setRetention {
			c.Downloader.HistoryRetentionDays = retention
		}
		if setFolder {
			c.Downloader.DefaultFolder = folder
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.queue.SetRetention(s.cfg.Current().Downloader.HistoryRetention())
	s.redirectFlash(w, r, "ok", "download settings saved")
}

// parseLookupOrder reads one chain-dialog position field: blank or 0 opts the
// source out, a positive integer is its chain position, anything else is an
// error.
func parseLookupOrder(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("lookup order must be a positive number or blank")
	}
	return n, nil
}

// resetSite drops a site's [[sites]] block - credentials, gallery, cookies
// path, options - reverting it to the profile defaults. The remove button
// only shows for sites that have a block to drop; a row with neither a block
// nor a custom profile disappears from the tables.
func (s *Server) resetSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.updateConfig(func(c *config.Config) error {
		c.Sites = slices.DeleteFunc(c.Sites, func(s config.Site) bool { return s.Name == name })
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.redirectFlash(w, r, "ok", "site "+name+" removed")
}

// testSite probes a site live and renders the outcome into the site's own
// state cell.
func (s *Server) testSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	probeURL := s.mapper.ExampleURL(s.extractors, name)
	if probeURL == "" {
		siteState(w, "err", "no example URL", "", time.Time{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.runner.Probe(ctx, probeURL)
	if err != nil {
		siteState(w, "err", "failed", err.Error(), time.Time{})
		return
	}
	if res.Status == gdl.ProbeOK {
		s.siteState.Reached(name, time.Now())
		siteState(w, "ok", "ok", "", s.siteState.LastReached(name))
		return
	}
	// A site that still lacks a credential it requires is the most actionable
	// diagnosis: report "needs cookies"/"needs api key" even when a cookies
	// site's gallery-dl error (a generic "not found") cannot be classified as
	// auth. Otherwise distinguish a bot-protection wall from a plain failure.
	p, _ := s.mapper.Lookup(name)
	if label, needs := loginInfo(p.Auth, s.cfg.Current().FindSite(name)); needs {
		siteState(w, "warn", "needs "+label, res.Detail, time.Time{})
		return
	}
	switch res.Status {
	case gdl.ProbeBlocked:
		siteState(w, "err", "blocked", res.Detail, time.Time{})
	case gdl.ProbeAuthRequired:
		// The required credential is present (the needs check above passed), so
		// the booru refused the credential itself - say "rejected", not the
		// "auth required" that reads as a missing key.
		siteState(w, "warn", "auth rejected", res.Detail, time.Time{})
	default:
		siteState(w, "err", "failed", res.Detail, time.Time{})
	}
}

// siteState writes a per-row test outcome swapped into a site's state cell: a
// colored status word with the failure detail on hover, followed by the muted
// "last reached" time when known. Landing the result in the tested row (not a
// shared flash) keeps probing several sites in a row legible - each row shows
// its own state.
func siteState(w http.ResponseWriter, kind, msg, detail string, lastReached time.Time) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if detail != "" {
		fmt.Fprintf(w, `<span class="flash-%s" title="%s">%s</span>`, kind, html.EscapeString(detail), html.EscapeString(msg))
	} else {
		fmt.Fprintf(w, `<span class="flash-%s">%s</span>`, kind, html.EscapeString(msg))
	}
	if !lastReached.IsZero() {
		fmt.Fprintf(w, ` <span class="site-last" title="last reached %s">%s</span>`, stampLocal(lastReached), humanSince(lastReached))
	}
}

func (s *Server) saveRaw(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	raw := r.FormValue("raw_config")
	if err := config.ValidateRawConfig(raw); err != nil {
		// The textarea is the only copy of a hand-written block, so the refusal
		// re-renders it as typed instead of redirecting to the stored config.
		data := s.settingsData(r)
		draft := *s.cfg.Current()
		draft.GalleryDL.RawConfig = raw
		data["Cfg"] = &draft
		setFlash(data, "err", "advanced", err.Error())
		s.render(w, "settings", data)
		return
	}
	if err := s.updateConfig(func(c *config.Config) error { c.GalleryDL.RawConfig = raw; return nil }); err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.redirectFlash(w, r, "ok", "raw config saved")
}
