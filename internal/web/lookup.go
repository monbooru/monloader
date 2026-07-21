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

// chainRow is one lookup-chain entry as the settings panel shows it.
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
}

// lookupPanel snapshots the effective chain and the dialog's source list.
func (s *Server) lookupPanel(csrf string) lookupPanelView {
	cfg := s.cfg.Current()
	now := time.Now()
	view := lookupPanelView{
		MinSimilarity:  cfg.Lookup.MinSimilarity,
		SaucenaoKeySet: cfg.Lookup.Saucenao.APIKey != "",
	}
	for i, src := range s.mapper.LookupChain() {
		row := chainRow{Position: i + 1, Name: src.Name, Similarity: src.Similarity, State: "ready", CSRFToken: csrf}
		if src.Similarity {
			if src.Name == "iqdb" {
				row.Note = "uses the danbooru site credentials"
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
		} else if label, needs := s.siteNeedsCredential(src.Name); needs {
			row.State, row.Warn = "needs "+label, true
		}
		view.Chain = append(view.Chain, row)
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
	return view
}

// siteNeedsCredential reports whether a site's profile requires a credential
// the config lacks, with its label - the same gate the lookup chain applies.
func (s *Server) siteNeedsCredential(site string) (string, bool) {
	p, _ := s.mapper.Lookup(site)
	return loginInfo(p.Auth, s.cfg.Current().FindSite(site))
}

// saveLookup updates the lookup section's inline settings: the similarity
// floor and (from its dialog) the saucenao api key. The key is a secret with
// the site-credential semantics: blank keeps the stored value.
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
	err := s.updateConfig(func(c *config.Config) error {
		if minSim > 0 {
			c.Lookup.MinSimilarity = minSim
		}
		if v := strings.TrimSpace(r.FormValue("api_key")); v != "" {
			c.Lookup.Saucenao.APIKey = v
		}
		return nil
	})
	if err != nil {
		s.redirectFlash(w, r, "err", "save failed: "+err.Error())
		return
	}
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
		var ce *queue.CodedError
		if !errors.As(err, &ce) {
			siteState(w, "err", "failed", err.Error(), time.Time{})
			return
		}
		switch ce.Code {
		case queue.ErrCodeMappingFailed:
			// The probe md5 matched nothing: the expected answer, and the
			// search executed, so the site was reached.
		case queue.ErrCodeRateLimited:
			siteState(w, "warn", "rate limited", ce.Msg, time.Time{})
			return
		case queue.ErrCodeAuthRequired:
			siteState(w, "warn", "auth rejected", ce.Msg, time.Time{})
			return
		case queue.ErrCodeBlocked:
			siteState(w, "err", "blocked", ce.Msg, time.Time{})
			return
		default:
			siteState(w, "err", "failed", ce.Msg, time.Time{})
			return
		}
	}
	s.siteState.Reached(site, time.Now())
	siteState(w, "ok", "ok", "", time.Time{})
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
		var ce *queue.CodedError
		if errors.As(err, &ce) {
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
			return
		}
		siteState(w, "err", "failed", err.Error(), time.Time{})
		return
	}
	msg := "ok"
	if res.DailyRemaining >= 0 {
		msg = "ok, " + strconv.Itoa(res.DailyRemaining) + " queries left today"
	}
	siteState(w, "ok", msg, "", time.Time{})
}
