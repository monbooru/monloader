package mapping

import (
	"cmp"
	"fmt"
	"html"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
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

// PushFields carries the rating twice: in Tags, and bare in Rating for the cbz
// strictest-wins rule. The pipeline fills in URL and PostID.
type PushFields struct {
	contract.PostFields
	Rating          string
	Via             string
	Collection      string
	CollectionOrder int
	ClientTags      []string
	// Said on the item: monbooru reads such a tag's prefix as part of its name.
	UnknownTargets []string
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
	// A typo in the file is only knowable once monbooru has answered, since
	// a custom category is as good a target or rule as a built-in one.
	overridesChecked sync.Once
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
	for _, name := range cfg.Current().Tags.DropCategories {
		if name == "" || !validCategoryTarget(name) {
			logx.Warnf("tags.drop_categories names %q, no tag category (%s); it is skipped", name, strings.Join(TagCategories, ", "))
		}
	}
	for _, o := range cfg.Current().TagOverrides {
		if !validCategoryTarget(o.To) {
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

// Only once a list has been read: before that nothing is known about a name.
func unknownTargets(profile Profile, tags []string) []string {
	customCategoriesMu.RLock()
	learned := customCategoriesLearned
	customCategoriesMu.RUnlock()
	if !learned {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	note := func(target string) {
		if target == "" || isTagCategory(target) || seen[target] {
			return
		}
		if slices.ContainsFunc(tags, func(tag string) bool {
			return strings.HasPrefix(tag, target+":") || strings.Contains(tag, ":"+target+":")
		}) {
			seen[target] = true
			out = append(out, fmt.Sprintf("category %q is not in monbooru, so its tags land as general tags named %s:...", target, target))
		}
	}
	for _, to := range profile.CategoryOverrides {
		note(to)
	}
	for _, to := range profile.TagRules {
		if prefix, _, found := strings.Cut(to, ":"); found {
			note(prefix)
		}
	}
	slices.Sort(out)
	return out
}

// CategoryListMissing is whether a rule names a category that is not a built-in
// while no complete list was ever learned: mapped now, a blacklist rule would fail open
// and a target would misfile its tags.
func (m *Mapper) CategoryListMissing() bool {
	customCategoriesMu.RLock()
	learned := customCategoriesLearned
	customCategoriesMu.RUnlock()
	if learned {
		return false
	}
	cfg := m.cfg.Current()
	unknown := func(target string) bool { return target != "" && !isTagCategory(target) }
	if slices.ContainsFunc(cfg.TagOverrides, func(o config.TagOverride) bool { return unknown(o.To) }) {
		return true
	}
	badRule := func(line string) bool {
		_, bad := parseBlacklistRule(line)
		return bad != ""
	}
	if slices.ContainsFunc(cfg.Tags.Blacklist, badRule) {
		return true
	}
	for _, site := range cfg.Sites {
		if slices.ContainsFunc(site.Blacklist, badRule) {
			return true
		}
	}
	for _, p := range m.view().profiles {
		for _, to := range p.CategoryOverrides {
			if unknown(to) {
				return true
			}
		}
		for _, to := range p.TagRules {
			if prefix, _, found := strings.Cut(to, ":"); found && unknown(prefix) {
				return true
			}
		}
	}
	return false
}

// LearnCustomCategories takes every gallery's list: a site's target gallery can
// change between jobs. A complete list replaces what was known, so a renamed or
// deleted category stops reading as there; a partial one only adds.
func (m *Mapper) LearnCustomCategories(names []string, complete bool) {
	customCategoriesMu.Lock()
	if complete {
		clear(customCategories)
	}
	for _, name := range names {
		customCategories[name] = true
	}
	customCategoriesLearned = customCategoriesLearned || complete
	customCategoriesMu.Unlock()
	if !complete {
		return
	}
	m.overridesChecked.Do(func() {
		cfg := m.cfg.Current()
		for _, o := range cfg.TagOverrides {
			if o.To != "" && validCategoryTarget(o.To) && !isTagCategory(o.To) {
				logx.Warnf("tag override %s %q -> %q names no category monbooru has; it is skipped", o.Site, o.From, o.To)
			}
		}
		warn := func(where string, lines []string) {
			for _, line := range lines {
				if _, bad := parseBlacklistRule(line); bad != "" {
					logx.Warnf("%s blacklist rule %q: %q is not a tag, a category:name or a rating; the rule is skipped", where, line, bad)
				}
			}
		}
		warn("the global", cfg.Tags.Blacklist)
		for _, site := range cfg.Sites {
			warn(site.Name+"'s", site.Blacklist)
		}
	})
}

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

func (m *Mapper) MissingCredential(site string) (string, bool) {
	prof, ok := m.Lookup(site)
	if !ok {
		return "", false
	}
	return RequiredCredential(prof.Auth, m.cfg.Current().FindSite(site))
}

func (m *Mapper) SourceQueryable(src LookupSource, simMissing func(string) (string, bool)) bool {
	var missing bool
	if src.Similarity {
		_, missing = simMissing(src.Name)
	} else {
		_, missing = m.MissingCredential(src.Name)
	}
	return !missing
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
	for cand := host; !ok && cand != ""; cand = parentDomain(cand) {
		category, ok = v.aliasSite[cand]
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
	if i := strings.LastIndex(host, ":"); i >= 0 && (i == len(host)-1 || allDigits(host[i+1:])) {
		return host[:i]
	}
	return host
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

	pf := PushFields{PostFields: contract.PostFields{Source: m.SourceLabel(category)}, Via: Via}
	if category == CategoryDirectlink {
		pf.Source = m.SourceLabel(kwdict.String(meta, "domain"))
	}

	var tags []string
	drop := m.cfg.Current().Tags.DropCategories
	var withheld []string
	add := func(_, s string) {
		listed := normalizeTag(s)
		s = applyTagRule(profile.TagRules, listed)
		if s == "" && listed != "" {
			withheld = append(withheld, listed)
		}
		if sentTag(drop, s) {
			tags = append(tags, s)
		}
	}
	dropped := func(_, s string) { withheld = append(withheld, normalizeTag(s)) }

	m.eachTag(category, profile, meta, add, dropped)

	pf.Rating = m.postRating(category, profile, meta)
	if pf.Rating != "" {
		tags = append(tags, "rating:"+pf.Rating)
	}

	slices.Sort(tags)
	pf.Tags = slices.Compact(tags)
	slices.Sort(withheld)
	pf.WithheldTags = slices.Compact(withheld)
	pf.UnknownTargets = unknownTargets(profile, pf.Tags)

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

// eachTag hands over every listed tag with its field, as category:name where the
// category is known and before any tag rule. dropped gets the tags of a category
// the site's map drops, under the category they would land in.
func (m *Mapper) eachTag(category string, profile Profile, meta map[string]any, visit, dropped func(field, tag string)) {
	named := func(next func(field, tag string)) func(field, tag string) {
		return func(field, tag string) {
			if normalizeTag(tag) != "" {
				next(field, tag)
			}
		}
	}
	visit, dropped = named(visit), named(dropped)
	suffixes := tagSuffixes(meta)
	// A post whose only suffix is an opt-in extra keeps its own flat list.
	flat := (len(suffixes) == 0 || meta["tags"] != nil) &&
		!slices.ContainsFunc(suffixes, func(suffix string) bool { return !extraTagSuffixes[suffix] })
	for _, suffix := range suffixes {
		if _, overridden := m.categoryOverride(category, profile, suffix); flat && !overridden {
			continue
		}
		cat, keep := m.categoryFor(category, profile, suffix)
		if !keep {
			if base, ok := baseCategory(suffix); ok {
				for _, name := range parseTagField(meta["tags_"+suffix]) {
					dropped("tags_"+suffix, formatTag(base, name))
				}
			}
			continue
		}
		for _, name := range parseTagField(meta["tags_"+suffix]) {
			visit("tags_"+suffix, formatTag(cat, name))
		}
	}
	if flat {
		// A flat tag list lands as general, except entries with a recognized
		// namespace prefix (zerochan's "Character:name", philomena boorus).
		for _, name := range parseTagField(flatTags(meta)) {
			if profile.Family == FamilyPhilomena && philomenaRatingTag(name) {
				continue // lifted to the rating field below, not a content tag
			}
			key, base, sub := "general", "general", name
			ns, rest, _ := strings.Cut(name, ":")
			ns = strings.ToLower(ns)
			if cat, n, ok := splitNamespace(name); ok {
				key, base, sub = ns, cat, n
			} else if _, row := m.categoryOverride(category, profile, ns); row && rest != "" {
				key, sub = ns, rest
			}
			m.fileTag(category, profile, "tags", key, base, sub, visit, dropped)
		}
	}

	if profile.Kind == KindManga {
		for _, f := range mangaTagFields {
			for _, name := range parseNameField(meta[f.field]) {
				m.fileTag(category, profile, f.field, f.field, f.category, name, visit, dropped)
			}
		}
	}
}

func (m *Mapper) fileTag(site string, profile Profile, field, key, base, name string, visit, dropped func(field, tag string)) {
	to, ok := m.categoryOverride(site, profile, key)
	switch {
	case !ok:
		visit(field, formatTag(base, name))
	case to == "":
		dropped(field, formatTag(base, name))
	default:
		visit(field, formatTag(to, name))
	}
}

func (m *Mapper) postRating(category string, profile Profile, meta map[string]any) string {
	return m.ratingFor(category, profile, listedRating(profile, meta))
}

func listedRating(profile Profile, meta map[string]any) string {
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
	return raw
}

func categoryDropped(drop []string, tag string) bool { return droppedCategory(drop, tag) != "" }

// An unrated image shows under every rating ceiling, so the rating is
// never dropped.
func droppedCategory(drop []string, tag string) string {
	if len(drop) == 0 || strings.HasPrefix(tag, "rating:") {
		return ""
	}
	category := "general"
	if prefix, _, found := strings.Cut(tag, ":"); found && (isTagCategory(prefix) || slices.Contains(drop, prefix)) {
		category = prefix
	}
	if !slices.Contains(drop, category) {
		return ""
	}
	return category
}

// monbooru lets a later rating tag of a push replace an earlier one, so only the
// mapped rating's goes, not one a tag rule made.
func sentTag(drop []string, tag string) bool {
	return tag != "" && !strings.HasPrefix(tag, "rating:") && !categoryDropped(drop, tag)
}

// civitai nests an image's tags under file when it comes from a post;
// bluesky's only tags are the hashtags gallery-dl parses out of its facets.
func flatTags(meta map[string]any) any {
	if v := meta["tags"]; v != nil {
		return v
	}
	file, _ := meta["file"].(map[string]any)
	if v := file["tags"]; v != nil {
		return v
	}
	return meta["hashtags"]
}

// Anything missing stays zero, and monbooru keeps what it already had.
func postFileFor(meta map[string]any) (w, h int, size int64, ext string) {
	file, _ := meta["file"].(map[string]any)
	w = cmp.Or(kwdict.Int(meta, "width"), kwdict.Int(meta, "image_width"), kwdict.Int(file, "width"))
	h = cmp.Or(kwdict.Int(meta, "height"), kwdict.Int(meta, "image_height"), kwdict.Int(file, "height"))
	size = int64(cmp.Or(kwdict.Int(meta, "file_size"), kwdict.Int(meta, "filesize"), kwdict.Int(meta, "size"), kwdict.Int(file, "size")))
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
func notesFor(profile Profile, meta map[string]any) []contract.NoteBox {
	if profile.Family != FamilyDanbooru && profile.Family != FamilyE621 &&
		!needsNotesFamily(profile.Family) && !profile.HasNotes {
		return nil
	}
	raw, ok := meta["notes"].([]any)
	if !ok {
		return nil
	}
	var out []contract.NoteBox
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
		out = append(out, contract.NoteBox{
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

// Without a template or an id the post's own link metadata is used, so a bulk job's
// items link to their own posts rather than the submitted listing.
func (m *Mapper) PostURL(meta map[string]any) string {
	if u := fillTemplate(m.profileFor(kwdict.String(meta, "category")).PostURL, meta); u != "" {
		return u
	}
	return permalinkURL(meta)
}

// Some extractors put a site-relative path or free text in these.
func permalinkURL(meta map[string]any) string {
	for _, key := range []string{"permalink", "post_url"} {
		if raw := strings.TrimSpace(kwdict.String(meta, key)); config.IsHTTPURL(raw) {
			return raw
		}
	}
	return ""
}

func (m *Mapper) categoryFor(site string, profile Profile, suffix string) (string, bool) {
	if to, ok := m.categoryOverride(site, profile, suffix); ok {
		return to, to != ""
	}
	return baseCategory(suffix)
}

// ok reports an override exists; a blank to means it drops the tags.
func (m *Mapper) categoryOverride(site string, profile Profile, suffix string) (to string, ok bool) {
	for _, o := range m.cfg.Current().TagOverrides {
		if o.Site == site && o.From == suffix && (o.To == "" || isTagCategory(o.To)) {
			return o.To, true
		}
	}
	to, ok = profile.CategoryOverrides[suffix]
	return to, ok
}

// ActiveOverrides lists the toml overrides that win over a site's profile.
func (m *Mapper) ActiveOverrides(site string) (tags []config.TagOverride, ratings []config.RatingOverride) {
	cfg := m.cfg.Current()
	for _, o := range cfg.TagOverrides {
		if o.Site == site && (o.To == "" || isTagCategory(o.To)) {
			tags = append(tags, o)
		}
	}
	for _, o := range cfg.RatingOverrides {
		if o.Site == site && validRatings[o.To] {
			ratings = append(ratings, o)
		}
	}
	return tags, ratings
}

func (m *Mapper) ratingFor(site string, profile Profile, raw string) string {
	for _, o := range m.cfg.Current().RatingOverrides {
		if o.Site == site && strings.EqualFold(o.From, strings.TrimSpace(raw)) && validRatings[o.To] {
			return o.To
		}
	}
	for from, to := range profile.RatingOverrides {
		if strings.EqualFold(from, strings.TrimSpace(raw)) {
			return to
		}
	}
	if r := mapRating(profile.Family, raw); r != "" {
		return r
	}
	return profile.DefaultRating
}

const FlattenedTagsMsg = "the post came with no per-category tags - the site's page or gallery-dl's tag fields may have changed, so nothing was sent"

// SuspectFlattenedTags flags a categorizing site's post with tags but no
// tags_<x> fields: the fingerprint of a gallery-dl bump or a post page with no
// tag list, which would flatten every tag into general.
func (m *Mapper) SuspectFlattenedTags(meta map[string]any) bool {
	p := m.profileFor(kwdict.String(meta, "category"))
	categorized := p.Family == FamilyDanbooru || p.Family == FamilyE621 ||
		needsTagsFamily(p.Family) || p.NeedsTags
	return categorized && len(tagSuffixes(meta)) == 0 && len(parseTagField(flatTags(meta))) > 0
}
