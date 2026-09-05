package mapping

// SiteView is what monloader knows about one gallery-dl category, assembled
// from the effective profile, the supportedsites row and the site's own
// config block. The settings tables and GET /api/v1/sites both render it
// rather than each deriving it, so the two surfaces cannot answer differently
// about the same site.
type SiteView struct {
	Category string
	// Curated marks a category a profile covers, shipped or the operator's;
	// the rest fall back to the generic mapping.
	Curated bool
	// Auth is the profile's kind, or the one seeded from the supportedsites
	// row when no profile carries one. Login is its settings label and
	// NeedsCredential says the config lacks a credential the kind requires.
	Auth            string
	Login           string
	NeedsCredential bool
	Kind            string
	Hosts           []string
	CustomProfile   bool
	// Configured is the settings tables' visibility rule: user-set site data,
	// or a profile of the operator's own.
	Configured bool
}

// SiteView derives the view for a category. seedAuth is the supportedsites
// Authentication column, the one input that is not the mapper's own ("" when
// that table has no row for the category).
func (m *Mapper) SiteView(category, seedAuth string) SiteView {
	v := SiteView{Category: category, CustomProfile: m.CustomProfile(category)}
	if p, ok := m.Lookup(category); ok {
		v.Curated, v.Auth, v.Hosts, v.Kind = true, p.Auth, p.Hosts, p.Kind
		if v.Kind == "" {
			v.Kind = KindBooru
		}
	} else {
		v.Auth = SeedAuthKind(seedAuth)
		v.Kind = KindOther
	}
	site := m.cfg.Current().FindSite(category)
	v.Login, v.NeedsCredential = LoginLabel(v.Auth, site)
	v.Configured = (site != nil && site.HasUserData()) || v.CustomProfile
	return v
}
