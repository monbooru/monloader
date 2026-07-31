package mapping

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

// categoryNameRE bounds a gallery-dl category as it is used in profile file
// names and managed-config keys, so a crafted name cannot escape the profiles
// directory or smuggle config structure.
var categoryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidCategoryName reports whether a site name is safe to use as a profile
// file name and a gallery-dl extractor key.
func ValidCategoryName(name string) bool { return categoryNameRE.MatchString(name) }

// The value lists the settings profile editor renders; the validation sets
// derive from them so the form and the file loader cannot drift apart.
var (
	ProfileFamilies = []string{FamilyGeneric, FamilyDanbooru, FamilyE621, FamilyMoebooru, FamilyGelbooruV02, FamilyPhilomena}
	ProfileKinds    = []string{KindBooru, KindManga, KindOther}
	ProfileAuths    = []string{AuthNone, AuthAPIOptional, AuthAPIRequired, AuthUsernamePassword, AuthCookies, AuthOAuth}
	RatingLevels    = []string{RatingGeneral, RatingSensitive, RatingQuestionable, RatingExplicit}
	// TagCategories are the monbooru categories a profile may route tags to;
	// in an override map, "" drops the tag.
	TagCategories = []string{"general", "artist", "character", "copyright", "meta", "species", "person", "medium", "year"}
)

func setOf(vals []string, extra ...string) map[string]bool {
	set := make(map[string]bool, len(vals)+len(extra))
	for _, v := range append(vals, extra...) {
		set[v] = true
	}
	return set
}

var (
	validFamilies      = setOf(ProfileFamilies)
	validKinds         = setOf(ProfileKinds, "")
	validAuths         = setOf(ProfileAuths, "")
	validRatings       = setOf(RatingLevels)
	validTagCategories = setOf(TagCategories, "")
)

// SeedAuthKind maps supportedsites.md's Authentication column to the profile
// auth vocabulary. "Supported" and "Required" are both account logins - the
// column does not say whether the login is optional, so neither seeds a
// required credential.
func SeedAuthKind(label string) string {
	switch label {
	case "Supported", "Required":
		return AuthUsernamePassword
	case "API Key":
		return AuthAPIRequired
	case "Cookies":
		return AuthCookies
	case "OAuth":
		return AuthOAuth
	}
	return AuthNone
}

// ValidateProfile checks a profile the way the settings save refuses and the
// file loader skips, so a hand-edited file and a form submit meet one rule.
func ValidateProfile(p Profile) error {
	if !validFamilies[p.Family] {
		return fmt.Errorf("unknown family %q", p.Family)
	}
	if !validKinds[p.Kind] {
		return fmt.Errorf("unknown kind %q", p.Kind)
	}
	if !validAuths[p.Auth] {
		return fmt.Errorf("unknown auth kind %q", p.Auth)
	}
	if p.PostURL != "" && (!config.IsHTTPURL(p.PostURL) || !strings.Contains(p.PostURL, "{id}")) {
		return fmt.Errorf("post url template must be an http(s) URL containing {id}")
	}
	if p.MD5Search != "" && (!config.IsHTTPURL(p.MD5Search) || !strings.Contains(p.MD5Search, "{md5}")) {
		return fmt.Errorf("md5 search template must be an http(s) URL containing {md5}")
	}
	if p.Example != "" && !config.IsHTTPURL(p.Example) {
		return fmt.Errorf("example must be an http(s) URL")
	}
	for _, h := range p.Hosts {
		if h == "" || strings.ContainsAny(h, "/ \t") || strings.Contains(h, "://") {
			return fmt.Errorf("host alias %q is not a bare hostname", h)
		}
	}
	if p.DefaultRating != "" && !validRatings[p.DefaultRating] {
		return fmt.Errorf("unknown default rating %q", p.DefaultRating)
	}
	for from, to := range p.RatingOverrides {
		if !validRatings[to] {
			return fmt.Errorf("rating %q maps to unknown level %q", from, to)
		}
	}
	for from, to := range p.CategoryOverrides {
		if !validTagCategories[to] {
			return fmt.Errorf("tag category %q maps to unknown category %q", from, to)
		}
	}
	for from, to := range p.TagRules {
		if from == "" || normalizeTag(from) != from {
			return fmt.Errorf("tag rule %q is not a normalized tag name", from)
		}
		if to != "" && (normalizeTag(to) != to || normalizeTag(to) == "") {
			return fmt.Errorf("tag rule %q has a replacement that is not a normalized tag name", from)
		}
	}
	return nil
}

// loadUserProfiles reads the user profile files in dir, one <category>.json
// per site. A missing directory is fine; a file that fails to parse or
// validate is skipped with a warning, never a boot failure - the built-in or
// generic profile applies for that category until the file is fixed.
func loadUserProfiles(dir string) map[string]Profile {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("profiles: reading %s: %v", dir, err)
		}
		return nil
	}
	out := map[string]Profile{}
	for _, e := range entries {
		name, isJSON := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !isJSON {
			continue
		}
		if !ValidCategoryName(name) {
			logx.Warnf("profiles: ignoring %s: not a valid gallery-dl category name", e.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			logx.Warnf("profiles: reading %s: %v", e.Name(), err)
			continue
		}
		var p Profile
		if err := json.Unmarshal(data, &p); err != nil {
			logx.Warnf("profiles: skipping %s: %v", e.Name(), err)
			continue
		}
		if err := ValidateProfile(p); err != nil {
			logx.Warnf("profiles: skipping %s: %v", e.Name(), err)
			continue
		}
		out[name] = p
	}
	return out
}

// CustomProfile reports whether a user profile file overrides the shipped
// profile (or adds one) for a category.
func (m *Mapper) CustomProfile(category string) bool { return m.view().custom[category] }

// CustomCategories returns the categories whose user profile differs from the
// shipped one, sorted. The settings sites tables surface these even without a
// [[sites]] block.
func (m *Mapper) CustomCategories() []string {
	return slices.Sorted(maps.Keys(m.view().custom))
}

// SaveUserProfile validates and writes a category's user profile file, then
// reloads the merged view so the change applies at once. A profile equal to
// the shipped one deletes the file instead of writing it, so "differs from
// the built-in" needs no separate bookkeeping.
func (m *Mapper) SaveUserProfile(category string, p Profile) error {
	if m.userDir == "" {
		return fmt.Errorf("no profiles directory configured")
	}
	if !ValidCategoryName(category) {
		return fmt.Errorf("invalid site name %q", category)
	}
	if err := ValidateProfile(p); err != nil {
		return err
	}
	if p.MD5Search == "" {
		if site := m.cfg.Current().FindSite(category); site != nil && site.LookupOrder > 0 {
			return fmt.Errorf("site %s is in the lookup chain; remove it there (settings, lookup section) before dropping its md5 search template", category)
		}
	}
	if b, ok := m.builtin[category]; ok && reflect.DeepEqual(p, b) {
		return m.DeleteUserProfile(category)
	}
	if err := os.MkdirAll(m.userDir, 0o755); err != nil {
		return fmt.Errorf("creating profiles directory: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding profile: %w", err)
	}
	err = config.WriteFileAtomic(filepath.Join(m.userDir, category+".json"), func(f *os.File) error {
		_, err := f.Write(append(data, '\n'))
		return err
	})
	if err != nil {
		return err
	}
	m.reload()
	return nil
}

// DeleteUserProfile removes a category's user profile file (absent is fine)
// and reloads, so the shipped or generic profile shows through again.
func (m *Mapper) DeleteUserProfile(category string) error {
	if m.userDir == "" {
		return fmt.Errorf("no profiles directory configured")
	}
	if !ValidCategoryName(category) {
		return fmt.Errorf("invalid site name %q", category)
	}
	// Reverting can drop a user-supplied md5 template the lookup chain relies
	// on; refuse the same way a template-dropping save is refused.
	if site := m.cfg.Current().FindSite(category); site != nil && site.LookupOrder > 0 {
		if b, ok := m.builtin[category]; !ok || b.MD5Search == "" {
			return fmt.Errorf("site %s is in the lookup chain; remove it there (settings, lookup section) before resetting its profile", category)
		}
	}
	if err := os.Remove(filepath.Join(m.userDir, category+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.reload()
	return nil
}
