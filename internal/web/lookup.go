package web

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/similarity"
)

// simService is the similarity surface the web layer drives: credential state
// for the lookup panel, the live query behind its test button, and the active
// rate-limit cooldown. *similarity.Client satisfies it; tests inject a stub.
type simService interface {
	Missing(service string) (label string, missing bool)
	Search(ctx context.Context, service string, image []byte) (similarity.Result, error)
	LimitedUntil(service string) time.Time
}

// chainRow is one lookup-chain entry as the settings panel shows it. A site
// row carries its sites-table row too, so the edit-site shortcut opens the
// shared dialog with the site's data; Position 0 marks a source holding an
// order the walk skips (no md5 template).
type chainRow struct {
	Position   int
	Name       string
	Similarity bool
	// Note is the muted sub-label (where a service's credential lives); State
	// is the readiness word, red-flagged by Warn.
	Note      string
	State     string
	Warn      bool
	CSRFToken string
	Site      *siteRow
}

// lookupPanelView backs the settings lookup section. DialogOn and DialogOff
// are the edit-order dialog's two drag columns: the chain in its order, and
// every other chain-capable source.
type lookupPanelView struct {
	Chain          []chainRow
	DialogOn       []mapping.LookupSource
	DialogOff      []mapping.LookupSource
	MinSimilarity  int
	SaucenaoKeySet bool
	// The scheduled-lookup budget, what is left of it today, and the
	// saucenao cooldown the similarity client already tracks. The two
	// stamps are unix seconds for humanDue, zero when nothing is pending.
	ScheduledBudget int
	ScheduledLeft   int
	ScheduledResets int64
	SaucenaoLimited int64
}

// lookupPanel snapshots the effective chain and the dialog's source list.
func (s *Server) lookupPanel(csrf string) lookupPanelView {
	cfg := s.cfg.Current()
	now := time.Now()
	budget, left, resets := s.queue.LookupBudget()
	view := lookupPanelView{
		MinSimilarity:   cfg.Lookup.MinSimilarity,
		SaucenaoKeySet:  cfg.Lookup.Saucenao.APIKey != "",
		ScheduledBudget: budget,
		ScheduledLeft:   left,
		ScheduledResets: resets.Unix(),
	}
	if until := s.sim.LimitedUntil("saucenao"); now.Before(until) {
		view.SaucenaoLimited = until.Unix()
	}
	for i, src := range s.mapper.LookupChain() {
		row := chainRow{Position: i + 1, Name: src.Name, Similarity: src.Similarity, State: "ready", CSRFToken: csrf}
		if src.Similarity {
			if src.Name == "iqdb" {
				row.Note = "uses the danbooru site credentials"
				// iqdb has no credentials of its own, so its edit shortcut
				// opens the danbooru site it authenticates through.
				edit := s.siteRows([]string{"danbooru"}, csrf)[0]
				row.Site = &edit
			} else if view.SaucenaoKeySet {
				row.Note = "api key: set"
			} else {
				row.Note = "api key: not set"
			}
			if label, missing := s.sim.Missing(src.Name); missing {
				row.State, row.Warn = "needs "+label, true
			} else if now.Before(s.sim.LimitedUntil(src.Name)) {
				row.State, row.Warn = "rate limited", true
			}
		} else {
			if label, needs := s.siteNeedsCredential(src.Name); needs {
				row.State, row.Warn = "needs "+label, true
			}
			edit := s.siteRows([]string{src.Name}, csrf)[0]
			row.Site = &edit
		}
		view.Chain = append(view.Chain, row)
	}
	// An order on a site whose effective profile carries no md5 template is
	// skipped by the walk (a profile edit can take the template away);
	// surface the skip instead of silently hiding the site.
	for _, site := range cfg.Sites {
		if site.LookupOrder > 0 && s.mapper.LookupURL(site.Name, "") == "" {
			edit := s.siteRows([]string{site.Name}, csrf)[0]
			view.Chain = append(view.Chain, chainRow{
				Name: site.Name, Note: "the profile carries no md5 search template",
				State: "no md5 template", Warn: true, CSRFToken: csrf, Site: &edit,
			})
		}
	}
	view.DialogOn = s.mapper.LookupChain()
	inChain := map[string]bool{}
	for _, src := range view.DialogOn {
		inChain[src.Name] = true
	}
	for _, name := range []string{"iqdb", "saucenao"} {
		if !inChain[name] {
			view.DialogOff = append(view.DialogOff, mapping.LookupSource{Name: name, Similarity: true})
		}
	}
	for _, cat := range s.mapper.CuratedCategories() {
		if !inChain[cat] && s.mapper.LookupURL(cat, "") != "" {
			view.DialogOff = append(view.DialogOff, mapping.LookupSource{Name: cat})
		}
	}
	// A skipped site sits in the off column so a chain save clears its order.
	for _, site := range cfg.Sites {
		if site.LookupOrder > 0 && s.mapper.LookupURL(site.Name, "") == "" {
			view.DialogOff = append(view.DialogOff, mapping.LookupSource{Name: site.Name})
		}
	}
	return view
}

// siteNeedsCredential reports whether a site's profile requires a credential
// the config lacks, with its label - the same gate the lookup chain applies.
func (s *Server) siteNeedsCredential(site string) (string, bool) {
	p, _ := s.mapper.Lookup(site)
	return loginInfo(p.Auth, s.cfg.Current().FindSite(site))
}

// saveLookup updates the lookup section's inline settings: the similarity
// floor and the scheduled-lookup budget from the section's own row, and the
// saucenao api key from its dialog. Each sub-form is recognised by a field
// only it posts. The key is a secret with the site-credential semantics:
// blank keeps the stored value.
func (s *Server) saveLookup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	minSim := 0
	if r.Form.Has("min_similarity") {
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("min_similarity")))
		if err != nil || n < 1 || n > 100 {
			s.redirectFlash(w, r, "err", "min similarity must be a percentage from 1 to 100")
			return
		}
		minSim = n
	}
	scheduled := r.Form.Has("scheduled_daily_budget")
	budget := 0
	if scheduled {
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("scheduled_daily_budget")))
		if err != nil || n < 0 {
			s.redirectFlash(w, r, "err", "the daily budget must be zero or more images")
			return
		}
		budget = n
	}
	err := s.updateConfig(func(c *config.Config) error {
		if minSim > 0 {
			c.Lookup.MinSimilarity = minSim
		}
		if v := strings.TrimSpace(r.FormValue("api_key")); v != "" {
			c.Lookup.Saucenao.APIKey = v
		}
		if scheduled {
			c.Lookup.ScheduledDailyBudget = budget
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.queue.SetLookupBudget(s.cfg.Current().Lookup.ScheduledDailyBudget)
	s.redirectFlash(w, r, "ok", "lookup settings saved")
}

// saveLookupChain writes the edit-order dialog: one nullable position per
// source, blank leaving it out of the chain. Both number spaces - the sites'
// lookup_order and the services' order - are saved in one pass.
func (s *Server) saveLookupChain(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	orders := map[string]int{}
	for field := range r.Form {
		name, ok := strings.CutPrefix(field, "order-")
		if !ok {
			continue
		}
		order, err := parseLookupOrder(r.FormValue(field))
		if err != nil {
			s.redirectFlash(w, r, "err", name+": "+err.Error())
			return
		}
		if order > 0 && name != "iqdb" && name != "saucenao" && s.mapper.LookupURL(name, "") == "" {
			s.redirectFlash(w, r, "err", "site "+name+" does not support md5 lookup")
			return
		}
		orders[name] = order
	}
	err := s.updateConfig(func(c *config.Config) error {
		for name, order := range orders {
			switch name {
			case "iqdb":
				c.Lookup.Iqdb.Order = order
			case "saucenao":
				c.Lookup.Saucenao.Order = order
			default:
				site := c.FindSite(name)
				if site == nil {
					if order == 0 {
						continue // no block and no order: nothing to record
					}
					c.Sites = append(c.Sites, config.Site{Name: name})
					site = &c.Sites[len(c.Sites)-1]
				}
				site.LookupOrder = order
			}
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "lookup chain saved")
}

// testLookupSource probes one lookup source live and renders the outcome into
// the source's own state cell. A similarity service runs one real query with
// the built-in probe image; an exact-md5 site runs its search with the
// probe's md5. A clean "no match" is a successful round trip either way - the
// probe is a generated gradient no booru holds - so what this reports is the
// auth path and, for saucenao, the daily quota.
func (s *Server) testLookupSource(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	switch source {
	case "iqdb", "saucenao":
		s.testSimilarityService(w, r, source)
	default:
		s.testLookupSite(w, r, source)
	}
}

// testLookupSite runs one exact-md5 site search with the probe image's md5.
// The search matching nothing is the expected success; what can fail is the
// route, the auth, or a rate limit.
func (s *Server) testLookupSite(w http.ResponseWriter, r *http.Request, site string) {
	sum := md5.Sum(similarity.ProbeImage())
	searchURL := s.mapper.LookupURL(site, hex.EncodeToString(sum[:]))
	if searchURL == "" {
		http.NotFound(w, r)
		return
	}
	if label, needs := s.siteNeedsCredential(site); needs {
		siteState(w, "warn", "needs "+label, "", time.Time{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, err := s.runner.FetchMeta(ctx, searchURL)
	if err != nil {
		// The probe md5 matching nothing is the expected answer: the search
		// executed, so the site was reached. Every other error is a failure.
		var ce *queue.CodedError
		if !errors.As(err, &ce) || ce.Code != queue.ErrCodeMappingFailed {
			probeFailure(w, err)
			return
		}
	}
	s.siteState.Reached(site, time.Now())
	siteState(w, "ok", "ok", "", time.Time{})
}

// probeFailure renders a live probe's error into the site's state cell. An
// unclassified error falls back to its message.
func probeFailure(w http.ResponseWriter, err error) {
	var ce *queue.CodedError
	if !errors.As(err, &ce) {
		siteState(w, "err", "failed", err.Error(), time.Time{})
		return
	}
	switch ce.Code {
	case queue.ErrCodeRateLimited:
		siteState(w, "warn", "rate limited", ce.Msg, time.Time{})
	case queue.ErrCodeAuthRequired:
		siteState(w, "warn", "auth rejected", ce.Msg, time.Time{})
	case queue.ErrCodeBlocked:
		siteState(w, "err", "blocked", ce.Msg, time.Time{})
	default:
		siteState(w, "err", "failed", ce.Msg, time.Time{})
	}
}

func (s *Server) testSimilarityService(w http.ResponseWriter, r *http.Request, service string) {
	if label, missing := s.sim.Missing(service); missing {
		siteState(w, "warn", "needs "+label, "", time.Time{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.sim.Search(ctx, service, similarity.ProbeImage())
	if err != nil {
		probeFailure(w, err)
		return
	}
	msg := "ok"
	if res.DailyRemaining >= 0 {
		msg = "ok, " + strconv.Itoa(res.DailyRemaining) + " queries left today"
	}
	siteState(w, "ok", msg, "", time.Time{})
}
