package mapping

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
)

// Booru families, which decide rating semantics (the `s` overload) and the
// per-category tag regime.
const (
	FamilyDanbooru    = "danbooru"
	FamilyE621        = "e621"
	FamilyMoebooru    = "moebooru"
	FamilyGelbooruV02 = "gelbooru_v02"
	FamilyPhilomena   = "philomena"
	FamilyGeneric     = "generic"
)

// Site kinds. A booru post is pushed on its own; a manga/comic gallery's pages
// bundle into one cbz the way a booru pool does. KindOther groups the sites
// that are neither (twitter, artstation) in the settings tables; the pipeline
// treats it like a booru.
const (
	KindBooru = "booru"
	KindManga = "manga"
	KindOther = "other"
)

// Profile auth kinds: which credential a site signs in with. Optional kinds
// work anonymously; oauth is surfaced but driven by gallery-dl's own flow,
// its tokens carried via the site's options.
const (
	AuthNone             = "none"
	AuthAPIOptional      = "api_optional"
	AuthAPIRequired      = "api_required"
	AuthUsernamePassword = "username_password"
	AuthCookies          = "cookies"
	AuthOAuth            = "oauth"
)

// Profile is one curated site mapping keyed by gallery-dl category.
// Example is a representative URL for the settings "test" probe;
// it is carried here because gallery-dl's --list-extractors blanks the category
// for the booru base extractors, so it cannot be looked up by category there.
type Profile struct {
	Family  string `json:"family"`
	Kind    string `json:"kind,omitempty"`
	PostURL string `json:"post_url_template"`
	// MD5Search is the site's md5 search URL with {md5} substituted - a
	// tag-search form gallery-dl's extractors match, so the hash lookup rides
	// the normal resolve path. Absent for sites that do not index md5.
	MD5Search string `json:"md5_search_template,omitempty"`
	Auth      string `json:"auth"`
	Example   string `json:"example,omitempty"`
	// Hosts are extra hosts that belong to the site beyond its post URL -
	// file CDNs and mirror instances - so a bare file link on one of them is
	// labeled as this site rather than by its raw hostname, and API clients
	// can recognize the site on any of them. A registrable domain covers its
	// subdomains for clients that suffix-match.
	Hosts             []string          `json:"hosts,omitempty"`
	CategoryOverrides map[string]string `json:"category_overrides,omitempty"`
	RatingOverrides   map[string]string `json:"rating_overrides,omitempty"`
	// DefaultRating applies when the source provides no rating (manga and
	// comic galleries carry none); adult-only sites set "explicit" so their
	// pushes are not visible under a safe rating ceiling.
	DefaultRating string `json:"default_rating,omitempty"`
	// NeedsTags marks a generic-family site that only emits per-category
	// tags_<category> with gallery-dl's `tags: true` (an extra request per
	// post), e.g. sankaku. moebooru/gelbooru get it by family instead.
	NeedsTags bool `json:"needs_tags,omitempty"`
	// HasNotes marks a generic-family site whose extractor emits positional
	// note boxes with gallery-dl's `notes: true`, e.g. sankaku. The booru
	// families that carry notes get it by family instead.
	HasNotes bool `json:"has_notes,omitempty"`
	// Options are gallery-dl extractor options written under
	// extractor.<category> in the managed config. Profiles are shareable, so
	// secrets belong in the site's [[sites]] options, never here.
	Options map[string]any `json:"options,omitempty"`
	// TagRules corrects individual tags a site sends, keyed by the normalized
	// tag name (or a full "category:name" form): "" suppresses the tag, a
	// bare name renames it keeping its category, and a "category:name" value
	// retargets it.
	TagRules map[string]string `json:"tag_rules,omitempty"`
}

// The built-in profiles ship one file per gallery-dl category, named
// <category>.json, so a contributed profile is a single new file.
//
//go:embed profiles/*.json
var profilesFS embed.FS

// loadProfiles decodes the embedded built-in profiles, keyed by file name.
func loadProfiles() (map[string]Profile, error) {
	entries, err := profilesFS.ReadDir("profiles")
	if err != nil {
		return nil, fmt.Errorf("reading embedded profiles: %w", err)
	}
	profiles := make(map[string]Profile, len(entries))
	for _, e := range entries {
		data, err := profilesFS.ReadFile("profiles/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading embedded profile %s: %w", e.Name(), err)
		}
		var p Profile
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("decoding embedded profile %s: %w", e.Name(), err)
		}
		profiles[strings.TrimSuffix(e.Name(), ".json")] = p
	}
	return profiles, nil
}

// RequiredCredential names the credential a profile's auth kind demands and
// whether the site config lacks it. The optional and none kinds demand
// nothing. Both the lookup walk and the settings sites table gate on this, so
// the two views cannot disagree about what a site needs.
func RequiredCredential(auth string, site *config.Site) (label string, missing bool) {
	switch auth {
	case AuthAPIRequired:
		return "api key", site == nil || site.APIKey == ""
	case AuthCookies:
		return "cookies", site == nil || site.Cookies == ""
	}
	return "", false
}

// genericProfile is the fallback for any gallery-dl category without a
// curated entry: per-category tags by name where they match a monbooru
// category else general, tolerant rating with `s` treated as safe, and no
// post URL template.
var genericProfile = Profile{Family: FamilyGeneric}

// profileFor returns the curated profile for a category, or the generic
// fallback so an unmapped site still works the day gallery-dl supports it.
func (m *Mapper) profileFor(category string) Profile {
	if p, ok := m.view().profiles[category]; ok {
		return p
	}
	return genericProfile
}

// Lookup returns the curated profile for a gallery-dl category and whether a
// curated entry exists (false means the generic fallback applies).
func (m *Mapper) Lookup(category string) (Profile, bool) {
	p, ok := m.view().profiles[category]
	return p, ok
}

// BuiltinProfile returns the shipped profile for a category, ignoring
// any user file. The site dialog's export tab diffs the effective
// profile against this to color what the install changes.
func (m *Mapper) BuiltinProfile(category string) (Profile, bool) {
	p, ok := m.builtin[category]
	return p, ok
}

// ExampleURL returns a representative URL to probe for a site: the curated
// profile's example first (gallery-dl's --list-extractors blanks the category
// for the booru base extractors, so its example cannot be found by name there),
// then the cached extractor list as a fallback.
func (m *Mapper) ExampleURL(extractors []gdl.Extractor, category string) string {
	if p, ok := m.Lookup(category); ok && p.Example != "" {
		return p.Example
	}
	for _, ex := range extractors {
		if ex.Category == category && ex.Example != "" {
			return ex.Example
		}
	}
	return ""
}

// LookupURL builds a site's md5 search URL, or "" when its profile carries no
// template (the site does not index md5, so it cannot be looked up).
func (m *Mapper) LookupURL(category, md5 string) string {
	p, ok := m.view().profiles[category]
	if !ok || p.MD5Search == "" {
		return ""
	}
	return strings.ReplaceAll(p.MD5Search, "{md5}", md5)
}

// PostURLFor builds a category's canonical post URL from a bare post id, or
// "" when its profile carries no template.
func (m *Mapper) PostURLFor(category, id string) string {
	p, ok := m.view().profiles[category]
	if !ok || p.PostURL == "" || id == "" {
		return ""
	}
	return strings.ReplaceAll(p.PostURL, "{id}", id)
}

// hostIndex maps each templated profile's post-URL host (lowercased, www
// stripped) to its category, so a URL can be recognized by host alone.
func hostIndex(profiles map[string]Profile) map[string]string {
	idx := map[string]string{}
	for category, p := range profiles {
		if p.PostURL == "" {
			continue
		}
		if u, err := url.Parse(p.PostURL); err == nil && u.Host != "" {
			idx[normalizeHost(u.Host)] = category
		}
	}
	return idx
}

// aliasIndex maps each profile's declared host aliases to its category. Kept
// apart from hostIndex: an alias is a file host, not a post-URL shape, so it
// names a site for labeling but is never rebuilt into a canonical post URL.
func aliasIndex(profiles map[string]Profile) map[string]string {
	idx := map[string]string{}
	for category, p := range profiles {
		for _, h := range p.Hosts {
			idx[normalizeHost(h)] = category
		}
	}
	return idx
}

// normalizeHost folds a host to the form the indexes compare: lowercased,
// leading www stripped.
func normalizeHost(host string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
}

// CanonicalPostURL recognizes a curated site's post URL in any of its
// historical forms - similarity services return links in decade-old
// "post/show" shapes - and rebuilds it through the profile's template. The
// host names the site; the id is the "id" query parameter or an all-digit
// path segment sitting in a post position, so a curated-host link that is not
// a post (/users/999, /pools/456) is dropped rather than rebuilt into one.
func (m *Mapper) CanonicalPostURL(rawURL string) (site, canonical string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	category, ok := m.view().hostSite[normalizeHost(u.Host)]
	if !ok {
		return "", "", false
	}
	id := u.Query().Get("id")
	if id == "" {
		id = postIDFromPath(u.Path, m.view().profiles[category].PostURL)
	}
	if id == "" || !allDigits(id) {
		return "", "", false
	}
	return category, m.PostURLFor(category, id), true
}

// postPathMarkers are the segments that may precede a post id across the
// families' current and historical URL shapes: /posts/123, /post/show/123,
// /post/view/123.
var postPathMarkers = map[string]bool{"posts": true, "post": true, "show": true, "view": true}

// postIDFromPath finds the post id in a URL path: the last all-digit segment
// in a post position - after a generic post marker, after the segment the
// site's own template puts before {id}, or leading the path when the template
// puts {id} first (zerochan, twibooru).
func postIDFromPath(path, template string) string {
	marker, rootOK := templateIDPosition(template)
	segs := pathSegments(path)
	var id string
	for i, seg := range segs {
		if !allDigits(seg) {
			continue
		}
		switch {
		case i == 0:
			if rootOK {
				id = seg
			}
		case postPathMarkers[segs[i-1]] || (marker != "" && segs[i-1] == marker):
			id = seg
		}
	}
	return id
}

// templateIDPosition locates {id} in a post-URL template's path: the segment
// before it, and whether it leads the path. A template carrying {id} only in
// its query (the gelbooru family) yields neither, so for such a site only the
// id parameter or a generic marker names a post.
func templateIDPosition(template string) (marker string, rootOK bool) {
	u, err := url.Parse(template)
	if err != nil {
		return "", false
	}
	segs := pathSegments(u.Path)
	for i, seg := range segs {
		if strings.Contains(seg, "{id}") {
			if i == 0 {
				return "", true
			}
			return segs[i-1], false
		}
	}
	return "", false
}

func pathSegments(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// KindOf returns a category's curated kind (KindBooru or KindManga). Unmapped
// or unspecified sites are booru-shaped, so they push per post rather than
// bundling into a cbz.
func (m *Mapper) KindOf(category string) string {
	if p, ok := m.view().profiles[category]; ok && p.Kind == KindManga {
		return KindManga
	}
	return KindBooru
}

// needsTagsFamily reports whether a family must set gallery-dl `tags: true`
// to emit per-category tags (the flat-tag families).
func needsTagsFamily(family string) bool {
	return family == FamilyMoebooru || family == FamilyGelbooruV02
}

// needsNotesFamily reports whether a family emits its positional note boxes
// only with gallery-dl `notes: true`, parsed from the same post page the
// `tags: true` fetch already loads.
func needsNotesFamily(family string) bool {
	return family == FamilyMoebooru || family == FamilyGelbooruV02
}

// curatedWhere returns the curated categories whose profile matches pred,
// sorted.
func (m *Mapper) curatedWhere(pred func(Profile) bool) []string {
	var out []string
	for category, p := range m.view().profiles {
		if pred(p) {
			out = append(out, category)
		}
	}
	slices.Sort(out)
	return out
}

// CuratedCategories returns every curated gallery-dl category, sorted. The
// sites endpoint surfaces each as a named entry so a multi-instance family
// (gallery-dl lists the danbooru family only as danbooru.donmai.us) stays
// recognizable by its other instance hosts, e.g. aibooru.online.
func (m *Mapper) CuratedCategories() []string {
	return m.curatedWhere(func(Profile) bool { return true })
}

// CuratedByKind returns the curated categories of a given kind (KindBooru or
// KindManga), sorted, so the settings page can group them into two tables. A
// profile with no kind set counts as a booru.
func (m *Mapper) CuratedByKind(kind string) []string {
	return m.curatedWhere(func(p Profile) bool {
		pk := p.Kind
		if pk == "" {
			pk = KindBooru
		}
		return pk == kind
	})
}

// FlatTagSites returns the curated categories whose family needs
// `tags: true`, sorted. The gallery-dl config writer consumes this so those
// sites emit categorized tags.
func (m *Mapper) FlatTagSites() []string {
	return m.curatedWhere(func(p Profile) bool { return needsTagsFamily(p.Family) || p.NeedsTags })
}

// MetadataSites returns the curated categories whose extractor needs
// gallery-dl's `metadata` include, sorted. The danbooru family carries artist
// commentary and notes behind it; e621 reads the same include for its notes
// (one extra request per noted post - the description rides the default post
// fields, and the commentary token is ignored).
func (m *Mapper) MetadataSites() []string {
	return m.curatedWhere(func(p Profile) bool { return p.Family == FamilyDanbooru || p.Family == FamilyE621 })
}

// NotesSites returns the curated categories whose extractor emits positional
// note boxes only with gallery-dl's `notes: true`, sorted. The gelbooru and
// moebooru families parse them from the same post page their `tags: true`
// fetch already loads; sankaku-like sites (HasNotes) ask their notes API for
// posts flagged as noted.
func (m *Mapper) NotesSites() []string {
	return m.curatedWhere(func(p Profile) bool { return needsNotesFamily(p.Family) || p.HasNotes })
}

// SiteOptions collects each profile's own gallery-dl options for the managed
// config writer, keyed by category.
func (m *Mapper) SiteOptions() map[string]map[string]any {
	out := map[string]map[string]any{}
	for category, p := range m.view().profiles {
		if len(p.Options) > 0 {
			out[category] = p.Options
		}
	}
	return out
}
