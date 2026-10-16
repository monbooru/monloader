package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/similarity"
)

func (p *Processor) processLookup(ctx context.Context, job, snap *queue.Job) error {
	job.SetItems([]queue.Item{p.enrichItem(ctx, snap, "")})
	if err := p.checkLookupHash(ctx, snap); err != nil {
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	switch snap.Backend {
	case queue.BackendBooru:
		return p.lookupBooru(ctx, job, snap)
	case queue.BackendPTR:
		return p.lookupPTR(ctx, job, snap)
	case queue.BackendAll:
		return p.lookupAll(ctx, job, snap)
	default:
		err := &outcome.CodedError{Code: outcome.ErrCodePTRUnavailable, Msg: "unknown lookup backend"}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
}

// monbooru matches this name to render the PTR's line apart from the
// online walk.
const ptrTrailName = contract.TrailPTR

// A PTR enrich is unverified, so only the caller's image/hash pair ties the
// tags to the image, and a replace can leave that hash stale. An error or a
// blank digest from monbooru trusts the pair.
func (p *Processor) checkLookupHash(ctx context.Context, snap *queue.Job) error {
	if snap.SHA256 == "" || snap.ImageID == 0 {
		return nil
	}
	sha, err := p.client.ImageSHA256(ctx, snap.ImageID, snap.Gallery)
	if err != nil || sha == "" || strings.EqualFold(sha, snap.SHA256) {
		return nil
	}
	return &outcome.CodedError{
		Code: outcome.ErrCodeHashMismatch,
		Msg:  fmt.Sprintf("the sha256 is not image %d's; nothing was applied", snap.ImageID),
	}
}

func (p *Processor) lookupAll(ctx context.Context, job, snap *queue.Job) error {
	var trail []string
	ptrTags, ptrHit, ptrMiss := p.ptrTagsFor(snap.SHA256)
	if ptrMiss != "" {
		trail = append(trail, ptrTrailName+": "+ptrMiss)
	}
	meta, hit, chainTrail, err := p.lookupChain(ctx, snap)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A chain that cannot start is one more trail entry, not an error
		// eclipsing the PTR's answer.
		var ce *outcome.CodedError
		if errors.As(err, &ce) {
			chainTrail = []string{ce.Msg}
		} else {
			chainTrail = []string{err.Error()}
		}
	}
	trail = append(trail, chainTrail...)
	if !ptrHit && meta == nil && hit == nil {
		p.failEnrichFetch(ctx, job, snap, missError(trail))
		return nil
	}

	// Held, not finalized: the online enrich drives the one item, and
	// both outcomes fold into its note.
	ptrApplied := false
	var ptrNote string
	var warnings []string
	if ptrHit {
		res, enrichErr := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, monbooru.EnrichPayload{
			PostFields: monbooru.PostFields{Tags: ptrTags, Source: contract.SourcePTR},
			Verify:     false,
		})
		if enrichErr != nil {
			if meta == nil && hit == nil {
				p.failEnrichFetch(ctx, job, snap, enrichErr)
				return nil
			}
			logx.Warnf("queue: job %d ptr enrich failed: %v", snap.ID, enrichErr)
		} else {
			ptrApplied = true
			ptrNote = res.MergeNote
			warnings = res.TagWarnings
		}
	}

	if meta == nil && hit == nil {
		// Only the PTR matched: its tags landed but no source URL did, so
		// the trail still reports the misses.
		p.markEnriched(job, snap, &monbooru.Result{MonbooruID: snap.ImageID, MergeNote: ptrNote, TagWarnings: warnings}, "")
		p.reportPTRTrail(ctx, snap, chainTrail)
		return nil
	}

	res, url, site, onlineErr := p.enrichAllOnline(ctx, job, snap, meta, hit)
	switch {
	case onlineErr == nil:
		note := composeLookupNote(ptrApplied, ptrNote, ptrMiss, site, res.MergeNote)
		p.markEnriched(job, snap, &monbooru.Result{
			MonbooruID: snap.ImageID, MergeNote: note,
			TagWarnings: append(warnings, res.TagWarnings...),
		}, url)
	case ptrApplied:
		// The PTR tags did land, so a PTR-led trail replaces the "no tags
		// applied" monbooru recorded for the online failure.
		p.markEnriched(job, snap, &monbooru.Result{MonbooruID: snap.ImageID, MergeNote: ptrNote, TagWarnings: warnings}, "")
		p.reportPTRTrail(ctx, snap, append(chainTrail, site+": "+errorCode(onlineErr)))
	default:
		failItem(job, 0, errorCode(onlineErr), onlineErr.Error())
	}
	return nil
}

// An off backend reports no miss, since the job never promised one; a syncing
// one is skipped, since a partial tag set reads like the whole truth.
func (p *Processor) ptrTagsFor(sha256 string) (tags []string, hit bool, miss string) {
	if p.ptr == nil || !p.ptr.Enabled() || sha256 == "" {
		return nil, false, ""
	}
	if !p.ptr.CaughtUp() {
		return nil, false, "skipped, still syncing"
	}
	rawTags, ok, err := p.ptr.TagsForHash(sha256)
	switch {
	case err != nil:
		return nil, false, err.Error()
	case !ok:
		return nil, false, "no match"
	}
	return mapping.MapPTRTags(rawTags), true, ""
}

// The caller settles the item, folding in the PTR outcome.
func (p *Processor) enrichAllOnline(ctx context.Context, job, snap *queue.Job, meta map[string]any, hit *simHit) (*monbooru.Result, string, string, error) {
	if meta != nil {
		var sim float64
		if hit != nil {
			job.UpdateItem(0, func(it *queue.Item) { it.PostID = hit.note })
			sim = hit.score
		}
		site := kwdict.String(meta, "category")
		job.SetSite(site)
		labelEnrichPost(job, meta)
		url := p.mapper.PostURL(meta)
		res, err := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery,
			p.mapEnrichPayload(meta, url, claimedMD5(meta, snap.MD5, sim), sim))
		return res, url, site, err
	}
	res, err := p.sourceOnlyEnrich(ctx, job, snap, hit)
	return res, hit.url, hit.site, err
}

// The caller settles the item.
func (p *Processor) sourceOnlyEnrich(ctx context.Context, job, snap *queue.Job, hit *simHit) (*monbooru.Result, error) {
	job.SetSite(hit.site)
	job.UpdateItem(0, func(it *queue.Item) { it.PostID = hit.note + ", source only" })
	return p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, monbooru.EnrichPayload{
		PostFields: monbooru.PostFields{Source: p.mapper.SourceLabel(hit.site), URL: hit.url},
		Verify:     false,
		Similarity: hit.score,
	})
}

// A skipped PTR and one that held nothing must not read alike: monbooru's
// retry ladder advances on both.
func composeLookupNote(ptrApplied bool, ptrNote, ptrMiss, site, onlineNote string) string {
	var parts []string
	switch {
	case ptrApplied:
		parts = append(parts, "ptr "+cmp.Or(ptrNote, "no new tags"))
	case ptrMiss != "":
		parts = append(parts, "ptr "+ptrMiss)
	}
	parts = append(parts, site+" "+cmp.Or(onlineNote, "no new tags"))
	return strings.Join(parts, "; ")
}

func (p *Processor) reportPTRTrail(ctx context.Context, snap *queue.Job, onlineTrail []string) {
	report := append([]string{ptrTrailName + ": match, tags applied"}, onlineTrail...)
	if rerr := p.client.ReportFetchOutcome(ctx, snap.ImageID, snap.Gallery, string(snap.Kind), snap.Backend, outcome.ErrCodeHashNotFound, strings.Join(report, "; ")); rerr != nil {
		logx.Warnf("queue: job %d fetch-status report failed: %v", snap.ID, rerr)
	}
}

func (p *Processor) lookupBooru(ctx context.Context, job, snap *queue.Job) error {
	meta, hit, trail, err := p.lookupChain(ctx, snap)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	if meta == nil && hit == nil {
		p.failEnrichFetch(ctx, job, snap, missError(trail))
		return nil
	}
	res, url, _, err := p.enrichAllOnline(ctx, job, snap, meta, hit)
	if err != nil {
		if ctx.Err() == nil {
			failItem(job, 0, errorCode(err), err.Error())
		}
		return nil
	}
	p.markEnriched(job, snap, res, url)
	return nil
}

// A similarity hit must not claim the query hash, as its file differs from
// the local one; an exact hit may, having been found by it.
func claimedMD5(meta map[string]any, queryMD5 string, sim float64) string {
	if md5 := kwdict.String(meta, "md5"); md5 != "" {
		return md5
	}
	if sim == 0 {
		return queryMD5
	}
	return ""
}

// monbooru splits the "; "-joined entries back into a list, so each must
// stay "source: reason".
func missError(trail []string) error {
	return &outcome.CodedError{Code: outcome.ErrCodeHashNotFound, Msg: strings.Join(trail, "; ")}
}

func (p *Processor) lookupPTR(ctx context.Context, job, snap *queue.Job) error {
	if p.ptr == nil || !p.ptr.Enabled() {
		err := &outcome.CodedError{Code: outcome.ErrCodePTRUnavailable, Msg: "the ptr lookup backend is not available"}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	if !p.ptr.CaughtUp() {
		err := &outcome.CodedError{Code: outcome.ErrCodePTRSyncing, Msg: "the ptr index is not fully synced yet"}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	rawTags, ok, err := p.ptr.TagsForHash(snap.SHA256)
	if err != nil {
		p.failEnrichFetch(ctx, job, snap, &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: err.Error()})
		return nil
	}
	if !ok {
		p.failEnrichFetch(ctx, job, snap, missError([]string{ptrTrailName + ": no match"}))
		return nil
	}
	tags := mapping.MapPTRTags(rawTags)
	res, err := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, monbooru.EnrichPayload{
		PostFields: monbooru.PostFields{Tags: tags, Source: contract.SourcePTR},
		Verify:     false,
	})
	if err != nil {
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	p.markEnriched(job, snap, res, "")
	return nil
}

func (p *Processor) processHashImport(ctx context.Context, job, snap *queue.Job) error {
	meta, trail, err := p.lookupWalk(ctx, snap.MD5)
	if err == nil && meta == nil {
		err = missError(trail)
	}
	if err != nil {
		return abortJob(ctx, job, err)
	}
	postURL := p.mapper.PostURL(meta)
	if postURL == "" {
		cerr := &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: "the matched post has no canonical URL"}
		job.Fail(cerr.Code, cerr.Msg, time.Now())
		return cerr
	}
	job.SetURL(postURL)
	snap.URL = postURL
	return p.processDownload(ctx, job, snap)
}

type simHit struct {
	site, url, note string
	score           float64
}

// A source error goes on the trail, not up: one source being down must
// not kill the lookup.
func (p *Processor) lookupChain(ctx context.Context, snap *queue.Job) (map[string]any, *simHit, []string, error) {
	chain := p.mapper.LookupChain()
	if len(chain) == 0 {
		return nil, nil, nil, &outcome.CodedError{Code: outcome.ErrCodeHashNotFound, Msg: "no lookup source has an order set in settings"}
	}
	var trail []string
	var fallback *simHit
	// Fetched lazily, so a chain that resolves exactly uploads nothing
	// anywhere.
	var thumb []byte
	thumbTried := false
	for _, src := range chain {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		var meta map[string]any
		if src.Similarity {
			if p.sim == nil {
				continue
			}
			// Before the thumbnail fetch, so unconfigured services never
			// cause one.
			if label, missing := p.sim.Missing(src.Name); missing {
				trail = append(trail, src.Name+": skipped, needs "+label)
				continue
			}
			if !thumbTried {
				thumbTried = true
				var terr error
				if thumb, terr = p.client.FetchThumbnail(ctx, snap.ImageID, snap.Gallery); terr != nil {
					if ctx.Err() != nil {
						return nil, nil, nil, ctx.Err()
					}
					trail = append(trail, "similarity: no thumbnail available")
					logx.Warnf("queue: job %d thumbnail fetch failed: %v", snap.ID, terr)
				}
			}
			if thumb == nil {
				continue // already on the trail once; exact sources still run
			}
			var hit *simHit
			meta, hit = p.similaritySource(ctx, src.Name, thumb, &trail)
			if meta != nil {
				return meta, hit, trail, nil
			}
			if fallback == nil {
				fallback = hit
			}
		} else {
			meta = p.walkSite(ctx, src.Name, snap.MD5, &trail)
		}
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		if meta != nil {
			return meta, nil, trail, nil
		}
	}
	return nil, fallback, trail, nil
}

// lookupWalk skips the similarity sources: a pasted hash has no image to
// query with.
func (p *Processor) lookupWalk(ctx context.Context, md5 string) (map[string]any, []string, error) {
	sites := p.mapper.LookupSites()
	if len(sites) == 0 {
		return nil, nil, &outcome.CodedError{Code: outcome.ErrCodeHashNotFound, Msg: "no site has a lookup order set in settings"}
	}
	var trail []string
	for _, site := range sites {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if meta := p.walkSite(ctx, site, md5, &trail); meta != nil {
			return meta, trail, nil
		}
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return nil, trail, nil
}

func (p *Processor) walkSite(ctx context.Context, site, md5 string, trail *[]string) map[string]any {
	if label, missing := p.missingCredential(site); missing {
		*trail = append(*trail, site+": skipped, needs "+label)
		return nil
	}
	meta, err := p.runner.FetchMeta(ctx, p.mapper.LookupURL(site, md5))
	if err == nil {
		return meta
	}
	if ctx.Err() != nil {
		return nil
	}
	if code := errorCode(err); code == outcome.ErrCodeMappingFailed {
		// FetchMeta reports an empty resolve as mapping_failed, which
		// here means no match.
		*trail = append(*trail, site+": no match")
	} else {
		*trail = append(*trail, site+": "+code)
		logx.Warnf("queue: hash lookup on %s failed: %v", site, err)
	}
	return nil
}

// Ranked by lookup order, which says whose tags are wanted, then by score;
// at most three fetches, so bad links cannot stall the chain.
func (p *Processor) similaritySource(ctx context.Context, service string, thumb []byte, trail *[]string) (map[string]any, *simHit) {
	res, err := p.sim.Search(ctx, service, thumb)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		*trail = append(*trail, service+": "+errorCode(err))
		logx.Warnf("queue: %s similarity lookup failed: %v", service, err)
		return nil, nil
	}
	minSim := float64(p.cfg.Current().Lookup.MinSimilarity)
	seen := map[string]bool{}
	var cands, missed []similarity.Candidate
	for _, c := range res.Candidates {
		if c.URL == "" || seen[c.URL] {
			continue
		}
		seen[c.URL] = true
		if c.Similarity >= minSim {
			cands = append(cands, c)
		} else {
			missed = append(missed, c)
		}
	}
	if len(cands) == 0 {
		var extras []similarity.Candidate
		for _, c := range res.Extras {
			if c.URL != "" && !seen[c.URL] {
				seen[c.URL] = true
				extras = append(extras, c)
			}
		}
		*trail = append(*trail, service+": no match"+closestNote(append(missed, extras...)))
		// A confident extra is still the likely source, so it comes back
		// for a source-only record.
		slices.SortStableFunc(extras, func(a, b similarity.Candidate) int { return cmp.Compare(b.Similarity, a.Similarity) })
		if len(extras) > 0 && extras[0].Similarity >= minSim {
			return nil, candHit(extras[0])
		}
		return nil, nil
	}
	rank := map[string]int{}
	for i, site := range p.mapper.LookupSites() {
		rank[site] = i + 1
	}
	rankOf := func(site string) int { return cmp.Or(rank[site], len(rank)+1) }
	slices.SortStableFunc(cands, func(a, b similarity.Candidate) int {
		return cmp.Or(
			cmp.Compare(rankOf(a.Site), rankOf(b.Site)),
			cmp.Compare(b.Similarity, a.Similarity))
	})
	attempts := 0
	for _, cand := range cands {
		if attempts == 3 || ctx.Err() != nil {
			break
		}
		if label, missing := p.missingCredential(cand.Site); missing {
			*trail = append(*trail, cand.Site+": skipped, needs "+label)
			continue
		}
		attempts++
		meta, err := p.runner.FetchMeta(ctx, cand.URL)
		if err == nil {
			return meta, candHit(cand)
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		*trail = append(*trail, cand.Site+": "+errorCode(err))
		logx.Warnf("queue: %s candidate %s failed: %v", service, cand.Site, err)
	}
	if ctx.Err() != nil {
		return nil, nil
	}
	return nil, candHit(cands[0])
}

func closestNote(missed []similarity.Candidate) string {
	if len(missed) == 0 {
		return ""
	}
	slices.SortStableFunc(missed, func(a, b similarity.Candidate) int { return cmp.Compare(b.Similarity, a.Similarity) })
	if len(missed) > 3 {
		missed = missed[:3]
	}
	parts := make([]string, 0, len(missed))
	for _, c := range missed {
		parts = append(parts, fmt.Sprintf("%s (%.0f%%)", c.URL, c.Similarity))
	}
	return ", closest candidates: " + strings.Join(parts, ", ")
}

func candHit(cand similarity.Candidate) *simHit {
	return &simHit{
		site:  cand.Site,
		url:   cand.URL,
		note:  fmt.Sprintf("%s %.0f%%", cand.Site, cand.Similarity),
		score: cand.Similarity,
	}
}

func (p *Processor) missingCredential(site string) (string, bool) {
	prof, ok := p.mapper.Lookup(site)
	if !ok {
		return "", false
	}
	return mapping.RequiredCredential(prof.Auth, p.cfg.Current().FindSite(site))
}
