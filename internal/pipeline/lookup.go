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
	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/similarity"
)

// processLookup handles a hash lookup: find the tags for the hash and fold them
// into the monbooru image the job targets. The job carries one item so the
// queue shows a row, like a metadata refetch.
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
		err := &queue.CodedError{Code: queue.ErrCodePTRUnavailable, Msg: "unknown lookup backend"}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
}

// ptrTrailName is how the trail entries name the local PTR backend; monbooru
// splits on it to render the PTR's line apart from the online walk.
const ptrTrailName = "Public Tag Repository"

// checkLookupHash refuses a sha256 that is not the target image's. A PTR
// enrich is unverified by construction - there is no source page or file to
// compare - so the pair the caller names is the only thing tying those tags to
// that image, and a replace rewrites an image's bytes and digest, which makes
// a cached hash a reachable state rather than a hypothetical. Only a digest
// monbooru actually reported can contradict the caller: an error or a blank
// answer leaves the pair trusted, since the enrich that follows would fail on
// its own.
func (p *Processor) checkLookupHash(ctx context.Context, snap *queue.Job) error {
	if snap.SHA256 == "" || snap.ImageID == 0 {
		return nil
	}
	sha, err := p.client.ImageSHA256(ctx, snap.ImageID, snap.Gallery)
	if err != nil || sha == "" || strings.EqualFold(sha, snap.SHA256) {
		return nil
	}
	return &queue.CodedError{
		Code: queue.ErrCodeHashMismatch,
		Msg:  fmt.Sprintf("the sha256 is not image %d's; nothing was applied", snap.ImageID),
	}
}

// lookupAll runs every available lookup backend for one image: the local PTR
// index when enabled, then the booru chain. Both apply to one job item: the
// PTR contributes tags, a booru match also records the post as a source, and
// the item's terminal note names what each backend folded in. A full miss
// reports one hash_not_found trail covering everything searched; a PTR-only
// hit reports that trail too, led by the PTR match, since it leaves the image
// without a source URL.
func (p *Processor) lookupAll(ctx context.Context, job, snap *queue.Job) error {
	var trail []string
	ptrTags, ptrHit, ptrMiss := p.ptrTagsFor(snap.SHA256)
	if ptrMiss != "" {
		trail = append(trail, ptrMiss)
	}
	meta, hit, chainTrail, err := p.lookupChain(ctx, snap)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A chain that can't start (no lookup order configured) reads as one
		// more trail entry rather than eclipsing the PTR's answer.
		var ce *queue.CodedError
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

	// The PTR has no source URL, so enrich it first but hold the result rather
	// than finalizing the item: the online enrich drives the single item, so
	// both outcomes fold into one note instead of the second write clobbering
	// the first.
	ptrApplied := false
	var ptrNote string
	if ptrHit {
		res, enrichErr := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, monbooru.EnrichPayload{
			Tags:   ptrTags,
			Source: "ptr",
			Verify: false,
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
		}
	}

	if meta == nil && hit == nil {
		// Only the PTR matched: its tags landed but no source URL did, so the
		// item is enriched and the online trail still reports what missed.
		p.markEnriched(job, snap, &monbooru.Result{MonbooruID: snap.ImageID, MergeNote: ptrNote}, "")
		p.reportPTRTrail(ctx, snap, chainTrail)
		return nil
	}

	res, url, site, onlineErr := p.enrichAllOnline(ctx, job, snap, meta, hit)
	switch {
	case onlineErr == nil:
		note := composeLookupNote(ptrApplied, ptrNote, site, res.MergeNote)
		p.markEnriched(job, snap, &monbooru.Result{MonbooruID: snap.ImageID, MergeNote: note}, url)
	case ptrApplied:
		// The PTR tags landed but the online post could not be enriched (a
		// changed file, a rejection). Keep the item enriched and report a
		// PTR-led trail so monbooru's pill is truthful rather than the "no
		// tags applied" its enrich handler recorded for the online failure.
		p.markEnriched(job, snap, &monbooru.Result{MonbooruID: snap.ImageID, MergeNote: ptrNote}, "")
		p.reportPTRTrail(ctx, snap, append(chainTrail, site+": "+errorCode(onlineErr)))
	default:
		failItem(job, 0, errorCode(onlineErr), onlineErr.Error())
	}
	return nil
}

// ptrTagsFor asks the local index for a sha256's tags on a backend=all job,
// returning the mapped tags and the trail entry naming why nothing came back.
// A backend that is off answers neither, since the job never promised one; one
// still syncing says so, because a partial index answers a partial tag set
// that reads like the whole truth.
func (p *Processor) ptrTagsFor(sha256 string) (tags []string, hit bool, miss string) {
	if p.ptr == nil || !p.ptr.Enabled() || sha256 == "" {
		return nil, false, ""
	}
	if !p.ptr.CaughtUp() {
		return nil, false, ptrTrailName + ": skipped, still syncing"
	}
	rawTags, ok, err := p.ptr.TagsForHash(sha256)
	switch {
	case err != nil:
		return nil, false, ptrTrailName + ": " + err.Error()
	case !ok:
		return nil, false, ptrTrailName + ": no match"
	}
	return mapping.MapPTRTags(rawTags), true, ""
}

// enrichAllOnline runs the online half of a backend=all lookup - the exact or
// similarity post's enrich, or a source-only record when only a candidate URL
// resolved - labelling the row but leaving the item's terminal state to the
// caller so it can fold in the PTR outcome. site names the matched site.
func (p *Processor) enrichAllOnline(ctx context.Context, job, snap *queue.Job, meta map[string]any, hit *simHit) (*monbooru.Result, string, string, error) {
	if meta != nil {
		var sim float64
		if hit != nil {
			job.UpdateItem(0, func(it *queue.Item) { it.PostID = hit.note })
			sim = hit.score
		}
		site := kwdict.String(meta, "category")
		job.SetSite(site)
		url := p.mapper.PostURL(meta)
		res, err := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery,
			p.mapEnrichPayload(meta, url, claimedMD5(meta, snap.MD5, sim), sim))
		return res, url, site, err
	}
	res, err := p.sourceOnlyEnrich(ctx, job, snap, hit)
	return res, hit.url, hit.site, err
}

// sourceOnlyEnrich labels the item as a source-only candidate and records the
// hit as the image's source (unverified, no tags to merge); the caller owns
// the item's terminal state.
func (p *Processor) sourceOnlyEnrich(ctx context.Context, job, snap *queue.Job, hit *simHit) (*monbooru.Result, error) {
	job.SetSite(hit.site)
	job.UpdateItem(0, func(it *queue.Item) { it.PostID = hit.note + ", source only" })
	return p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, monbooru.EnrichPayload{
		Source:     p.mapper.SourceLabel(hit.site),
		URL:        hit.url,
		Verify:     false,
		Similarity: hit.score,
	})
}

// composeLookupNote summarises a backend=all outcome for the queue row: the
// PTR's merge note and the online site's, each shown only when that backend
// applied. An empty merge note reads as "no new tags".
func composeLookupNote(ptrApplied bool, ptrNote, site, onlineNote string) string {
	noteOrNone := func(note string) string {
		if note == "" {
			return "no new tags"
		}
		return note
	}
	var parts []string
	if ptrApplied {
		parts = append(parts, "ptr "+noteOrNone(ptrNote))
	}
	parts = append(parts, site+" "+noteOrNone(onlineNote))
	return strings.Join(parts, "; ")
}

// reportPTRTrail reports a PTR-hit-with-online-miss to monbooru's fetch-status
// so the detail pill leads with the PTR match (tags already applied) above the
// online misses.
func (p *Processor) reportPTRTrail(ctx context.Context, snap *queue.Job, onlineTrail []string) {
	report := append([]string{ptrTrailName + ": match, tags applied"}, onlineTrail...)
	if rerr := p.client.ReportFetchOutcome(ctx, snap.ImageID, snap.Gallery, queue.ErrCodeHashNotFound, strings.Join(report, "; ")); rerr != nil {
		logx.Warnf("queue: job %d fetch-status report failed: %v", snap.ID, rerr)
	}
}

// lookupBooru walks the lookup chain - exact-md5 searches and similarity
// services in one configured order - and enriches from the first hit.
func (p *Processor) lookupBooru(ctx context.Context, job, snap *queue.Job) error {
	meta, hit, trail, err := p.lookupChain(ctx, snap)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	if meta == nil {
		if hit != nil {
			p.enrichSourceOnly(ctx, job, snap, hit)
			return nil
		}
		p.failEnrichFetch(ctx, job, snap, missError(trail))
		return nil
	}
	var sim float64
	if hit != nil {
		job.UpdateItem(0, func(it *queue.Item) { it.PostID = hit.note })
		sim = hit.score
	}
	p.enrichFromMeta(ctx, job, snap, meta, p.mapper.PostURL(meta), claimedMD5(meta, snap.MD5, sim), sim)
	return nil
}

// claimedMD5 picks the md5 an enrich records on the origin: the matched
// post's own kwdict claim. A similarity hit's file differs from the local
// one by design, so passing the query hash would record the wrong side of
// that comparison. An exact hit whose kwdict lacks the field falls back to
// the query hash - equal by construction, since the post was found by
// searching it.
func claimedMD5(meta map[string]any, queryMD5 string, sim float64) string {
	if md5 := kwdict.String(meta, "md5"); md5 != "" {
		return md5
	}
	if sim == 0 {
		return queryMD5
	}
	return ""
}

// enrichSourceOnly records a similarity candidate as the image's source when
// no candidate's metadata could be fetched: the post the service found is
// provenance worth keeping even with no tags to merge.
func (p *Processor) enrichSourceOnly(ctx context.Context, job, snap *queue.Job, hit *simHit) {
	res, err := p.sourceOnlyEnrich(ctx, job, snap, hit)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		failItem(job, 0, errorCode(err), err.Error())
		return
	}
	p.markEnriched(job, snap, res, hit.url)
}

// missError folds a walk's per-source trail into the hash_not_found error the
// fetch-status report carries. monbooru splits the "; "-joined entries back
// into the list its "No source found" flash renders, so entries must stay
// "source: reason" shaped.
func missError(trail []string) error {
	return &queue.CodedError{Code: queue.ErrCodeHashNotFound, Msg: strings.Join(trail, "; ")}
}

// lookupPTR queries the local PTR index by sha256 and enriches with the tags it
// holds. The PTR has no source page or file, so the enrich carries no url and
// is unverified.
func (p *Processor) lookupPTR(ctx context.Context, job, snap *queue.Job) error {
	if p.ptr == nil || !p.ptr.Enabled() {
		err := &queue.CodedError{Code: queue.ErrCodePTRUnavailable, Msg: "the ptr lookup backend is not available"}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	if !p.ptr.CaughtUp() {
		err := &queue.CodedError{Code: queue.ErrCodePTRSyncing, Msg: "the ptr index is not fully synced yet"}
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	rawTags, ok, err := p.ptr.TagsForHash(snap.SHA256)
	if err != nil {
		p.failEnrichFetch(ctx, job, snap, &queue.CodedError{Code: queue.ErrCodeMappingFailed, Msg: err.Error()})
		return nil
	}
	if !ok {
		p.failEnrichFetch(ctx, job, snap, missError([]string{"Public Tag Repository: no match"}))
		return nil
	}
	tags := mapping.MapPTRTags(rawTags)
	res, err := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, monbooru.EnrichPayload{
		Tags:   tags,
		Source: "ptr",
		Verify: false,
	})
	if err != nil {
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	p.markEnriched(job, snap, res, "")
	return nil
}

// processHashImport resolves an md5 to a post via the lookup walk, then runs
// the normal single-post pipeline on the post it found, so the file lands in
// monbooru with the usual created / duplicate outcome.
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
		cerr := &queue.CodedError{Code: queue.ErrCodeMappingFailed, Msg: "the matched post has no canonical URL"}
		job.Fail(cerr.Code, cerr.Msg, time.Now())
		return cerr
	}
	job.SetURL(postURL)
	snap.URL = postURL
	return p.processDownload(ctx, job, snap)
}

// simHit describes the winning similarity candidate: the post whose metadata
// resolved, or - when none could be fetched - the best one left to record as
// a source-only match. note names the site and score for the queue row's
// source link; score rides the enrich so monbooru marks the origin
// similarity-matched.
type simHit struct {
	site, url, note string
	score           float64
}

// lookupChain queries each opted-in lookup source in its configured order:
// a site source is its exact-md5 search, a similarity source a perceptual
// query against the image's thumbnail. The first matching post's metadata
// wins. A source missing its credential is skipped, a miss continues, and a
// per-source error is noted without ending the chain - one source being down
// must not kill the lookup. A full miss returns a nil meta with the trail, so
// "nobody has it" reads differently from "the likely sources were down" and
// callers can merge the trail with other backends' entries; a miss where a
// similarity service still offered candidates carries the best one back so
// callers can record it as a source.
func (p *Processor) lookupChain(ctx context.Context, snap *queue.Job) (map[string]any, *simHit, []string, error) {
	chain := p.mapper.LookupChain()
	if len(chain) == 0 {
		return nil, nil, nil, &queue.CodedError{Code: queue.ErrCodeHashNotFound, Msg: "no lookup source has an order set in settings"}
	}
	var trail []string
	var fallback *simHit
	// The thumbnail is fetched once, when the chain first reaches a similarity
	// source, so a chain that resolves exactly uploads nothing anywhere.
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
			// The credential gate comes before the thumbnail fetch, so a chain
			// whose services are all unconfigured never fetches one at all.
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

// lookupWalk is the chain's exact-md5 half on its own: what a hash import
// runs, since a pasted hash carries no image bytes for a similarity query.
func (p *Processor) lookupWalk(ctx context.Context, md5 string) (map[string]any, []string, error) {
	sites := p.mapper.LookupSites()
	if len(sites) == 0 {
		return nil, nil, &queue.CodedError{Code: queue.ErrCodeHashNotFound, Msg: "no site has a lookup order set in settings"}
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

// walkSite runs one site's exact-md5 search step, returning the matched
// post's metadata or nil with the miss, skip, or error noted on the trail.
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
	if code := errorCode(err); code == queue.ErrCodeMappingFailed {
		// FetchMeta reports an empty resolve as mapping_failed: the search
		// matched nothing, so this site does not hold the hash.
		*trail = append(*trail, site+": no match")
	} else {
		*trail = append(*trail, site+": "+code)
		logx.Warnf("queue: hash lookup on %s failed: %v", site, err)
	}
	return nil
}

// similaritySource queries one similarity service with the thumbnail and
// fetches the best candidate's metadata. Candidates below the configured
// similarity floor are dropped; the rest are tried in the operator's chain
// order - the ranking says which booru's tags they want - with the unranked
// ones behind, best score first, so a hit on no ranked site still lands on
// the most confident candidate. The walk is capped at three fetch attempts,
// so one service's bad links cannot stall the chain. When no candidate's
// metadata resolves, the preferred one still comes back as a nil-meta hit so
// the caller can record the post as a source; with no curated candidate
// above the floor at all, the best extra clearing it comes back the same
// way, its host standing in for the site label.
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
		// Extras join the note here: a miss should show the service's best
		// fit even when it sits on a site the walk cannot fetch.
		var extras []similarity.Candidate
		for _, c := range res.Extras {
			if c.URL != "" && !seen[c.URL] {
				seen[c.URL] = true
				extras = append(extras, c)
			}
		}
		*trail = append(*trail, service+": no match"+closestNote(append(missed, extras...)))
		// An extra that clears the floor is a confident hit on a site the
		// walk cannot fetch tags from - still the image's likely source, so
		// hand the best one back for the source-only record.
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
	// An unranked site sorts behind every ranked one; among equals the higher
	// score wins.
	rankOf := func(site string) int {
		if r := rank[site]; r != 0 {
			return r
		}
		return len(rank) + 1
	}
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

// closestNote lists the best candidates a service offered that the walk could
// not use - under the similarity floor, or on a site outside the curated
// profiles - so a miss shows what almost matched and the operator can judge
// the floor. Capped at three; empty when the service offered nothing at all.
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

// missingCredential reports whether a site's profile requires a credential the
// config does not provide, with its label for the walk trail. The walk skips
// such a site: the lookup order expresses intent, the credential gates
// execution.
func (p *Processor) missingCredential(site string) (string, bool) {
	prof, ok := p.mapper.Lookup(site)
	if !ok {
		return "", false
	}
	label, missing := mapping.RequiredCredential(prof.Auth, p.cfg.Current().FindSite(site))
	if !missing {
		return "", false
	}
	return label, true
}
