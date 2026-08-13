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
)

// searchLimit bounds one add-site search fragment; the note under the list
// says how many more matched so a broad query is not mistaken for complete.
const searchLimit = 12

// siteSearchRow is one add-site search hit. Configured hits already have a
// row in the tables, so their add button is disabled; the rest carry the
// dataset the shared edit dialog opens from. Family names the mapping
// profile that would apply (generic when none ships).
type siteSearchRow struct {
	Category   string
	Name       string
	Family     string
	Login      string
	Configured bool
}

// searchSites answers the add-site search box (GET /settings/sites/search)
// with the supported sites matching the query: the cached extractor list plus
// every profiled category, by category, subcategory, or the supportedsites
// display name. Adding stores nothing - the button opens the edit dialog and
// the site materializes when a save writes something.
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
		if matchesQuery(q, ex.Category, ex.Subcategory, supported[ex.Category].Name) {
			matches[ex.Category] = true
		}
	}
	// A multi-instance family is listed under one example host, so its other
	// profiled instances (aibooru, ...) are only findable by profile name; the
	// supported rows keep the search working when the boot-time extractor
	// listing failed.
	for _, cat := range s.mapper.CuratedCategories() {
		if matchesQuery(q, cat, supported[cat].Name) {
			matches[cat] = true
		}
	}
	for cat, sup := range supported {
		if matchesQuery(q, cat, sup.Name) {
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
		auth := s.effectiveAuth(cat)
		label, _ := loginInfo(auth, nil)
		family := mapping.FamilyGeneric
		if p, ok := s.mapper.Lookup(cat); ok {
			family = p.Family
		}
		rows = append(rows, siteSearchRow{
			Category: cat, Name: supported[cat].Name, Family: family, Login: label,
			Configured: configured[cat],
		})
	}
	s.render(w, "site_search", map[string]any{"Rows": rows, "More": total - len(cats)})
}

// matchesQuery reports whether the lower-cased query is a substring of any
// field, the add-site box's matching rule.
func matchesQuery(q string, fields ...string) bool {
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// effectiveAuth resolves a category's auth kind: its profile's when one
// exists, else the kind seeded from the bundled supportedsites data. The seed
// is a starting point the profile editor can correct; nothing is stored until
// a save.
func (s *Server) effectiveAuth(category string) string {
	if p, ok := s.mapper.Lookup(category); ok {
		return p.Auth
	}
	return mapping.SeedAuthKind(s.catalog.Supported()[category].Auth)
}

// siteDialog renders the whole edit dialog (GET /settings/sites/{name}/dialog):
// the [[sites]] block's values and the effective profile, or for a category
// without one, a form seeded generic/other with the supportedsites auth kind.
// Nothing is stored until the form is saved.
func (s *Server) siteDialog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !mapping.ValidCategoryName(name) {
		http.NotFound(w, r)
		return
	}
	p, ok := s.mapper.Lookup(name)
	if !ok {
		p = mapping.Profile{Family: mapping.FamilyGeneric, Kind: mapping.KindOther, Auth: s.effectiveAuth(name)}
	}
	// The export tab serializes the stored shape, captured before the
	// display defaults below pad the form's selects.
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
		"OptionsPrivate": "", "HasPassword": false, "HasAPIKey": false, "CookieCount": 0,
	}
	if site := s.cfg.Current().FindSite(name); site != nil {
		data["Username"] = site.Username
		data["UserID"] = site.UserID
		data["Gallery"] = site.Gallery
		data["Label"] = site.Label
		data["Cookies"] = site.Cookies
		data["OptionsPrivate"] = site.Options
		data["HasPassword"] = site.Password != ""
		data["HasAPIKey"] = site.APIKey != ""
		if site.Cookies != "" {
			data["CookieCount"] = countCookies(site.Cookies)
		}
	}
	if galleries, ok := s.galleries(r); ok {
		data["Galleries"] = galleries
	}
	s.render(w, "site_dialog", data)
}

// exportLine is one rendered line of the export tab's profile view.
// Mark selects the color: "" (as shipped), "add" (a line the built-in
// does not carry), "del" (a built-in line this profile drops or
// alters - shown struck, absent from the copied file).
type exportLine struct {
	Text string
	Mark string
}

// exportProfileView serializes the effective profile as the shareable
// profiles/<category>.json content plus its display lines. Both sides
// marshal through the same encoder, so field order matches and a merge
// walk over the two colors the delta: a line only the profile has reads
// added, a line only the built-in has reads struck, and where the two
// diverge on the same field the replacement is followed by the value it
// displaced - so a changed line always shows its pair, wherever in the
// object it sits. Lines match on their content without the trailing
// comma, so a field that only stopped being the object's last one does
// not read changed. A category with no built-in gets no colors (the
// whole file is new, marking every line would say nothing).
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
			// The profile keeps this line further down, so the built-in lines
			// before it are ones the profile dropped or altered.
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

// sameExportLine compares two rendered lines ignoring the trailing comma
// that only says where in the object the field landed.
func sameExportLine(a, b string) bool {
	return strings.TrimSuffix(a, ",") == strings.TrimSuffix(b, ",")
}

// exportLineIndex is the position of line in lines, or -1.
func exportLineIndex(lines []string, line string) int {
	for i, l := range lines {
		if sameExportLine(l, line) {
			return i
		}
	}
	return -1
}

// profileFromForm builds the profile the dialog's profile, mapping, and
// advanced tabs describe. A rule row's spelling is folded to the form the
// mapper compares; an empty replacement suppresses the tag, an emptied rule
// field drops the row.
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
	// The category select's "drop" option means "suppress the tag", which the
	// profile spells as an empty replacement.
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

// formPairs reads one of the mapping tab's row editors: two parallel form
// arrays, folded to the spelling the mapper compares. A row with an empty key
// or no partner is dropped, so an emptied field removes the row; nil is
// returned when nothing survives, which is how a profile spells "no overrides".
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

// saveSite writes the whole edit dialog (POST /settings/sites/{name}): the
// site's [[sites]] block and, when the form carries the profile fields, its
// user profile in the same save. The name becomes a gallery-dl extractor key
// and, on a cookie paste, a file name under the cookies dir, so anything
// path-shaped is refused.
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
	if r.Form.Has("family") {
		p, err := profileFromForm(r)
		if err != nil {
			s.redirectFlash(w, r, "err", err.Error())
			return
		}
		if err := s.mapper.SaveUserProfile(name, p); err != nil {
			s.redirectFlash(w, r, "err", err.Error())
			return
		}
	}
	// A pasted Netscape export is written under the cookies dir and the site
	// pointed at it; the paste box is write-only, so its content is only ever
	// read here.
	pastedCookies := ""
	if paste := r.FormValue("cookies_paste"); strings.TrimSpace(paste) != "" {
		if !looksLikeNetscapeCookies(paste) {
			s.redirectFlash(w, r, "err", "pasted cookies are not a Netscape cookies.txt export")
			return
		}
		pastedCookies = filepath.Join(s.cfg.Current().GalleryDL.CookiesDir, name+".txt")
		err := config.WriteFileAtomic(pastedCookies, func(f *os.File) error {
			_, err := f.WriteString(paste)
			return err
		})
		if err != nil {
			s.redirectFlash(w, r, "err", "writing the cookies file: "+err.Error())
			return
		}
	}
	err := s.updateConfig(func(c *config.Config) error {
		site := c.FindSite(name)
		if site == nil {
			c.Sites = append(c.Sites, config.Site{Name: name})
			site = &c.Sites[len(c.Sites)-1]
		}
		// The dialog renders username and user id as their stored values, so an
		// emptied one reads as "remove this" and is honoured like the gallery
		// and label beside them; a field the form did not post keeps its value.
		// The two password-type fields render empty with the placeholder that
		// states the keep-on-blank rule, so blank there means "leave it".
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
		if r.Form.Has("options") {
			opts := strings.TrimSpace(r.FormValue("options"))
			if err := config.ValidateSiteOptions(opts); err != nil {
				return err
			}
			site.Options = opts
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.refreshSiteLists()
	s.redirectFlash(w, r, "ok", "site "+name+" saved")
}

// saveHostLabels writes the host labels table (POST /settings/host-labels):
// the source label a push stamps for a host outside the profiles. The rows
// replace the table whole, so an emptied row drops its entry.
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

// resetSiteProfile deletes the site's user profile file
// (POST /settings/sites/{name}/profile/reset) so the shipped or generic
// profile shows through again.
func (s *Server) resetSiteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mapper.DeleteUserProfile(name); err != nil {
		s.redirectFlash(w, r, "err", err.Error())
		return
	}
	s.refreshSiteLists()
	s.redirectFlash(w, r, "ok", "profile for "+name+" reset to the shipped mapping")
}

// cookieLine reports whether a cookies.txt line is a cookie: seven or more
// tab-separated fields. A browser export prefixes http-only cookies with
// "#HttpOnly_", which reads as a comment but is a cookie.
func cookieLine(line string) bool {
	line = strings.TrimRight(line, "\r")
	line = strings.TrimPrefix(line, "#HttpOnly_")
	if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
		return false
	}
	return len(strings.Split(line, "\t")) >= 7
}

// looksLikeNetscapeCookies accepts a paste holding at least one cookie line,
// so a stray copy of something else is refused before it overwrites a
// working cookies file.
func looksLikeNetscapeCookies(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if cookieLine(line) {
			return true
		}
	}
	return false
}

// countCookies counts a cookies file's cookie lines, 0 when unreadable. The
// count is the only thing the UI shows about the file - its contents are
// credentials and are never echoed.
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

// refreshSiteLists re-derives the gallery-dl-facing site lists after a
// profile change: the runner's resolve-pass overrides and the managed config
// both follow the effective profiles.
func (s *Server) refreshSiteLists() {
	if rl, ok := s.runner.(interface {
		SetSiteLists(flatTagSites, metadataSites, notesSites []string)
	}); ok {
		rl.SetSiteLists(s.mapper.FlatTagSites(), s.mapper.MetadataSites(), s.mapper.NotesSites())
	}
	s.rewriteGDLConfig()
}
