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
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/similarity"
)

type simService interface {
	Missing(service string) (label string, missing bool)
	Search(ctx context.Context, service string, image []byte) (similarity.Result, error)
	LimitedUntil(service string) time.Time
}

// Position 0 marks a site whose order the walk skips (no md5 template).
type chainRow struct {
	Position   int
	Name       string
	Similarity bool
	Note       string
	State      string
	Warn       bool
	CSRFToken  string
	Site       *siteRow
}

type lookupPanelView struct {
	Chain           []chainRow
	DialogOn        []mapping.LookupSource
	DialogOff       []mapping.LookupSource
	MinSimilarity   int
	SaucenaoKeySet  bool
	ScheduledBudget int
	ScheduledLeft   int
	ScheduledResets int64
	SaucenaoLimited int64
}

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
			label, missing := s.sim.Missing(src.Name)
			if site := similarity.CredentialSite(src.Name); site != "" {
				row.Note = "uses the " + site + " site credentials"
				edit := s.siteRows([]string{site}, csrf)[0]
				row.Site = &edit
			} else if missing {
				row.Note = "api key: not set"
			} else {
				row.Note = "api key: set"
			}
			if missing {
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
	// The walk skips an order on a site whose profile lost its md5
	// template; list it rather than hide it.
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
	for _, name := range config.LookupServices {
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

func (s *Server) siteNeedsCredential(site string) (string, bool) {
	v := s.siteView(site)
	return v.Login, v.NeedsCredential
}

// The section's row and the saucenao key dialog both post here, so a
// field applies only when sent; a blank key keeps the stored one.
func (s *Server) saveLookup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectFlash(w, r, "err", "bad form")
		return
	}
	minSim := 0
	if r.Form.Has("min_similarity") {
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("min_similarity")))
		if err != nil || n < 1 || n > 100 {
			s.refuseSave(w, r, "min similarity must be a percentage from 1 to 100")
			return
		}
		minSim = n
	}
	scheduled := r.Form.Has("scheduled_daily_budget")
	budget := 0
	if scheduled {
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("scheduled_daily_budget")))
		if err != nil || n < 0 {
			s.refuseSave(w, r, "the daily budget must be zero or more images")
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
		if order > 0 && s.cfg.Current().Lookup.Service(name) == nil && s.mapper.LookupURL(name, "") == "" {
			s.redirectFlash(w, r, "err", "site "+name+" does not support md5 lookup")
			return
		}
		orders[name] = order
	}
	err := s.updateConfig(func(c *config.Config) error {
		for name, order := range orders {
			if svc := c.Lookup.Service(name); svc != nil {
				svc.Order = order
				continue
			}
			site := c.FindSite(name)
			if site == nil {
				if order == 0 {
					continue
				}
				c.Sites = append(c.Sites, config.Site{Name: name})
				site = &c.Sites[len(c.Sites)-1]
			}
			site.LookupOrder = order
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "ok", "lookup chain saved")
}

func (s *Server) testLookupSource(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if s.cfg.Current().Lookup.Service(source) != nil {
		s.testSimilarityService(w, r, source)
		return
	}
	s.testLookupSite(w, r, source)
}

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
		// A miss is expected: the search ran, so the site was reached.
		var ce *outcome.CodedError
		if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeMappingFailed {
			probeFailure(w, err)
			return
		}
	}
	s.siteState.Reached(site, time.Now())
	siteState(w, "ok", "ok", "", time.Time{})
}

func probeFailure(w http.ResponseWriter, err error) {
	var ce *outcome.CodedError
	if !errors.As(err, &ce) {
		siteState(w, "err", "failed", err.Error(), time.Time{})
		return
	}
	switch ce.Code {
	case outcome.ErrCodeRateLimited:
		siteState(w, "warn", "rate limited", ce.Msg, time.Time{})
	case outcome.ErrCodeAuthRequired:
		siteState(w, "warn", "auth rejected", ce.Msg, time.Time{})
	case outcome.ErrCodeBlocked:
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
