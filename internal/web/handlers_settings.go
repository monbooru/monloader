package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
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
	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func (s *Server) settingsScreen(w http.ResponseWriter, r *http.Request) {
	data := s.settingsData(r)
	if msg := r.URL.Query().Get("msg"); msg != "" {
		setFlash(data, cmp.Or(r.URL.Query().Get("kind"), "ok"), r.URL.Query().Get("section"), msg)
	}
	s.render(w, "settings", data)
}

// The page's own address replaces the one it was rendered under: a
// refusal's POST-only path, or a flash in the query that a reload replays.
func setFlash(data map[string]any, kind, section, msg string) {
	data["Flash"] = msg
	data["FlashKind"] = kind
	data["FlashSection"] = section
	data["CanonicalURL"] = "/settings"
	if section != "" {
		data["CanonicalURL"] = "/settings#" + section
	}
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
	data["TagCategories"] = s.tagCategoryRows(r, data["Galleries"] != nil)
	data["SiteBlacklists"] = siteBlacklists(s.cfg.Current().Sites)
	data["MonbooruURL"] = s.monbooruWebBase()

	csrf := s.csrf(r)
	boorus, manga, other := s.visibleSites()
	data["PTRSite"] = s.ptrSiteRows(csrf)
	data["BooruSites"] = s.siteRows(boorus, csrf)
	data["MangaSites"] = s.siteRows(manga, csrf)
	data["OtherSites"] = s.siteRows(other, csrf)
	data["LookupPanel"] = s.lookupPanel(csrf)
	data["GDLPanel"] = s.gdlPanel(csrf)
	data["Themes"] = s.themeCluster()
	data["ThemesDir"] = s.themesDir()
	ptrStatus := s.ptr.Status()
	data["PTRDiskBytes"] = ptrStatus.DiskBytes
	data["PTREnabled"] = ptrStatus.Enabled
	if r.Form.Has("data_path") {
		data["PTREnabled"] = r.FormValue("enabled") == "1"
	}
	data["HostLabels"] = postedHostLabels(r, s.cfg.Current().HostLabels)
	data["Stats"] = s.gatherStats()
	data["MonbooruPairWaiting"] = s.getPairAttempt() != nil
	data["MonsenderPending"] = s.pendingPairs()
	data["MonsenderAwaiting"] = s.pairs.ListApproved()
	data["MonsenderPaired"] = s.pairedApps()
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
	if r.Form.Has("default_gallery") {
		gallery = strings.TrimSpace(r.FormValue("default_gallery"))
	}
	data["Galleries"] = galleries
	data["GalleryMissing"] = galleryMissing(gallery, galleries)
	data["GalleryWarn"] = defaultGalleryWarning(gallery, galleries)
}

func defaultGalleryWarning(name string, galleries []monbooru.Gallery) string {
	if name == "" {
		return ""
	}
	if galleryMissing(name, galleries) {
		return "Gallery \"" + name + "\" is not in monbooru - images will be refused until it exists."
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
		"Paired":         s.hasPairedToken(contract.AppMonbooru),
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

// settingsForm parses a settings form before h runs. session.CheckCSRF has done
// so already for a plain form; on the X-CSRF-Token path a garbage body must not
// go on with blank fields.
func (s *Server) settingsForm(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			s.redirectFlash(w, r, "err", "Bad form data.")
			return
		}
		h(w, r)
	}
}

func flashForm(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			flashFragment(w, "err", "Bad form data.")
			return
		}
		h(w, r)
	}
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

func (s *Server) finishSave(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		s.refuseSave(w, r, "Could not save: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "Saved.")
}

func (s *Server) saveFlash(w http.ResponseWriter, r *http.Request, err error, okMsg string) {
	if err != nil {
		s.redirectFlash(w, r, "err", "Could not save: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", okMsg)
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

var settingsSections = map[string]string{
	"monbooru": "monbooru", "downloader": "downloads", "rules": "rules", "lookup": "lookup", "ptr": "ptr",
	"sites": "sites", "host-labels": "sites", "gallerydl": "advanced", "raw": "advanced", "desktop": "desktop",
}

func sectionForPath(path string) string {
	if path == "/internal/open-folder" {
		return "desktop"
	}
	section, _, _ := strings.Cut(strings.TrimPrefix(path, "/settings/"), "/")
	return settingsSections[section]
}

func (s *Server) saveMonbooru(w http.ResponseWriter, r *http.Request) {
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
		s.refuseSave(w, r, strings.Join(bad, "; ")+" - nothing was saved.")
		return
	}
	err := s.cfg.Update(func(c *config.Config) error {
		c.Monbooru.APIURL = apiURL
		c.Monbooru.WebURL = webURL
		// The field renders only once paired, so an unpaired save leaves it.
		if r.Form.Has("default_gallery") {
			c.Monbooru.DefaultGallery = strings.TrimSpace(r.FormValue("default_gallery"))
		}
		return nil
	})
	s.finishSave(w, r, err)
}

func (s *Server) testMonbooru(w http.ResponseWriter, r *http.Request) {
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
		flashFragment(w, "err", "Connection failed: "+err.Error()+note)
		return
	}
	// A swap, not a redirect, so the form's unsaved values survive.
	flashFragment(w, "ok", "monbooru reachable - save to keep these settings.")
}

func (s *Server) saveDownloader(w http.ResponseWriter, r *http.Request) {
	// An absent field keeps its stored value; a posted one that does not
	// parse, blank included, is refused by name.
	var bad []string
	var edits []func(*config.Config)
	num := func(name, label string, floor int, set func(*config.Config, int)) {
		if !r.Form.Has(name) {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
		if err != nil || n < floor {
			bad = append(bad, fmt.Sprintf("%s must be a whole number of %d or more", label, floor))
			return
		}
		edits = append(edits, func(c *config.Config) { set(c, n) })
	}
	num("concurrency", "parallel downloads", 1, func(c *config.Config, n int) { c.Downloader.Concurrency = n })
	num("max_items_per_job", "max posts per search", 1, func(c *config.Config, n int) { c.Downloader.MaxItemsPerJob = n })
	// Zero is valid here: keep history until the ring evicts it.
	num("history_retention_days", "clear finished jobs after (days)", 0, func(c *config.Config, n int) { c.Downloader.HistoryRetentionDays = n })
	num("success_retention_days", "clear succeeded jobs after (days)", 0, func(c *config.Config, n int) { c.Downloader.SuccessRetentionDays = n })
	// monbooru refuses an absolute folder or a .. segment, which would
	// fail every push.
	if r.Form.Has("default_folder") {
		folder := strings.TrimSpace(r.FormValue("default_folder"))
		if config.IsRelativeFolder(folder) {
			edits = append(edits, func(c *config.Config) { c.Downloader.DefaultFolder = folder })
		} else {
			bad = append(bad, "default folder must be a relative path with no .. segment")
		}
	}
	if r.Form.Has("sleep_request") {
		if f, ferr := strconv.ParseFloat(strings.TrimSpace(r.FormValue("sleep_request")), 64); ferr == nil && f >= 0 && !math.IsInf(f, 0) {
			edits = append(edits, func(c *config.Config) { c.GalleryDL.SleepRequest = f })
		} else {
			bad = append(bad, "delay between requests must be a number of 0 or more")
		}
	}
	apply := func(c *config.Config) {
		for _, edit := range edits {
			edit(c)
		}
	}
	// The sweeps run independently, so a succeeded window past the
	// history one would never take effect.
	next := s.cfg.Current().Clone()
	apply(next)
	if d := next.Downloader; d.HistoryRetentionDays > 0 && d.SuccessRetentionDays > d.HistoryRetentionDays {
		bad = append(bad, "clear succeeded jobs after must not be longer than clear finished jobs after")
	}
	if len(bad) > 0 {
		s.refuseSave(w, r, strings.Join(bad, "; ")+" - nothing was saved.")
		return
	}
	err := s.cfg.Update(func(c *config.Config) error {
		apply(c)
		return nil
	})
	s.finishSave(w, r, err)
}

type siteBlacklist struct {
	Category string
	Rules    int
}

func siteBlacklists(sites []config.Site) []siteBlacklist {
	var lists []siteBlacklist
	for _, site := range sites {
		if n := len(mapping.BlacklistRuleLines(site.Blacklist)); n > 0 {
			lists = append(lists, siteBlacklist{site.Name, n})
		}
	}
	return lists
}

type tagCategoryRow struct {
	Name string
	Sent bool
}

// A refused save shows the boxes as they were posted. A dropped category
// monbooru does not list keeps its box, or it could never be sent again.
func (s *Server) tagCategoryRows(r *http.Request, reachable bool) []tagCategoryRow {
	names := slices.Clone(mapping.TagCategories)
	var custom []string
	if reachable {
		custom, _ = s.customCategories(r)
	}
	drop := postedDropCategories(r, s.cfg.Current().Tags.DropCategories)
	for _, name := range append(custom, drop...) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	rows := make([]tagCategoryRow, len(names))
	for i, name := range names {
		rows[i] = tagCategoryRow{Name: name, Sent: !slices.Contains(drop, name)}
	}
	return rows
}

// The form names the boxes it showed, so a category it could not list stays
// as stored and an unticked box is told from an absent one.
func postedDropCategories(r *http.Request, stored []string) []string {
	if !r.Form.Has("category_shown") {
		return stored
	}
	shown := r.Form["category_shown"]
	sent := r.Form["category_sent"]
	drop := slices.DeleteFunc(slices.Clone(stored), func(name string) bool { return slices.Contains(shown, name) })
	for _, name := range shown {
		if mapping.ValidTagCategoryName(name) && !slices.Contains(sent, name) && !slices.Contains(drop, name) {
			drop = append(drop, name)
		}
	}
	return drop
}

// Learns the categories first: a rule naming a custom one is valid only once
// monloader knows it exists.
func (s *Server) cleanBlacklist(r *http.Request) ([]string, error) {
	text := r.FormValue("blacklist")
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	s.customCategories(r)
	return mapping.CleanBlacklist(text)
}

func (s *Server) saveTags(w http.ResponseWriter, r *http.Request) {
	var blacklist []string
	if r.Form.Has("blacklist") {
		var err error
		if blacklist, err = s.cleanBlacklist(r); err != nil {
			s.refuseSave(w, r, err.Error()+" - nothing was saved.")
			return
		}
	}
	err := s.cfg.Update(func(c *config.Config) error {
		c.Tags.DropCategories = postedDropCategories(r, c.Tags.DropCategories)
		if r.Form.Has("blacklist") {
			c.Tags.Blacklist = blacklist
		}
		return nil
	})
	s.finishSave(w, r, err)
}

type traceFailure struct {
	Kind   string
	Msg    string
	Detail string
}

func (s *Server) tracePost(w http.ResponseWriter, r *http.Request) {
	raw := config.NormalizeURL(strings.TrimSpace(r.FormValue("url")))
	if raw == "" {
		s.render(w, "tag_trace", map[string]any{"Failure": traceFailure{Kind: "warn", Msg: "Paste a post URL."}})
		return
	}
	// gallery-dl's own prefixes would otherwise apply: r:file:// reads a local file.
	if !config.IsHTTPURL(raw) {
		s.render(w, "tag_trace", map[string]any{"Failure": traceFailure{Kind: "warn", Msg: "Trace takes an http(s) post URL, not a hash or a path."}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	meta, err := s.runner.FirstPost(ctx, raw)
	if err != nil {
		site, _, _ := s.mapper.CanonicalPostURL(raw)
		res := gdl.ProbeResult{Status: gdl.ProbeFailed, Detail: err.Error()}
		var ce *outcome.CodedError
		if errors.As(err, &ce) {
			res = gdl.ProbeFromError(ce)
		}
		kind, msg := s.probeVerdict(site, res.Status)
		s.render(w, "tag_trace", map[string]any{"Failure": traceFailure{kind, msg, res.Detail}})
		return
	}
	s.customCategories(r)
	trace := s.mapper.Trace(meta)
	if kwdict.String(meta, "category") == mapping.CategoryDirectlink || kwdict.String(meta, "subcategory") == "post" {
		trace.PostURL = ""
	}
	data := map[string]any{"Trace": trace}
	s.traceGallery(ctx, trace, data)
	s.render(w, "tag_trace", data)
}

// The gallery column: what the target gallery does with each tag the push
// would send. Unpaired, the trace says it stops at the push.
func (s *Server) traceGallery(ctx context.Context, trace mapping.PostTrace, data map[string]any) {
	data["Resolved"] = map[string]monbooru.ResolvedTag{}
	if s.cfg.Current().Monbooru.Unconfigured() {
		data["Unpaired"] = true
		return
	}
	gallery := s.mapper.Gallery(trace.Site)
	data["Gallery"] = cmp.Or(gallery, "the default gallery")
	if len(trace.Tags) == 0 || trace.Skipped != "" || trace.Refused != "" {
		return
	}
	// The mapped rating rides in Tags already.
	resolved, err := s.client.ResolveTags(ctx, gallery, trace.Tags)
	if err != nil {
		data["GalleryErr"] = err.Error()
		var ce *outcome.CodedError
		if errors.As(err, &ce) {
			data["GalleryErr"] = ce.Msg
		}
		return
	}
	byName := make(map[string]monbooru.ResolvedTag, len(resolved))
	for _, r := range resolved {
		byName[r.Name] = r
	}
	data["Resolved"] = byName
}

func (s *Server) revealRaw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "site_options", s.cfg.Current().GalleryDL.RawConfig)
}

func (s *Server) saveRaw(w http.ResponseWriter, r *http.Request) {
	raw := r.FormValue("raw_config")
	// Withheld until revealed, so blank means untouched unless it was shown.
	if raw == "" && r.FormValue("raw_config_edit") != "1" {
		s.redirectFlash(w, r, "ok", "Saved.")
		return
	}
	if err := config.ValidateRawConfig(raw); err != nil {
		s.refuseSave(w, r, err.Error())
		return
	}
	err := s.cfg.Update(func(c *config.Config) error { c.GalleryDL.RawConfig = raw; return nil })
	s.finishSave(w, r, err)
}
