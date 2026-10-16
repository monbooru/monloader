package web

import (
	"cmp"
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
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/logx"
)

func (s *Server) addScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "add", s.titleName())
	data["Lookup"] = s.lookupStatus()
	data["ItemChoices"] = s.itemsChoices(0)
	s.render(w, "add", data)
}

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
		items, ok := parseItems(r.FormValue("items"))
		if !ok {
			flashFragment(w, "err", "pick how many items to fetch")
			return
		}
		s.queue.Enqueue(target, queue.Options{}.WithItems(items))
	}
	// A redirect would reload the queue screen and drop its
	// expand/collapse state.
	if onQueueScreen(r) {
		w.Header().Set("HX-Retarget", "#queue-rows")
		w.Header().Set("HX-Reswap", "innerHTML")
		s.queueRows(w, r)
		return
	}
	w.Header().Set("HX-Redirect", "/queue")
}

// The controls are disabled client-side too; this guards a stale page.
func (s *Server) monbooruBlocked() string {
	if !s.monbooruConfigured() {
		return "monbooru is not configured - set its connection in settings"
	}
	if s.monbooruPaused() {
		return "the monbooru link is paused - resume it from the light in the footer"
	}
	return ""
}

// Lower-cased before the prefix trim: a hash pasted upper-case carries an
// upper-case prefix.
func hashImportTarget(s string) (string, bool) {
	s = strings.TrimPrefix(strings.ToLower(s), "md5:")
	if config.IsHexHash(s, 32) {
		return s, true
	}
	return "", false
}

func looksLikeSHA256(s string) bool {
	return config.IsHexHash(strings.TrimPrefix(strings.ToLower(s), "sha256:"), 64)
}

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

type itemsChoice struct {
	Value    string
	Label    string
	Selected bool
}

var itemsPresets = []int{1, 5, 10, 20, 50, 100, 200, 500, 1000}

// A pick past the cap is offered because the queue fetches it in cap-sized windows.
func (s *Server) itemsChoices(current int) []itemsChoice {
	limit := s.cfg.Current().Downloader.MaxItemsPerJob
	nums := slices.Clone(itemsPresets)
	for _, n := range []int{limit, current} {
		if n > 0 && !slices.Contains(nums, n) {
			nums = append(nums, n)
		}
	}
	slices.Sort(nums)
	out := make([]itemsChoice, 0, len(nums)+1)
	for _, n := range nums {
		c := itemsChoice{Value: strconv.Itoa(n), Label: "max " + strconv.Itoa(n), Selected: n == current}
		if n == limit {
			c.Value, c.Selected = "", current == 0 || current == limit
		}
		out = append(out, c)
	}
	return append(out, itemsChoice{Value: "all", Label: "all", Selected: current == queue.ItemsAll})
}

// "" keeps the cap.
func parseItems(v string) (int, bool) {
	switch v {
	case "":
		return 0, true
	case "all":
		return queue.ItemsAll, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n > 0
}

type lookupStatusView struct {
	BooruSources []string
	PTREnabled   bool
	PTRSyncing   bool
}

func (s *Server) lookupStatus() lookupStatusView {
	st := s.ptr.Status()
	view := lookupStatusView{
		PTREnabled: st.Enabled,
		PTRSyncing: st.Enabled && !st.Queryable,
	}
	for _, src := range s.mapper.LookupChain() {
		switch {
		case !s.lookupQueryable(src):
		case src.Similarity:
			view.BooruSources = append(view.BooruSources, src.Name+" (similarity)")
		default:
			view.BooruSources = append(view.BooruSources, src.Name)
		}
	}
	return view
}

// The walk skips a source whose credential is missing; the add bar and the
// API's chain both report only the ones it would query.
func (s *Server) lookupQueryable(src mapping.LookupSource) bool {
	if src.Similarity {
		_, missing := s.sim.Missing(src.Name)
		return !missing
	}
	_, needs := s.siteNeedsCredential(src.Name)
	return !needs
}

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
	data["ItemChoices"] = s.itemsChoices(0)
	data["Actions"] = s.queueActions()
	s.render(w, "queue", data)
}

type queueActionsView struct {
	HasPending   bool
	HasFinished  bool
	HasSucceeded bool
}

func (s *Server) queueActions() queueActionsView {
	counts := s.queueCounts()
	return queueActionsView{
		HasPending:   counts.Queued > 0,
		HasFinished:  counts.Finished > 0,
		HasSucceeded: s.queue.ClearableSucceeded() > 0,
	}
}

// Without OmitItems the snapshot deep-copies every job's items.
func (s *Server) queueCounts() queueStats {
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	var st queueStats
	// Counted per row, as the queue page shows them, so both screens
	// agree on a continued search.
	for _, g := range groupJobs(jobs) {
		switch g.Worst.Status {
		case queue.JobQueued:
			st.Queued++
		case queue.JobRunning:
			st.Running++
		default:
			st.Finished++
		}
	}
	return st
}

func (s *Server) queueActionsFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "queue_actions", s.queueActions())
}

func (s *Server) queueRows(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"CSRFToken": s.sessions.CSRFToken(sessionFromContext(r.Context()))}
	s.fillQueue(r, data)
	s.render(w, "queue_rows", data)
}

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
			s.render(w, "queue_items_capped", map[string]any{"Items": g.Items, "MonbooruURL": s.monbooruWebBase(), "Site": g.Lead.Site, "ImageID": g.Lead.ImageID, "Kind": g.Lead.Kind, "JobURL": g.Lead.URL, "PageURL": g.Lead.PageURL})
			return
		}
	}
	http.NotFound(w, r)
}

type jobGroup struct {
	Root    int64
	Lead    *queue.Job
	Summary queue.Summary
	// The row reports on its most demanding window, so a clean
	// continuation cannot bury an earlier one that needs re-running.
	Worst *queue.Job
	// Not the lead: a cancel restamps its window as the newest activity.
	// Any capped window is a safe target; the follow-up starts past the
	// series' furthest reach.
	Continue *queue.Job
	IDs      []int64
	Items    []queue.Item
	// Summed from the snapshots: a finished row's Items stay empty until
	// it is expanded.
	Count      int
	NextWindow int
	widest     int
	Watchable  bool
	Watched    bool
}

var rowStatusOrder = []queue.JobStatus{queue.JobRunning, queue.JobQueued, queue.JobFailed, queue.JobPartial, queue.JobInterrupted, queue.JobCanceled}

// Anything else, succeeded included, ranks last.
func rowStatusRank(s queue.JobStatus) int {
	if i := slices.Index(rowStatusOrder, s); i >= 0 {
		return i
	}
	return len(rowStatusOrder)
}

func continuable(j *queue.Job) bool {
	return j.Capped && j.Status != queue.JobCanceled && j.Status != queue.JobFailed
}

func (g jobGroup) Live() bool {
	return g.Worst.Status == queue.JobQueued || g.Worst.Status == queue.JobRunning
}

func (g jobGroup) Windows() int { return len(g.IDs) }

// Only archive skips: re-downloading a type monbooru cannot ingest
// changes nothing.
func (g jobGroup) ArchiveSkips() int { return g.Summary.Archived }

func (g jobGroup) Phase() string {
	if g.Lead == nil {
		return ""
	}
	if !g.Live() {
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

// jobs must be newest first and snapshots the caller owns: a
// single-window group keeps its item slice.
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
			g.Count += j.ItemCount()
			if rowStatusRank(j.Status) < rowStatusRank(g.Worst.Status) {
				g.Worst = j
			}
			g.widest = max(g.widest, j.Cap)
			if continuable(j) && (g.Continue == nil || j.ID > g.Continue.ID) {
				g.Continue = j
			}
			continue
		}
		at[root] = len(groups)
		g := jobGroup{
			Root:    root,
			Lead:    j,
			Worst:   j,
			Summary: j.Summary,
			IDs:     []int64{j.ID},
			Items:   j.Items,
			Count:   j.ItemCount(),
			widest:  j.Cap,
		}
		if continuable(j) {
			g.Continue = j
		}
		groups = append(groups, g)
	}
	return groups
}

func (s *Server) fillItems(g *jobGroup) {
	for _, id := range g.IDs {
		if j, err := s.queue.Get(id); err == nil {
			g.Items = append(g.Items, j.Items...)
		}
	}
}

func (s *Server) fillQueue(r *http.Request, data map[string]any) {
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	groups := groupJobs(jobs)
	page, totalPages := pageWindow(r, len(groups))
	lo := (page - 1) * pageSize
	shown := groups[lo:min(lo+pageSize, len(groups))]
	configured := s.cfg.Current().Downloader.MaxItemsPerJob
	watched := s.watchedURLs()
	for i := range shown {
		shown[i].Watched = watched[shown[i].Lead.URL]
		shown[i].Watchable = !shown[i].Live() && s.watchableJob(shown[i].Lead, shown[i].Summary.Total)
		if shown[i].Continue != nil {
			shown[i].NextWindow = nextWindow(shown[i].widest, configured)
		}
		// A finished row loads its items on expand, so the poll does not
		// copy them out from under the queue lock.
		if shown[i].Live() {
			s.fillItems(&shown[i])
		}
	}
	data["Groups"] = shown
	data["Page"] = page
	data["TotalPages"] = totalPages
	data["MonbooruURL"] = s.monbooruWebBase()
	data["Lookup"] = s.lookupStatus()
}

func nextWindow(seriesCap, configured int) int {
	if configured > 0 && configured < seriesCap {
		return configured
	}
	return seriesCap
}

// FormValue: htmx sends the inherited hx-vals in the body on a POST and
// in the query on the poll.
func pageWindow(r *http.Request, n int) (page, totalPages int) {
	totalPages = max((n+pageSize-1)/pageSize, 1)
	page, _ = strconv.Atoi(r.FormValue("page"))
	return min(max(page, 1), totalPages), totalPages
}

func (s *Server) monbooruWebBase() string {
	return s.cfg.Current().Monbooru.WebBase()
}

// Never falls back to api_url, an internal address a browser may not resolve.
func (s *Server) monbooruWebLink() string {
	return strings.TrimRight(s.cfg.Current().Monbooru.WebURL, "/")
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, action func(id int64) error) {
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		switch err := action(id); {
		case errors.Is(err, queue.ErrNotFound):
			addBarFlash(w, "that job is no longer in the queue")
			return
		case errors.Is(err, queue.ErrNoneRetried):
			addBarFlash(w, "this download is still running - wait for it to finish")
			return
		case errors.Is(err, queue.ErrNotCapped):
			addBarFlash(w, "this search has no more items to fetch")
			return
		}
	}
	s.queueRows(w, r)
}

// Retargeted so a refusal does not swap the unchanged rows back in as if
// it worked.
func addBarFlash(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#add-flash")
	w.Header().Set("HX-Reswap", "innerHTML")
	flashFragment(w, "err", msg)
}

// force re-runs the whole series: the row's archive skips are summed
// across its windows.
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

func (s *Server) continueJob(w http.ResponseWriter, r *http.Request) {
	s.continueAction(w, r, s.queue.Continue)
}

func (s *Server) continueAllJob(w http.ResponseWriter, r *http.Request) {
	s.continueAction(w, r, s.queue.ContinueAll)
}

func (s *Server) continueAction(w http.ResponseWriter, r *http.Request, run func(id int64) (int64, error)) {
	if msg := s.monbooruBlocked(); msg != "" {
		addBarFlash(w, msg)
		return
	}
	s.jobAction(w, r, func(id int64) error {
		_, err := run(id)
		return err
	})
}

// CancelLive, not CancelSeries: a cancel clicked on a row that finished
// since the last poll must not delete its history.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, s.queue.CancelLive)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, s.queue.CancelSeries)
}

func (s *Server) then(op func(), render http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		op()
		render(w, r)
	}
}

func (s *Server) renderPauseToggle(w http.ResponseWriter, r *http.Request) {
	s.render(w, "pause_toggle", map[string]any{
		"Paused":    s.queue.Paused(),
		"CSRFToken": s.sessions.CSRFToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) monbooruStatus(w http.ResponseWriter, r *http.Request) {
	status, version := s.monbooruStatusCached(r.Context())
	s.renderConnLight(w, status, version)
}

func (s *Server) monbooruPause(w http.ResponseWriter, r *http.Request) {
	s.setMonbooruPaused(w, true)
}

func (s *Server) monbooruResume(w http.ResponseWriter, r *http.Request) {
	s.setMonbooruPaused(w, false)
}

func (s *Server) setMonbooruPaused(w http.ResponseWriter, paused bool) {
	if err := s.updateConfig(func(c *config.Config) error { c.Monbooru.Paused = paused; return nil }); err != nil {
		logx.Errorf("monbooru: could not persist the link hold: %v", err)
	}
	// What was stored, not what was asked: a failed save leaves the old
	// state in force. A live link renders checking, whose swap probes a
	// second later rather than at the next 15 s poll, on a fresh check.
	status := "paused"
	if !s.monbooruPaused() {
		status = "checking"
		s.statusMu.Lock()
		s.monbooruConn = ""
		s.statusMu.Unlock()
	}
	s.renderConnLight(w, status, "")
}

func (s *Server) renderConnLight(w http.ResponseWriter, status, version string) {
	// The poll must answer a resolved state: "checking" arms the load
	// trigger, so each swap would poll again a second later.
	s.render(w, "conn_light", map[string]any{
		"Conn":             status,
		"MonbooruWebURL":   s.monbooruWebLink(),
		"MonbooruVersion":  version,
		"MonbooruMismatch": s.monbooruAPIMismatch(),
		"MonbooruPaired":   s.hasPairedToken("monbooru"),
	})
}

type siteRow struct {
	Category      string
	Login         string
	Auth          string
	NeedsCred     bool
	CustomProfile bool
	// The only thing the UI reveals about the cookies file.
	CookieCount int
	AuthSet     string
	Site        *config.Site
	CSRFToken   string
	LastReached time.Time
}

func (s *Server) siteRows(cats []string, csrf string) []siteRow {
	rows := make([]siteRow, 0, len(cats))
	for _, cat := range cats {
		v := s.siteView(cat)
		site := s.cfg.Current().FindSite(cat)
		row := siteRow{
			Category: cat, Login: v.Login, Auth: v.Auth, NeedsCred: v.NeedsCredential,
			CustomProfile: v.CustomProfile, AuthSet: authSet(site),
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

// authSet names the credentials a site block holds, never their values.
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
		switch s.siteView(cat).Kind {
		case mapping.KindManga:
			manga = append(manga, cat)
		case mapping.KindOther:
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
		setFlash(data, cmp.Or(r.URL.Query().Get("kind"), "ok"), r.URL.Query().Get("section"), msg)
	}
	s.render(w, "settings", data)
}

func setFlash(data map[string]any, kind, section, msg string) {
	data["Flash"] = msg
	data["FlashKind"] = kind
	data["FlashSection"] = section
}

func (s *Server) settingsData(r *http.Request) map[string]any {
	data := s.base(r, "settings", "Settings - "+s.titleName())
	data["Cfg"] = s.cfg.Current()
	local := s.desktopLocal(r)
	data["DesktopProfile"] = local
	if local {
		data["DesktopFolders"] = s.availableFolders()
		data["Desktop"] = s.desktopIntegration()
		data["DesktopQuitPrompt"] = s.desktopQuitPrompt()
		data["DesktopRestartPrompt"] = s.desktopRestartPrompt()
	}
	data["Defaults"] = config.Default()
	data["MonbooruAPIHint"] = s.monbooruAPIHint()

	s.fillGalleryFields(r, data)

	csrf := s.sessions.CSRFToken(sessionFromContext(r.Context()))
	boorus, manga, other := s.visibleSites()
	data["BooruSites"] = s.siteRows(boorus, csrf)
	data["MangaSites"] = s.siteRows(manga, csrf)
	data["OtherSites"] = s.siteRows(other, csrf)
	data["LookupPanel"] = s.lookupPanel(csrf)
	data["GDLPanel"] = s.gdlPanel(r.Context(), csrf)
	ptrStatus := s.ptr.Status()
	data["PTRDiskBytes"] = ptrStatus.DiskBytes
	data["PTREnabled"] = ptrStatus.Enabled
	data["Stats"] = s.gatherStats()
	data["MonbooruPaired"] = s.hasPairedToken("monbooru")
	data["MonbooruPairWaiting"] = s.getPairAttempt() != nil
	data["MonsenderPending"] = s.pairs.ListPending()
	data["MonsenderPaired"] = s.hasPairedToken("monsender")
	// Empty except on a refused save; the fields read the typed values
	// from here first.
	data["Draft"] = map[string]string{}
	return data
}

// A short budget: the page must render when monbooru is slow or down.
// ok=false means unavailable; no galleries is an empty success.
func (s *Server) galleries(r *http.Request) ([]monbooru.Gallery, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	galleries, err := s.client.ListGalleries(ctx)
	return galleries, err == nil
}

func (s *Server) fillGalleryFields(r *http.Request, data map[string]any) {
	galleries, ok := s.galleries(r)
	if !ok {
		return
	}
	gallery := s.cfg.Current().Monbooru.DefaultGallery
	data["Galleries"] = galleries
	data["GalleryMissing"] = galleryMissing(gallery, galleries)
	if warn := defaultGalleryWarning(gallery, galleries); warn != "" {
		data["GalleryWarn"] = warn
	}
}

func defaultGalleryWarning(name string, galleries []monbooru.Gallery) string {
	if name == "" {
		return "no default gallery set - downloads use monbooru's active gallery; pick one to set a fixed target"
	}
	if galleryMissing(name, galleries) {
		return "gallery \"" + name + "\" is not in monbooru - pushes will be rejected until it exists"
	}
	return ""
}

// A stored name monbooru lacks must stay a dropdown option, or a later
// save submits the empty choice and clears it.
func galleryMissing(name string, galleries []monbooru.Gallery) bool {
	if name == "" {
		return false
	}
	return !slices.ContainsFunc(galleries, func(g monbooru.Gallery) bool { return g.Name == name })
}

func (s *Server) renderDefaultGalleryOOB(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"OOB":            true,
		"Paired":         s.hasPairedToken("monbooru"),
		"DefaultGallery": s.cfg.Current().Monbooru.DefaultGallery,
	}
	s.fillGalleryFields(r, data)
	s.render(w, "monbooru_gallery", data)
}

type statsData struct {
	Mem        memStats
	GDLVersion string
	Extractors int
	Queue      queueStats
}

// RSS drops after a job frees its buffers; Sys never shrinks and is only
// the fallback where RSS is unavailable.
type memStats struct {
	RSS        int64
	Sys        int64
	HeapAlloc  int64
	Goroutines int
}

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

func (s *Server) gatherStats() statsData {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := statsData{
		Mem:        memStats{RSS: readRSS(), Sys: int64(ms.Sys), HeapAlloc: int64(ms.HeapAlloc), Goroutines: runtime.NumGoroutine()},
		GDLVersion: s.catalog.Version(),
		Extractors: len(s.catalog.Extractors()),
		Queue:      s.queueCounts(),
	}
	// Concurrency applies on restart, so the running count, not the setting.
	st.Queue.Workers = s.queue.Workers()
	return st
}

func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	section := sectionForPath(r.URL.Path)
	loc := "/settings?kind=" + kind + "&section=" + section + "&msg=" + url.QueryEscape(msg)
	if section != "" {
		loc += "#" + section
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// Re-renders rather than redirects, so the operator keeps what they typed.
func (s *Server) refuseSave(w http.ResponseWriter, r *http.Request, msg string) {
	data := s.settingsData(r)
	data["Draft"] = draftForm(r)
	setFlash(data, "err", sectionForPath(r.URL.Path), msg)
	s.render(w, "settings", data)
}

func draftForm(r *http.Request) map[string]string {
	draft := map[string]string{}
	for name, values := range r.Form {
		if name != "_csrf" && len(values) > 0 {
			draft[name] = values[0]
		}
	}
	return draft
}

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
	case strings.HasPrefix(path, "/settings/gallerydl"), strings.HasPrefix(path, "/settings/raw"):
		return "advanced"
	case strings.HasPrefix(path, "/settings/desktop"), path == "/internal/open-folder":
		return "desktop"
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
	// Blank is allowed (an empty api url means no monbooru), but a
	// non-URL would fail every push.
	var bad []string
	for _, f := range []struct{ label, value string }{{"api url", apiURL}, {"web url", webURL}} {
		if f.value != "" && !config.IsHTTPURL(f.value) {
			bad = append(bad, f.label+" must be an http(s) URL")
		}
	}
	if len(bad) > 0 {
		s.refuseSave(w, r, strings.Join(bad, "; ")+" - nothing was saved")
		return
	}
	err := s.updateConfig(func(c *config.Config) error {
		c.Monbooru.APIURL = apiURL
		c.Monbooru.WebURL = webURL
		c.Monbooru.DefaultGallery = strings.TrimSpace(r.FormValue("default_gallery"))
		return nil
	})
	if err != nil {
		s.refuseSave(w, r, "save failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "monbooru settings saved")
}

func (s *Server) testMonbooru(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	tmp := *s.cfg.Current()
	// The stored token goes only to the address it was paired with, or a
	// typed URL would read it off this request.
	note := ""
	if v := strings.TrimSpace(r.FormValue("api_url")); v != "" && v != tmp.Monbooru.APIURL {
		tmp.Monbooru.APIURL = v
		tmp.Monbooru.APIToken = ""
		note = " (the saved token is only sent to the saved address)"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := monbooru.New(config.NewProvider(&tmp)).TestConnection(ctx); err != nil {
		flashFragment(w, "err", "connection failed: "+err.Error()+note)
		return
	}
	// A swap, not a redirect, so the form's unsaved values survive.
	flashFragment(w, "ok", "monbooru reachable - save to keep these settings")
}

func (s *Server) saveDownloader(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	// An absent field keeps its stored value; a posted one that does not
	// parse, blank included, is refused by name.
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
	// Zero is valid here: keep history until the ring evicts it.
	retention, setRetention := num("history_retention_days", "clear all history after (days)", 0)
	successRetention, setSuccessRetention := num("success_retention_days", "clear succeeded after (days)", 0)
	// monbooru refuses an absolute folder or a .. segment, which would
	// fail every push.
	folder, setFolder := "", false
	if r.Form.Has("default_folder") {
		folder = strings.TrimSpace(r.FormValue("default_folder"))
		if !config.IsRelativeFolder(folder) {
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
	// The sweeps run independently, so a succeeded window past the
	// history one would never take effect.
	cur := s.cfg.Current().Downloader
	keep := func(set bool, posted, stored int) int {
		if set {
			return posted
		}
		return stored
	}
	if history := keep(setRetention, retention, cur.HistoryRetentionDays); history > 0 &&
		keep(setSuccessRetention, successRetention, cur.SuccessRetentionDays) > history {
		bad = append(bad, "clear succeeded after must not be longer than clear all history after")
	}
	if len(bad) > 0 {
		s.refuseSave(w, r, strings.Join(bad, "; ")+" - nothing was saved")
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
		if setSuccessRetention {
			c.Downloader.SuccessRetentionDays = successRetention
		}
		if setFolder {
			c.Downloader.DefaultFolder = folder
		}
		return nil
	})
	if err != nil {
		s.refuseSave(w, r, "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.queue.SetRetention(s.cfg.Current().Downloader.HistoryRetention())
	s.queue.SetSuccessRetention(s.cfg.Current().Downloader.SuccessRetention())
	s.redirectFlash(w, r, "ok", "download settings saved")
}

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

func (s *Server) resetSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Profile first: it may refuse (the chain needs its template), and a
	// refusal must leave the block alone.
	if err := s.mapper.DeleteUserProfile(name); err != nil {
		s.redirectFlash(w, r, "err", err.Error())
		return
	}
	err := s.updateConfig(func(c *config.Config) error {
		site := c.FindSite(name)
		if site == nil {
			return nil
		}
		// The block survives to carry its chain position; a seeded chain site
		// keeps it even at 0, or a load would re-seed the default position.
		if site.LookupOrder == 0 && !config.DefaultChainSite(name) {
			c.Sites = slices.DeleteFunc(c.Sites, func(s config.Site) bool { return s.Name == name })
			return nil
		}
		*site = config.Site{Name: name, LookupOrder: site.LookupOrder}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.refreshSiteLists()
	s.redirectFlash(w, r, "ok", "site "+name+" removed")
}

func (s *Server) testSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	probeURL := s.mapper.ExampleURL(name, s.catalog.ExampleFor(name))
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
	// A missing required credential outranks a failed probe's verdict: a
	// cookies site's failure often reads as a plain "not found".
	if v := s.siteView(name); v.NeedsCredential {
		siteState(w, "warn", "needs "+v.Login, res.Detail, time.Time{})
		return
	}
	switch res.Status {
	case gdl.ProbeBlocked:
		siteState(w, "err", "blocked", res.Detail, time.Time{})
	case gdl.ProbeAuthRequired:
		siteState(w, "warn", "auth rejected", res.Detail, time.Time{})
	default:
		siteState(w, "err", "failed", res.Detail, time.Time{})
	}
}

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
		s.refuseSave(w, r, err.Error())
		return
	}
	if err := s.updateConfig(func(c *config.Config) error { c.GalleryDL.RawConfig = raw; return nil }); err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.redirectFlash(w, r, "ok", "raw config saved")
}
