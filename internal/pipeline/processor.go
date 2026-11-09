// Package pipeline is the queue's Processor: it resolves, downloads, maps
// and pushes one job.
package pipeline

import (
	"archive/zip"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/fsx"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/internal/sitestate"
)

type PTRService interface {
	Enabled() bool
	CaughtUp() bool
	TagsForHash(hashHex string) (tags []string, ok bool, err error)
}

type SimilarityService interface {
	Missing(service string) (label string, missing bool)
	Search(ctx context.Context, service string, image []byte) (similarity.Result, error)
}

type Processor struct {
	runner    gdl.Runner
	mapper    *mapping.Mapper
	client    *monbooru.Client
	cfg       *config.Provider
	workRoot  string
	siteState *sitestate.Tracker
	ptr       PTRService
	sim       SimilarityService
	books     *queue.Store
}

func New(runner gdl.Runner, mapper *mapping.Mapper, client *monbooru.Client, cfg *config.Provider, workRoot string, siteState *sitestate.Tracker, ptr PTRService, sim SimilarityService) *Processor {
	return &Processor{runner: runner, mapper: mapper, client: client, cfg: cfg, workRoot: workRoot, siteState: siteState, ptr: ptr, sim: sim}
}

var (
	_ queue.Processor    = (*Processor)(nil)
	_ queue.DropReporter = (*Processor)(nil)
	// Reached by assertion in processContrib, where a mismatch would compile
	// and fail every send at run time.
	_ ContribSender = (*ptr.Engine)(nil)
)

func (p *Processor) UseStore(st *queue.Store) { p.books = st }

func (p *Processor) ReportDropped(ctx context.Context, snap *queue.Job) {
	switch snap.Kind {
	case queue.KindLookup, queue.KindMetadata, queue.KindReplace:
	default:
		return
	}
	if err := p.client.ReportFetchOutcome(ctx, snap.ImageID, snap.Gallery, string(snap.Kind), snap.Backend,
		outcome.ErrCodeCanceled, "the job was dropped before it ran"); err != nil {
		logx.Warnf("queue: job %d dropped-state report failed: %v", snap.ID, err)
	}
}

func (p *Processor) Process(ctx context.Context, job *queue.Job) error {
	snap := job.Snapshot()

	if snap.Kind == queue.KindContrib {
		return p.processContrib(ctx, job, snap)
	}
	if err := p.learnCategories(ctx); err != nil {
		if snap.ImageID != 0 {
			if rerr := p.client.ReportFetchOutcome(ctx, snap.ImageID, snap.Gallery, string(snap.Kind), snap.Backend, errorCode(err), bareMessage(err)); rerr != nil {
				logx.Warnf("queue: job %d fetch-status report failed: %v", snap.ID, rerr)
			}
		}
		return abortJob(ctx, job, err)
	}
	switch snap.Kind {
	case queue.KindMetadata:
		return p.processMetadata(ctx, job, snap)
	case queue.KindLookup:
		return p.processLookup(ctx, job, snap)
	case queue.KindHashImport:
		return p.processHashImport(ctx, job, snap)
	case queue.KindReplace:
		return p.processReplace(ctx, job, snap)
	}
	return p.processDownload(ctx, job, snap)
}

// Best-effort: monbooru being down fails the push on its own, with a better
// reason than this list could give. The exception is a rule naming a category
// no list ever confirmed, which would map wrongly on a push that goes through.
func (p *Processor) learnCategories(ctx context.Context) error {
	listCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	names, complete, err := p.client.CustomCategories(listCtx)
	if err != nil {
		logx.Infof("queue: reading monbooru's categories: %v", err)
		if p.mapper.CategoryListMissing() {
			return &outcome.CodedError{Code: outcome.ErrCodeMonbooruUnreachable, Msg: "a rule names a category, and monbooru's category list could not be read: " + err.Error()}
		}
		return nil
	}
	p.mapper.LearnCustomCategories(names, complete)
	if !complete && p.mapper.CategoryListMissing() {
		return &outcome.CodedError{Code: outcome.ErrCodeMonbooruUnreachable, Msg: "a rule names a category, and a gallery's category list could not be read"}
	}
	return nil
}

// gallery-dl records a post in its archive once the file lands, before the
// push, so a run that could not push would leave its posts skipped for good.
func (p *Processor) processDownload(ctx context.Context, job, snap *queue.Job) error {
	if err := p.client.Reachable(ctx); err != nil {
		return abortJob(ctx, job, err)
	}
	rng, limit := p.rangeFor(snap)
	// One past the window, so a full window can tell whether the listing
	// goes on.
	probe := rng
	if limit > 0 {
		probe = strconv.Itoa(snap.Offset+1) + "-" + strconv.Itoa(snap.Offset+limit+1)
	}
	res, err := p.runner.Resolve(ctx, snap.URL, probe, false)
	if err != nil {
		return abortJob(ctx, job, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(res.Items) == 0 {
		// A dispatcher's -j pass lists its Message.Queue handoffs without
		// following them.
		if len(res.Queue) > 0 {
			return p.processDispatch(ctx, job, snap, res, rng, limit)
		}
		return p.noPosts(ctx, job, snap)
	}

	resolved, listErr := res.Items, res.Err
	// An error past a full window says nothing about the window itself.
	more := limit > 0 && (len(resolved) > limit || (len(resolved) == limit && listErr != nil))
	if more {
		resolved, listErr = resolved[:limit], nil
	}
	site := resolved[0].Category
	job.SetSubcategory(resolved[0].Subcategory)
	cbz := p.mapper.KindOf(site) == mapping.KindManga
	if cbz && snap.Watch != 0 {
		job.SetSite(site)
		return abortJob(ctx, job, errWatchManga)
	}

	bundle := resolved[0].Subcategory == "pool" || cbz
	if bundle && more {
		// A pool or a book is one work, and the cap exists to bound
		// open-ended searches.
		whole, rerr := p.runner.Resolve(ctx, snap.URL, "", false)
		if rerr != nil {
			return abortJob(ctx, job, rerr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A short re-resolve (a rate limit, a flaky page) must not empty
		// a work the capped pass proved non-empty.
		switch {
		case len(whole.Items) >= len(resolved):
			resolved, listErr = whole.Items, whole.Err
			rng = ""
		case cbz:
			// A book is one file: bundling the window would push a short
			// archive as complete.
			return abortJob(ctx, job, &outcome.CodedError{
				Code: outcome.ErrCodeDownloadFailed,
				Msg:  fmt.Sprintf("re-resolving the whole gallery returned %d pages, fewer than the %d already found", len(whole.Items), len(resolved)),
			})
		default:
			// Flagged capped, or the partial pool reads complete with
			// nothing to continue.
			job.SetCapped(limit)
			logx.Infof("queue: job %d capped to the first %d pages: re-resolving the whole pool returned %d, fewer than the %d already found", snap.ID, limit, len(whole.Items), len(resolved))
		}
	} else if more {
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d items (--range %s); re-submit with a higher range to fetch more", snap.ID, limit, rng)
	}

	return p.fetch(ctx, job, snap, resolved, site, cbz, false, rng, listErr)
}

// An extractor skips a post with no file_url silently, which reads as a search
// that matched nothing. gallery-dl's one-post extractors are all subcategory
// post, so that one can be told apart; a search's skipped post cannot.
func (p *Processor) noPosts(ctx context.Context, job, snap *queue.Job) error {
	if snap.Offset != 0 {
		return nil
	}
	site, sub := p.runner.ExtractorInfo(ctx, snap.URL)
	if sub != "post" {
		return nil
	}
	job.SetSite(site)
	job.SetSubcategory(sub)
	return abortJob(ctx, job, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: gdl.NoFileMsg})
}

func (p *Processor) processMetadata(ctx context.Context, job *queue.Job, snap *queue.Job) error {
	job.SetItems([]queue.Item{p.enrichItem(ctx, snap, snap.URL)})
	meta, err := p.runner.FirstPost(ctx, snap.URL)
	if err == nil {
		err = p.manyPostsErr(ctx, snap.URL, meta)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	job.SetSite(kwdict.String(meta, "category"))
	labelEnrichPost(job, meta)
	if err := p.flattenedErr(meta); err != nil {
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	payload, warnings := p.mapEnrichPayload(meta, snap.URL, kwdict.String(meta, "md5"), 0)
	res, err := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, payload)
	if err != nil {
		if ctx.Err() == nil {
			failItem(job, 0, errorCode(err), err.Error())
		}
		return nil
	}
	res.TagWarnings = slices.Concat(warnings, res.TagWarnings)
	p.markEnriched(job, snap, res, snap.URL, payload.PostFields)
	return nil
}

// A stored url with no post link of its own can be the search it came from, and
// with no md5 monbooru cannot tell the first post's tags from the image's.
func (p *Processor) manyPostsErr(ctx context.Context, url string, meta map[string]any) error {
	if kwdict.String(meta, "md5") != "" || p.mapper.PostURL(meta) != "" || p.mapper.KindOf(kwdict.String(meta, "category")) == mapping.KindManga {
		return nil
	}
	res, err := p.runner.Resolve(ctx, url, "1-2", false)
	if err != nil {
		return err
	}
	if len(res.Items) > 1 && (res.Items[0].ID == "" || res.Items[0].ID != res.Items[1].ID) {
		return &outcome.CodedError{Code: outcome.ErrCodeUnsupportedURL, Msg: "the url names more than one post"}
	}
	return nil
}

// Best-effort: the sha256 only makes the row's link a /i/<sha256>
// permalink, where /images/<id> would open that id in the active gallery.
func (p *Processor) enrichItem(ctx context.Context, snap *queue.Job, url string) queue.Item {
	it := queue.Item{URL: url, Status: queue.ItemPending, SHA256: snap.SHA256}
	if it.SHA256 == "" && snap.ImageID != 0 {
		if sha, err := p.client.ImageSHA256(ctx, snap.ImageID, snap.Gallery); err == nil {
			it.SHA256 = sha
		}
	}
	return it
}

// Landing such a post would file its artists and characters as general tags,
// and a re-push would flag the real ones stale.
func (p *Processor) flattenedErr(meta map[string]any) error {
	if !p.mapper.SuspectFlattenedTags(meta) {
		return nil
	}
	return errFlattened
}

var errFlattened = &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: mapping.FlattenedTagsMsg}

// monbooru records an enrich it refused; a post stopped before the enrich it
// has to be told of.
func (p *Processor) failOnline(ctx context.Context, job, snap *queue.Job, err error) {
	if errors.Is(err, errFlattened) {
		p.failEnrichFetch(ctx, job, snap, err)
		return
	}
	if ctx.Err() == nil {
		failItem(job, 0, errorCode(err), err.Error())
	}
}

// A nonzero sim marks the origin similarity-matched; 0 is an exact match
// or a refetch.
func (p *Processor) mapEnrichPayload(meta map[string]any, url, md5 string, sim float64) (monbooru.EnrichPayload, []string) {
	pf := p.mapper.Map(meta)
	return monbooru.EnrichPayload{
		PostFields: postFieldsFrom(pf, url, kwdict.ID(meta)),
		SourceMD5:  md5,
		Verify:     true,
		Similarity: sim,
	}, pf.UnknownTargets
}

func (p *Processor) markEnriched(job, snap *queue.Job, res *monbooru.Result, url string, sent monbooru.PostFields) {
	note := cmp.Or(res.MergeNote, "no new tags")
	if snap.URL == "" && url != "" {
		job.SetURL(url)
	}
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = outcome.Enriched
		it.MonbooruID = snap.ImageID
		it.MergeNote = note
		it.TagWarnings = res.TagWarnings
		it.TagsSent, it.Held = len(sent.Tags), heldTags(sent.WithheldTags, res.RejectedTags)
		if it.URL == "" {
			it.URL = url
		}
	})
}

// What never reached the image: what monloader's rules held back, then
// what the gallery rejected.
func heldTags(withheld, rejected []string) []string {
	held := make([]string, 0, len(withheld)+len(rejected))
	for _, t := range withheld {
		held = append(held, t+" (held by your rules)")
	}
	for _, t := range rejected {
		held = append(held, t+" (rejected by the gallery)")
	}
	return held
}

// A similarity hit's label already names its score, which says more than
// the post id.
func labelEnrichPost(job *queue.Job, meta map[string]any) {
	job.UpdateItem(0, func(it *queue.Item) {
		if it.PostID == "" {
			it.PostID = kwdict.ID(meta)
		}
	})
}

// Without the report monbooru's detail poll never gets an outcome.
func (p *Processor) failEnrichFetch(ctx context.Context, job, snap *queue.Job, err error) {
	// A cancel's kill error is not the fetch outcome; the worker labels
	// the item canceled.
	if ctx.Err() != nil {
		return
	}
	code := errorCode(err)
	failItem(job, 0, code, err.Error())
	if rerr := p.client.ReportFetchOutcome(ctx, snap.ImageID, snap.Gallery, string(snap.Kind), snap.Backend, code, bareMessage(err)); rerr != nil {
		logx.Warnf("queue: job %d fetch-status report failed: %v", snap.ID, rerr)
	}
}

func (p *Processor) processDispatch(ctx context.Context, job, snap *queue.Job, res gdl.ResolveResult, rng string, limit int) error {
	if p.mapper.KindOf(res.Category) == mapping.KindManga {
		job.SetSubcategory(res.Subcategory)
		if snap.Watch != 0 && !gdl.IsListing(res.Subcategory) {
			job.SetSite(res.Category)
			return abortJob(ctx, job, errWatchManga)
		}
		p.processChapters(ctx, job, snap, res, limit)
		return nil
	}
	// With deep set, rng becomes --chapter-range, so an open thread's
	// children stay bounded.
	deep, err := p.runner.Resolve(ctx, snap.URL, rng, true)
	if err != nil {
		return abortJob(ctx, job, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(deep.Items) == 0 {
		if err := cmp.Or(deep.Err, res.Err); err != nil {
			return abortJob(ctx, job, err)
		}
		return nil
	}
	// res.Queue stops one past this window's end, which is what says more is
	// left to fetch.
	if limit > 0 && len(res.Queue) > snap.Offset+limit {
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d children (--chapter-range %s); re-submit with a higher range to fetch more", snap.ID, limit, rng)
	}
	// The site is the dispatcher, not the first leaf's image host.
	err = p.fetch(ctx, job, snap, deep.Items, res.Category, false, true, rng, deep.Err)
	if res.Err != nil && err == nil && ctx.Err() == nil && (limit == 0 || len(res.Queue) <= snap.Offset+limit) {
		p.markCutShort(job, snap, res.Err, len(res.Queue))
	}
	return err
}

func (p *Processor) fetch(ctx context.Context, job, snap *queue.Job, resolved []gdl.Item, site string, cbz, deep bool, rng string, listErr error) error {
	gallery := p.beginJob(job, snap, site)

	// Published before the download so the rows show while it runs.
	job.SetItems(p.initialItems(resolved, cbz, snap.URL, snap.PageURL))

	// A book is one work, and every page carries its tags.
	if cbz {
		if note, hit := p.mapper.Blacklisted(resolved[0].Meta); hit {
			skipItem(job, 0, outcome.SkippedBlacklist, note)
			return nil
		}
	}
	// A deep pass's --chapter-range counts children, not these rows, so its
	// files land and only the sidecar check can drop them.
	var blacklisted map[int]string
	if !cbz && !deep {
		blacklisted = p.blacklistedRows(job, resolved)
	}
	fetchRange, anyKept := keptRange(rng, snap.Offset, len(resolved), blacklisted)

	workDir := filepath.Join(p.workRoot, fmt.Sprintf("job-%d", snap.ID))
	if mkErr := os.MkdirAll(workDir, 0o755); mkErr != nil {
		job.Fail(outcome.ErrCodeDownloadFailed, mkErr.Error(), time.Now())
		return mkErr
	}
	defer os.RemoveAll(workDir)

	var onFile func(gdl.Downloaded)
	if !cbz {
		rows := rowsByPost(resolved)
		onFile = func(d gdl.Downloaded) {
			if i, ok := rows[postKeyOf(d.Meta)]; ok {
				job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
			}
		}
	}

	var downloaded []gdl.Downloaded
	var dlErr error
	stopped := false
	if anyKept {
		mode := p.archiveMode(snap, resolved, site, cbz, deep)
		downloaded, dlErr = p.runner.Download(ctx, snap.URL, fetchRange, workDir, mode, onFile, deep)
		stopped = mode == gdl.ArchiveCatchUp && gdl.CaughtUp(downloaded)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if cbz {
		p.processCBZ(ctx, job, 0, writtenOnly(downloaded), len(resolved), gallery, workDir, cmp.Or(dlErr, entryErr(downloaded)), snap.URL)
		return nil
	}
	entries := pairDownloads(resolved, downloaded, blacklisted)
	if extra := entries[len(resolved):]; len(extra) > 0 {
		// Already in gallery-dl's archive, so dropping them would lose
		// them for good.
		logx.Warnf("queue: job %d (%s) downloaded %d posts the resolve did not list; adding them", snap.ID, site, len(extra))
		added := make([]gdl.Item, len(extra))
		for i, d := range extra {
			added[i] = gdl.Item{ID: kwdict.ID(d.Meta), Num: kwdict.Num(d.Meta), Meta: d.Meta}
		}
		job.AddItems(p.initialItems(added, false, snap.URL, snap.PageURL))
	}
	reached := len(entries)
	if stopped {
		reached = lastEntry(entries) + 1
	}
	p.processItems(ctx, job, entries, blacklisted, gallery, snap.URL, snap.PageURL, dlErr, reached)
	if ctx.Err() == nil {
		if cut := cutShort(listErr, dlErr, entries, blacklisted, reached); cut != nil {
			listed := snap.Offset + len(entries)
			if deep {
				listed = -1
			}
			p.markCutShort(job, snap, cut, listed)
		}
	}
	return nil
}

func lastEntry(entries []*gdl.Downloaded) int {
	for i, d := range slices.Backward(entries) {
		if d != nil {
			return i
		}
	}
	return -1
}

// The error no row carries: a download that walked the same range cleanly overrules
// the resolve's, unless kept positions stopped it short of where that one hit.
func cutShort(listErr, dlErr error, entries []*gdl.Downloaded, blacklisted map[int]string, reached int) error {
	if dlErr == nil && len(blacklisted) == 0 {
		return nil
	}
	if listErr != nil {
		return listErr
	}
	for i, d := range entries[:reached] {
		if _, out := blacklisted[i]; d == nil && !out {
			return nil
		}
	}
	return dlErr
}

// A check that reached an archived post has caught up, so what lay past it does not
// matter. A negative listed is a deep pass, whose failing children leave no window short.
func (p *Processor) markCutShort(job, snap *queue.Job, cut error, listed int) {
	if snap.Watch != 0 && job.Snapshot().Summary.Archived > 0 {
		return
	}
	job.CutShort(errorCode(cut), cut.Error())
	if _, limit := p.rangeFor(snap); listed >= 0 && limit > 0 {
		job.SetListedTo(limit, listed)
	}
}

func (p *Processor) blacklistedRows(job *queue.Job, resolved []gdl.Item) map[int]string {
	rows := map[int]string{}
	for i, it := range resolved {
		if note, hit := p.mapper.Blacklisted(it.Meta); hit {
			rows[i] = note
			skipItem(job, i, outcome.SkippedBlacklist, note)
		}
	}
	return rows
}

// Both passes enumerate the same listing, so the positions of the rows left
// over are what the download fetches. With nothing excluded the range stays as
// it was, open end included.
func keptRange(rng string, offset, total int, blacklisted map[int]string) (string, bool) {
	if len(blacklisted) == 0 {
		return rng, true
	}
	first := 1
	if rng != "" {
		first = offset + 1
	}
	var kept []int
	for i := range total {
		if _, out := blacklisted[i]; !out {
			kept = append(kept, first+i)
		}
	}
	return gdl.PositionRange(kept), len(kept) > 0
}

type postKey struct {
	id  string
	num int
}

func postKeyOf(meta map[string]any) postKey {
	return postKey{id: kwdict.ID(meta), num: kwdict.Num(meta)}
}

func rowsByPost(resolved []gdl.Item) map[postKey]int {
	rows := make(map[postKey]int, len(resolved))
	for i := len(resolved) - 1; i >= 0; i-- {
		if resolved[i].ID != "" {
			rows[postKey{id: resolved[i].ID, num: resolved[i].Num}] = i
		}
	}
	return rows
}

// A failed download prints no line, so stream positions drift: entries match rows
// by post, searching forward since both passes list in the same order. A blacklisted
// row was never fetched, so an entry with no id must not land on it.
func pairDownloads(resolved []gdl.Item, downloaded []gdl.Downloaded, blacklisted map[int]string) []*gdl.Downloaded {
	entries := make([]*gdl.Downloaded, len(resolved))
	next := 0
	for i := range downloaded {
		d := &downloaded[i]
		row := -1
		if key := postKeyOf(d.Meta); key.id != "" {
			for j := next; j < len(resolved); j++ {
				if resolved[j].ID == key.id && resolved[j].Num == key.num {
					row = j
					break
				}
			}
		} else {
			for _, out := blacklisted[next]; out; _, out = blacklisted[next] {
				next++
			}
			if next < len(resolved) {
				row = next
			}
		}
		if row < 0 {
			entries = append(entries, d)
			continue
		}
		entries[row] = d
		next = row + 1
	}
	return entries
}

func (p *Processor) beginJob(job, snap *queue.Job, site string) string {
	job.SetSite(site)
	p.siteState.Reached(site, time.Now())
	gallery := cmp.Or(snap.Gallery, p.mapper.Gallery(site))
	job.SetGallery(gallery)
	return gallery
}

func written(d gdl.Downloaded) bool { return !d.Skipped && d.Err == "" }

func writtenOnly(downloaded []gdl.Downloaded) []gdl.Downloaded {
	return slices.DeleteFunc(slices.Clone(downloaded), func(d gdl.Downloaded) bool { return !written(d) })
}

// The reason a file landed but cannot be used, for a path that pushes one
// file built from every entry.
func entryErr(downloaded []gdl.Downloaded) error {
	for _, d := range downloaded {
		if d.Err != "" {
			return &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: d.Err}
		}
	}
	return nil
}

func (p *Processor) initialItems(resolved []gdl.Item, cbz bool, submittedURL, pageURL string) []queue.Item {
	if cbz {
		return []queue.Item{{PostID: bundleKey(resolved), Status: queue.ItemPending}}
	}
	items := make([]queue.Item, len(resolved))
	for i, it := range resolved {
		items[i] = queue.Item{PostID: it.ID, Num: it.Num, URL: p.itemURL(it.Meta, submittedURL, pageURL), Status: queue.ItemPending}
	}
	return items
}

// A directlink links its page or the submitted URL, never one rebuilt from
// the sidecar, whose extension gallery-dl may rewrite.
func (p *Processor) itemURL(meta map[string]any, submittedURL, pageURL string) string {
	if kwdict.String(meta, "category") == mapping.CategoryDirectlink {
		return cmp.Or(pageURL, submittedURL)
	}
	return cmp.Or(p.mapper.PostURL(meta), submittedURL)
}

func pageHost(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// A per-job max_items can only lower the configured cap, which bounds one
// job's footprint.
func (p *Processor) rangeFor(snap *queue.Job) (string, int) {
	limit := p.cfg.Current().Downloader.MaxItemsPerJob
	if m := snap.MaxItems; m > 0 && (limit <= 0 || m < limit) {
		limit = m
	}
	if limit <= 0 {
		return "", 0
	}
	start := snap.Offset + 1
	return strconv.Itoa(start) + "-" + strconv.Itoa(snap.Offset+limit), limit
}

func (p *Processor) processItems(ctx context.Context, job *queue.Job, entries []*gdl.Downloaded, blacklisted map[int]string, gallery, submittedURL, pageURL string, dlErr error, reached int) {
	folder, extras := p.folder(job), p.extras(job)
	for i, d := range entries {
		if ctx.Err() != nil {
			return // the worker marks the remaining pending items canceled
		}
		// Landed anyway: the listing moved between the two passes.
		if _, out := blacklisted[i]; out {
			if d != nil && written(*d) {
				removeScratch(d.Path)
			}
			continue
		}
		if d == nil {
			if i >= reached {
				skipItem(job, i, outcome.SkippedArchive, "") // a catch-up stop never got here
				continue
			}
			// An archive skip prints a line, so with kept positions asked for
			// by number, a row with none fell outside them: a new post shifted it.
			if dlErr == nil && len(blacklisted) > 0 {
				failItem(job, i, outcome.ErrCodeDownloadFailed, "the listing moved between the two passes; retry to fetch it")
				continue
			}
			p.markUndownloaded(job, i, dlErr)
			continue
		}
		// gallery-dl's non-zero exit covers any one failed file, not the
		// posts its archive held.
		if d.Skipped {
			skipItem(job, i, outcome.SkippedArchive, "")
			continue
		}
		if d.Err != "" {
			// A failure with its reason, never an archive skip, which
			// reads "already in your library".
			failItem(job, i, outcome.ErrCodeMappingFailed, d.Err)
			continue
		}
		if !gdl.Ingestable(d.Path) {
			skipItem(job, i, outcome.SkippedUnsupported, "")
			continue
		}
		// The sidecar is the first sight of a flat-tag site's categories.
		if note, hit := p.mapper.Blacklisted(d.Meta); hit {
			removeScratch(d.Path)
			skipItem(job, i, outcome.SkippedBlacklist, note)
			continue
		}
		if err := p.flattenedErr(d.Meta); err != nil {
			removeScratch(d.Path)
			failItem(job, i, errorCode(err), err.Error())
			continue
		}
		pf := p.mapper.Map(d.Meta)
		labelFrom := kwdict.String(d.Meta, "category")
		if labelFrom == mapping.CategoryDirectlink {
			labelFrom = kwdict.String(d.Meta, "domain")
			// Named by the page's host, not the cdn the file happened to
			// live on.
			if h := pageHost(pageURL); h != "" {
				labelFrom = h
				pf.Source = p.mapper.SourceLabel(h)
			}
		}
		pf = p.withExtras(pf, extras, labelFrom)
		order := pf.CollectionOrder
		if pf.Collection != "" && order == 0 {
			order = i + 1
		}
		meta := pushMetaFrom(pf, d.Path, p.itemURL(d.Meta, submittedURL, pageURL), d.Meta)
		meta.Collection, meta.CollectionOrder = pf.Collection, order
		meta.Via, meta.Folder, meta.ClientTags = pf.Via, folder, pf.ClientTags
		p.pushOne(ctx, job, i, d.Path, meta, gallery, pf.UnknownTargets)
	}
}

func pushMetaFrom(pf mapping.PushFields, path, url string, meta map[string]any) monbooru.PushMeta {
	return monbooru.PushMeta{
		PostFields: postFieldsFrom(pf, url, kwdict.ID(meta)),
		MD5:        kwdict.String(meta, "md5"),
		Filename:   filepath.Base(path),
	}
}

func postFieldsFrom(pf mapping.PushFields, url, postID string) monbooru.PostFields {
	fields := pf.PostFields
	fields.URL, fields.PostID = url, postID
	return fields
}

func (p *Processor) pushOne(ctx context.Context, job *queue.Job, i int, path string, meta monbooru.PushMeta, gallery string, warnings []string) {
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	res, err := p.client.PushImageFile(ctx, path, meta, gallery)
	if err != nil {
		failItem(job, i, errorCode(err), err.Error())
		return
	}
	res.TagWarnings = slices.Concat(warnings, res.TagWarnings)
	recordSuccess(job, i, res, meta.PostFields)
	removeScratch(path)
}

func removeScratch(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + ".json")
}

// The resolve lists one handoff past the window, so only a title with
// chapters past it reads capped.
func (p *Processor) processChapters(ctx context.Context, job, snap *queue.Job, res gdl.ResolveResult, limit int) {
	gallery := p.beginJob(job, snap, res.Category)

	// The handoffs count from the listing's start, so the offset is applied
	// here; without it a fetch-all chain never ends.
	chapters := res.Queue[min(snap.Offset, len(res.Queue)):]
	capped := limit > 0 && len(chapters) > limit
	if capped {
		chapters = chapters[:limit]
	}
	job.SetItems(chapterItems(chapters))
	if capped {
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d books; re-submit with a higher range to fetch more", snap.ID, limit)
	}

	if res.Err != nil && len(chapters) == 0 {
		_ = abortJob(ctx, job, res.Err)
		return
	}
	for i, ch := range chapters {
		if ctx.Err() != nil {
			return // the worker marks the remaining pending items canceled
		}
		if p.bookLanded(snap, ch.URL) {
			skipItem(job, i, outcome.SkippedArchive, "")
			continue
		}
		if note, hit := p.bookBlacklisted(snap, ch.URL); hit {
			skipItem(job, i, outcome.SkippedBlacklist, note)
			continue
		}
		chapterDir := filepath.Join(p.workRoot, fmt.Sprintf("job-%d-ch%d", snap.ID, i))
		p.importChapter(ctx, job, i, ch.URL, gallery, chapterDir)
		os.RemoveAll(chapterDir)
	}
	if res.Err != nil && !capped && ctx.Err() == nil {
		p.markCutShort(job, snap, res.Err, snap.Offset+len(chapters))
	}
}

// A forced check carries a ForceTo and retries only what never landed; an
// operator's force carries none and fetches again.
func (p *Processor) readsBooks(snap *queue.Job) bool {
	return snap.Watch != 0 && p.books != nil && (!snap.Force || snap.ForceTo > 0)
}

func (p *Processor) bookLanded(snap *queue.Job, url string) bool {
	if !p.readsBooks(snap) {
		return false
	}
	landed, err := p.books.BookLanded(url)
	if err != nil {
		logx.Warnf("queue: job %d reading landed books: %v", snap.ID, err)
	}
	return landed
}

// Judged again from the stored metadata, so a removed rule lets the book through
// and a kept one costs the site no requests.
func (p *Processor) bookBlacklisted(snap *queue.Job, url string) (string, bool) {
	if !p.readsBooks(snap) {
		return "", false
	}
	meta, err := p.books.BlacklistedBook(url)
	if err != nil {
		logx.Warnf("queue: job %d reading blacklisted books: %v", snap.ID, err)
	}
	if meta == nil {
		return "", false
	}
	return p.mapper.Blacklisted(meta)
}

func (p *Processor) markBookBlacklisted(job *queue.Job, url string, meta map[string]any) {
	if p.books == nil {
		return
	}
	if err := p.books.MarkBookBlacklisted(url, meta); err != nil {
		logx.Warnf("queue: job %d recording a blacklisted book: %v", job.ID, err)
	}
}

// A chapter is one book, so the archive is bypassed and every page lands.
func (p *Processor) importChapter(ctx context.Context, job *queue.Job, i int, chapterURL, gallery, workDir string) {
	// The link comes from gallery-dl's output, not from the operator.
	if !config.IsHTTPURL(chapterURL) {
		failItem(job, i, outcome.ErrCodeUnsupportedURL, "not an http(s) chapter link: "+chapterURL)
		return
	}
	res, err := p.runner.Resolve(ctx, chapterURL, "", false)
	if err != nil {
		failItem(job, i, errorCode(err), err.Error())
		return
	}
	if ctx.Err() != nil {
		return
	}
	// The download follows every handoff, so several would bundle as one book.
	// A single one is still a book: exhentai hands a removed gallery to hitomi.
	if len(res.Items) == 0 && len(res.Queue) > 1 {
		failItem(job, i, outcome.ErrCodeUnsupportedURL, "a listed title is not one book; paste its own URL to fetch its chapters: "+chapterURL)
		return
	}
	if len(res.Items) > 0 {
		if note, hit := p.mapper.Blacklisted(res.Items[0].Meta); hit {
			p.markBookBlacklisted(job, chapterURL, res.Items[0].Meta)
			skipItem(job, i, outcome.SkippedBlacklist, note)
			return
		}
	}
	downloaded, dlErr := p.runner.Download(ctx, chapterURL, "", workDir, gdl.ArchiveBypass, nil, false)
	if ctx.Err() != nil {
		return
	}
	p.processCBZ(ctx, job, i, writtenOnly(downloaded), len(res.Items), gallery, workDir, cmp.Or(dlErr, entryErr(downloaded)), chapterURL)
}

func chapterItems(chapters []gdl.QueueItem) []queue.Item {
	items := make([]queue.Item, len(chapters))
	for i, ch := range chapters {
		id := kwdict.ID(ch.Meta)
		if id != "" {
			id = "chapter:" + id
		} else {
			id = chapterLabel(ch.URL)
		}
		items[i] = queue.Item{PostID: id, URL: ch.URL, Status: queue.ItemPending}
	}
	return items
}

func chapterLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	p := strings.TrimRight(u.Path, "/")
	return p[strings.LastIndex(p, "/")+1:]
}

func (p *Processor) processCBZ(ctx context.Context, job *queue.Job, itemIdx int, downloaded []gdl.Downloaded, total int, gallery, workDir string, dlErr error, sourceURL string) {
	bundleName := fsx.SanitizeFilename(poolName(downloaded))

	if ctx.Err() != nil {
		return
	}

	pages := orderedPages(downloaded)
	if len(pages) == 0 {
		// A book bypasses the archive, so nothing here was already fetched.
		if dlErr == nil {
			dlErr = &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "the gallery yielded no pages"}
		}
		failItem(job, itemIdx, errorCode(dlErr), dlErr.Error())
		return
	}
	if note, hit := p.mapper.Blacklisted(downloaded[0].Meta); hit {
		p.markBookBlacklisted(job, sourceURL, downloaded[0].Meta)
		skipItem(job, itemIdx, outcome.SkippedBlacklist, note)
		return
	}
	if dlErr != nil {
		failItem(job, itemIdx, errorCode(dlErr), dlErr.Error())
		return
	}
	if len(pages) < total {
		failItem(job, itemIdx, outcome.ErrCodeDownloadFailed, fmt.Sprintf("bundled %d of %d pages", len(pages), total))
		return
	}

	dest := filepath.Join(workDir, "bundle.cbz")
	if err := buildCBZFile(pages, dest); err != nil {
		failItem(job, itemIdx, outcome.ErrCodeMappingFailed, err.Error())
		return
	}
	job.UpdateItem(itemIdx, func(it *queue.Item) { it.Status = queue.ItemDownloaded })

	meta, warnings := p.aggregatePool(downloaded, sourceURL, bundleName, p.folder(job), p.extras(job))
	job.UpdateItem(itemIdx, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	res, err := p.client.PushImageFile(ctx, dest, meta, gallery)
	if err != nil {
		failItem(job, itemIdx, errorCode(err), err.Error())
		return
	}
	res.TagWarnings = slices.Concat(warnings, res.TagWarnings)
	recordSuccess(job, itemIdx, res, meta.PostFields)
	if p.books != nil {
		if err := p.books.MarkBookLanded(sourceURL); err != nil {
			logx.Warnf("queue: job %d recording a landed book: %v", job.ID, err)
		}
	}
}

func (p *Processor) aggregatePool(downloaded []gdl.Downloaded, poolURL, poolName, folder string, extras mapping.Extras) (monbooru.PushMeta, []string) {
	tagSet := map[string]bool{}
	var withheld, warnings []string
	strictest := ""
	site, category := "", ""
	for _, d := range downloaded {
		pf := p.mapper.Map(d.Meta)
		site = pf.Source
		category = kwdict.String(d.Meta, "category")
		strictest = mapping.Stricter(strictest, pf.Rating)
		withheld = append(withheld, pf.WithheldTags...)
		warnings = append(warnings, pf.UnknownTargets...)
		for _, tag := range pf.Tags {
			if !strings.HasPrefix(tag, "rating:") {
				tagSet[tag] = true
			}
		}
	}
	tags := slices.Collect(maps.Keys(tagSet))
	if strictest != "" {
		tags = append(tags, "rating:"+strictest)
	}
	slices.Sort(tags)
	slices.Sort(withheld)
	withheld = slices.Compact(withheld)
	slices.Sort(warnings)
	warnings = slices.Compact(warnings)
	bundle := p.withExtras(mapping.PushFields{PostFields: monbooru.PostFields{Tags: tags, Source: site}, Rating: strictest, Via: mapping.Via}, extras, category)

	name := cmp.Or(poolName, "pool")
	// No Collection: monbooru ingests a cbz as one manga, not a
	// collection member.
	return monbooru.PushMeta{
		PostFields: monbooru.PostFields{
			Tags:         bundle.Tags,
			WithheldTags: withheld,
			Source:       bundle.Source,
			URL:          poolURL,
			Commentary:   bundle.Commentary,
		},
		Filename:   name + ".cbz",
		Via:        bundle.Via,
		Folder:     folder,
		ClientTags: bundle.ClientTags,
	}, warnings
}

func (p *Processor) markUndownloaded(job *queue.Job, i int, dlErr error) {
	if dlErr != nil {
		failItem(job, i, errorCode(dlErr), dlErr.Error())
		return
	}
	skipItem(job, i, outcome.SkippedArchive, "")
}

// Skipped, not failed: a rule doing its job or a type monbooru cannot ingest
// leaves a clean job clean. A blacklist's note says which rule on which list,
// since that is what there is to go and edit.
func skipItem(job *queue.Job, i int, oc outcome.Outcome, note string) {
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemSkipped
		it.Outcome = oc
		it.MergeNote = note
	})
}

func (p *Processor) folder(job *queue.Job) string {
	return cmp.Or(job.Snapshot().Folder, p.cfg.Current().Downloader.DefaultFolder)
}

func (p *Processor) extras(job *queue.Job) mapping.Extras {
	snap := job.Snapshot()
	return mapping.Extras{
		Tags:       snap.Tags,
		Commentary: snap.Commentary,
		Source:     snap.Source,
		Via:        snap.Via,
	}
}

// The client's source applies only where no profile claims the label: the
// bare host of a direct link.
func (p *Processor) withExtras(pf mapping.PushFields, e mapping.Extras, labelFrom string) mapping.PushFields {
	if e.Empty() {
		return pf
	}
	return mapping.MergeExtras(pf, e, p.mapper.SourceClaimed(labelFrom))
}

func recordSuccess(job *queue.Job, i int, res *monbooru.Result, sent monbooru.PostFields) {
	job.UpdateItem(i, func(it *queue.Item) {
		it.Outcome = res.Outcome
		it.MonbooruID = res.MonbooruID
		it.TagWarnings = res.TagWarnings
		it.MergeNote = res.MergeNote
		it.TagsSent, it.Held = len(sent.Tags), heldTags(sent.WithheldTags, res.RejectedTags)
		if res.SHA256 != "" {
			it.SHA256 = res.SHA256
		}
		if res.Outcome == outcome.Created {
			it.Status = queue.ItemDone
		} else {
			it.Status = queue.ItemSkipped
		}
	})
}

// A book and its chapters bypass the archive, so a check would fetch them
// whole every time.
var errWatchManga = &outcome.CodedError{Code: outcome.ErrCodeUnsupportedURL, Msg: "only a manga listing can be watched: this link's books would download again on every check"}

// A book must assemble whole and a lone re-submitted post is a deliberate
// refresh, so both bypass the archive; a watch check of one post is not.
// Catching up assumes newest first, which a pool, a dispatcher's thread and
// an unprofiled site do not promise.
func (p *Processor) archiveMode(snap *queue.Job, resolved []gdl.Item, site string, cbz, deep bool) gdl.Archive {
	switch {
	case snap.Force || cbz:
		return gdl.ArchiveBypass
	case snap.Watch == 0 && len(resolved) == 1:
		return gdl.ArchiveBypass
	case snap.Watch != 0 && !deep && resolved[0].Subcategory != "pool" && p.isBooru(site):
		return gdl.ArchiveCatchUp
	}
	return gdl.ArchiveRead
}

func (p *Processor) isBooru(site string) bool {
	prof, ok := p.mapper.Lookup(site)
	return ok && (prof.Kind == "" || prof.Kind == mapping.KindBooru)
}

// A cancel kills the resolve, and its error must not stamp the job failed
// before the worker labels it canceled.
func abortJob(ctx context.Context, job *queue.Job, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	job.Fail(errorCode(err), err.Error(), time.Now())
	return err
}

func failItem(job *queue.Job, i int, code, msg string) {
	post := strconv.Itoa(i)
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemFailed
		it.Outcome = outcome.Failed
		it.ErrorCode = code
		it.Error = msg
		if it.PostID != "" {
			post = it.PostID
			if it.Num > 0 {
				post = fmt.Sprintf("%s#%d", it.PostID, it.Num)
			}
		}
	})
	logx.Warnf("queue: job %d item %s failed: %s", job.ID, post, msg)
}

// pages must already be in reading order.
func buildCBZFile(pages []string, dest string) (err error) {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	// A close error fails an otherwise clean build; after an earlier
	// error the close is only cleanup.
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	zw := zip.NewWriter(f)
	// Readers sort entry names alphabetically, so the padding must cover
	// the last page number.
	width := max(len(strconv.Itoa(len(pages))), 3)
	for i, p := range pages {
		fw, err := zw.Create(fmt.Sprintf("%0*d%s", width, i+1, filepath.Ext(p)))
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

func poolName(downloaded []gdl.Downloaded) string {
	for _, d := range downloaded {
		if name := mapping.PoolName(d.Meta); name != "" {
			return name
		}
	}
	for _, d := range downloaded {
		if title, ok := d.Meta["title"].(string); ok && title != "" {
			return title
		}
	}
	return ""
}

func bundleKey(resolved []gdl.Item) string {
	if len(resolved) > 0 && resolved[0].ID != "" {
		return "gallery:" + resolved[0].ID
	}
	return "gallery"
}

func orderedPages(downloaded []gdl.Downloaded) []string {
	ordered := slices.Clone(downloaded)
	// Chapter extractors number their pages as page, not num.
	ordinal := func(m map[string]any) int { return cmp.Or(kwdict.Num(m), kwdict.Int(m, "page")) }
	slices.SortStableFunc(ordered, func(a, b gdl.Downloaded) int {
		return cmp.Compare(ordinal(a.Meta), ordinal(b.Meta))
	})
	paths := make([]string, len(ordered))
	for i, d := range ordered {
		paths[i] = d.Path
	}
	return paths
}

// A fetch-status report carries the code in its state field already.
func bareMessage(err error) string {
	var ce *outcome.CodedError
	if errors.As(err, &ce) {
		return ce.Msg
	}
	return err.Error()
}

func errorCode(err error) string {
	var ce *outcome.CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return outcome.ErrCodeDownloadFailed
}
