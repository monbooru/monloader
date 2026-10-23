package mapping

import (
	"cmp"
	"html"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/logx"
)

// Via lands in monbooru's images.origin and each pushed tag's tagger_name.
const Via = contract.Via

// CategoryDirectlink is gallery-dl's pseudo-extractor for a bare media
// URL with no booru behind it.
const CategoryDirectlink = "directlink"

// PushFields carries the rating twice: in Tags, and bare in Rating for
// the cbz strictest-wins rule.
type PushFields struct {
	Tags                 []string
	Rating               string
	Source               string
	Via                  string
	Collection           string
	CollectionOrder      int
	Commentary           string
	CommentaryTranslated string
	// The DText the source published, which monbooru converts; an older
	// monbooru reads the plain fields above.
	CommentaryDText           string
	CommentaryTranslatedDText string
	Original                  string
	ParentURL                 string
	Notes                     []NoteBox
	PostWidth, PostHeight     int
	PostSize                  int64
	PostExt                   string
}

// NoteBox coordinates are original-image pixels. monbooru converts
// BodyHTML and falls back to Body.
type NoteBox struct {
	X, Y, W, H int
	Body       string
	BodyHTML   string
}

// Mapper reads the config provider on every call, so a settings save
// lands without racing the worker goroutine.
type Mapper struct {
	builtin map[string]Profile
	userDir string
	cfg     *config.Provider
	// Swapped whole on a profile save, so a reader never sees a
	// half-rebuilt map.
	ps atomic.Pointer[profileSet]
}

type profileSet struct {
	profiles  map[string]Profile
	hostSite  map[string]string
	aliasSite map[string]string
	custom    map[string]bool
}

// A lookup order on a site with no md5 template only warns: profiles are
// editable at runtime, so a stale order degrades the chain rather than
// failing the boot.
func New(cfg *config.Provider, profilesDir string) (*Mapper, error) {
	builtin, err := loadProfiles()
	if err != nil {
		return nil, err
	}
	m := &Mapper{builtin: builtin, userDir: profilesDir, cfg: cfg}
	m.reload()
	for _, o := range cfg.Current().RatingOverrides {
		if !validRatings[o.To] {
			logx.Warnf("rating override %s %q -> %q names no rating level (%s); it is skipped", o.Site, o.From, o.To, strings.Join(RatingLevels, ", "))
		}
	}
	for _, o := range cfg.Current().TagOverrides {
		if o.To != "" && !validTagCategories[o.To] {
			logx.Warnf("tag override %s %q -> %q names no tag category (%s); it is skipped", o.Site, o.From, o.To, strings.Join(TagCategories, ", "))
		}
	}
	for _, site := range cfg.Current().Sites {
		if site.LookupOrder > 0 && m.LookupURL(site.Name, "") == "" {
			logx.Warnf("site %q has lookup_order set but no md5 lookup support; the lookup chain skips it", site.Name)
		}
	}
	return m, nil
}

func (m *Mapper) view() *profileSet { return m.ps.Load() }

func (m *Mapper) reload() {
	profiles := maps.Clone(m.builtin)
	custom := map[string]bool{}
	for category, p := range loadUserProfiles(m.userDir) {
		if b, ok := m.builtin[category]; ok && reflect.DeepEqual(p, b) {
			continue
		}
		profiles[category] = p
		custom[category] = true
	}
	m.ps.Store(&profileSet{profiles: profiles, hostSite: hostIndex(profiles), aliasSite: aliasIndex(profiles), custom: custom})
}

type LookupSource struct {
	Name       string
	Similarity bool
}

// A source without an order is never queried: opting in is as explicit as
// configuring a credential.
func (m *Mapper) LookupChain() []LookupSource {
	type entry struct {
		src   LookupSource
		order int
	}
	var entries []entry
	cfg := m.cfg.Current()
	for _, site := range cfg.Sites {
		if site.LookupOrder > 0 && m.LookupURL(site.Name, "") != "" {
			entries = append(entries, entry{LookupSource{Name: site.Name}, site.LookupOrder})
		}
	}
	for _, name := range config.LookupServices {
		if svc := cfg.Lookup.Service(name); svc.Order > 0 {
			entries = append(entries, entry{LookupSource{Name: name, Similarity: true}, svc.Order})
		}
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		return cmp.Or(cmp.Compare(a.order, b.order), cmp.Compare(a.src.Name, b.src.Name))
	})
	out := make([]LookupSource, len(entries))
	for i, e := range entries {
		out[i] = e.src
	}
	return out
}

func (m *Mapper) LookupSites() []string {
	var out []string
	for _, src := range m.LookupChain() {
		if !src.Similarity {
			out = append(out, src.Name)
		}
	}
	return out
}

func (m *Mapper) Gallery(category string) string {
	if site := m.cfg.Current().FindSite(category); site != nil && site.Gallery != "" {
		return site.Gallery
	}
	return m.cfg.Current().Monbooru.DefaultGallery
}

// SourceLabel keeps an unmatched name as-is, so no label lands that the
// operator did not choose.
func (m *Mapper) SourceLabel(name string) string {
	label, _ := m.sourceLabel(name)
	return label
}

func (m *Mapper) SourceClaimed(name string) bool {
	_, claimed := m.sourceLabel(name)
	return claimed
}

func (m *Mapper) sourceLabel(name string) (label string, claimed bool) {
	cfg := m.cfg.Current()
	if site := cfg.FindSite(name); site != nil && site.Label != "" {
		return site.Label, true
	}
	v := m.view()
	if _, ok := v.profiles[name]; ok {
		return name, true
	}
	host := normalizeHost(name)
	// Self-hosted sources usually carry a port: an exact host:port row
	// wins, then the bare host's row.
	for _, start := range []string{host, hostWithoutPort(host)} {
		for cand := start; cand != ""; cand = parentDomain(cand) {
			for _, hl := range cfg.HostLabels {
				if normalizeHost(hl.Host) == cand {
					return hl.Label, true
				}
			}
		}
	}
	category, ok := v.hostSite[host]
	if !ok {
		category, ok = v.aliasSite[host]
	}
	if ok {
		if site := cfg.FindSite(category); site != nil && site.Label != "" {
			return site.Label, true
		}
		return category, true
	}
	return name, false
}

func hostWithoutPort(host string) string {
	i := strings.LastIndex(host, ":")
	if i < 0 || strings.Contains(host[i+1:], "]") {
		return host
	}
	for _, r := range host[i+1:] {
		if r < '0' || r > '9' {
			return host
		}
	}
	return host[:i]
}

func parentDomain(host string) string {
	i := strings.Index(host, ".")
	if i < 0 || !strings.Contains(host[i+1:], ".") {
		return ""
	}
	return host[i+1:]
}

func (m *Mapper) Map(meta map[string]any) PushFields {
	category := kwdict.String(meta, "category")
	profile := m.profileFor(category)

	pf := PushFields{
		Source: m.SourceLabel(category),
		Via:    Via,
	}
	if category == CategoryDirectlink {
		pf.Source = m.SourceLabel(kwdict.String(meta, "domain"))
	}

	var tags []string
	// monbooru keeps the first rating tag of a push, so only pf.Rating's goes.
	add := func(s string) {
		if s = applyTagRule(profile.TagRules, normalizeTag(s)); s != "" && !strings.HasPrefix(s, "rating:") {
			tags = append(tags, s)
		}
	}

	suffixes := tagSuffixes(meta)
	// An opt-in extra such as pixiv's tags_bookmark is no category split:
	// the post's own flat list stays its tags.
	if meta["tags"] != nil && !m.anyCategorySuffix(category, profile, suffixes) {
		suffixes = nil
	}
	if len(suffixes) > 0 {
		for _, suffix := range suffixes {
			cat, keep := m.categoryFor(category, profile, suffix)
			if !keep {
				continue
			}
			for _, name := range parseTagField(meta["tags_"+suffix]) {
				add(formatTag(cat, name))
			}
		}
	} else {
		// A flat tag list lands as general, except entries with a recognized
		// namespace prefix (zerochan's "Character:name", philomena boorus).
		for _, name := range parseTagField(flatTags(meta)) {
			if profile.Family == FamilyPhilomena && philomenaRatingTag(name) {
				continue // lifted to the rating field below, not a content tag
			}
			if cat, n, ok := splitNamespace(name); ok {
				add(formatTag(cat, n))
			} else {
				add(name)
			}
		}
	}

	if profile.Kind == KindManga {
		for _, f := range mangaTagFields {
			for _, name := range parseNameField(meta[f.field]) {
				add(formatTag(f.category, name))
			}
		}
	}

	// Philomena boorus carry no rating field; the rating is a tag.
	raw := kwdict.String(meta, "rating")
	if raw == "" && profile.Family == FamilyPhilomena {
		raw = philomenaRating(meta)
	}
	if raw == "" {
		// civitai's numeric nsfwLevel means nothing until a profile's
		// rating overrides name each level.
		file, _ := meta["file"].(map[string]any)
		raw = kwdict.String(file, "nsfwLevel")
	}
	pf.Rating = m.ratingFor(category, profile, raw)
	if pf.Rating != "" {
		tags = append(tags, "rating:"+pf.Rating)
	}

	slices.Sort(tags)
	pf.Tags = slices.Compact(tags)

	if kwdict.String(meta, "subcategory") == "pool" {
		pf.Collection = PoolName(meta)
		pf.CollectionOrder = kwdict.Num(meta)
	}

	c := commentaryFor(profile.Family, meta)
	pf.Commentary, pf.CommentaryDText = c.Body, c.BodyDText
	pf.CommentaryTranslated, pf.CommentaryTranslatedDText = c.Translated, c.TranslatedDText
	pf.Original = originalFor(meta)
	if id := parentPostID(meta); id != "" {
		pf.ParentURL = m.PostURLFor(category, id)
	}
	pf.Notes = notesFor(profile, meta)
	pf.PostWidth, pf.PostHeight, pf.PostSize, pf.PostExt = postFileFor(meta)

	return pf
}

// civitai nests an image's tags under file when it comes from a post.
func flatTags(meta map[string]any) any {
	if v := meta["tags"]; v != nil {
		return v
	}
	file, _ := meta["file"].(map[string]any)
	return file["tags"]
}

// Anything missing stays zero, and monbooru keeps what it already had.
func postFileFor(meta map[string]any) (w, h int, size int64, ext string) {
	w, h = kwdict.Int(meta, "width"), kwdict.Int(meta, "height")
	for _, key := range []string{"file_size", "filesize", "size"} {
		if n := kwdict.Int(meta, key); n > 0 {
			size = int64(n)
			break
		}
	}
	return w, h, size, strings.ToLower(strings.TrimPrefix(kwdict.String(meta, "extension"), "."))
}

// Boorus emit parent_id at the top level (0 or null for none); e621 nests
// it under relationships.
func parentPostID(meta map[string]any) string {
	if id := kwdict.String(meta, "parent_id"); id != "" && id != "0" {
		return id
	}
	if rel, ok := meta["relationships"].(map[string]any); ok {
		if id := kwdict.String(rel, "parent_id"); id != "" && id != "0" {
			return id
		}
	}
	return ""
}

// e621 carries a sources list and philomena source_url plus source_urls;
// several entries join with newlines for monbooru's one-per-line render.
func originalFor(meta map[string]any) string {
	if s := strings.TrimSpace(kwdict.String(meta, "source")); s != "" {
		return s
	}
	for _, key := range []string{"sources", "source_urls"} {
		raw, ok := meta[key].([]any)
		if !ok {
			continue
		}
		var out []string
		for _, v := range raw {
			if s, ok := v.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		if len(out) > 0 {
			return strings.Join(out, "\n")
		}
	}
	return strings.TrimSpace(kwdict.String(meta, "source_url"))
}

type postCommentary struct {
	Body, BodyDText             string
	Translated, TranslatedDText string
}

// A translation with no original leads: monbooru shows the translation
// under the commentary, so an empty commentary would hide it.
func commentaryFor(family string, meta map[string]any) postCommentary {
	var c postCommentary
	switch family {
	case FamilyDanbooru:
		ac, ok := meta["artist_commentary"].(map[string]any)
		if !ok {
			return c
		}
		c.Body, c.BodyDText = joinCommentary(kwdict.String(ac, "original_title"), kwdict.String(ac, "original_description"))
		c.Translated, c.TranslatedDText = joinCommentary(kwdict.String(ac, "translated_title"), kwdict.String(ac, "translated_description"))
		if c.Body == "" {
			return postCommentary{Body: c.Translated, BodyDText: c.TranslatedDText}
		}
	case FamilyE621:
		desc := kwdict.String(meta, "description")
		c.Body, c.BodyDText = plainText(desc), rawText(desc)
	}
	return c
}

func joinCommentary(title, desc string) (plain, raw string) {
	return joinLines(plainText(title), plainText(desc)), joinLines(rawText(title), rawText(desc))
}

func joinLines(title, desc string) string {
	switch {
	case title == "":
		return desc
	case desc == "":
		return title
	default:
		return title + "\n\n" + desc
	}
}

// Note sources share danbooru's field names; only danbooru and e621 send
// is_active, the others send active notes only.
func notesFor(profile Profile, meta map[string]any) []NoteBox {
	if profile.Family != FamilyDanbooru && profile.Family != FamilyE621 &&
		!needsNotesFamily(profile.Family) && !profile.HasNotes {
		return nil
	}
	raw, ok := meta["notes"].([]any)
	if !ok {
		return nil
	}
	var out []NoteBox
	for _, item := range raw {
		n, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if active, ok := n["is_active"].(bool); ok && !active {
			continue
		}
		raw := kwdict.String(n, "body")
		body := plainText(raw)
		if body == "" {
			continue
		}
		out = append(out, NoteBox{
			X: kwdict.Int(n, "x"), Y: kwdict.Int(n, "y"),
			W: kwdict.Int(n, "width"), H: kwdict.Int(n, "height"),
			Body: body, BodyHTML: markupHTML(raw, body),
		})
	}
	return out
}

var (
	brRe  = regexp.MustCompile(`(?i)<br\s*/?>`)
	tagRe = regexp.MustCompile(`<[^>]*>`)
)

// Only these inline tags (tn is the translator note) survive: note markup
// comes from a remote, unreviewed api and monbooru cannot know where it
// came from.
var noteTagRe = regexp.MustCompile(`(?i)^</?(a|b|i|u|s|em|strong|br|tn)\s*/?>$`)

// A booru writes its tag links relative and styled, so only the href is kept.
var noteHrefRe = regexp.MustCompile(`(?i)^<a\s[^<>]*\bhref="([^"<>\s]+)"[^<>]*>$`)

func markupHTML(raw, plain string) string {
	safe := tagRe.ReplaceAllStringFunc(raw, func(tag string) string {
		if noteTagRe.MatchString(tag) {
			return tag
		}
		if m := noteHrefRe.FindStringSubmatch(tag); m != nil && noteHref(m[1]) {
			return `<a href="` + m[1] + `">`
		}
		return ""
	})
	if strings.TrimSpace(safe) == plain {
		return ""
	}
	return safe
}

// monbooru resolves a relative target against the post url, so only paths
// and http(s) URLs pass; anything else drops with its anchor.
func noteHref(href string) bool {
	low := strings.ToLower(href)
	return strings.HasPrefix(href, "/") ||
		strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")
}

// monbooru's converter reads the markup as written, and DText is not HTML
// to strip.
func rawText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimSpace(strings.ReplaceAll(s, "\r", "\n"))
}

// Entities decode before the strip, so entity-encoded markup is stripped
// like literal markup rather than reintroduced.
func plainText(s string) string {
	if s == "" {
		return ""
	}
	s = html.UnescapeString(s)
	s = brRe.ReplaceAllString(s, "\n")
	return rawText(tagRe.ReplaceAllString(s, ""))
}

func PoolName(meta map[string]any) string {
	if pool, ok := meta["pool"].(map[string]any); ok {
		if name := kwdict.String(pool, "name"); name != "" {
			return name
		}
	}
	if id := kwdict.String(meta, "pool"); id != "" {
		return "pool " + id
	}
	return ""
}

// Without a template or an id the permalink metadata is used, so a bulk job's
// items link to their own posts rather than the submitted listing.
func (m *Mapper) PostURL(meta map[string]any) string {
	category := kwdict.String(meta, "category")
	if category == CategoryDirectlink {
		return directlinkURL(meta)
	}
	profile := m.profileFor(category)
	if id := kwdict.ID(meta); profile.PostURL != "" && id != "" {
		return strings.ReplaceAll(profile.PostURL, "{id}", id)
	}
	return permalinkURL(meta)
}

// Some extractors put a site-relative path or free text in permalink.
func permalinkURL(meta map[string]any) string {
	raw := strings.TrimSpace(kwdict.String(meta, "permalink"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

func (m *Mapper) categoryFor(site string, profile Profile, suffix string) (string, bool) {
	for _, o := range m.cfg.Current().TagOverrides {
		if o.Site == site && o.From == suffix && validTagCategories[o.To] {
			return o.To, o.To != ""
		}
	}
	if to, ok := profile.CategoryOverrides[suffix]; ok {
		return to, to != ""
	}
	return baseCategory(suffix)
}

func (m *Mapper) anyCategorySuffix(site string, profile Profile, suffixes []string) bool {
	for _, suffix := range suffixes {
		if _, ok := baseCategories[suffix]; ok {
			return true
		}
		if _, ok := profile.CategoryOverrides[suffix]; ok {
			return true
		}
		if slices.ContainsFunc(m.cfg.Current().TagOverrides, func(o config.TagOverride) bool { return o.Site == site && o.From == suffix }) {
			return true
		}
	}
	return false
}

func (m *Mapper) ratingFor(site string, profile Profile, raw string) string {
	for _, o := range m.cfg.Current().RatingOverrides {
		if o.Site == site && strings.EqualFold(o.From, strings.TrimSpace(raw)) && validRatings[o.To] {
			return o.To
		}
	}
	if profile.RatingOverrides != nil {
		if to, ok := profile.RatingOverrides[strings.ToLower(strings.TrimSpace(raw))]; ok {
			return to
		}
	}
	if r := mapRating(profile.Family, raw); r != "" {
		return r
	}
	return profile.DefaultRating
}

// SuspectFlattenedTags flags a categorizing site's item with no tags_<x>
// fields: the fingerprint of a gallery-dl bump that would flatten every
// tag into general.
func (m *Mapper) SuspectFlattenedTags(meta map[string]any) bool {
	p := m.profileFor(kwdict.String(meta, "category"))
	categorized := p.Family == FamilyDanbooru || p.Family == FamilyE621 ||
		needsTagsFamily(p.Family) || p.NeedsTags
	return categorized && len(tagSuffixes(meta)) == 0
}

// The scheme is not among the parts gallery-dl exposes, so https is assumed.
func directlinkURL(meta map[string]any) string {
	domain := kwdict.String(meta, "domain")
	if domain == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("https://")
	b.WriteString(domain)
	if path := kwdict.String(meta, "path"); path != "" {
		b.WriteByte('/')
		b.WriteString(path)
	}
	b.WriteByte('/')
	b.WriteString(kwdict.String(meta, "filename"))
	if ext := kwdict.String(meta, "extension"); ext != "" {
		b.WriteByte('.')
		b.WriteString(ext)
	}
	if q := kwdict.String(meta, "query"); q != "" {
		b.WriteByte('?')
		b.WriteString(q)
	}
	if frag := kwdict.String(meta, "fragment"); frag != "" {
		b.WriteByte('#')
		b.WriteString(frag)
	}
	return b.String()
}
