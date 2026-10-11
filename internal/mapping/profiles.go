package mapping

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
)

// A booru family decides its rating semantics (the `s` overload) and its
// per-category tag regime.
const (
	FamilyDanbooru    = "danbooru"
	FamilyE621        = "e621"
	FamilyMoebooru    = "moebooru"
	FamilyGelbooruV02 = "gelbooru_v02"
	FamilyPhilomena   = "philomena"
	FamilyGeneric     = "generic"
)

// A manga gallery's pages bundle into one cbz; KindOther (twitter,
// artstation) only groups sites in the settings tables and pushes like a
// booru.
const (
	KindBooru = "booru"
	KindManga = "manga"
	KindOther = "other"
)

// Optional kinds work anonymously; oauth is driven by gallery-dl's own
// flow, its tokens carried in the site's options.
const (
	AuthNone             = "none"
	AuthAPIOptional      = "api_optional"
	AuthAPIRequired      = "api_required"
	AuthUsernamePassword = "username_password"
	AuthCookies          = "cookies"
	AuthOAuth            = "oauth"
)

// Profile carries Example because --list-extractors blanks the category of
// the booru base extractors, so their example cannot be looked up by name.
type Profile struct {
	Family  string `json:"family"`
	Kind    string `json:"kind,omitempty"`
	PostURL string `json:"post_url_template"`
	// A tag-search URL gallery-dl's extractors match, so the hash lookup
	// rides the normal resolve path.
	MD5Search string `json:"md5_search_template,omitempty"`
	Auth      string `json:"auth"`
	Example   string `json:"example,omitempty"`
	// File CDNs and mirror instances beyond the post-URL host. They label
	// a bare file link as this site, and a registrable domain covers its
	// subdomains for clients that suffix-match.
	Hosts             []string          `json:"hosts,omitempty"`
	CategoryOverrides map[string]string `json:"category_overrides,omitempty"`
	RatingOverrides   map[string]string `json:"rating_overrides,omitempty"`
	// Applies when the source has no rating; adult-only sites set
	// "explicit" so a push never lands under a safe ceiling.
	DefaultRating string `json:"default_rating,omitempty"`
	// For a generic-family site that emits tags_<category> only with
	// gallery-dl's `tags: true` (an extra request per post); moebooru and
	// gelbooru get it by family.
	NeedsTags bool `json:"needs_tags,omitempty"`
	// For a generic-family site whose extractor emits notes only with
	// gallery-dl's `notes: true`; the note-carrying families get it by
	// family.
	HasNotes bool `json:"has_notes,omitempty"`
	// Profiles are shareable, so secrets belong in the site's [[sites]]
	// options, never here.
	Options map[string]any `json:"options,omitempty"`
	// Keyed by normalized name or full category:name: "" suppresses the
	// tag, a bare name renames it in its category, and a category:name
	// value retargets it.
	TagRules map[string]string `json:"tag_rules,omitempty"`
}

//go:embed profiles/*.json
var profilesFS embed.FS

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

func RequiredCredential(auth string, site *config.Site) (label string, missing bool) {
	switch auth {
	case AuthAPIRequired:
		return "api key", site == nil || site.APIKey == ""
	case AuthCookies:
		return "cookies", site == nil || site.Cookies == ""
	}
	return "", false
}

func LoginLabel(auth string, site *config.Site) (label string, missing bool) {
	switch auth {
	case AuthAPIOptional:
		return "api (opt)", false
	case AuthAPIRequired, AuthCookies:
		return RequiredCredential(auth, site)
	case AuthUsernamePassword:
		return "user/pass", false
	case AuthOAuth:
		return "oauth", false
	default:
		return "none", false
	}
}

var genericProfile = Profile{Family: FamilyGeneric}

func (m *Mapper) profileFor(category string) Profile {
	if p, ok := m.view().profiles[category]; ok {
		return p
	}
	return genericProfile
}

func (m *Mapper) Lookup(category string) (Profile, bool) {
	p, ok := m.view().profiles[category]
	return p, ok
}

func (m *Mapper) BuiltinProfile(category string) (Profile, bool) {
	p, ok := m.builtin[category]
	return p, ok
}

func (m *Mapper) ExampleURL(category, fallback string) string {
	if p, ok := m.Lookup(category); ok && p.Example != "" {
		return p.Example
	}
	return fallback
}

func (m *Mapper) LookupURL(category, md5 string) string {
	p, ok := m.view().profiles[category]
	if !ok || p.MD5Search == "" {
		return ""
	}
	return strings.ReplaceAll(p.MD5Search, "{md5}", md5)
}

func (m *Mapper) PostURLFor(category, id string) string {
	p, ok := m.view().profiles[category]
	if !ok || p.PostURL == "" || id == "" {
		return ""
	}
	return strings.ReplaceAll(p.PostURL, "{id}", id)
}

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

// Kept apart from hostIndex: an alias is a file host, not a post-URL
// shape, so it labels a site but is never rebuilt into a post URL.
func aliasIndex(profiles map[string]Profile) map[string]string {
	idx := map[string]string{}
	for category, p := range profiles {
		for _, h := range p.Hosts {
			idx[normalizeHost(h)] = category
		}
	}
	return idx
}

func normalizeHost(host string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
}

// Similarity services return decade-old shapes like post/show. Only a digit
// id in a post position counts, so /users/999 is not rebuilt into a post.
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

var postPathMarkers = map[string]bool{"posts": true, "post": true, "show": true, "view": true}

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

// A gelbooru-style template with {id} only in the query yields neither,
// leaving the id parameter or a generic marker.
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
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func (m *Mapper) KindOf(category string) string {
	if p, ok := m.view().profiles[category]; ok && p.Kind == KindManga {
		return KindManga
	}
	return KindBooru
}

func needsTagsFamily(family string) bool {
	return family == FamilyMoebooru || family == FamilyGelbooruV02
}

func needsNotesFamily(family string) bool {
	return family == FamilyMoebooru || family == FamilyGelbooruV02
}

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

func (m *Mapper) CuratedCategories() []string {
	return m.curatedWhere(func(Profile) bool { return true })
}

func (m *Mapper) FlatTagSites() []string {
	return m.curatedWhere(func(p Profile) bool { return needsTagsFamily(p.Family) || p.NeedsTags })
}

// The danbooru family needs gallery-dl's metadata include for commentary
// and notes; e621 only for notes, one extra request per noted post.
func (m *Mapper) MetadataSites() []string {
	return m.curatedWhere(func(p Profile) bool { return p.Family == FamilyDanbooru || p.Family == FamilyE621 })
}

// The gelbooru and moebooru families parse notes from the page their
// tags: true fetch already loads; HasNotes sites ask a notes API for
// posts flagged as noted.
func (m *Mapper) NotesSites() []string {
	return m.curatedWhere(func(p Profile) bool { return needsNotesFamily(p.Family) || p.HasNotes })
}

func (m *Mapper) SiteOptions() map[string]map[string]any {
	out := map[string]map[string]any{}
	for category, p := range m.view().profiles {
		if len(p.Options) > 0 {
			out[category] = p.Options
		}
	}
	return out
}
