package mapping

type SiteView struct {
	Category        string
	Curated         bool
	Auth            string
	Login           string
	NeedsCredential bool
	Kind            string
	Hosts           []string
	CustomProfile   bool
	Configured      bool
}

// seedAuth is the supportedsites Authentication column, "" when it has no row.
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
