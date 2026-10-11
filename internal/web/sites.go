package web

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
)

const searchLimit = 12

type siteSearchRow struct {
	Category   string
	Name       string
	Family     string
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
		family := mapping.FamilyGeneric
		if p, ok := s.mapper.Lookup(cat); ok {
			family = p.Family
		}
		rows = append(rows, siteSearchRow{
			Category: cat, Name: supported[cat].Name, Family: family, Login: s.siteView(cat).Login,
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
		"Profile":            p,
		"Custom":             s.mapper.CustomProfile(name),
		"ExportJSON":         exportJSON,
		"ExportLines":        exportLines,
		"OptionsPublic":      optionsPublic,
		"CookiesPlaceholder": filepath.Join(s.cfg.Current().GalleryDL.CookiesDir, name+".txt"),
		"CSRFToken":          s.csrfToken(sessionFromContext(r.Context())),
		"Families":           mapping.ProfileFamilies,
		"Kinds":              mapping.ProfileKinds,
		"Auths":              mapping.ProfileAuths,
		"Ratings":            mapping.RatingLevels,
		"Categories":         mapping.TagCategories,
		"Username":           "", "UserID": "", "Gallery": "", "Label": "", "Cookies": "",
		"OptionsPrivate": "", "HasOptions": false, "HasPassword": false, "HasAPIKey": false, "CookieCount": 0,
		"CookiesPaste": "",
	}
	if site := s.cfg.Current().FindSite(name); site != nil {
		data["Username"] = site.Username
		data["UserID"] = site.UserID
		data["Gallery"] = site.Gallery
		data["Label"] = site.Label
		data["Cookies"] = site.Cookies
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
	}
	markMissingGallery(data)
	return data
}

// Read off the dialog, not the config: a refused save hands back the
// target it was sent.
func markMissingGallery(dialog map[string]any) {
	galleries, _ := dialog["Galleries"].([]monbooru.Gallery)
	name, _ := dialog["Gallery"].(string)
	dialog["GalleryMissing"] = galleries != nil && galleryMissing(name, galleries)
}

func (s *Server) refuseSiteSave(w http.ResponseWriter, r *http.Request, name, msg string) {
	dialog := s.siteDialogData(r, name)
	if p, err := profileFromForm(r); err == nil {
		dialog["Profile"] = p
	}
	for field, key := range map[string]string{
		"gallery": "Gallery", "label": "Label", "username": "Username",
		"user_id": "UserID", "cookies": "Cookies", "options": "OptionsPrivate",
		"cookies_paste": "CookiesPaste",
	} {
		if r.Form.Has(field) {
			dialog[key] = r.FormValue(field)
		}
	}
	if r.FormValue("options_edit") == "1" {
		dialog["HasOptions"] = false
	}
	markMissingGallery(dialog)
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
	// The select's "drop" is the profile's empty replacement: suppress the tag.
	p.CategoryOverrides = formPairs(r, "cat_from", "cat_to", foldKey, func(to string) string {
		if to == "drop" {
			return ""
		}
		return to
	})
	foldTag := func(s string) string { return mapping.NormalizeTag(strings.TrimSpace(s)) }
	p.TagRules = formPairs(r, "rule_from", "rule_to", foldTag, foldTag)
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

// The name becomes a gallery-dl extractor key and a cookies file name, so
// anything path-shaped is refused.
func (s *Server) saveSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	if !mapping.ValidCategoryName(name) {
		s.redirectFlash(w, r, "err", "invalid site name")
		return
	}
	// Everything the dialog can refuse is checked before anything is written,
	// or a failed save could still leave its profile half on disk.
	var profile *mapping.Profile
	if r.Form.Has("family") {
		p, err := profileFromForm(r)
		if err != nil {
			s.refuseSiteSave(w, r, name, err.Error())
			return
		}
		profile = &p
	}
	paste := r.FormValue("cookies_paste")
	if strings.TrimSpace(paste) != "" && !looksLikeNetscapeCookies(paste) {
		s.refuseSiteSave(w, r, name, "pasted cookies are not a Netscape cookies.txt export")
		return
	}
	options := strings.TrimSpace(r.FormValue("options"))
	if err := config.ValidateSiteOptions(options); err != nil {
		s.refuseSiteSave(w, r, name, err.Error())
		return
	}

	if profile != nil {
		if err := s.mapper.SaveUserProfile(name, *profile); err != nil {
			s.refuseSiteSave(w, r, name, err.Error())
			return
		}
	}
	pastedCookies := ""
	if strings.TrimSpace(paste) != "" {
		pastedCookies = filepath.Join(s.cfg.Current().GalleryDL.CookiesDir, name+".txt")
		if err := config.WriteFileBytesAtomic(pastedCookies, []byte(paste)); err != nil {
			s.refuseSiteSave(w, r, name, "writing the cookies file: "+err.Error())
			return
		}
	}
	err := s.updateConfig(func(c *config.Config) error {
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
		if r.Form.Has("cookies") {
			site.Cookies = strings.TrimSpace(r.FormValue("cookies"))
		}
		if pastedCookies != "" {
			site.Cookies = pastedCookies
		}
		// Until revealed the field renders empty, so a blank one keeps the
		// stored value; anything typed replaces it, as with the password.
		if r.Form.Has("options") && (r.FormValue("options_edit") == "1" || options != "") {
			site.Options = options
		}
		return nil
	})
	if err != nil {
		s.refuseSiteSave(w, r, name, "save failed: "+err.Error())
		return
	}
	s.refreshSiteLists()
	s.redirectFlash(w, r, "ok", "site "+name+" saved")
}

func (s *Server) saveHostLabels(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	var rows []config.HostLabel
	for i, host := range r.Form["hostlabel_host"] {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" || i >= len(r.Form["hostlabel_label"]) {
			continue
		}
		label := strings.TrimSpace(r.Form["hostlabel_label"][i])
		if label == "" {
			continue
		}
		if strings.ContainsAny(host, "/ \t") || strings.Contains(host, "://") {
			s.redirectFlash(w, r, "err", fmt.Sprintf("host %q is not a bare hostname", host))
			return
		}
		rows = append(rows, config.HostLabel{Host: host, Label: label})
	}
	err := s.updateConfig(func(c *config.Config) error {
		c.HostLabels = rows
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "host labels saved")
}

func (s *Server) resetSiteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mapper.DeleteUserProfile(name); err != nil {
		s.redirectFlash(w, r, "err", err.Error())
		return
	}
	s.refreshSiteLists()
	s.redirectFlash(w, r, "ok", "profile for "+name+" reset to the shipped mapping")
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
	if rl, ok := s.runner.(interface {
		SetSiteLists(flatTagSites, metadataSites, notesSites []string)
	}); ok {
		rl.SetSiteLists(s.mapper.FlatTagSites(), s.mapper.MetadataSites(), s.mapper.NotesSites())
	}
	s.rewriteGDLConfig()
}
