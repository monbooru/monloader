package gdl

import (
	_ "embed"
	"html"
	"os"
	"regexp"
	"strings"
)

// The site table is a build-time artifact of the pinned gallery-dl, it is
// text, and embedding it removes a file from the bundle and a path from the
// config. Refresh it with the pin.
//
//go:embed supportedsites.md
var bundledSupportedSites []byte

// SupportedSite is one row of gallery-dl's generated docs/supportedsites.md
// table, keyed by the row id, which is the gallery-dl category. Auth is the
// Authentication column as written there: "", "Supported", "Required",
// "API Key", "Cookies", or "OAuth".
type SupportedSite struct {
	Category string
	Name     string
	URL      string
	Auth     string
}

// The generated table wraps each site in <tr id="<category>"> with four cells:
// name, URL, capabilities, authentication. The capabilities cell is skipped.
var (
	siteRowRE = regexp.MustCompile(`(?s)<tr id="([^"]+)"[^>]*>\s*<td>(.*?)</td>\s*<td>(.*?)</td>\s*<td>.*?</td>\s*<td>(.*?)</td>`)
	htmlTagRE = regexp.MustCompile(`<[^>]+>`)
)

// ParseSupportedSites returns the site table by category. An empty path is
// the embedded copy, which matches the pinned gallery-dl; a path names a
// managed install's fetched copy or the operator's own, and a file that
// cannot be read is reported to the caller, who degrades (no display names,
// added sites seed no auth kind) rather than failing.
func ParseSupportedSites(path string) (map[string]SupportedSite, error) {
	if path == "" {
		return parseSupportedSites(bundledSupportedSites), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseSupportedSites(data), nil
}

func parseSupportedSites(data []byte) map[string]SupportedSite {
	out := map[string]SupportedSite{}
	for _, m := range siteRowRE.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = SupportedSite{
			Category: m[1],
			Name:     cellText(m[2]),
			URL:      cellText(m[3]),
			Auth:     cellText(m[4]),
		}
	}
	return out
}

// cellText strips a cell to its visible text: tags removed (the auth cell
// links its value), entities decoded, and the generator's non-breaking
// spaces ("API&nbsp;Key") folded to plain ones.
func cellText(cell string) string {
	s := htmlTagRE.ReplaceAllString(cell, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return strings.TrimSpace(s)
}
