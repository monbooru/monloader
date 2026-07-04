package mapping

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/leqwin/monloader/internal/gdl"
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
// bundle into one cbz the way a booru pool does.
const (
	KindBooru = "booru"
	KindManga = "manga"
)

// Profile is one curated site mapping keyed by gallery-dl category.
// Example is a representative URL for the settings "test" probe;
// it is carried here because gallery-dl's --list-extractors blanks the category
// for the booru base extractors, so it cannot be looked up by category there.
type Profile struct {
	Family            string            `json:"family"`
	Kind              string            `json:"kind,omitempty"`
	PostURL           string            `json:"post_url_template"`
	Auth              string            `json:"auth"`
	Example           string            `json:"example,omitempty"`
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
}

//go:embed profiles.json
var profilesJSON []byte

// loadProfiles decodes the embedded built-in profiles.
func loadProfiles() (map[string]Profile, error) {
	var profiles map[string]Profile
	if err := json.Unmarshal(profilesJSON, &profiles); err != nil {
		return nil, fmt.Errorf("decoding embedded profiles.json: %w", err)
	}
	return profiles, nil
}

// genericProfile is the fallback for any gallery-dl category without a
// curated entry: per-category tags by name where they match a monbooru
// category else general, tolerant rating with `s` treated as safe, and no
// post URL template.
var genericProfile = Profile{Family: FamilyGeneric}

// profileFor returns the curated profile for a category, or the generic
// fallback so an unmapped site still works the day gallery-dl supports it.
func (m *Mapper) profileFor(category string) Profile {
	if p, ok := m.profiles[category]; ok {
		return p
	}
	return genericProfile
}

// Lookup returns the curated profile for a gallery-dl category and whether a
// curated entry exists (false means the generic fallback applies).
func (m *Mapper) Lookup(category string) (Profile, bool) {
	p, ok := m.profiles[category]
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

// KindOf returns a category's curated kind (KindBooru or KindManga). Unmapped
// or unspecified sites are booru-shaped, so they push per post rather than
// bundling into a cbz.
func (m *Mapper) KindOf(category string) string {
	if p, ok := m.profiles[category]; ok && p.Kind == KindManga {
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

// CuratedCategories returns every curated gallery-dl category, sorted. The
// sites endpoint surfaces each as a named entry so a multi-instance family
// (gallery-dl lists the danbooru family only as danbooru.donmai.us) stays
// recognizable by its other instance hosts, e.g. aibooru.online.
func (m *Mapper) CuratedCategories() []string {
	out := make([]string, 0, len(m.profiles))
	for c := range m.profiles {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CuratedByKind returns the curated categories of a given kind (KindBooru or
// KindManga), sorted, so the settings page can group them into two tables. A
// profile with no kind set counts as a booru.
func (m *Mapper) CuratedByKind(kind string) []string {
	out := make([]string, 0, len(m.profiles))
	for c, p := range m.profiles {
		pk := p.Kind
		if pk == "" {
			pk = KindBooru
		}
		if pk == kind {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// FlatTagSites returns the curated categories whose family needs
// `tags: true`, sorted. The gallery-dl config writer consumes this so those
// sites emit categorized tags.
func (m *Mapper) FlatTagSites() []string {
	var out []string
	for category, p := range m.profiles {
		if needsTagsFamily(p.Family) || p.NeedsTags {
			out = append(out, category)
		}
	}
	sort.Strings(out)
	return out
}

// MetadataSites returns the curated categories whose extractor needs
// gallery-dl's `metadata` include, sorted. The danbooru family carries artist
// commentary and notes behind it; e621 reads the same include for its notes
// (one extra request per noted post - the description rides the default post
// fields, and the commentary token is ignored).
func (m *Mapper) MetadataSites() []string {
	var out []string
	for category, p := range m.profiles {
		if p.Family == FamilyDanbooru || p.Family == FamilyE621 {
			out = append(out, category)
		}
	}
	sort.Strings(out)
	return out
}

// NotesSites returns the curated categories whose extractor emits positional
// note boxes only with gallery-dl's `notes: true`, sorted. The gelbooru and
// moebooru families parse them from the same post page their `tags: true`
// fetch already loads; sankaku-like sites (HasNotes) ask their notes API for
// posts flagged as noted.
func (m *Mapper) NotesSites() []string {
	var out []string
	for category, p := range m.profiles {
		if needsNotesFamily(p.Family) || p.HasNotes {
			out = append(out, category)
		}
	}
	sort.Strings(out)
	return out
}
