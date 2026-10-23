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
	"github.com/monbooru/monloader/internal/shared/fsx"
	"github.com/monbooru/monloader/internal/shared/logx"
)

// The name becomes a profile file name and a managed-config key: a
// crafted one must not escape the directory or smuggle config structure.
var categoryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func ValidCategoryName(name string) bool { return categoryNameRE.MatchString(name) }

// MatchesQuery expects q already lower-cased.
func MatchesQuery(q string, fields ...string) bool {
	return slices.ContainsFunc(fields, func(f string) bool { return strings.Contains(strings.ToLower(f), q) })
}

var (
	ProfileFamilies = []string{FamilyGeneric, FamilyDanbooru, FamilyE621, FamilyMoebooru, FamilyGelbooruV02, FamilyPhilomena}
	ProfileKinds    = []string{KindBooru, KindManga, KindOther}
	ProfileAuths    = []string{AuthNone, AuthAPIOptional, AuthAPIRequired, AuthUsernamePassword, AuthCookies, AuthOAuth}
	RatingLevels    = []string{RatingGeneral, RatingSensitive, RatingQuestionable, RatingExplicit}
	TagCategories   = []string{"general", "artist", "character", "copyright", "meta", "species", "person", "medium", "year"}
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

// "Supported" and "Required" are both account logins; the column does not
// say whether one is optional, so neither seeds a required credential.
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

// A bad file is skipped with a warning, never a boot failure: the
// built-in or generic profile applies until it is fixed.
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

func (m *Mapper) CustomProfile(category string) bool { return m.view().custom[category] }

func (m *Mapper) CustomCategories() []string {
	return slices.Sorted(maps.Keys(m.view().custom))
}

// Saving the shipped profile deletes the file, so "differs from the
// built-in" needs no bookkeeping.
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
	if err := fsx.WriteFile(filepath.Join(m.userDir, category+".json"), append(data, '\n')); err != nil {
		return err
	}
	m.reload()
	return nil
}

func (m *Mapper) DeleteUserProfile(category string) error {
	if m.userDir == "" {
		return fmt.Errorf("no profiles directory configured")
	}
	if !ValidCategoryName(category) {
		return fmt.Errorf("invalid site name %q", category)
	}
	// Reverting can drop a user md5 template the lookup chain relies on.
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
