// Package pipeline wires the gallery-dl wrapper, the metadata mapper, and the
// monbooru client into the queue's Processor: it runs the full pipeline
// (resolve, download, map, push, clean up) for one job.
package pipeline

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/kwdict"
	"github.com/leqwin/monloader/internal/logx"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/monbooru"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/sitestate"
)

// Processor runs a job end to end and satisfies queue.Processor.
type Processor struct {
	runner    gdl.Runner
	mapper    *mapping.Mapper
	client    *monbooru.Client
	cfg       *config.Provider
	workRoot  string
	siteState *sitestate.Tracker
}

// New builds a Processor. workRoot is the parent of the per-job scratch
// directories (the ephemeral /work mount in the container); siteState records
// a successful resolve so the settings page can show when each site was last
// reached.
func New(runner gdl.Runner, mapper *mapping.Mapper, client *monbooru.Client, cfg *config.Provider, workRoot string, siteState *sitestate.Tracker) *Processor {
	return &Processor{runner: runner, mapper: mapper, client: client, cfg: cfg, workRoot: workRoot, siteState: siteState}
}

var _ queue.Processor = (*Processor)(nil)

// Process resolves the URL, downloads the files, maps each onto monbooru push
// fields, pushes them, and records per-item outcomes. It returns
// an error only for a job-level abort (a failed resolve); per-item failures
// live on the items so the job can still partially succeed.
func (p *Processor) Process(ctx context.Context, job *queue.Job) error {
	snap := job.Snapshot()

	if snap.Kind == queue.KindMetadata {
		return p.processMetadata(ctx, job, snap)
	}

	rng, limit := p.rangeFor(snap)
	res, err := p.runner.Resolve(ctx, snap.URL, rng, false)
	if err != nil {
		code := errorCode(err)
		job.Fail(code, err.Error(), time.Now())
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(res.Items) == 0 {
		// No downloadable posts. A dispatcher (a forum thread, a manga title)
		// hands off to child extractors via Message.Queue, which the -j pass
		// lists but does not follow; route those before calling it empty.
		if len(res.Queue) > 0 {
			return p.processDispatch(ctx, job, snap, res, rng, limit)
		}
		// Nothing matched: a clean, empty success.
		return nil
	}

	resolved := res.Items
	site := resolved[0].Category
	// A manga/comic gallery bundles its pages into one cbz for the reader; a
	// booru pool's pages push as an ordered collection (through processItems).
	cbz := p.mapper.KindOf(site) == mapping.KindManga

	bundle := resolved[0].Subcategory == "pool" || cbz
	if bundle && limit > 0 && len(resolved) >= limit {
		// A booru pool or a manga gallery is one work the user asked for as a
		// unit, so the per-job cap - which exists to bound an open-ended search -
		// must not truncate it. Re-resolve and download the whole thing.
		whole, rerr := p.runner.Resolve(ctx, snap.URL, "", false)
		if rerr != nil {
			job.Fail(errorCode(rerr), rerr.Error(), time.Now())
			return rerr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resolved = whole.Items
		rng = ""
	} else if limit > 0 && len(resolved) >= limit {
		// A resolve that returned the full cap likely truncated a larger source,
		// so flag and log it; the row and the API then say the import was capped
		// rather than letting it look complete.
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d items (--range %s); re-submit with a higher range to fetch more", snap.ID, limit, rng)
	}

	return p.fetch(ctx, job, snap, resolved, site, cbz, false, rng)
}

// processMetadata handles a metadata-only source refetch: it re-reads the job's
// URL for tags / commentary / notes (no file download) and enriches the
// monbooru image the job targets. A changed upstream file comes back as a
// failed item coded hash_mismatch; a rejected or unreachable source fails the
// same way. The job carries one item so the queue shows a row.
func (p *Processor) processMetadata(ctx context.Context, job *queue.Job, snap *queue.Job) error {
	job.SetItems([]queue.Item{{URL: snap.URL, Status: queue.ItemPending}})
	meta, err := p.runner.FetchMeta(ctx, snap.URL)
	if err != nil {
		code := errorCode(err)
		failItem(job, 0, code, err.Error())
		// A fetch that never reaches enrich leaves monbooru's detail poll with
		// no outcome; report it so the pill stops instead of spinning.
		if rerr := p.client.ReportFetchOutcome(ctx, snap.ImageID, snap.Gallery, code, err.Error()); rerr != nil {
			logx.Warnf("queue: job %d fetch-status report failed: %v", snap.ID, rerr)
		}
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Label the row's site like a download job's, even though nothing is fetched.
	job.SetSite(kwdict.String(meta, "category"))
	pf := p.mapper.Map(meta)
	payload := monbooru.EnrichPayload{
		Tags:       pf.Tags,
		Source:     pf.Source,
		URL:        snap.URL,
		SourceMD5:  kwdict.String(meta, "md5"),
		Verify:     true,
		Commentary: pf.Commentary,
		Notes:      toNoteBoxes(pf.Notes),
	}
	res, err := p.client.EnrichImage(ctx, snap.ImageID, snap.Gallery, payload)
	if err != nil {
		failItem(job, 0, errorCode(err), err.Error())
		return nil
	}
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = queue.OutcomeEnriched
		it.MonbooruID = snap.ImageID
		it.MergeNote = res.MergeNote
	})
	return nil
}

// processDispatch handles a URL that resolved to Message.Queue handoffs instead
// of files. A manga/comic title is a series the cbz path cannot bundle as one
// book; everything else (a forum thread, an archive board) re-resolves deep so
// its externally-hosted files come down as loose items.
func (p *Processor) processDispatch(ctx context.Context, job, snap *queue.Job, res gdl.ResolveResult, rng string, limit int) error {
	if p.mapper.KindOf(res.Category) == mapping.KindManga {
		// A manga/comic title lists its chapters; import each as its own cbz.
		p.processChapters(ctx, job, snap, res, limit)
		return nil
	}
	// -J follows the handoffs into their files; --chapter-range (carried by rng)
	// bounds the child window so an open thread or board is not unbounded.
	deep, err := p.runner.Resolve(ctx, snap.URL, rng, true)
	if err != nil {
		job.Fail(errorCode(err), err.Error(), time.Now())
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(deep.Items) == 0 {
		return nil
	}
	if limit > 0 && len(res.Queue) >= limit {
		job.SetCapped(limit)
		logx.Infof("queue: job %d capped to the first %d children (--chapter-range %s); re-submit with a higher range to fetch more", snap.ID, limit, rng)
	}
	// The job's site is the dispatcher itself (the forum), not the first image
	// host its leaves resolved to; each item still maps by its own metadata.
	return p.fetch(ctx, job, snap, deep.Items, res.Category, false, true, rng)
}

// fetch downloads the resolved posts and pushes them: a manga/comic gallery as
// one cbz, everything else as loose items. deep marks a dispatcher whose
// children were resolved with -J, so the download follows the same handoffs and
// bounds the child window with --chapter-range.
func (p *Processor) fetch(ctx context.Context, job, snap *queue.Job, resolved []gdl.Item, site string, cbz, deep bool, rng string) error {
	gallery := p.beginJob(job, snap, site)

	// Publish the resolved items before the download so the queue shows the
	// job's size and per-item rows right away, rather than nothing until the
	// whole (slow) download completes.
	job.SetItems(p.initialItems(resolved, cbz, snap.URL))

	workDir := filepath.Join(p.workRoot, fmt.Sprintf("job-%d", snap.ID))
	if mkErr := os.MkdirAll(workDir, 0o755); mkErr != nil {
		job.Fail(queue.ErrCodeDownloadFailed, mkErr.Error(), time.Now())
		return mkErr
	}
	defer os.RemoveAll(workDir)

	// Advance each item to downloaded the moment its file lands so the queue
	// shows progress through a long download. The download reports results in
	// source order, so the index is the item's row. A cbz is one bundle, so there
	// is nothing to stream.
	var onFile func(int, gdl.Downloaded)
	if !cbz {
		onFile = func(i int, _ gdl.Downloaded) {
			job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		}
	}

	// A cbz bundle bypasses the gallery-dl archive so every page is fetched into
	// /work and the book always assembles complete, never short from a prior run
	// having recorded some pages in the archive. A single resolved post bypasses
	// it too: re-submitting one post is a deliberate refresh, so re-download and
	// re-push to let monbooru merge any new tags instead of an archive skip that
	// changes nothing. A bulk search keeps the archive, so a re-run stays cheap.
	downloaded, dlErr := p.runner.Download(ctx, snap.URL, rng, workDir, snap.Force || cbz || len(resolved) == 1, onFile, deep)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A clean run prints one line per resolved post (a written file or an archive
	// skip), so the two passes stay position-aligned. A shorter stream means a
	// mid-batch failure dropped lines, which shifts per-row outcomes past the gap;
	// log it so the divergence is visible (counts and pushed files stay correct).
	if len(downloaded) != len(resolved) {
		logx.Warnf("queue: job %d (%s) downloaded %d entries for %d resolved posts; per-row outcomes past the gap may be mislabeled", snap.ID, site, len(downloaded), len(resolved))
	}
	// Per-category tags ride the download sidecars; checking the first written
	// file catches a gallery-dl tag-field change that would flatten every tag to
	// general.
	if w := firstWritten(downloaded); w != nil && p.mapper.SuspectFlattenedTags(w.Meta) {
		logx.Warnf("queue: job %d (%s) downloaded with no per-category tags; gallery-dl's tag fields may have changed, so every tag maps to general", snap.ID, site)
	}

	if cbz {
		p.processCBZ(ctx, job, 0, writtenOnly(downloaded), len(resolved), gallery, workDir, dlErr, snap.URL)
		return nil
	}
	p.processItems(ctx, job, downloaded, len(resolved), gallery, snap.URL, dlErr)
	return nil
}

// beginJob stamps a job with the site its resolve discovered and the effective
// monbooru gallery (per-job or per-site setting, else the default), returning
// the gallery. A successful resolve means the source was reached and returned
// posts, so it also feeds the settings "last reached" indicator.
func (p *Processor) beginJob(job, snap *queue.Job, site string) string {
	job.SetSite(site)
	p.siteState.Reached(site, time.Now())
	gallery := snap.Gallery
	if gallery == "" {
		gallery = p.mapper.Gallery(site)
	}
	job.SetGallery(gallery)
	return gallery
}

// firstWritten returns the first written file in the download results, or nil
// when they are all archive skips.
func firstWritten(downloaded []gdl.Downloaded) *gdl.Downloaded {
	for i := range downloaded {
		if !downloaded[i].Skipped {
			return &downloaded[i]
		}
	}
	return nil
}

// writtenOnly drops the archive-skip entries, leaving the files that landed.
func writtenOnly(downloaded []gdl.Downloaded) []gdl.Downloaded {
	out := downloaded[:0:0]
	for _, d := range downloaded {
		if !d.Skipped {
			out = append(out, d)
		}
	}
	return out
}

// initialItems is the pending item list published right after resolve: one
// item per resolved post, or a single bundle item when the job is pushed as
// one cbz.
func (p *Processor) initialItems(resolved []gdl.Item, cbz bool, submittedURL string) []queue.Item {
	if cbz {
		return []queue.Item{{PostID: bundleKey(resolved), Status: queue.ItemPending}}
	}
	items := make([]queue.Item, len(resolved))
	for i, it := range resolved {
		items[i] = queue.Item{PostID: it.ID, Num: it.Num, URL: p.itemURL(it.Meta, submittedURL), Status: queue.ItemPending}
	}
	return items
}

// itemURL is an item's source link: the per-site post URL, or the submitted
// page URL when the source has no template. A directlink uses the submitted URL
// directly, since gallery-dl may rewrite the sidecar's extension after download.
func (p *Processor) itemURL(meta map[string]any, submittedURL string) string {
	if kwdict.String(meta, "category") == mapping.CategoryDirectlink {
		return submittedURL
	}
	if u := p.mapper.PostURL(meta); u != "" {
		return u
	}
	return submittedURL
}

// rangeFor computes the --range value enforcing the per-job item cap and
// returns the effective limit (0 = no cap). Offset shifts the window so a
// continued job fetches the posts after a prior cap. A per-job max_items can
// only lower the cap, never raise it past the configured ceiling that bounds
// one job's footprint.
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

// processItems handles single posts and the pool-as-loose-collection mode:
// each post is mapped and pushed on its own, carrying its collection label and
// order when it came from a pool.
func (p *Processor) processItems(ctx context.Context, job *queue.Job, downloaded []gdl.Downloaded, total int, gallery, submittedURL string, dlErr error) {
	folder := p.folder(job)
	for i := 0; i < total; i++ {
		if ctx.Err() != nil {
			return // the worker marks the remaining pending items canceled
		}
		if i >= len(downloaded) || downloaded[i].Skipped {
			p.markUndownloaded(job, i, dlErr)
			continue
		}
		d := downloaded[i]
		if !gdl.Ingestable(d.Path) {
			skipUnsupported(job, i)
			continue
		}
		pf := p.mapper.Map(d.Meta)
		// A pool with no num orders by source position.
		order := pf.CollectionOrder
		if pf.Collection != "" && order == 0 {
			order = i + 1
		}
		meta := monbooru.PushMeta{
			Filename:        filepath.Base(d.Path),
			Tags:            pf.Tags,
			Source:          pf.Source,
			URL:             p.itemURL(d.Meta, submittedURL),
			Collection:      pf.Collection,
			CollectionOrder: order,
			Via:             pf.Via,
			Folder:          folder,
			Commentary:      pf.Commentary,
			Notes:           toNoteBoxes(pf.Notes),
		}
		p.pushOne(ctx, job, i, d.Path, meta, gallery)
	}
}

// toNoteBoxes converts the mapper's note boxes into the push client's shape.
// A pool bundle carries no per-post commentary or notes, so aggregatePool omits
// them.
func toNoteBoxes(in []mapping.NoteBox) []monbooru.NoteBox {
	if len(in) == 0 {
		return nil
	}
	out := make([]monbooru.NoteBox, len(in))
	for i, n := range in {
		out[i] = monbooru.NoteBox{X: n.X, Y: n.Y, W: n.W, H: n.H, Body: n.Body}
	}
	return out
}

// pushOne reads, pushes, and records the outcome of a single downloaded file.
func (p *Processor) pushOne(ctx context.Context, job *queue.Job, i int, path string, meta monbooru.PushMeta, gallery string) {
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
	job.UpdateItem(i, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	res, err := p.client.PushImageFile(ctx, path, meta, gallery)
	if err != nil {
		failItem(job, i, errorCode(err), err.Error())
		return
	}
	recordSuccess(job, i, res)
	// On a successful push the scratch file is no longer needed.
	_ = os.Remove(path)
	_ = os.Remove(path + ".json")
}

// processChapters imports a manga/comic title (series) URL: each queued chapter
// is resolved, downloaded, and bundled into its own cbz pushed as its own manga
// (the single-gallery cbz path, run once per chapter). The job cap bounds the
// chapter count; the full chapter list is known, so only an over-cap title is
// flagged capped.
func (p *Processor) processChapters(ctx context.Context, job, snap *queue.Job, res gdl.ResolveResult, limit int) {
	gallery := p.beginJob(job, snap, res.Category)

	chapters := res.Queue
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

// importChapter resolves one chapter URL, downloads its pages, and pushes the
// assembled cbz as item i. A chapter is one book, so the archive is bypassed
// (every page lands) and a short or failed download fails the item rather than
// pushing a truncated archive.
func (p *Processor) importChapter(ctx context.Context, job *queue.Job, i int, chapterURL, gallery, workDir string) {
	res, err := p.runner.Resolve(ctx, chapterURL, "", false)
	if err != nil {
		failItem(job, i, errorCode(err), err.Error())
		return
	}
	if ctx.Err() != nil {
		return
	}
	downloaded, dlErr := p.runner.Download(ctx, chapterURL, "", workDir, true, nil, false)
	if ctx.Err() != nil {
		return
	}
	p.processCBZ(ctx, job, i, writtenOnly(downloaded), len(res.Items), gallery, workDir, dlErr, chapterURL)
}

// chapterItems is the pending item list for a manga title: one bundle item per
// queued chapter, linking back to the chapter URL.
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

// chapterLabel is the chapter slug (the last path segment of its URL): a short,
// stable label for a chapter whose metadata carries no id, so the queue row is
// not the whole URL.
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

// processCBZ bundles a manga/comic gallery's pages into one .cbz and pushes it as
// a single archive: union tags, strictest rating, the gallery name as filename.
// The book must be whole, so a download error or a short page count fails the
// item rather than pushing a truncated archive. The .cbz is built to a scratch
// file and streamed to monbooru so a large gallery is never buffered in memory.
func (p *Processor) processCBZ(ctx context.Context, job *queue.Job, itemIdx int, downloaded []gdl.Downloaded, total int, gallery, workDir string, dlErr error, sourceURL string) {
	bundleName := poolName(downloaded)

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
		failItem(job, itemIdx, queue.ErrCodeDownloadFailed, fmt.Sprintf("bundled %d of %d pages", len(pages), total))
		return
	}

	dest := filepath.Join(workDir, "bundle.cbz")
	if err := buildCBZFile(pages, dest); err != nil {
		failItem(job, itemIdx, queue.ErrCodeMappingFailed, err.Error())
		return
	}
	job.UpdateItem(itemIdx, func(it *queue.Item) { it.Status = queue.ItemDownloaded })

	meta := p.aggregatePool(downloaded, sourceURL, bundleName, p.folder(job))
	job.UpdateItem(itemIdx, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	res, err := p.client.PushImageFile(ctx, dest, meta, gallery)
	if err != nil {
		failItem(job, itemIdx, errorCode(err), err.Error())
		return
	}
	recordSuccess(job, itemIdx, res)
}

// aggregatePool merges the bundle's pages into one push: union of non-rating
// tags, strictest rating, the bundle name as collection, and the submitted URL.
func (p *Processor) aggregatePool(downloaded []gdl.Downloaded, poolURL, poolName, folder string) monbooru.PushMeta {
	tagSet := map[string]bool{}
	strictest := ""
	site := ""
	for _, d := range downloaded {
		pf := p.mapper.Map(d.Meta)
		site = pf.Source
		strictest = mapping.Stricter(strictest, pf.Rating)
		for _, tag := range pf.Tags {
			if strings.HasPrefix(tag, "rating:") {
				continue
			}
			tagSet[tag] = true
		}
	}
	tags := make([]string, 0, len(tagSet)+1)
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	if strictest != "" {
		tags = append(tags, "rating:"+strictest)
	}
	sort.Strings(tags)

	name := poolName
	if name == "" {
		name = "pool"
	}
	// No Collection: a cbz is one archive monbooru ingests as a single manga,
	// so it must not be grouped into a collection (that is the collection
	// pool-mode's job, where each page is pushed separately).
	return monbooru.PushMeta{
		Filename: filepath.Base(name) + ".cbz",
		Tags:     tags,
		Source:   site,
		URL:      poolURL,
		Via:      mapping.Via,
		Folder:   folder,
	}
}

// markUndownloaded records a resolved item the download pass did not write:
// failed (with the download error's code) when the download errored, else
// skipped_archive.
func (p *Processor) markUndownloaded(job *queue.Job, i int, dlErr error) {
	if dlErr != nil {
		failItem(job, i, errorCode(dlErr), dlErr.Error())
		return
	}
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemSkipped
		it.Outcome = queue.OutcomeSkippedArchive
	})
}

// skipUnsupported records a downloaded file whose type monbooru cannot ingest:
// skipped, not failed, so it does not drag an otherwise-clean job to partial.
func skipUnsupported(job *queue.Job, i int) {
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemSkipped
		it.Outcome = queue.OutcomeSkippedUnsupported
	})
}

func (p *Processor) folder(job *queue.Job) string {
	if f := job.Snapshot().Folder; f != "" {
		return f
	}
	return p.cfg.Current().Downloader.DefaultFolder
}

// recordSuccess walks an item to its terminal state from the push result:
// created -> done, duplicate -> skipped.
func recordSuccess(job *queue.Job, i int, res *monbooru.Result) {
	job.UpdateItem(i, func(it *queue.Item) {
		it.Outcome = res.Outcome
		it.MonbooruID = res.MonbooruID
		it.TagWarnings = res.TagWarnings
		it.MergeNote = res.MergeNote
		if res.SHA256 != "" {
			it.SHA256 = res.SHA256
		}
		if res.Outcome == queue.OutcomeCreated {
			it.Status = queue.ItemDone
		} else {
			it.Status = queue.ItemSkipped
		}
	})
}

func failItem(job *queue.Job, i int, code, msg string) {
	post := strconv.Itoa(i)
	job.UpdateItem(i, func(it *queue.Item) {
		it.Status = queue.ItemFailed
		it.Outcome = queue.OutcomeFailed
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

// buildCBZFile writes the ordered page files into a zip at dest (the .cbz
// monbooru ingests as manga), streaming each page so the archive is never held
// whole in memory. Pages arrive in reading order.
func buildCBZFile(pages []string, dest string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for i, p := range pages {
		fw, err := zw.Create(fmt.Sprintf("%03d%s", i+1, filepath.Ext(p)))
		if err != nil {
			f.Close()
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			f.Close()
			return err
		}
		_, err = io.Copy(fw, src)
		src.Close()
		if err != nil {
			f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// poolName reads the bundle's display name from the first page that carries it:
// a booru pool's name, else a manga gallery's title.
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

// bundleKey is the stable item id for a manga/comic gallery's single cbz bundle
// item: the gallery's shared post id.
func bundleKey(resolved []gdl.Item) string {
	if len(resolved) > 0 && resolved[0].ID != "" {
		return "gallery:" + resolved[0].ID
	}
	return "gallery"
}

// orderedPages returns the downloaded files' paths in reading order: by the
// per-file ordinal (`num`, or `no` on the sites that use it), then filename. A
// pool or manga gallery thus bundles in page order regardless of the order the
// files were written.
func orderedPages(downloaded []gdl.Downloaded) []string {
	ordered := make([]gdl.Downloaded, len(downloaded))
	copy(ordered, downloaded)
	sort.SliceStable(ordered, func(i, j int) bool {
		ni, nj := kwdict.Num(ordered[i].Meta), kwdict.Num(ordered[j].Meta)
		if ni != nj {
			return ni < nj
		}
		return ordered[i].Path < ordered[j].Path
	})
	paths := make([]string, len(ordered))
	for i, d := range ordered {
		paths[i] = d.Path
	}
	return paths
}

// errorCode pulls the stable error code out of a coded gdl/monbooru error,
// defaulting to download_failed.
func errorCode(err error) string {
	var ce *queue.CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return queue.ErrCodeDownloadFailed
}
