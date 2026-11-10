package mapping

import (
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func TestLoginLabel(t *testing.T) {
	withKey := &config.Site{APIKey: "k"}
	withCookies := &config.Site{Cookies: "/c"}
	cases := []struct {
		auth      string
		site      *config.Site
		label     string
		needsCred bool
	}{
		{"api_optional", nil, "api (opt)", false},
		{"api_required", nil, "api key", true},
		{"api_required", withKey, "api key", false},
		{"cookies", nil, "cookies", true},
		{"cookies", withCookies, "cookies", false},
		{"none", nil, "none", false},
		{"", nil, "none", false},
	}
	for _, c := range cases {
		label, needs := LoginLabel(c.auth, c.site)
		if label != c.label || needs != c.needsCred {
			t.Errorf("LoginLabel(%q,%v) = (%q,%v), want (%q,%v)", c.auth, c.site, label, needs, c.label, c.needsCred)
		}
	}
}

func TestSiteView(t *testing.T) {
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "gelbooru", APIKey: "k"}, {Name: "e621", LookupOrder: 3}}
	m := newMapper(t, cfg)

	v := m.SiteView("gelbooru", "Cookies")
	if !v.Curated || v.Auth != AuthAPIRequired || v.Kind != KindBooru {
		t.Errorf("gelbooru = %+v, want a curated booru on its profile's auth, not the seed", v)
	}
	if v.Login != "api key" || v.NeedsCredential {
		t.Errorf("gelbooru login = (%q,%v), want the key it carries to satisfy the kind", v.Login, v.NeedsCredential)
	}
	if !v.Configured {
		t.Error("a block with an api key is user data, so the tables show the site")
	}

	if m.SiteView("e621", "").Configured {
		t.Error("a lookup-order-only block must not surface a row")
	}

	if got := m.SiteView("nhentai", "").Kind; got != KindManga {
		t.Errorf("nhentai kind = %q, want %q", got, KindManga)
	}

	v = m.SiteView("nosuchsite", "Cookies")
	if v.Curated || v.Auth != AuthCookies || v.Kind != KindOther {
		t.Errorf("unprofiled site = %+v, want the seeded cookies kind grouped as other", v)
	}
	if v.Login != "cookies" || !v.NeedsCredential {
		t.Errorf("unprofiled login = (%q,%v), want the missing cookies named", v.Login, v.NeedsCredential)
	}
}
