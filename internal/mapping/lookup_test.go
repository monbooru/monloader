package mapping

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
)

// Every shipped md5 template must be an absolute http(s) URL carrying exactly
// one {md5} slot, so LookupURL substitution cannot mangle it.
func TestLookupTemplatesWellFormed(t *testing.T) {
	m := newMapper(t, nil)
	count := 0
	for _, cat := range m.CuratedCategories() {
		p, _ := m.Lookup(cat)
		if p.MD5Search == "" {
			continue
		}
		count++
		if strings.Count(p.MD5Search, "{md5}") != 1 {
			t.Errorf("%s: template %q must carry {md5} exactly once", cat, p.MD5Search)
		}
		u, err := url.Parse(m.LookupURL(cat, "0123456789abcdef0123456789abcdef"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			t.Errorf("%s: substituted lookup URL from %q is not an absolute http(s) URL", cat, p.MD5Search)
		}
	}
	if count != 20 {
		t.Errorf("lookupable profiles = %d, want 20", count)
	}
}

func TestLookupURL(t *testing.T) {
	m := newMapper(t, nil)
	md5 := "0123456789abcdef0123456789abcdef"
	if got, want := m.LookupURL("danbooru", md5), "https://danbooru.donmai.us/posts?tags=md5:"+md5; got != want {
		t.Errorf("danbooru = %q, want %q", got, want)
	}
	if got, want := m.LookupURL("gelbooru", md5), "https://gelbooru.com/index.php?page=post&s=list&tags=md5:"+md5; got != want {
		t.Errorf("gelbooru = %q, want %q", got, want)
	}
	// A manga site and an unknown category have no template.
	if got := m.LookupURL("nhentai", md5); got != "" {
		t.Errorf("nhentai = %q, want empty", got)
	}
	if got := m.LookupURL("nosuchsite", md5); got != "" {
		t.Errorf("unknown site = %q, want empty", got)
	}
}

func TestLookupSitesOrder(t *testing.T) {
	cfg := config.Default()
	cfg.Sites = []config.Site{
		{Name: "gelbooru", LookupOrder: 2},
		{Name: "e621"},                    // no order: out of the walk
		{Name: "yandere", LookupOrder: 2}, // ties with gelbooru, sorts by name
		{Name: "danbooru", LookupOrder: 1},
	}
	m := newMapper(t, cfg)
	want := []string{"danbooru", "gelbooru", "yandere"}
	if got := m.LookupSites(); !reflect.DeepEqual(got, want) {
		t.Errorf("LookupSites = %v, want %v", got, want)
	}
}

func TestLookupSitesDefaultOrder(t *testing.T) {
	m := newMapper(t, nil)
	want := []string{"danbooru", "gelbooru", "rule34", "e621", "yandere", "konachan"}
	if got := m.LookupSites(); !reflect.DeepEqual(got, want) {
		t.Errorf("default LookupSites = %v, want %v", got, want)
	}
}

func TestLookupChainMergesSitesAndServices(t *testing.T) {
	cfg := config.Default()
	cfg.Sites = []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "gelbooru", LookupOrder: 3},
		{Name: "e621"}, // no order: out of the chain
	}
	cfg.Lookup.Iqdb.Order = 2
	cfg.Lookup.Saucenao.Order = 3 // ties with gelbooru, sorts by name
	m := newMapper(t, cfg)
	want := []LookupSource{
		{Name: "danbooru"},
		{Name: "iqdb", Similarity: true},
		{Name: "gelbooru"},
		{Name: "saucenao", Similarity: true},
	}
	if got := m.LookupChain(); !reflect.DeepEqual(got, want) {
		t.Errorf("LookupChain = %v, want %v", got, want)
	}
	// The exact-md5 view drops the services but keeps their ranking.
	if got, wantSites := m.LookupSites(), []string{"danbooru", "gelbooru"}; !reflect.DeepEqual(got, wantSites) {
		t.Errorf("LookupSites = %v, want %v", got, wantSites)
	}

	// A cleared service leaves the chain entirely.
	cfg.Lookup.Iqdb.Order = 0
	cfg.Lookup.Saucenao.Order = 0
	want = []LookupSource{{Name: "danbooru"}, {Name: "gelbooru"}}
	if got := m.LookupChain(); !reflect.DeepEqual(got, want) {
		t.Errorf("LookupChain without services = %v, want %v", got, want)
	}
}

func TestLookupChainDefault(t *testing.T) {
	m := newMapper(t, nil)
	want := []LookupSource{
		{Name: "danbooru"},
		{Name: "iqdb", Similarity: true},
		{Name: "saucenao", Similarity: true},
		{Name: "gelbooru"}, {Name: "rule34"}, {Name: "e621"}, {Name: "yandere"}, {Name: "konachan"},
	}
	if got := m.LookupChain(); !reflect.DeepEqual(got, want) {
		t.Errorf("default LookupChain = %v, want %v", got, want)
	}
}

func TestPostURLFor(t *testing.T) {
	m := newMapper(t, nil)
	if got, want := m.PostURLFor("danbooru", "123"), "https://danbooru.donmai.us/posts/123"; got != want {
		t.Errorf("danbooru = %q, want %q", got, want)
	}
	if got := m.PostURLFor("8muses", "123"); got != "" {
		t.Errorf("a templateless site = %q, want empty", got)
	}
	if got := m.PostURLFor("danbooru", ""); got != "" {
		t.Errorf("an empty id = %q, want empty", got)
	}
}

func TestCanonicalPostURL(t *testing.T) {
	m := newMapper(t, nil)
	cases := []struct {
		in, site, want string
	}{
		{"https://danbooru.donmai.us/posts/123", "danbooru", "https://danbooru.donmai.us/posts/123"},
		// The decade-old link shapes similarity services return.
		{"https://danbooru.donmai.us/post/show/123", "danbooru", "https://danbooru.donmai.us/posts/123"},
		{"https://gelbooru.com/index.php?page=post&s=view&id=456", "gelbooru", "https://gelbooru.com/index.php?page=post&s=view&id=456"},
		{"https://yande.re/post/show/789", "yandere", "https://yande.re/post/show/789"},
		{"https://www.sakugabooru.com/post/show/1", "sakugabooru", "https://www.sakugabooru.com/post/show/1"},
		// Sites whose template puts {id} first or behind a site-specific marker.
		{"https://www.zerochan.net/3220071", "zerochan", "https://www.zerochan.net/3220071"},
		{"https://derpibooru.org/images/271338", "derpibooru", "https://derpibooru.org/images/271338"},
	}
	for _, tc := range cases {
		site, canonical, ok := m.CanonicalPostURL(tc.in)
		if !ok || site != tc.site || canonical != tc.want {
			t.Errorf("CanonicalPostURL(%q) = %q %q %v, want %q %q", tc.in, site, canonical, ok, tc.site, tc.want)
		}
	}
	for _, bad := range []string{
		"https://nowhere.example/posts/123",     // unknown host
		"https://danbooru.donmai.us/posts",      // no id anywhere
		"https://sankaku.app/posts/vkr3EelGKMZ", // non-numeric id
		"https://danbooru.donmai.us/users/999",  // a number, but not a post
		"https://danbooru.donmai.us/pools/456",
		"not a url",
	} {
		if _, _, ok := m.CanonicalPostURL(bad); ok {
			t.Errorf("CanonicalPostURL(%q) matched, want a miss", bad)
		}
	}
}

func TestNewRejectsOrderWithoutTemplate(t *testing.T) {
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "nhentai", LookupOrder: 1}}
	if _, err := New(config.NewProvider(cfg)); err == nil {
		t.Fatal("want an error for lookup_order on a site without md5 lookup support")
	}
}
