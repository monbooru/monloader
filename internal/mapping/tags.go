package mapping

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/monbooru/monloader/internal/shared/contract"
)

// A comma is part of a name on space-joined sites (hello,_happy_world!), so it
// splits only when it ends every field but the last: realbooru's ", " join.
func parseTagField(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			// civitai sends each tag as an object carrying its name.
			if obj, ok := e.(map[string]any); ok {
				e = obj["name"]
			}
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		fields := strings.Fields(t)
		for i, f := range fields {
			if i < len(fields)-1 && !strings.HasSuffix(f, ",") {
				return fields
			}
		}
		for i, f := range fields {
			fields[i] = strings.TrimSuffix(f, ",")
		}
		return fields
	default:
		return nil
	}
}

// Never split on spaces: a string credit is one name ("Nakatani Nio") or
// several joined with commas.
func parseNameField(v any) []string {
	if s, ok := v.(string); ok {
		var out []string
		for _, name := range strings.Split(s, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	return parseTagField(v)
}

func tagSuffixes(meta map[string]any) []string {
	var out []string
	for k := range meta {
		if suffix, ok := strings.CutPrefix(k, "tags_"); ok && suffix != "" {
			out = append(out, suffix)
		}
	}
	slices.Sort(out)
	return out
}

// Manga extractors (nhentai, hitomi, mangadex, ...) publish categorized
// tags in these fields rather than tags_<category> keys.
var mangaTagFields = []struct {
	field, category string
}{
	{"artist", "artist"},
	{"artists", "artist"},
	{"author", "artist"},
	{"authors", "artist"},
	{"circle", "artist"},
	{"group", "artist"},
	{"characters", "character"},
	{"character", "character"},
	{"parody", "copyright"},
	{"parodies", "copyright"},
	{"genre", "general"},
	{"genres", "general"},
	{"type", "meta"},
}

// An empty category drops the tag: invalid and deprecated are not real tags.
var baseCategories = map[string]string{
	"general":     "general",
	"artist":      "artist",
	"character":   "character",
	"copyright":   "copyright",
	"meta":        "meta",
	"metadata":    "meta",
	"species":     "species",
	"medium":      "medium",
	"person":      "person",
	"year":        "year",
	"lore":        "meta",
	"contributor": "artist",
	"circle":      "artist",    // doujin circle
	"group":       "artist",    // doujin group
	"parody":      "copyright", // the parodied work
	"series":      "copyright", // the source work (e-hentai, schalenetwork)
	"studio":      "copyright", // animation / production studio (sankaku)
	"model":       "person",    // real-person model (realbooru and other photo boorus)
	"faults":      "meta",
	"invalid":     "",
	"deprecated":  "",
}

// pixiv's metadata-bookmark adds tags_bookmark beside a post's own flat list.
var extraTagSuffixes = map[string]bool{"bookmark": true}

func baseCategory(suffix string) (category string, keep bool) {
	cat, ok := baseCategories[suffix]
	if !ok {
		if isTagCategory(suffix) {
			return suffix, true
		}
		return "general", true
	}
	return cat, cat != ""
}

// An unrecognized prefix is an incidental colon: the tag stays whole, as
// general.
func splitNamespace(tag string) (category, name string, ok bool) {
	ns, rest, found := strings.Cut(tag, ":")
	if !found || rest == "" {
		return "", "", false
	}
	switch strings.ToLower(ns) {
	case "artist", "mangaka", "creator", "group", "circle":
		return "artist", rest, true
	case "character":
		return "character", rest, true
	case "copyright", "parody", "series", "game", "franchise", "studio":
		return "copyright", rest, true
	case "meta", "source":
		return "meta", rest, true
	case "medium":
		return "medium", rest, true
	case "general", "theme", "tag":
		return "general", rest, true
	}
	return "", "", false
}

// monbooru's categories beyond the built-ins, as far as a job or the settings
// page has seen them. A tag folds in places no mapper reaches, so they live
// here.
var (
	customCategoriesMu      sync.RWMutex
	customCategories        = map[string]bool{}
	customCategoriesLearned bool
)

func customCategory(name string) bool {
	customCategoriesMu.RLock()
	defer customCategoriesMu.RUnlock()
	return customCategories[name]
}

func isTagCategory(name string) bool {
	return name != "" && (validTagCategories[name] || customCategory(name))
}

// Must match monbooru's tags.SplitForm and tags.NormalizeName byte for byte,
// so a PTR spelling round-trips instead of minting a duplicate.
func normalizeTag(tag string) string { return foldTag(tag, false) }

func normalizeName(name string) string { return foldName(name, false) }

// keepStar is for a tag rule's key and target, where a * stands for any text.
func foldTag(tag string, keepStar bool) string {
	tag = strings.TrimSpace(tag)
	if prefix, rest, ok := strings.Cut(tag, ":"); ok {
		category := strings.ToLower(prefix)
		if slices.Contains(contract.BuiltinCategories, category) || customCategory(category) {
			name := foldName(rest, keepStar)
			if name == "" || category == "general" {
				return name
			}
			return category + ":" + name
		}
	}
	return foldName(tag, keepStar)
}

func foldName(name string, keepStar bool) string {
	name = strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(name))
	pendingFold := false
	for _, r := range name {
		switch {
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co):
		case unicode.IsSpace(r) || r == '"' || (r == '*' && !keepStar):
			pendingFold = true
		default:
			if pendingFold && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingFold = false
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "_")
}

// The pre-widening charset: tags monbooru stored before the punctuation
// widening were never rewritten and must still match their raw PTR spelling.
var (
	legacyDisallowedTagChars = regexp.MustCompile(`[^a-z0-9_()!@#$.~+:?<>=^-]+`)
	underscoreRuns           = regexp.MustCompile(`_+`)
)

// LegacyFoldTag returns the spelling an old pull stored for a raw tag: the
// old fold merged a disallowed character and its neighbouring spaces into
// one underscore run ("girls' frontline" -> "girls_frontline").
func LegacyFoldTag(tag string) string {
	tag = legacyDisallowedTagChars.ReplaceAllString(tag, "_")
	tag = underscoreRuns.ReplaceAllString(tag, "_")
	return strings.Trim(tag, "_")
}

func formatTag(category, name string) string {
	if category == "" || name == "" {
		return ""
	}
	if category == "general" {
		return name
	}
	return category + ":" + name
}

func NormalizeTag(tag string) string { return normalizeTag(tag) }

func NormalizeRuleKey(from string) string { return foldTag(from, true) }

// NormalizeRuleTarget keeps a general: prefix, which normalizeTag folds away:
// as a rule's target it moves a tag out of its category, where a bare name
// renames it in place.
func NormalizeRuleTarget(to string) string {
	to = strings.TrimSpace(to)
	if prefix, rest, ok := strings.Cut(to, ":"); ok && strings.EqualFold(prefix, "general") {
		if name := foldName(rest, true); name != "" {
			return "general:" + name
		}
		return ""
	}
	return foldTag(to, true)
}

// CheckTagRules refuses what the mapper could not apply as written.
func CheckTagRules(rules map[string]string) error {
	for from, to := range rules {
		if from == "" || NormalizeRuleKey(from) != from {
			return fmt.Errorf("tag rule %q is not a normalized tag name", from)
		}
		stars := strings.Count(from, "*")
		_, name := splitRuleKey(from)
		switch {
		case stars > 1:
			return fmt.Errorf("tag rule %q has more than one *", from)
		case name == "*":
			return fmt.Errorf("tag rule %q matches every tag: a * needs some text beside it", from)
		case strings.Count(to, "*") > stars:
			if stars == 0 {
				return fmt.Errorf("tag rule %q has a * in its replacement and none in the tag", from)
			}
			return fmt.Errorf("tag rule %q has a replacement with more than one *", from)
		}
		if to != "" && NormalizeRuleTarget(to) != to {
			return fmt.Errorf("tag rule %q has a replacement that is not a normalized tag name", from)
		}
	}
	return nil
}

func splitRuleKey(key string) (category, name string) {
	if cat, rest, found := strings.Cut(key, ":"); found && isTagCategory(cat) {
		return cat, rest
	}
	return "", key
}

// DroppedRuleTarget names the dropped category a tag rule moves its tag into.
// A bare rename stays in a category only the post knows, so it names none.
func DroppedRuleTarget(drop []string, from, to string) string {
	if strings.HasPrefix(to, "general:") && slices.Contains(drop, "general") {
		return "general"
	}
	prefix, _, found := strings.Cut(applyTagRule(map[string]string{from: to}, strings.ReplaceAll(from, "*", "x")), ":")
	if !found || !slices.Contains(drop, prefix) {
		return ""
	}
	return prefix
}

// The full form is tried before the bare name, and a general tag
// containing a colon matches by its full form. A key holding a * is only
// read when no exact key names the tag.
func applyTagRule(rules map[string]string, tag string) string {
	after, _ := matchTagRule(rules, tag)
	return after
}

// key is the rule that matched, empty when none did.
func matchTagRule(rules map[string]string, tag string) (after, key string) {
	if len(rules) == 0 {
		return tag, ""
	}
	prefix, name := "", tag
	if cat, rest, found := strings.Cut(tag, ":"); found && isTagCategory(cat) {
		prefix, name = cat, rest
	}
	repl, ok := rules[tag]
	key = tag
	if !ok && prefix != "" {
		repl, ok = rules[name]
		key = name
	}
	if !ok {
		repl, key = patternRule(rules, tag, name)
		ok = key != ""
	}
	switch {
	case !ok:
		return tag, ""
	case repl == "":
		return "", key
	}
	if cat, rest, found := strings.Cut(repl, ":"); found && isTagCategory(cat) && rest != "" {
		return normalizeTag(repl), key
	}
	if prefix != "" {
		return prefix + ":" + repl, key
	}
	return repl, key
}

// A site's rules with the list for all sites behind them: a tag a site rule
// matches never reaches that list.
type tagRules struct{ site, global map[string]string }

func (m *Mapper) rulesFor(profile Profile) tagRules {
	return tagRules{site: profile.TagRules, global: m.globalTagRules()}
}

// A rule typed into monloader.toml that a save would refuse is left out.
func (m *Mapper) globalTagRules() map[string]string {
	valid := map[string]string{}
	for from, to := range m.cfg.Current().Tags.Rules {
		if CheckTagRules(map[string]string{from: to}) == nil {
			valid[from] = to
		}
	}
	return valid
}

func (r tagRules) apply(tag string) string {
	after, _, _ := r.match(tag)
	return after
}

func (r tagRules) match(tag string) (after, key, list string) {
	if after, key = matchTagRule(r.site, tag); key != "" {
		return after, key, contract.ListSite
	}
	if after, key = matchTagRule(r.global, tag); key != "" {
		return after, key, contract.ListGlobal
	}
	return tag, "", ""
}

// The longest key wins, then the first in key order, so the answer never
// depends on the map's order.
func patternRule(rules map[string]string, tag, name string) (repl, best string) {
	for key, to := range rules {
		head, tail, found := strings.Cut(key, "*")
		if !found || (best != "" && (len(key) < len(best) || (len(key) == len(best) && key > best))) {
			continue
		}
		subject := name
		if category, _ := splitRuleKey(key); category != "" {
			subject = tag
		}
		if text, hit := starText(subject, head, tail); hit {
			best, repl = key, strings.ReplaceAll(to, "*", text)
		}
	}
	return repl, best
}

func starText(s, head, tail string) (string, bool) {
	if len(s) <= len(head)+len(tail) || !strings.HasPrefix(s, head) || !strings.HasSuffix(s, tail) {
		return "", false
	}
	return s[len(head) : len(s)-len(tail)], true
}
