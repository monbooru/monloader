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
	"github.com/monbooru/monloader/internal/logx"
)

// Via is the origin label stamped on every pushed image and its initial tags.
// It lands on monbooru's images.origin and each tag's
// tagger_name.
const Via = "monloader"

// CategoryDirectlink is gallery-dl's pseudo-extractor for a bare media URL with
// no booru behind it. It carries no post id or template, so its source and url
// come from the file's host and the file URL itself.
const CategoryDirectlink = "directlink"

// PushFields is the monbooru-shaped result of mapping one gallery-dl item.
// Tags already carry their category prefixes and include the rating tag;
// Rating is also exposed bare for the cbz strictest-wins bundle rule.
type PushFields struct {
	Tags            []string
	Rating          string
	Source          string
	Via             string
	Collection      string
	CollectionOrder int
	Commentary      string
	Original        string
	ParentURL       string
	Notes           []NoteBox
	// What the post says about the file it serves, for monbooru's upgrade
	// surface. Zero where the site publishes nothing, which is common.
	PostWidth, PostHeight int
	PostSize              int64
	PostExt               string
}

// NoteBox is one positional note overlaid on an image, in original-image pixel
// coordinates (Danbooru "notes"). BodyHTML keeps the source's own markup beside
// the flattened body; monbooru converts it and falls back to Body.
type NoteBox struct {
	X, Y, W, H int
	Body       string
	BodyHTML   string
}

// Mapper turns gallery-dl metadata into monbooru push fields using the
// built-in profiles, the user profile files layered over them, and the user
// override tables in config. The override tables and per-site galleries are
// read from the config provider on each map, so a settings save takes effect
// without racing the worker goroutine.
type Mapper struct {
	builtin map[string]Profile
	userDir string
	cfg     *config.Provider
	// ps is the merged profile view, swapped whole on a profile save so a
	// reader never observes a half-rebuilt map.
	ps atomic.Pointer[profileSet]
}

// profileSet is one immutable snapshot of the merged profiles. hostSite
// indexes each templated profile's post-URL host so a similarity service's
// result URL can be recognized and rebuilt canonically; aliasSite indexes the
// declared host aliases, which only name a site for source labeling; custom
// marks the categories whose user profile differs from the shipped one.
type profileSet struct {
	profiles  map[string]Profile
	hostSite  map[string]string
	aliasSite map[string]string
	custom    map[string]bool
}

// New builds a Mapper from the embedded profiles, the user profile files in
// profilesDir (missing or empty is fine), and the config provider. A lookup
// order on a site whose profile has no md5 template only warns: profiles are
// runtime-editable files, so a stale order must degrade the chain, not fail
// the boot.
func New(cfg *config.Provider, profilesDir string) (*Mapper, error) {
	builtin, err := loadProfiles()
	if err != nil {
		return nil, err
	}
	m := &Mapper{builtin: builtin, userDir: profilesDir, cfg: cfg}
	m.reload()
	for _, site := range cfg.Current().Sites {
		if site.LookupOrder > 0 && m.LookupURL(site.Name, "") == "" {
			logx.Warnf("site %q has lookup_order set but no md5 lookup support; the lookup chain skips it", site.Name)
		}
	}
	return m, nil
}

// view returns the current profile snapshot.
func (m *Mapper) view() *profileSet { return m.ps.Load() }

// reload rebuilds the merged view from the built-ins and the user profile
// files. A user profile identical to the shipped one is not a customization,
// so it is ignored (and the settings save deletes such a file instead of
// writing it).
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

// LookupSource is one entry of the unified lookup chain: a site's exact-md5
// search, or an image-similarity service when Similarity is set.
type LookupSource struct {
	Name       string
	Similarity bool
}

// LookupChain is the unified lookup walk: the sites whose [[sites]] block
// carries a lookup order and the similarity services with one, merged on the
// shared number space, ascending (ties by name). A source without an order is
// never queried, so opting in is as explicit as configuring a credential.
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
	for name, svc := range map[string]config.LookupService{
		"iqdb":     cfg.Lookup.Iqdb,
		"saucenao": cfg.Lookup.Saucenao,
	} {
		if svc.Order > 0 {
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

// LookupSites is the chain's exact-md5 half: the site sources in chain order.
// A hash import walks only these - a pasted hash carries no image bytes for a
// similarity query - and they rank the candidates a similarity hit offers.
func (m *Mapper) LookupSites() []string {
	var out []string
	for _, src := range m.LookupChain() {
		if !src.Similarity {
			out = append(out, src.Name)
		}
	}
	return out
}

// Gallery resolves the target monbooru gallery for a site: the per-source
// setting when set, else the configured default.
func (m *Mapper) Gallery(category string) string {
	if site := m.cfg.Current().FindSite(category); site != nil && site.Gallery != "" {
		return site.Gallery
	}
	return m.cfg.Current().Monbooru.DefaultGallery
}

// SourceLabel resolves what a push stamps as its source for a site name or a
// bare host: the [[sites]] label when set, a known category unchanged, and a
// host through the config host_labels rows (exact host first, parent domains
// after, winning over profiles as the override tables do) then the profiles'
// post-URL hosts and host aliases. Nothing matching keeps the name as-is, so
// no label lands that the operator did not choose.
func (m *Mapper) SourceLabel(name string) string {
	cfg := m.cfg.Current()
	if site := cfg.FindSite(name); site != nil && site.Label != "" {
		return site.Label
	}
	v := m.view()
	if _, ok := v.profiles[name]; ok {
		return name
	}
	host := normalizeHost(name)
	// A self-hosted source almost always carries a port, which is the case host
	// labels exist for; an exact host:port row still wins, and the bare host the
	// field's hint describes matches it too.
	for _, start := range []string{host, hostWithoutPort(host)} {
		for cand := start; cand != ""; cand = parentDomain(cand) {
			for _, hl := range cfg.HostLabels {
				if normalizeHost(hl.Host) == cand {
					return hl.Label
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
			return site.Label
		}
		return category
	}
	return name
}

// hostWithoutPort drops a trailing :port, leaving a bracketed IPv6 literal and
// a bare host alone. Returns host unchanged when there is nothing to drop, so
// the caller's second pass is a no-op rather than a special case.
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

// parentDomain strips a host's leftmost segment ("" past the registrable
// part), so img.website.com walks to website.com and stops.
func parentDomain(host string) string {
	i := strings.Index(host, ".")
	if i < 0 || !strings.Contains(host[i+1:], ".") {
		return ""
	}
	return host[i+1:]
}

// Map turns one gallery-dl item's metadata into monbooru push fields.
func (m *Mapper) Map(meta map[string]any) PushFields {
	category := kwdict.String(meta, "category")
	profile := m.profileFor(category)

	pf := PushFields{
		Source: m.SourceLabel(category),
		Via:    Via,
	}
	if category == CategoryDirectlink {
		// A bare media URL has no booru behind it, so its host is the closest
		// thing to a site label.
		pf.Source = m.SourceLabel(kwdict.String(meta, "domain"))
	}

	seen := map[string]bool{}
	var tags []string
	add := func(s string) {
		s = applyTagRule(profile.TagRules, normalizeTag(s))
		if s != "" && !seen[s] {
			seen[s] = true
			tags = append(tags, s)
		}
	}

	if suffixes := tagSuffixes(meta); len(suffixes) > 0 {
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
		// No per-category tags (a flat-tag site without tags:true, or an
		// unmapped source): the combined list lands as general, except entries
		// carrying a recognized namespace prefix (zerochan's "Character:name",
		// philomena boorus), which route by it.
		for _, name := range parseTagField(meta["tags"]) {
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

	// Manga and comic gallery extractors expose their categorized tags as
	// separate list fields (artist, parody, characters, ...) rather than
	// tags_<category> keys; fold those in for manga-kind sources.
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
	pf.Rating = m.ratingFor(category, profile, raw)
	if pf.Rating != "" {
		add("rating:" + pf.Rating)
	}

	slices.Sort(tags)
	pf.Tags = tags

	if kwdict.String(meta, "subcategory") == "pool" {
		pf.Collection = PoolName(meta)
		pf.CollectionOrder = kwdict.Num(meta)
	}

	pf.Commentary = commentaryFor(profile.Family, meta)
	pf.Original = originalFor(meta)
	if id := parentPostID(meta); id != "" {
		pf.ParentURL = m.PostURLFor(category, id)
	}
	pf.Notes = notesFor(profile, meta)
	pf.PostWidth, pf.PostHeight, pf.PostSize, pf.PostExt = postFileFor(meta)

	return pf
}

// postFileFor reads what the post says its file is. gallery-dl normalizes
// width / height across the boorus; the size and extension keys vary, so
// each falls back through the spellings the supported sites use. Anything
// missing stays zero: monbooru keeps whatever it already had.
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

// parentPostID returns the id of the post's declared parent, "" when none.
// Boorus emit parent_id at the top level (0, null, or absent for none); the
// e621 family nests it under relationships.
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

// originalFor reads the booru post's own source field - the upstream artist
// URL (pixiv, twitter), sometimes a dead host or free text. Most families
// carry a single `source` string; e621 a `sources` list, philomena a
// `source_url` string plus a `source_urls` list. Several entries join with
// newlines, matching monbooru's one-per-line render.
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

// commentaryFor reduces a post's artist commentary to a single plain-text body.
// Danbooru carries a nested artist_commentary object (each field translated
// preferred over its original, so a title-only translation still keeps the
// original body; title folded in as the first line); e621 a flat description.
func commentaryFor(family string, meta map[string]any) string {
	switch family {
	case FamilyDanbooru:
		ac, ok := meta["artist_commentary"].(map[string]any)
		if !ok {
			return ""
		}
		title := kwdict.String(ac, "translated_title")
		desc := kwdict.String(ac, "translated_description")
		if title == "" {
			title = kwdict.String(ac, "original_title")
		}
		if desc == "" {
			desc = kwdict.String(ac, "original_description")
		}
		return joinCommentary(title, desc)
	case FamilyE621:
		return plainText(kwdict.String(meta, "description"))
	}
	return ""
}

func joinCommentary(title, desc string) string {
	title, desc = plainText(title), plainText(desc)
	switch {
	case title == "":
		return desc
	case desc == "":
		return title
	default:
		return title + "\n\n" + desc
	}
}

// notesFor extracts the active positional note boxes (Danbooru "notes"). Every
// note-carrying source shares the danbooru field names: danbooru and e621 flag
// each note is_active, the gelbooru/moebooru pages and the sankaku notes API
// carry only active ones. Other sources carry none.
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

// markupHTML returns the source's own body only when it carries markup the
// flattening dropped, so a plain note is not pushed twice.
func markupHTML(raw, plain string) string {
	if strings.TrimSpace(raw) == plain {
		return ""
	}
	return raw
}

// plainText reduces a booru's HTML note body (or commentary) to plain text:
// entities are decoded first so entity-encoded markup is stripped like literal
// markup instead of being reintroduced after the strip, then <br> and CRLF
// become newlines and the remaining tags are dropped. DText markup, which is
// not HTML, is left as readable text. Notes also travel in their original form
// (NoteBox.BodyHTML); a monbooru that cannot read that falls back to this body.
func plainText(s string) string {
	if s == "" {
		return ""
	}
	s = html.UnescapeString(s)
	s = brRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

// PoolName resolves a pool's name. `pool` is either a map carrying the name or
// a bare id; fall back to "pool <id>" for the bare form. Empty when absent.
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

// PostURL builds the canonical source post page from the site profile's
// template. A directlink (a bare media URL) has no template, so its canonical
// link is the file URL itself, rebuilt from the metadata. A source with no
// template (the generic fallback) still gets a page URL when the extractor
// exposes one as permalink metadata, so a bulk job's items link to their own
// posts rather than all inheriting the submitted listing URL.
func (m *Mapper) PostURL(meta map[string]any) string {
	category := kwdict.String(meta, "category")
	if category == CategoryDirectlink {
		return directlinkURL(meta)
	}
	profile := m.profileFor(category)
	if profile.PostURL == "" {
		return permalinkURL(meta)
	}
	return strings.ReplaceAll(profile.PostURL, "{id}", kwdict.ID(meta))
}

// permalinkURL reads gallery-dl's permalink key, taken only when it is an
// absolute http(s) URL: some extractors emit site-relative paths or free text
// there, which would make a broken source link.
func permalinkURL(meta map[string]any) string {
	raw := strings.TrimSpace(kwdict.String(meta, "permalink"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

// categoryFor resolves a tag suffix to a monbooru category, with user config
// overrides winning over the profile, which wins over the universal table.
func (m *Mapper) categoryFor(site string, profile Profile, suffix string) (string, bool) {
	for _, o := range m.cfg.Current().TagOverrides {
		if o.Site == site && o.From == suffix && o.To != "" {
			return o.To, true
		}
	}
	if to, ok := profile.CategoryOverrides[suffix]; ok {
		return to, to != ""
	}
	return baseCategory(suffix)
}

// ratingFor resolves a booru rating value to a monbooru level, with user
// config overrides winning over the profile, which wins over the family rule.
func (m *Mapper) ratingFor(site string, profile Profile, raw string) string {
	for _, o := range m.cfg.Current().RatingOverrides {
		if o.Site == site && strings.EqualFold(o.From, strings.TrimSpace(raw)) {
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
	// Adult-only sites with no per-post rating fall back to their profile
	// default so the push is not exposed under a safe ceiling.
	return profile.DefaultRating
}

// SuspectFlattenedTags reports whether a site that should carry per-category
// tags_<x> fields returned an item with none of them. Every categorizing family
// must carry them, so none present is the fingerprint of a gallery-dl bump
// renaming or dropping those fields, after which the mapper would silently
// flatten every tag into general. The pipeline logs it so the regression
// surfaces instead of quietly corrupting the destination library.
func (m *Mapper) SuspectFlattenedTags(meta map[string]any) bool {
	p := m.profileFor(kwdict.String(meta, "category"))
	categorized := p.Family == FamilyDanbooru || p.Family == FamilyE621 ||
		needsTagsFamily(p.Family) || p.NeedsTags
	return categorized && len(tagSuffixes(meta)) == 0
}

// directlinkURL rebuilds the file URL gallery-dl's directlink pseudo-extractor
// matched from the split parts it exposes. The scheme is not among them; these
// links are https in practice, so https is assumed. An empty path is skipped so
// a root-level file does not gain a double slash.
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
