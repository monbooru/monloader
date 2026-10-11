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
	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
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
}

func New(runner gdl.Runner, mapper *mapping.Mapper, client *monbooru.Client, cfg *config.Provider, workRoot string, siteState *sitestate.Tracker, ptr PTRService, sim SimilarityService) *Processor {
	return &Processor{runner: runner, mapper: mapper, client: client, cfg: cfg, workRoot: workRoot, siteState: siteState, ptr: ptr, sim: sim}
}

var (
	_ queue.Processor    = (*Processor)(nil)
	_ queue.DropReporter = (*Processor)(nil)
)

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

	switch snap.Kind {
	case queue.KindContrib:
		return p.processContrib(ctx, job, snap)
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

func (p *Processor) processDownload(ctx context.Context, job, snap *queue.Job) error {
	rng, limit := p.rangeFor(snap)
	res, err := p.runner.Resolve(ctx, snap.URL, rng, false)
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
		return nil
	}

	resolved := res.Items
	site := resolved[0].Category
	cbz := p.mapper.KindOf(site) == mapping.KindManga
	if cbz && snap.Watch != 0 {
		return abortJob(ctx, job, errWatchManga)
	}

	bundle := resolved[0].Subcategory == "pool" || cbz
	if bundle && limit > 0 && len(resolved) >= limit {
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
			resolved = whole.Items
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
	} else if limit > 0 && len(resolved) >= limit {
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d items (--range %s); re-submit with a higher range to fetch more", snap.ID, limit, rng)
	}

	return p.fetch(ctx, job, snap, resolved, site, cbz, false, rng)
}

func (p *Processor) processMetadata(ctx context.Context, job *queue.Job, snap *queue.Job) error {
	job.SetItems([]queue.Item{p.enrichItem(ctx, snap, snap.URL)})
	meta, err := p.runner.FetchMeta(ctx, snap.URL)
	if err != nil {
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.enrichFromMeta(ctx, job, snap, meta, snap.URL, kwdict.String(meta, "md5"), 0)
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

func (p *Processor) enrichFromMeta(ctx context.Context, job, snap *queue.Job, meta map[string]any, url, md5 string, sim float64) {
	job.SetSite(kwdict.String(meta, "category"))
	labelEnrichPost(job, meta)
	res, err := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, p.mapEnrichPayload(meta, url, md5, sim))
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		failItem(job, 0, errorCode(err), err.Error())
		return
	}
	p.markEnriched(job, snap, res, url)
}

// A nonzero sim marks the origin similarity-matched; 0 is an exact match
// or a refetch.
func (p *Processor) mapEnrichPayload(meta map[string]any, url, md5 string, sim float64) monbooru.EnrichPayload {
	return monbooru.EnrichPayload{
		PostFields: postFieldsFrom(p.mapper.Map(meta), url, kwdict.ID(meta)),
		SourceMD5:  md5,
		Verify:     true,
		Similarity: sim,
	}
}

func (p *Processor) markEnriched(job, snap *queue.Job, res *monbooru.Result, url string) {
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
		if it.URL == "" {
			it.URL = url
		}
	})
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

// Without the report monbooru's detail poll never gets an outcome. It
// carries the bare message: the code already travels in the state field.
func (p *Processor) failEnrichFetch(ctx context.Context, job, snap *queue.Job, err error) {
	// A cancel's kill error is not the fetch outcome; the worker labels
	// the item canceled.
	if ctx.Err() != nil {
		return
	}
	code := errorCode(err)
	msg := err.Error()
	var ce *outcome.CodedError
	if errors.As(err, &ce) {
		msg = ce.Msg
	}
	failItem(job, 0, code, err.Error())
	if rerr := p.client.ReportFetchOutcome(ctx, snap.ImageID, snap.Gallery, string(snap.Kind), snap.Backend, code, msg); rerr != nil {
		logx.Warnf("queue: job %d fetch-status report failed: %v", snap.ID, rerr)
	}
}

func (p *Processor) processDispatch(ctx context.Context, job, snap *queue.Job, res gdl.ResolveResult, rng string, limit int) error {
	if p.mapper.KindOf(res.Category) == mapping.KindManga {
		if snap.Watch != 0 {
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
		return nil
	}
	// --range does not bound the handoffs, so res.Queue is every child:
	// only one past this window's end leaves more to fetch.
	if limit > 0 && len(res.Queue) > snap.Offset+limit {
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d children (--chapter-range %s); re-submit with a higher range to fetch more", snap.ID, limit, rng)
	}
	// The site is the dispatcher, not the first leaf's image host.
	return p.fetch(ctx, job, snap, deep.Items, res.Category, false, true, rng)
}

func (p *Processor) fetch(ctx context.Context, job, snap *queue.Job, resolved []gdl.Item, site string, cbz, deep bool, rng string) error {
	gallery := p.beginJob(job, snap, site)

	// Published before the download so the rows show while it runs.
	job.SetItems(p.initialItems(resolved, cbz, snap.URL, snap.PageURL))

	workDir := filepath.Join(p.workRoot, fmt.Sprintf("job-%d", snap.ID))
	if mkErr := os.MkdirAll(workDir, 0o755); mkErr != nil {
		job.Fail(outcome.ErrCodeDownloadFailed, mkErr.Error(), time.Now())
		return mkErr
	}
	defer os.RemoveAll(workDir)

	var onFile func(int, gdl.Downloaded)
	if !cbz {
		rows := rowsByPost(resolved)
		onFile = func(_ int, d gdl.Downloaded) {
			if i, ok := rows[postKeyOf(d.Meta)]; ok {
				job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
			}
		}
	}

	downloaded, dlErr := p.runner.Download(ctx, snap.URL, rng, workDir, p.archiveMode(snap, resolved, site, cbz, deep), onFile, deep)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if w := firstWritten(downloaded); w != nil && p.mapper.SuspectFlattenedTags(w.Meta) {
		logx.Warnf("queue: job %d (%s) downloaded with no per-category tags; gallery-dl's tag fields may have changed, so every tag maps to general", snap.ID, site)
	}

	if cbz {
		p.processCBZ(ctx, job, 0, writtenOnly(downloaded), len(resolved), gallery, workDir, cmp.Or(dlErr, entryErr(downloaded)), snap.URL)
		return nil
	}
	entries := pairDownloads(resolved, downloaded)
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
	p.processItems(ctx, job, entries, gallery, snap.URL, snap.PageURL, dlErr)
	return nil
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

// A failed download prints no line, so stream positions drift; entries are
// matched to their rows by post instead, searching forward since both passes
// list in the same order. A nil slot is a row with no entry.
func pairDownloads(resolved []gdl.Item, downloaded []gdl.Downloaded) []*gdl.Downloaded {
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
		} else if next < len(resolved) {
			row = next
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

func firstWritten(downloaded []gdl.Downloaded) *gdl.Downloaded {
	if i := slices.IndexFunc(downloaded, written); i >= 0 {
		return &downloaded[i]
	}
	return nil
}

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

func (p *Processor) processItems(ctx context.Context, job *queue.Job, entries []*gdl.Downloaded, gallery, submittedURL, pageURL string, dlErr error) {
	folder, extras := p.folder(job), p.extras(job)
	for i, d := range entries {
		if ctx.Err() != nil {
			return // the worker marks the remaining pending items canceled
		}
		if d == nil {
			p.markUndownloaded(job, i, dlErr)
			continue
		}
		// gallery-dl's non-zero exit covers any one failed file, not the
		// posts its archive held.
		if d.Skipped {
			skipArchived(job, i)
			continue
		}
		if d.Err != "" {
			// A failure with its reason, never an archive skip, which
			// reads "already in your library".
			failItem(job, i, outcome.ErrCodeMappingFailed, d.Err)
			continue
		}
		if !gdl.Ingestable(d.Path) {
			skipUnsupported(job, i)
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
		meta := pushMetaFrom(pf, d.Path, p.itemURL(d.Meta, submittedURL, pageURL), kwdict.ID(d.Meta))
		meta.Collection, meta.CollectionOrder = pf.Collection, order
		meta.Via, meta.Folder = pf.Via, folder
		p.pushOne(ctx, job, i, d.Path, meta, gallery)
	}
}

func pushMetaFrom(pf mapping.PushFields, path, url, postID string) monbooru.PushMeta {
	return monbooru.PushMeta{
		PostFields: postFieldsFrom(pf, url, postID),
		Filename:   filepath.Base(path),
	}
}

// Shared by the push and the enrich, so a mapped field added here reaches both.
func postFieldsFrom(pf mapping.PushFields, url, postID string) monbooru.PostFields {
	return monbooru.PostFields{
		Tags:                      pf.Tags,
		Source:                    pf.Source,
		PostID:                    postID,
		URL:                       url,
		Commentary:                pf.Commentary,
		CommentaryTranslated:      pf.CommentaryTranslated,
		CommentaryDText:           pf.CommentaryDText,
		CommentaryTranslatedDText: pf.CommentaryTranslatedDText,
		Original:                  pf.Original,
		ParentURL:                 pf.ParentURL,
		Notes:                     toNoteBoxes(pf.Notes),
		PostWidth:                 pf.PostWidth,
		PostHeight:                pf.PostHeight,
		PostSize:                  pf.PostSize,
		PostExt:                   pf.PostExt,
	}
}

func toNoteBoxes(in []mapping.NoteBox) []monbooru.NoteBox {
	if len(in) == 0 {
		return nil
	}
	out := make([]monbooru.NoteBox, len(in))
	for i, n := range in {
		out[i] = monbooru.NoteBox{X: n.X, Y: n.Y, W: n.W, H: n.H, Body: n.Body, BodyHTML: n.BodyHTML}
	}
	return out
}

func (p *Processor) pushOne(ctx context.Context, job *queue.Job, i int, path string, meta monbooru.PushMeta, gallery string) {
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	res, err := p.client.PushImageFile(ctx, path, meta, gallery)
	if err != nil {
		failItem(job, i, errorCode(err), err.Error())
		return
	}
	recordSuccess(job, i, res)
	_ = os.Remove(path)
	_ = os.Remove(path + ".json")
}

// The full chapter list is known, so only a title with chapters past the
// window reads capped.
func (p *Processor) processChapters(ctx context.Context, job, snap *queue.Job, res gdl.ResolveResult, limit int) {
	gallery := p.beginJob(job, snap, res.Category)

	// --range bounds files, not a title's chapter handoffs, so the offset
	// is applied here; without it a fetch-all chain never ends.
	chapters := res.Queue[min(snap.Offset, len(res.Queue)):]
	capped := limit > 0 && len(chapters) > limit
	if capped {
		chapters = chapters[:limit]
	}
	job.SetItems(chapterItems(chapters))
	if capped {
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d chapters; re-submit with a higher range to fetch more", snap.ID, limit)
	}

	for i, ch := range chapters {
		if ctx.Err() != nil {
			return // the worker marks the remaining pending items canceled
		}
		chapterDir := filepath.Join(p.workRoot, fmt.Sprintf("job-%d-ch%d", snap.ID, i))
		p.importChapter(ctx, job, i, ch.URL, gallery, chapterDir)
		os.RemoveAll(chapterDir)
	}
}

// A chapter is one book, so the archive is bypassed and every page lands.
func (p *Processor) importChapter(ctx context.Context, job *queue.Job, i int, chapterURL, gallery, workDir string) {
	res, err := p.runner.Resolve(ctx, chapterURL, "", false)
	if err != nil {
		failItem(job, i, errorCode(err), err.Error())
		return
	}
	if ctx.Err() != nil {
		return
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
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (p *Processor) processCBZ(ctx context.Context, job *queue.Job, itemIdx int, downloaded []gdl.Downloaded, total int, gallery, workDir string, dlErr error, sourceURL string) {
	bundleName := sanitizeFilename(poolName(downloaded))

	if ctx.Err() != nil {
		return
	}

	pages := orderedPages(downloaded)
	if len(pages) == 0 {
		p.markUndownloaded(job, itemIdx, dlErr)
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

	meta := p.aggregatePool(downloaded, sourceURL, bundleName, p.folder(job), p.extras(job))
	job.UpdateItem(itemIdx, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	res, err := p.client.PushImageFile(ctx, dest, meta, gallery)
	if err != nil {
		failItem(job, itemIdx, errorCode(err), err.Error())
		return
	}
	recordSuccess(job, itemIdx, res)
}

// monbooru writes the file, maybe on Windows or an SMB share, so Windows' set
// is folded whatever OS monloader runs on.
func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	return strings.Trim(name, " .")
}

func (p *Processor) aggregatePool(downloaded []gdl.Downloaded, poolURL, poolName, folder string, extras mapping.Extras) monbooru.PushMeta {
	tagSet := map[string]bool{}
	strictest := ""
	site, category := "", ""
	for _, d := range downloaded {
		pf := p.mapper.Map(d.Meta)
		site = pf.Source
		category = kwdict.String(d.Meta, "category")
		strictest = mapping.Stricter(strictest, pf.Rating)
		for _, tag := range pf.Tags {
			if strings.HasPrefix(tag, "rating:") {
				continue
			}
			tagSet[tag] = true
		}
	}
	tags := slices.Collect(maps.Keys(tagSet))
	if strictest != "" {
		tags = append(tags, "rating:"+strictest)
	}
	slices.Sort(tags)
	bundle := p.withExtras(mapping.PushFields{Tags: tags, Source: site, Via: mapping.Via}, extras, category)

	name := cmp.Or(poolName, "pool")
	// No Collection: monbooru ingests a cbz as one manga, not a
	// collection member.
	return monbooru.PushMeta{
		PostFields: monbooru.PostFields{
			Tags:       bundle.Tags,
			Source:     bundle.Source,
			URL:        poolURL,
			Commentary: bundle.Commentary,
		},
		Filename: name + ".cbz",
		Via:      bundle.Via,
		Folder:   folder,
	}
}

func (p *Processor) markUndownloaded(job *queue.Job, i int, dlErr error) {
	if dlErr != nil {
		failItem(job, i, errorCode(dlErr), dlErr.Error())
		return
	}
	skipArchived(job, i)
}

func skipArchived(job *queue.Job, i int) {
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemSkipped
		it.Outcome = outcome.SkippedArchive
	})
}

// Skipped, not failed, so it does not drag an otherwise clean job to partial.
func skipUnsupported(job *queue.Job, i int) {
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemSkipped
		it.Outcome = outcome.SkippedUnsupported
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

func recordSuccess(job *queue.Job, i int, res *monbooru.Result) {
	job.UpdateItem(i, func(it *queue.Item) {
		it.Outcome = res.Outcome
		it.MonbooruID = res.MonbooruID
		it.TagWarnings = res.TagWarnings
		it.MergeNote = res.MergeNote
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
var errWatchManga = &outcome.CodedError{Code: outcome.ErrCodeUnsupportedURL, Msg: "a manga title cannot be watched: every check would download it again"}

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
	slices.SortStableFunc(ordered, func(a, b gdl.Downloaded) int {
		return cmp.Or(cmp.Compare(kwdict.Num(a.Meta), kwdict.Num(b.Meta)), cmp.Compare(a.Path, b.Path))
	})
	paths := make([]string, len(ordered))
	for i, d := range ordered {
		paths[i] = d.Path
	}
	return paths
}

func errorCode(err error) string {
	var ce *outcome.CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return outcome.ErrCodeDownloadFailed
}
