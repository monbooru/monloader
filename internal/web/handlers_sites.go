package web

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/shared/fsx"
	"github.com/monbooru/monloader/internal/shared/logx"
)

const searchLimit = 12

var authLabels = map[string]string{
	mapping.AuthAPIOptional:      "API key (optional)",
	mapping.AuthAPIRequired:      "API key",
	mapping.AuthUsernamePassword: "username + password",
	mapping.AuthCookies:          "cookies",
	mapping.AuthOAuth:            "OAuth",
}

func authLabel(auth string) string {
	return cmp.Or(authLabels[auth], mapping.AuthNone)
}

type siteSearchRow struct {
	Category   string
	Name       string
	Login      string
	Configured bool
}

func (s *Server) searchSites(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if q == "" {
		return
	}

	supported := s.catalog.Supported()
	matches := map[string]bool{}
	for _, ex := range s.catalog.Extractors() {
		if ex.Category == "" {
			continue
		}
		if mapping.MatchesQuery(q, ex.Category, ex.Subcategory, supported[ex.Category].Name) {
			matches[ex.Category] = true
		}
	}
	// A multi-instance family lists one host, so its other profiled
	// instances match only by profile name; the supported rows cover a
	// failed extractor listing.
	for _, cat := range s.mapper.CuratedCategories() {
		if mapping.MatchesQuery(q, cat, supported[cat].Name) {
			matches[cat] = true
		}
	}
	for cat, sup := range supported {
		if mapping.MatchesQuery(q, cat, sup.Name) {
			matches[cat] = true
		}
	}
	cats := slices.Sorted(maps.Keys(matches))

	configured := map[string]bool{}
	boorus, manga, other := s.visibleSites()
	for _, cat := range append(append(boorus, manga...), other...) {
		configured[cat] = true
	}

	total := len(cats)
	if len(cats) > searchLimit {
		cats = cats[:searchLimit]
	}
	rows := make([]siteSearchRow, 0, len(cats))
	for _, cat := range cats {
		rows = append(rows, siteSearchRow{
			Category: cat, Name: supported[cat].Name, Login: authLabel(s.siteView(cat).Auth),
			Configured: configured[cat],
		})
	}
	s.render(w, "site_search", map[string]any{"Rows": rows, "More": total - len(cats)})
}

func (s *Server) siteView(category string) mapping.SiteView {
	return s.mapper.SiteView(category, s.catalog.Supported()[category].Auth)
}

func (s *Server) siteDialog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !mapping.ValidCategoryName(name) {
		http.NotFound(w, r)
		return
	}
	s.render(w, "site_dialog", s.siteDialogData(r, name))
}

func (s *Server) siteOptions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	site := s.cfg.Current().FindSite(name)
	if !mapping.ValidCategoryName(name) || site == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "site_options", site.Options)
}

func (s *Server) siteDialogData(r *http.Request, name string) map[string]any {
	p, ok := s.mapper.Lookup(name)
	if !ok {
		p = mapping.Profile{Family: mapping.FamilyGeneric, Kind: mapping.KindOther, Auth: s.siteView(name).Auth}
	}
	// Before the display defaults below: the export shows the stored shape.
	exportJSON, exportLines := exportProfileView(s.mapper, name, p)
	if p.Kind == "" {
		p.Kind = mapping.KindBooru
	}
	if p.Auth == "" {
		p.Auth = mapping.AuthNone
	}
	optionsPublic := ""
	if len(p.Options) > 0 {
		if b, err := json.MarshalIndent(p.Options, "", "  "); err == nil {
			optionsPublic = string(b)
		}
	}
	data := map[string]any{
		"Category":           name,
		"PTR":                name == config.SitePTR,
		"Profile":            p,
		"Custom":             s.mapper.CustomProfile(name),
		"ExportJSON":         exportJSON,
		"ExportLines":        exportLines,
		"OptionsPublic":      optionsPublic,
		"CookiesPlaceholder": filepath.Join(s.cfg.Current().GalleryDL.CookiesDir, name+".txt"),
		"CSRFToken":          s.csrf(r),
		"Families":           mapping.ProfileFamilies,
		"Kinds":              mapping.ProfileKinds,
		"Auths":              mapping.ProfileAuths,
		"Ratings":            mapping.RatingLevels,
		"Categories":         mapping.TagCategories,
		"Username":           "", "UserID": "", "Gallery": "", "Label": "", "Cookies": "", "UserAgent": "",
		"OptionsPrivate": "", "HasOptions": false, "HasPassword": false, "HasAPIKey": false, "CookieCount": 0,
		"CookiesPaste": "", "Blacklist": "",
		"GlobalRules": mapping.BlacklistRuleLines(s.cfg.Current().Tags.Blacklist),
		"MonbooruURL": s.monbooruWebBase(),
	}
	if site := s.cfg.Current().FindSite(name); site != nil {
		data["Username"] = site.Username
		data["UserID"] = site.UserID
		data["Gallery"] = site.Gallery
		data["Label"] = site.Label
		data["Cookies"] = site.Cookies
		data["UserAgent"] = site.Agent
		data["Blacklist"] = strings.Join(site.Blacklist, "\n")
		// Withheld like the credentials: this field holds gallery-dl
		// oauth tokens.
		data["HasOptions"] = site.Options != ""
		data["HasPassword"] = site.Password != ""
		data["HasAPIKey"] = site.APIKey != ""
		if site.Cookies != "" {
			data["CookieCount"] = countCookies(site.Cookies)
		}
	}
	if galleries, ok := s.galleries(r); ok {
		data["Galleries"] = galleries
		data["LiveCategories"], data["CategoriesKnown"] = s.customCategories(r)
	}
	data["TagOverrides"], data["RatingTomlOverrides"] = s.mapper.ActiveOverrides(name)
	markMissingGallery(data)
	markCategoryOptions(data)
	markDroppedTargets(data, s.cfg.Current().Tags.DropCategories)
	s.markDefaultAgent(data)
	return data
}

// A short budget; ok=false means unavailable.
func (s *Server) customCategories(r *http.Request) ([]string, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	names, complete, err := s.client.CustomCategories(ctx)
	if err != nil {
		return nil, false
	}
	s.mapper.LearnCustomCategories(names, complete)
	return names, true
}

type categoryOption struct {
	Name    string
	Missing bool
}

// A stored target the list lacks stays offered, or the save would silently move
// its tags to general.
func markCategoryOptions(dialog map[string]any) {
	live, _ := dialog["LiveCategories"].([]string)
	known, _ := dialog["CategoriesKnown"].(bool)
	p, _ := dialog["Profile"].(mapping.Profile)
	names := slices.Clone(live)
	for _, to := range p.CategoryOverrides {
		if to != "" && !slices.Contains(mapping.TagCategories, to) {
			names = append(names, to)
		}
	}
	slices.Sort(names)
	options := make([]categoryOption, 0, len(names))
	for _, name := range slices.Compact(names) {
		options = append(options, categoryOption{Name: name, Missing: known && !slices.Contains(live, name)})
	}
	dialog["CustomCategories"] = options
}

// A refused save warns about the rows it hands back.
func markDroppedTargets(dialog map[string]any, drop []string) {
	p, _ := dialog["Profile"].(mapping.Profile)
	cats, rules := map[string]string{}, map[string]string{}
	for from, to := range p.CategoryOverrides {
		if to != "" && slices.Contains(drop, to) {
			cats[from] = to
		}
	}
	for from, to := range p.TagRules {
		if target := mapping.DroppedRuleTarget(drop, from, to); target != "" {
			rules[from] = target
		}
	}
	dialog["DroppedCats"], dialog["DroppedRules"] = cats, rules
}

func (s *Server) markDefaultAgent(dialog map[string]any) {
	p, _ := dialog["Profile"].(mapping.Profile)
	userID, _ := dialog["UserID"].(string)
	dialog["DefaultAgent"] = mapping.DefaultAgent(p)
	if p.Family == mapping.FamilyDanbooru && userID != "" {
		dialog["DefaultAgent"] = (&config.Site{UserID: userID}).UserAgent()
	}
}

// Read off the dialog, not the config: a refused save hands back the
// target it was sent.
func markMissingGallery(dialog map[string]any) {
	galleries, _ := dialog["Galleries"].([]monbooru.Gallery)
	name, _ := dialog["Gallery"].(string)
	dialog["GalleryMissing"] = galleries != nil && galleryMissing(name, galleries)
}

func (s *Server) refuseSiteSave(w http.ResponseWriter, r *http.Request, name, tab, msg string) {
	dialog := s.siteDialogData(r, name)
	dialog["Refusal"], dialog["RefusedTab"] = msg, tab
	// profileFromForm's fields stand even when it refuses the JSON.
	if r.Form.Has("family") {
		dialog["Profile"], _ = profileFromForm(r)
	}
	for field, key := range map[string]string{
		"gallery": "Gallery", "label": "Label", "username": "Username",
		"user_id": "UserID", "cookies": "Cookies", "options": "OptionsPrivate",
		"cookies_paste": "CookiesPaste", "blacklist": "Blacklist", "profile_options": "OptionsPublic",
		"user_agent": "UserAgent",
	} {
		if r.Form.Has(field) {
			dialog[key] = r.FormValue(field)
		}
	}
	if r.FormValue("options_edit") == "1" {
		dialog["HasOptions"] = false
	}
	markMissingGallery(dialog)
	markCategoryOptions(dialog)
	markDroppedTargets(dialog, s.cfg.Current().Tags.DropCategories)
	s.markDefaultAgent(dialog)
	data := s.settingsData(r)
	data["SiteDialog"] = dialog
	setFlash(data, "err", "sites", msg)
	s.render(w, "settings", data)
}

// Mark is "", "add" or "del"; a del line shows struck and is left out of
// the copied file.
type exportLine struct {
	Text string
	Mark string
}

// Both sides go through the same encoder, so field order matches and a merge
// walk marks the delta; a changed field shows its replacement, then the value
// it displaced. With no built-in every line would be new, so none is marked.
func exportProfileView(m *mapping.Mapper, category string, p mapping.Profile) (string, []exportLine) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", nil
	}
	eff := strings.Split(string(b), "\n")
	var builtin []string
	if bp, ok := m.BuiltinProfile(category); ok {
		if bb, err := json.MarshalIndent(bp, "", "  "); err == nil {
			builtin = strings.Split(string(bb), "\n")
		}
	}
	if builtin == nil {
		lines := make([]exportLine, 0, len(eff))
		for _, l := range eff {
			lines = append(lines, exportLine{Text: l})
		}
		return string(b) + "\n", lines
	}
	var lines []exportLine
	for i, k := 0, 0; i < len(eff) || k < len(builtin); {
		switch {
		case i >= len(eff):
			lines = append(lines, exportLine{Text: builtin[k], Mark: "del"})
			k++
		case k >= len(builtin):
			lines = append(lines, exportLine{Text: eff[i], Mark: "add"})
			i++
		case sameExportLine(eff[i], builtin[k]):
			lines = append(lines, exportLine{Text: eff[i]})
			i, k = i+1, k+1
		case exportLineIndex(builtin[k:], eff[i]) >= 0:
			// The profile has this line further down: the built-in lines
			// before it were dropped.
			lines = append(lines, exportLine{Text: builtin[k], Mark: "del"})
			k++
		case exportLineIndex(eff[i:], builtin[k]) >= 0:
			lines = append(lines, exportLine{Text: eff[i], Mark: "add"})
			i++
		default:
			lines = append(lines, exportLine{Text: eff[i], Mark: "add"},
				exportLine{Text: builtin[k], Mark: "del"})
			i, k = i+1, k+1
		}
	}
	return string(b) + "\n", lines
}

// The trailing comma only says where in the object the field landed.
func sameExportLine(a, b string) bool {
	return strings.TrimSuffix(a, ",") == strings.TrimSuffix(b, ",")
}

func exportLineIndex(lines []string, line string) int {
	return slices.IndexFunc(lines, func(l string) bool { return sameExportLine(l, line) })
}

func profileFromForm(r *http.Request) (mapping.Profile, error) {
	p := mapping.Profile{
		Family:        r.FormValue("family"),
		Kind:          r.FormValue("kind"),
		Auth:          r.FormValue("auth"),
		PostURL:       strings.TrimSpace(r.FormValue("post_url_template")),
		MD5Search:     strings.TrimSpace(r.FormValue("md5_search_template")),
		Example:       strings.TrimSpace(r.FormValue("example")),
		DefaultRating: r.FormValue("default_rating"),
		NeedsTags:     r.FormValue("needs_tags") != "",
		HasNotes:      r.FormValue("has_notes") != "",
	}
	for _, h := range r.Form["host_alias"] {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			p.Hosts = append(p.Hosts, h)
		}
	}
	foldKey := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	p.RatingOverrides = formPairs(r, "rating_from", "rating_to", foldKey, nil)
	p.CategoryOverrides = formPairs(r, "cat_from", "cat_to", foldKey, nil)
	// A key holding a * stays as typed, target included: folded, the check
	// that refuses it would never see the *.
	foldTag := func(s string) string {
		if s = strings.TrimSpace(s); strings.Contains(s, "*") {
			return s
		}
		return mapping.NormalizeTag(s)
	}
	p.TagRules = formPairs(r, "rule_from", "rule_to", foldTag, mapping.NormalizeRuleTarget)
	for i, from := range r.Form["rule_from"] {
		if tos := r.Form["rule_to"]; strings.Contains(from, "*") && i < len(tos) {
			p.TagRules[foldTag(from)] = strings.TrimSpace(tos[i])
		}
	}
	if raw := strings.TrimSpace(r.FormValue("profile_options")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p.Options); err != nil {
			return p, fmt.Errorf("public gallery-dl options must be a JSON object: %w", err)
		}
	}
	return p, nil
}

// nil when no row survives: that is how a profile spells "no overrides".
func formPairs(r *http.Request, fromKey, toKey string, key, val func(string) string) map[string]string {
	var out map[string]string
	tos := r.Form[toKey]
	for i, from := range r.Form[fromKey] {
		from = key(from)
		if from == "" || i >= len(tos) {
			continue
		}
		to := tos[i]
		if val != nil {
			to = val(to)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[from] = to
	}
	return out
}

func (s *Server) clearSiteCredential(w http.ResponseWriter, r *http.Request) {
	name, field := r.PathValue("name"), r.PathValue("field")
	if field != "password" && field != "api_key" {
		http.NotFound(w, r)
		return
	}
	err := s.cfg.Update(func(c *config.Config) error {
		site := c.FindSite(name)
		if site == nil {
			return nil
		}
		if field == "password" {
			site.Password = ""
		} else {
			site.APIKey = ""
		}
		return nil
	})
	s.saveFlash(w, r, err, "Cleared "+name+"'s stored credential.")
}

// The name becomes a gallery-dl extractor key and a cookies file name, so
// anything path-shaped is refused.
func (s *Server) saveSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !mapping.ValidCategoryName(name) {
		s.redirectFlash(w, r, "err", "Invalid site name.")
		return
	}
	// Everything the dialog can refuse is checked before anything is written,
	// or a failed save could still leave its profile half on disk.
	var profile *mapping.Profile
	if r.Form.Has("family") {
		p, err := profileFromForm(r)
		if err != nil {
			s.refuseSiteSave(w, r, name, "sd-advanced", err.Error())
			return
		}
		if err := mapping.CheckTagRules(p.TagRules); err != nil {
			s.refuseSiteSave(w, r, name, "sd-mapping", err.Error())
			return
		}
		profile = &p
	}
	paste := r.FormValue("cookies_paste")
	if strings.TrimSpace(paste) != "" && !looksLikeNetscapeCookies(paste) {
		s.refuseSiteSave(w, r, name, "sd-auth-panel", "pasted cookies are not a Netscape cookies.txt export")
		return
	}
	options := strings.TrimSpace(r.FormValue("options"))
	if err := config.ValidateSiteOptions(options); err != nil {
		s.refuseSiteSave(w, r, name, "sd-advanced", err.Error())
		return
	}
	agent := strings.TrimSpace(r.FormValue("user_agent"))
	if err := config.ValidateSiteAgent(agent); err != nil {
		s.refuseSiteSave(w, r, name, "sd-advanced", err.Error())
		return
	}
	blacklist, err := s.cleanBlacklist(r)
	if err != nil {
		s.refuseSiteSave(w, r, name, "sd-mapping", err.Error())
		return
	}

	if profile != nil {
		if err := s.mapper.CheckUserProfile(name, *profile); err != nil {
			s.refuseSiteSave(w, r, name, "sd-profile", err.Error())
			return
		}
	}
	pastedCookies := ""
	if strings.TrimSpace(paste) != "" {
		pastedCookies = filepath.Join(s.cfg.Current().GalleryDL.CookiesDir, name+".txt")
	}
	err = s.cfg.Update(func(c *config.Config) error {
		site := c.FindSite(name)
		if site == nil {
			c.Sites = append(c.Sites, config.Site{Name: name})
			site = &c.Sites[len(c.Sites)-1]
		}
		// Username and user id render their stored values, so blank
		// clears them; the password fields render empty, so blank keeps
		// the stored value.
		if r.Form.Has("username") {
			site.Username = strings.TrimSpace(r.FormValue("username"))
		}
		if r.Form.Has("user_id") {
			site.UserID = strings.TrimSpace(r.FormValue("user_id"))
		}
		if v := strings.TrimSpace(r.FormValue("password")); v != "" {
			site.Password = v
		}
		if v := strings.TrimSpace(r.FormValue("api_key")); v != "" {
			site.APIKey = v
		}
		if r.Form.Has("gallery") {
			site.Gallery = strings.TrimSpace(r.FormValue("gallery"))
		}
		if r.Form.Has("label") {
			site.Label = strings.TrimSpace(r.FormValue("label"))
		}
		if r.Form.Has("blacklist") {
			site.Blacklist = blacklist
		}
		if r.Form.Has("cookies") {
			site.Cookies = strings.TrimSpace(r.FormValue("cookies"))
		}
		if pastedCookies != "" {
			site.Cookies = pastedCookies
		}
		if r.Form.Has("user_agent") {
			site.Agent = agent
		}
		// Until revealed the field renders empty, so a blank one keeps the
		// stored value; anything typed replaces it, as with the password.
		if r.Form.Has("options") && (r.FormValue("options_edit") == "1" || options != "") {
			site.Options = options
		}
		return nil
	})
	if err != nil {
		s.refuseSiteSave(w, r, name, "", "Could not save: "+err.Error())
		return
	}
	// The files follow the config, so a save refused above changes nothing.
	var writeErr error
	if profile != nil {
		writeErr = s.mapper.SaveUserProfile(name, *profile)
	}
	if writeErr == nil && pastedCookies != "" {
		if err := fsx.WriteFile(pastedCookies, []byte(paste)); err != nil {
			writeErr = fmt.Errorf("writing the cookies file: %w", err)
		}
	}
	s.refreshSiteLists()
	if writeErr != nil {
		s.refuseSiteSave(w, r, name, "", "Saved, but "+writeErr.Error())
		return
	}
	boorus, manga, other := s.visibleSites()
	if name != config.SitePTR && !slices.Contains(slices.Concat(boorus, manga, other), name) {
		s.redirectFlash(w, r, "ok", "Nothing to save for "+name+": it works with the defaults.")
		return
	}
	s.redirectFlash(w, r, "ok", "Site "+name+" saved.")
}

func (s *Server) saveHostLabels(w http.ResponseWriter, r *http.Request) {
	var rows []config.HostLabel
	for _, row := range postedHostLabels(r, nil) {
		host, label := strings.ToLower(row.Host), row.Label
		if host == "" || label == "" {
			continue
		}
		if strings.ContainsAny(host, "/ \t") || strings.Contains(host, "://") {
			s.refuseSave(w, r, fmt.Sprintf("Host %q is not a bare hostname.", host))
			return
		}
		rows = append(rows, config.HostLabel{Host: host, Label: label})
	}
	err := s.cfg.Update(func(c *config.Config) error {
		c.HostLabels = rows
		return nil
	})
	s.finishSave(w, r, err)
}

func postedHostLabels(r *http.Request, stored []config.HostLabel) []config.HostLabel {
	hosts, labels := r.Form["hostlabel_host"], r.Form["hostlabel_label"]
	if len(hosts) == 0 {
		return stored
	}
	rows := make([]config.HostLabel, 0, len(hosts))
	for i, host := range hosts {
		row := config.HostLabel{Host: strings.TrimSpace(host)}
		if i < len(labels) {
			row.Label = strings.TrimSpace(labels[i])
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) resetSiteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mapper.DeleteUserProfile(name); err != nil {
		s.redirectFlash(w, r, "err", err.Error())
		return
	}
	s.refreshSiteLists()
	s.redirectFlash(w, r, "ok", "Profile for "+name+" reset to the shipped mapping.")
}

// A browser export marks http-only cookies "#HttpOnly_", which reads as a
// comment but is a cookie.
func cookieLine(line string) bool {
	line = strings.TrimRight(line, "\r")
	line = strings.TrimPrefix(line, "#HttpOnly_")
	if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
		return false
	}
	return len(strings.Split(line, "\t")) >= 7
}

func looksLikeNetscapeCookies(s string) bool {
	return slices.ContainsFunc(strings.Split(s, "\n"), cookieLine)
}

func countCookies(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if cookieLine(line) {
			n++
		}
	}
	return n
}

// Call after any profile change: the runner's overrides and the managed
// config follow the effective profiles.
func (s *Server) refreshSiteLists() {
	s.runner.SetSiteLists(gdl.ListsFrom(s.mapper))
	s.rewriteGDLConfig()
}

type siteRow struct {
	Category       string
	Login          string
	NeedsCred      bool
	CustomProfile  bool
	AuthSet        string
	Site           *config.Site
	CSRFToken      string
	LastReached    time.Time
	BlacklistRules int
	PTR            bool
	PTRState       string
}

func (s *Server) siteRows(cats []string, csrf string) []siteRow {
	rows := make([]siteRow, 0, len(cats))
	for _, cat := range cats {
		v := s.siteView(cat)
		site := s.cfg.Current().FindSite(cat)
		row := siteRow{
			Category: cat, Login: authLabel(v.Auth), NeedsCred: v.NeedsCredential,
			CustomProfile: v.CustomProfile, AuthSet: authSet(site),
			Site: site, CSRFToken: csrf,
			LastReached: s.siteState.LastReached(cat),
		}
		if site != nil {
			row.BlacklistRules = len(mapping.BlacklistRuleLines(site.Blacklist))
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) ptrSiteRows(csrf string) []siteRow {
	rows := s.siteRows([]string{config.SitePTR}, csrf)
	rows[0].PTR, rows[0].PTRState = true, string(s.ptr.Status().State)
	if s.cfg.Current().PTR.AccessKey != "" {
		rows[0].AuthSet = "access key set"
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
		{site.APIKey, "API key"}, {site.UserID, "user id"}, {site.Cookies, "cookies"},
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
	delete(names, config.SitePTR)
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

func (s *Server) resetSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Profile first: it may refuse (the chain needs its template), and a
	// refusal must leave the block alone.
	if err := s.mapper.DeleteUserProfile(name); err != nil {
		s.redirectFlash(w, r, "err", err.Error())
		return
	}
	pasted := filepath.Join(s.cfg.Current().GalleryDL.CookiesDir, name+".txt")
	site := s.cfg.Current().FindSite(name)
	hadPasted := site != nil && site.Cookies == pasted
	err := s.cfg.Update(func(c *config.Config) error {
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
		s.redirectFlash(w, r, "err", "Could not save: "+err.Error())
		return
	}
	if hadPasted {
		if err := os.Remove(pasted); err != nil && !os.IsNotExist(err) {
			logx.Warnf("removing %s's pasted cookies: %v", name, err)
		}
	}
	s.refreshSiteLists()
	s.redirectFlash(w, r, "ok", "Site "+name+" removed.")
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
	kind, msg := s.probeVerdict(name, res.Status)
	siteState(w, kind, msg, res.Detail, time.Time{})
}

// A missing required credential outranks a failed probe's verdict: a
// cookies site's failure often reads as a plain "not found".
func (s *Server) probeVerdict(name string, status gdl.ProbeStatus) (kind, msg string) {
	if name != "" {
		if v := s.siteView(name); v.NeedsCredential {
			return "warn", "needs " + authLabel(v.Auth)
		}
	}
	switch status {
	case gdl.ProbeBlocked:
		return "err", "blocked"
	case gdl.ProbeAuthRequired:
		return "warn", "auth rejected"
	case gdl.ProbeRateLimited:
		return "warn", "rate limited"
	}
	return "err", "failed"
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
