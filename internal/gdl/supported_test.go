package gdl

import (
	"os"
	"path/filepath"
	"testing"
)

const supportedSitesSample = `# Supported Sites

<table>
<thead valign="bottom">
<tr>
    <th>Site</th>
    <th>URL</th>
    <th>Capabilities</th>
    <th>Authentication</th>
</tr>
</thead>
<tbody valign="top">
<tr id="3dbooru" title="3dbooru">
    <td>3dbooru</td>
    <td>http://behoimi.org/</td>
    <td>Pools, Posts, Tag Searches</td>
    <td></td>
</tr>
<tr id="twitter" title="twitter">
    <td>Twitter</td>
    <td>https://x.com/</td>
    <td>Media Timelines, Tweets</td>
    <td><a href="https://github.com/mikf/gallery-dl#cookies">Cookies</a></td>
</tr>
<tr id="newgrounds" title="newgrounds">
    <td>Newgrounds</td>
    <td>https://www.newgrounds.com/</td>
    <td>Art, Audio, Movies</td>
    <td>Supported</td>
</tr>
<tr id="wallhaven" title="wallhaven">
    <td>Wallhaven</td>
    <td>https://wallhaven.cc/</td>
    <td>Collections, Search Results</td>
    <td><a href="https://wallhaven.cc/settings/account">API&nbsp;Key</a></td>
</tr>
</tbody>
</table>
`

func TestParseSupportedSites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supportedsites.md")
	if err := os.WriteFile(path, []byte(supportedSitesSample), 0o644); err != nil {
		t.Fatalf("writing sample: %v", err)
	}
	sites, err := ParseSupportedSites(path)
	if err != nil {
		t.Fatalf("ParseSupportedSites: %v", err)
	}
	if len(sites) != 4 {
		t.Errorf("parsed %d rows, want 4", len(sites))
	}
	for _, want := range []SupportedSite{
		{Category: "3dbooru", Name: "3dbooru", URL: "http://behoimi.org/", Auth: ""},
		{Category: "twitter", Name: "Twitter", URL: "https://x.com/", Auth: "Cookies"},
		{Category: "newgrounds", Name: "Newgrounds", URL: "https://www.newgrounds.com/", Auth: "Supported"},
		{Category: "wallhaven", Name: "Wallhaven", URL: "https://wallhaven.cc/", Auth: "API Key"},
	} {
		if got := sites[want.Category]; got != want {
			t.Errorf("row %s = %+v, want %+v", want.Category, got, want)
		}
	}
}

func TestParseSupportedSitesTolerant(t *testing.T) {
	if got := parseSupportedSites([]byte("not a table at all")); len(got) != 0 {
		t.Errorf("parsed %d rows from garbage, want none", len(got))
	}
	if _, err := ParseSupportedSites(filepath.Join(t.TempDir(), "absent.md")); err == nil {
		t.Error("a missing file should be reported to the caller")
	}
}
