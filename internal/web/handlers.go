package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/monbooru"
	"github.com/leqwin/monloader/internal/ptr"
	"github.com/leqwin/monloader/internal/queue"
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
	// With no monbooru to push to, a queued download could only fail at the
	// push step; refuse it here so the operator fixes the connection first. The
	// add bar is also disabled client-side, so this guards a stale page.
	if !s.monbooruConfigured() {
		flashFragment(w, "err", "monbooru is not configured - set its connection in settings")
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
		flashFragment(w, "err", sha256AddHint(s.ptr.Enabled()))
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
// whether the PTR is available to do that lookup at all.
func sha256AddHint(ptrEnabled bool) string {
	if ptrEnabled {
		return "a sha256 is for looking tags up from the PTR (via monbooru), not for importing here - paste an md5 or a URL"
	}
	return "a sha256 looks tags up from the PTR, which is not synced yet - enable it on the ptr page"
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
	data := s.base(r, "queue", "queue - "+s.titleName())
	s.fillQueue(r, data)
	s.render(w, "queue", data)
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
	jobs, _ := s.queue.List(queue.ListOptions{})
	for _, g := range groupJobs(jobs) {
		if g.Root == root {
			s.render(w, "queue_items_capped", map[string]any{"Items": g.Items, "MonbooruURL": s.monbooruWebBase(), "Site": g.Lead.Site})
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
	Items   []queue.Item
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
// order between groups and oldest-first items within each.
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
			g.Items = append(append([]queue.Item{}, j.Items...), g.Items...)
			g.Summary = g.Summary.Add(j.Summary)
			continue
		}
		at[root] = len(groups)
		groups = append(groups, jobGroup{
			Root:    root,
			Lead:    j,
			Summary: j.Summary,
			Items:   append([]queue.Item{}, j.Items...),
		})
	}
	return groups
}

// fillQueue adds the grouped job list and the monbooru web base (for image
// links) to the template data.
func (s *Server) fillQueue(r *http.Request, data map[string]any) {
	jobs, _ := s.queue.List(queue.ListOptions{})
	data["Groups"] = groupJobs(jobs)
	data["MonbooruURL"] = s.monbooruWebBase()
	data["Lookup"] = s.lookupStatus()
}

// monbooruWebBase is the browser-facing monbooru base for image links: the
// configured web_url when set, else the API URL.
func (s *Server) monbooruWebBase() string {
	base := s.cfg.Current().Monbooru.WebURL
	if base == "" {
		base = s.cfg.Current().Monbooru.APIURL
	}
	return strings.TrimRight(base, "/")
}

// monbooruWebLink is the base for the footer "connected to monbooru" link, or
// "" when no web_url is set: unlike the image links it never falls back to
// api_url, which is an internal address that would not resolve from a browser.
func (s *Server) monbooruWebLink() string {
	return strings.TrimRight(s.cfg.Current().Monbooru.WebURL, "/")
}

// jobAction parses the row's {id}, runs one queue action on it, and re-renders
// the rows; a refused action just re-renders (the poll reports the state).
func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, action func(id int64)) {
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		action(id)
	}
	s.queueRows(w, r)
}

// retryJob re-queues a finished job. With ?force=1 the re-run bypasses the
// download-archive so a post already fetched (e.g. since deleted in monbooru)
// is downloaded again.
func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(id int64) { _ = s.queue.Retry(id, r.URL.Query().Get("force") == "1") })
}

// continueJob enqueues a follow-up job for the next window of a capped job, so
// the user can keep pulling a truncated search past the per-job cap.
func (s *Server) continueJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(id int64) { _, _ = s.queue.Continue(id) })
}

// continueAllJob starts a fetch-all chain: the queue keeps pulling the next
// window until the capped search runs short, instead of one click per window.
func (s *Server) continueAllJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(id int64) { _, _ = s.queue.ContinueAll(id) })
}

// deleteJob cancels or removes a queue row. The row collapses a continue-series,
// so it clears every window in the series, not just the one clicked.
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(id int64) { _ = s.queue.CancelSeries(id) })
}

// clearQueue drops the finished-job history; running and pending jobs stay.
func (s *Server) clearQueue(w http.ResponseWriter, r *http.Request) {
	s.queue.Clear()
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
	s.render(w, "conn_light", map[string]any{
		"Conn":            status,
		"MonbooruWebURL":  s.monbooruWebLink(),
		"MonbooruVersion": version,
		"MonbooruPaired":  s.hasPairedToken("monbooru"),
	})
}

// siteRow is one curated site as the settings table shows it. CSRFToken rides
// along so the shared row partial can post the test probe and the edit dialog.
// LastReached is the most recent successful test or fetch, shown in the state
// cell (zero = never reached this run).
type siteRow struct {
	Category    string
	Login       string
	Auth        string
	NeedsCred   bool
	Site        *config.Site
	CSRFToken   string
	LastReached time.Time
}

// siteRows builds the settings table rows for a list of curated categories.
func (s *Server) siteRows(cats []string, csrf string) []siteRow {
	rows := make([]siteRow, 0, len(cats))
	for _, cat := range cats {
		p, _ := s.mapper.Lookup(cat)
		site := s.cfg.Current().FindSite(cat)
		label, needs := loginInfo(p.Auth, site)
		rows = append(rows, siteRow{
			Category: cat, Login: label, Auth: p.Auth, NeedsCred: needs, Site: site, CSRFToken: csrf,
			LastReached: s.siteState.LastReached(cat),
		})
	}
	return rows
}

func (s *Server) settingsScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "settings", "settings - "+s.titleName())
	data["Cfg"] = s.cfg.Current()

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	if galleries, err := s.client.ListGalleries(ctx); err == nil {
		data["Galleries"] = galleries
		if warn := defaultGalleryWarning(s.cfg.Current().Monbooru.DefaultGallery, galleries); warn != "" {
			data["GalleryWarn"] = warn
		}
	}

	csrf := s.csrfToken(sessionFromContext(r.Context()))
	data["BooruSites"] = s.siteRows(s.mapper.CuratedByKind(mapping.KindBooru), csrf)
	data["MangaSites"] = s.siteRows(s.mapper.CuratedByKind(mapping.KindManga), csrf)
	data["LookupPanel"] = s.lookupPanel(csrf)
	data["PTRDiskBytes"] = s.ptr.Status().DiskBytes
	data["Stats"] = s.gatherStats()
	data["MonbooruPaired"] = s.hasPairedToken("monbooru")
	data["MonbooruPairWaiting"] = s.getPairAttempt() != nil
	data["MonsenderPending"] = s.pairs.listPending()
	data["MonsenderPaired"] = s.hasPairedToken("monsender")

	if msg := r.URL.Query().Get("msg"); msg != "" {
		data["Flash"] = msg
		data["FlashSection"] = r.URL.Query().Get("section")
		kind := r.URL.Query().Get("kind")
		if kind == "" {
			kind = "ok"
		}
		data["FlashKind"] = kind
	}
	s.render(w, "settings", data)
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
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	if galleries, err := s.client.ListGalleries(ctx); err == nil {
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
		// The running worker count, not the saved setting: concurrency takes
		// effect only on restart, so report what is actually running.
		Queue: queueStats{Workers: s.queue.Workers()},
	}
	jobs, _ := s.queue.List(queue.ListOptions{})
	for _, j := range jobs {
		switch j.Status {
		case queue.JobQueued:
			st.Queue.Queued++
		case queue.JobRunning:
			st.Queue.Running++
		default:
			st.Queue.Finished++
		}
	}
	return st
}

// loginInfo maps a profile auth kind to a settings label and whether a
// required credential is missing (per the shared rule the lookup walk also
// gates on).
func loginInfo(auth string, site *config.Site) (string, bool) {
	switch auth {
	case "api_optional":
		return "api (opt)", false
	case "api_required", "cookies":
		return mapping.RequiredCredential(auth, site)
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
	case strings.HasPrefix(path, "/settings/sites"):
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
	err := s.updateConfig(func(c *config.Config) error {
		c.Monbooru.APIURL = strings.TrimSpace(r.FormValue("api_url"))
		c.Monbooru.WebURL = strings.TrimSpace(r.FormValue("web_url"))
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
	err := s.updateConfig(func(c *config.Config) error {
		if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("concurrency"))); err == nil && n > 0 {
			c.Downloader.Concurrency = n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("sleep_request")), 64); err == nil && f >= 0 {
			c.GalleryDL.SleepRequest = f
		}
		if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_items_per_job"))); err == nil && n > 0 {
			c.Downloader.MaxItemsPerJob = n
		}
		// Zero is meaningful here (keep history until the ring evicts it), so
		// unlike the caps above it is accepted rather than treated as unset.
		if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("history_retention_days"))); err == nil && n >= 0 {
			c.Downloader.HistoryRetentionDays = n
		}
		c.Downloader.DefaultFolder = strings.TrimSpace(r.FormValue("default_folder"))
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

func (s *Server) saveSite(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.redirectFlash(w, r, "err", "site name required")
		return
	}
	err := s.updateConfig(func(c *config.Config) error {
		site := c.FindSite(name)
		if site == nil {
			c.Sites = append(c.Sites, config.Site{Name: name})
			site = &c.Sites[len(c.Sites)-1]
		}
		if v := strings.TrimSpace(r.FormValue("username")); v != "" {
			site.Username = v
		}
		if v := strings.TrimSpace(r.FormValue("api_key")); v != "" {
			site.APIKey = v
		}
		if v := strings.TrimSpace(r.FormValue("user_id")); v != "" {
			site.UserID = v
		}
		site.Gallery = strings.TrimSpace(r.FormValue("gallery"))
		site.Cookies = strings.TrimSpace(r.FormValue("cookies"))
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.redirectFlash(w, r, "ok", "site "+name+" saved")
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

// resetSite drops a site's per-site credentials block so it reverts to the
// curated profile defaults (no auth, default gallery). The reset button only
// shows for sites that have a block to remove.
func (s *Server) resetSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.updateConfig(func(c *config.Config) error {
		for i := range c.Sites {
			if c.Sites[i].Name == name {
				c.Sites = append(c.Sites[:i], c.Sites[i+1:]...)
				break
			}
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.redirectFlash(w, r, "ok", "site "+name+" reset to defaults")
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
		s.redirectFlash(w, r, "err", err.Error())
		return
	}
	if err := s.updateConfig(func(c *config.Config) error { c.GalleryDL.RawConfig = raw; return nil }); err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.rewriteGDLConfig()
	s.redirectFlash(w, r, "ok", "raw config saved")
}
