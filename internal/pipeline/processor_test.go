package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/monbooru"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/sitestate"
)

// fakeRunner is a gdl.Runner that returns canned resolve results and writes a
// chosen subset of files on download, simulating archive skips and errors.
type fakeRunner struct {
	resolved   []gdl.Item
	resolveErr error
	writeIdx   []int
	dlErr      error
	ext        string
	// extByIdx overrides the written file's extension per resolved index, modeling
	// a page that mixes media types (an image plus an audio clip); unset falls back
	// to ext.
	extByIdx map[int]string
	// queue, category, and deepItems model a dispatcher URL: the shallow -j pass
	// returns queue handoffs under category; a deep -J re-resolve returns
	// deepItems (the leaf files), which the download then writes.
	queue          []gdl.QueueItem
	category       string
	deepItems      []gdl.Item
	gotResolveDeep bool
	// chapterPages models a manga title's per-chapter import: a resolve or
	// download of a chapter URL (a Queue handoff) returns/writes that chapter's
	// pages, keyed by the chapter URL.
	chapterPages map[string][]gdl.Item
	// shortStream, when >0, ends the download stream after that many entries,
	// modeling a mid-batch failure that prints fewer lines than were resolved.
	shortStream int
	// dlExt, when set, overrides the `extension` in each downloaded item's
	// sidecar metadata, modeling gallery-dl rewriting it to the real content
	// type after download so it can differ from the resolved item.
	dlExt string

	blockDownload   bool
	downloadStarted chan struct{}
	// liveIdx are resolved indices reported via onFile before blockDownload
	// blocks, modeling files that have landed while the download is still in
	// flight (so a test can observe items advancing mid-download).
	liveIdx []int

	gotForce bool   // records the force arg of the last Download call
	gotRange string // records the rng arg of the last Resolve call

	// fetchMeta / fetchErr back FetchMeta (the metadata-only source refetch).
	fetchMeta map[string]any
	fetchErr  error
	// fetchByURL routes FetchMeta by exact URL (the lookup walk hits one URL
	// per site); a URL not in the map falls back to fetchMeta / fetchErr.
	// gotFetchURLs records the calls so a test can assert the walk order.
	fetchByURL   map[string]fetchResp
	gotFetchURLs []string
}

type fetchResp struct {
	meta map[string]any
	err  error
}

func (f *fakeRunner) Resolve(ctx context.Context, url, rng string, deep bool) (gdl.ResolveResult, error) {
	if deep {
		f.gotResolveDeep = true
		return gdl.ResolveResult{Items: capItems(f.deepItems, rng), Category: f.category}, nil
	}
	if pages, ok := f.chapterPages[url]; ok {
		return gdl.ResolveResult{Items: pages}, nil
	}
	f.gotRange = rng
	if f.resolveErr != nil {
		return gdl.ResolveResult{}, f.resolveErr
	}
	cat := f.category
	if cat == "" && len(f.resolved) > 0 {
		cat = f.resolved[0].Category
	}
	return gdl.ResolveResult{Items: capItems(f.resolved, rng), Queue: f.queue, Category: cat}, nil
}

func (f *fakeRunner) FetchMeta(ctx context.Context, url string) (map[string]any, error) {
	f.gotFetchURLs = append(f.gotFetchURLs, url)
	if r, ok := f.fetchByURL[url]; ok {
		return r.meta, r.err
	}
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.fetchMeta, nil
}

func (f *fakeRunner) Download(ctx context.Context, url, rng, workDir string, force bool, onFile func(int, gdl.Downloaded), deep bool) ([]gdl.Downloaded, error) {
	f.gotForce = force
	// The real Download creates its work dir; honor that so callers that do not
	// pre-make it (per-chapter import) write into an existing dir.
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	if pages, ok := f.chapterPages[url]; ok {
		out := make([]gdl.Downloaded, len(pages))
		for i, it := range pages {
			name := it.ID + "-" + strconv.Itoa(it.Num)
			path := filepath.Join(workDir, name+".jpg")
			if err := os.WriteFile(path, []byte("bytes-"+name), 0o644); err != nil {
				return nil, err
			}
			out[i] = gdl.Downloaded{Path: path, Meta: it.Meta}
		}
		return out, nil
	}
	// A deep download follows the dispatcher's handoffs, so its files are the
	// deep-resolve leaves, not the shallow resolved list.
	src := f.resolved
	if deep {
		src = f.deepItems
	}
	if f.blockDownload {
		for _, i := range f.liveIdx {
			if onFile != nil {
				it := src[i]
				onFile(i, gdl.Downloaded{Meta: it.Meta})
			}
		}
		if f.downloadStarted != nil {
			close(f.downloadStarted)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ext := f.ext
	if ext == "" {
		ext = ".jpg"
	}
	write := map[int]bool{}
	for _, i := range f.writeIdx {
		write[i] = true
	}
	// One result per resolved item in source order: a written file or an archive
	// skip, as the real download reports them; the range bounds the set, as the
	// real download honors --range.
	items := capItems(src, rng)
	out := make([]gdl.Downloaded, len(items))
	for i, it := range items {
		if f.shortStream > 0 && i >= f.shortStream {
			break // the stream ends early, as a mid-batch failure truncates it
		}
		if !write[i] {
			out[i] = gdl.Downloaded{Skipped: true}
			continue
		}
		// Page-numbered names so a gallery whose pages share one id still yields
		// distinct files on disk, as real gallery-dl writes them.
		name := it.ID
		if it.Num > 0 {
			name = it.ID + "-" + strconv.Itoa(it.Num)
		}
		fileExt := ext
		if e, ok := f.extByIdx[i]; ok {
			fileExt = e
		}
		path := filepath.Join(workDir, name+fileExt)
		if err := os.WriteFile(path, []byte("bytes-"+name), 0o644); err != nil {
			return nil, err
		}
		meta := it.Meta
		if f.dlExt != "" {
			meta = maps.Clone(it.Meta)
			meta["extension"] = f.dlExt
		}
		d := gdl.Downloaded{Path: path, Meta: meta}
		if onFile != nil {
			onFile(i, d)
		}
		out[i] = d
	}
	if f.shortStream > 0 && f.shortStream < len(out) {
		out = out[:f.shortStream]
	}
	return out, f.dlErr
}

func (f *fakeRunner) ListExtractors(ctx context.Context) ([]gdl.Extractor, error) { return nil, nil }
func (f *fakeRunner) Probe(ctx context.Context, url string) (gdl.ProbeResult, error) {
	return gdl.ProbeResult{}, nil
}
func (f *fakeRunner) Version(ctx context.Context) string { return "fake" }

// rangeUpper parses a "1-N" range to N; 0 means unbounded (an empty range).
func rangeUpper(rng string) int {
	_, hi, ok := strings.Cut(rng, "-")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(hi)
	return n
}

// capItems truncates items to the upper bound of a gallery-dl --range, modeling
// how the real resolve and download honor the cap.
func capItems(items []gdl.Item, rng string) []gdl.Item {
	if n := rangeUpper(rng); n > 0 && n < len(items) {
		return items[:n]
	}
	return items
}

func danbooruPost(id string) gdl.Item {
	return gdl.Item{
		Category: "danbooru", Subcategory: "post", ID: id,
		Meta: map[string]any{
			"category": "danbooru", "id": id, "subcategory": "post", "rating": "g",
			"tags_general": []any{"tag_" + id},
		},
	}
}

func poolPost(id string, num int, rating string) gdl.Item {
	return gdl.Item{
		Category: "danbooru", Subcategory: "pool", ID: id, Num: num,
		Meta: map[string]any{
			"category": "danbooru", "id": id, "subcategory": "pool", "rating": rating, "num": float64(num),
			"pool":         map[string]any{"id": float64(29906), "name": "A Quiet Afternoon"},
			"tags_general": []any{"page_" + id, "comic"},
		},
	}
}

// mangaPage models one page of a manga gallery: every page shares the gallery
// id and differs only by num, the shape that collapsed to a single page.
func mangaPage(galleryID string, num int) gdl.Item {
	return gdl.Item{
		Category: "nhentai", Subcategory: "gallery", ID: galleryID, Num: num,
		Meta: map[string]any{
			"category": "nhentai", "id": galleryID, "subcategory": "gallery",
			"num": float64(num), "title": "Test Doujin",
			"tags": []any{"tag_" + strconv.Itoa(num)},
		},
	}
}

// multiAssetPost models one file of a multi-asset post: every file shares the
// post id and differs only by num, the shape that collapsed to a single file.
func multiAssetPost(id string, num int) gdl.Item {
	return gdl.Item{
		Category: "artstation", Subcategory: "artwork", ID: id, Num: num,
		Meta: map[string]any{
			"category": "artstation", "id": id, "subcategory": "artwork",
			"num": float64(num), "tags": []any{"art"},
		},
	}
}

// moebooruPoolPost models one post of a moebooru pool: `pool` is a bare id with
// no name and the post carries no num, the shape collection mode dropped.
func moebooruPoolPost(id string) gdl.Item {
	return gdl.Item{
		Category: "yandere", Subcategory: "pool", ID: id,
		Meta: map[string]any{
			"category": "yandere", "id": id, "subcategory": "pool",
			"rating": "s", "pool": float64(12), "tags_general": []any{"comic"},
		},
	}
}

// directlinkItem models a bare media URL resolved through gallery-dl's
// directlink pseudo-extractor: no booru, no id, just the file's host and the
// path parts the canonical URL is rebuilt from.
func directlinkItem() gdl.Item {
	return gdl.Item{
		Category: "directlink", Subcategory: "example.com",
		Meta: map[string]any{
			"category": "directlink", "subcategory": "example.com",
			"domain": "img.example.com", "path": "art/2024",
			"filename": "picture", "extension": "jpg",
		},
	}
}

func testEnv(t *testing.T, fake gdl.Runner, handler http.HandlerFunc) (*queue.Queue, func()) {
	t.Helper()
	return lookupEnvCfg(t, fake, handler, config.Default(), nil, nil)
}

func waitJob(t *testing.T, q *queue.Queue, id int64) *queue.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	job, err := q.Wait(ctx, id)
	if err != nil {
		t.Fatalf("waiting for job %d: %v", id, err)
	}
	return job
}

func TestPipelineMetadataEnriches(t *testing.T) {
	meta := danbooruPost("100001").Meta
	meta["md5"] = "abc123def456"
	fake := &fakeRunner{fetchMeta: meta}

	var gotPath, gotBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"merge": map[string]any{"tags_added": 2}, "verified": true})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://danbooru.donmai.us/posts/100001"))

	if job.Status != queue.JobSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("items = %+v, want one enriched", job.Items)
	}
	if job.Summary.Enriched != 1 || job.Summary.Total != 1 {
		t.Errorf("summary = %+v, want 1 enriched / 1 total", job.Summary)
	}
	if job.Site != "danbooru" {
		t.Errorf("site = %q, want the refetched post's category", job.Site)
	}
	if gotPath != "/api/v1/images/42/enrich?gallery=art" {
		t.Errorf("enrich path = %q, want /api/v1/images/42/enrich?gallery=art", gotPath)
	}
	if !strings.Contains(gotBody, `"verify":true`) ||
		!strings.Contains(gotBody, `"source_md5":"abc123def456"`) ||
		!strings.Contains(gotBody, "tag_100001") {
		t.Errorf("enrich body wrong: %s", gotBody)
	}
}

func TestPipelineMetadataHashMismatchFails(t *testing.T) {
	fake := &fakeRunner{fetchMeta: danbooruPost("100002").Meta}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "changed", "code": "hash_mismatch"})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueMetadata(7, "art", "https://danbooru.donmai.us/posts/100002"))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != queue.ErrCodeHashMismatch {
		t.Fatalf("items = %+v, want one failed with hash_mismatch", job.Items)
	}
}

// A fetch that fails before enrich (unsupported URL, timeout) is reported to
// monbooru's fetch-status endpoint so the detail poll can surface it instead of
// spinning; enrich is never called.
func TestPipelineMetadataFetchFailureReported(t *testing.T) {
	fake := &fakeRunner{fetchErr: &queue.CodedError{Code: queue.ErrCodeUnsupportedURL, Msg: "no suitable extractor"}}
	var gotPath, gotBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://example.com/x"))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != queue.ErrCodeUnsupportedURL {
		t.Fatalf("items = %+v, want one failed with unsupported_url", job.Items)
	}
	if gotPath != "/api/v1/images/42/fetch-status?gallery=art" {
		t.Errorf("report path = %q, want /api/v1/images/42/fetch-status?gallery=art", gotPath)
	}
	if !strings.Contains(gotBody, `"state":"unsupported_url"`) {
		t.Errorf("report body = %q, want the unsupported_url state", gotBody)
	}
}

func TestPipelineMixedOutcomes(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{danbooruPost("100001"), danbooruPost("100002"), danbooruPost("100003"), danbooruPost("100004")},
		writeIdx: []int{0, 1, 3}, // 100003 is "already archived" - never written
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		_, fh, _ := r.FormFile("file")
		switch {
		case strings.HasPrefix(fh.Filename, "100001"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 11})
		case strings.HasPrefix(fh.Filename, "100002"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"image": map[string]any{"id": 22}, "alias_added": true})
		case strings.HasPrefix(fh.Filename, "100004"):
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom", "code": "internal_error"})
		default:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		}
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	id := q.Enqueue("http://danbooru/posts", queue.Options{})
	job := waitJob(t, q, id)

	if job.Status != queue.JobPartial {
		t.Errorf("status = %s, want partial", job.Status)
	}
	want := queue.Summary{Created: 1, Duplicate: 1, Skipped: 1, Failed: 1, Total: 4}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	check := []struct {
		idx       int
		outcome   queue.Outcome
		monbooru  int64
		errorCode string
	}{
		{0, queue.OutcomeCreated, 11, ""},
		{1, queue.OutcomeDuplicate, 22, ""},
		{2, queue.OutcomeSkippedArchive, 0, ""},
		{3, queue.OutcomeFailed, 0, queue.ErrCodeMonbooruRejected},
	}
	for _, c := range check {
		it := job.Items[c.idx]
		if it.Outcome != c.outcome {
			t.Errorf("item %d outcome = %s, want %s", c.idx, it.Outcome, c.outcome)
		}
		if it.MonbooruID != c.monbooru {
			t.Errorf("item %d monbooru id = %d, want %d", c.idx, it.MonbooruID, c.monbooru)
		}
		if c.errorCode != "" && it.ErrorCode != c.errorCode {
			t.Errorf("item %d error code = %q, want %q", c.idx, it.ErrorCode, c.errorCode)
		}
	}
	// The created item carries its sha256.
	if job.Items[0].SHA256 == "" {
		t.Error("created item should carry a sha256")
	}
}

// articleAsset models one media file from a non-booru page (a wiki article),
// the shape that mixes an image with a file monbooru cannot ingest.
func articleAsset(id string) gdl.Item {
	return gdl.Item{
		Category: "fandom-touhou", Subcategory: "article", ID: id,
		Meta: map[string]any{
			"category": "fandom-touhou", "subcategory": "article", "id": id,
			"tags": []any{"x"},
		},
	}
}

func TestPipelineSkipsUnsupportedMedia(t *testing.T) {
	// A wiki/article page returns mixed media: an image monbooru ingests and an
	// audio clip it cannot. The audio is skipped, not failed, so the job that
	// imported the image still succeeds rather than reading partial.
	fake := &fakeRunner{
		resolved: []gdl.Item{articleAsset("image"), articleAsset("audio")},
		writeIdx: []int{0, 1},
		extByIdx: map[int]string{1: ".ogg"}, // index 0 keeps the default .jpg
	}
	var pushes int
	handler := func(w http.ResponseWriter, r *http.Request) {
		pushes++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": pushes})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("https://example.com/wiki/some-article", queue.Options{}))

	if job.Status != queue.JobSucceeded {
		t.Errorf("status = %s, want succeeded (an unsupported file is skipped, not failed)", job.Status)
	}
	want := queue.Summary{Created: 1, Skipped: 1, Total: 2}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	if job.Items[0].Outcome != queue.OutcomeCreated {
		t.Errorf("image outcome = %s, want created", job.Items[0].Outcome)
	}
	if job.Items[1].Outcome != queue.OutcomeSkippedUnsupported {
		t.Errorf("audio outcome = %s, want skipped_unsupported", job.Items[1].Outcome)
	}
	if pushes != 1 {
		t.Errorf("pushed %d files, want 1 (the audio is not pushed)", pushes)
	}
}

func TestPipelinePartialDownloadStream(t *testing.T) {
	// A mid-stream download error truncates gallery-dl's output to fewer lines
	// than the resolve list. The written files still push; the unwritten tail is
	// marked failed and the aggregate counts stay coherent.
	fake := &fakeRunner{
		resolved:    []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")},
		writeIdx:    []int{0, 1, 2},
		shortStream: 2, // only the first two posts reach stdout
		dlErr:       &queue.CodedError{Code: queue.ErrCodeDownloadFailed},
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{}))

	if job.Status != queue.JobPartial {
		t.Errorf("status = %s, want partial", job.Status)
	}
	want := queue.Summary{Created: 2, Failed: 1, Total: 3}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	for _, i := range []int{0, 1} {
		if job.Items[i].Outcome != queue.OutcomeCreated {
			t.Errorf("item %d outcome = %s, want created", i, job.Items[i].Outcome)
		}
	}
	// The truncated tail post is failed with the download code, never dropped.
	if job.Items[2].Outcome != queue.OutcomeFailed || job.Items[2].ErrorCode != queue.ErrCodeDownloadFailed {
		t.Errorf("tail item = %+v, want failed/download_failed", job.Items[2])
	}
}

func TestPipelineRecordsTagWarnings(t *testing.T) {
	// monbooru returns tag_warnings for tags it rejected; the downloader records
	// them on the item so the queue and API can surface them.
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image":        map[string]any{"id": 5},
			"tag_warnings": []string{"rejected:foo"},
		})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if len(job.Items) != 1 || len(job.Items[0].TagWarnings) != 1 || job.Items[0].TagWarnings[0] != "rejected:foo" {
		t.Errorf("item tag warnings = %v, want [rejected:foo]", job.Items[0].TagWarnings)
	}
}

func TestPipelineMultiImagePostKeepsEveryImage(t *testing.T) {
	// A multi-asset post: one id, several files differing only by num. The bug
	// collapsed them so one image was pushed and the rest failed.
	fake := &fakeRunner{
		resolved: []gdl.Item{multiAssetPost("5719873", 1), multiAssetPost("5719873", 2), multiAssetPost("5719873", 3)},
		writeIdx: []int{0, 1, 2},
	}
	var pushes int
	seen := map[string]bool{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		_, fh, _ := r.FormFile("file")
		seen[fh.Filename] = true
		pushes++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": pushes})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("https://example.com/artwork/2x3LaB", queue.Options{}))
	if job.Status != queue.JobSucceeded || job.Summary.Created != 3 {
		t.Fatalf("status=%s created=%d, want succeeded/3 (one per asset)", job.Status, job.Summary.Created)
	}
	if pushes != 3 || len(seen) != 3 {
		t.Errorf("expected 3 distinct files pushed, got %d pushes / %d distinct", pushes, len(seen))
	}
}

func TestPipelineGenericSourceUsesSubmittedURL(t *testing.T) {
	// A source with no profile (no post-url template) still links back: the queue
	// item and the pushed image fall back to the submitted page URL.
	item := gdl.Item{
		Category: "desuarchive", Subcategory: "thread", Num: 288484266,
		Meta: map[string]any{
			"category": "desuarchive", "subcategory": "thread",
			"num": "288484266", "tags": []any{"x"},
		},
	}
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotURL string
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		gotURL = r.FormValue("url")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	submitted := "https://example.com/a/thread/288484266/"
	job := waitJob(t, q, q.Enqueue(submitted, queue.Options{}))
	if job.Status != queue.JobSucceeded {
		t.Fatalf("status = %s, want succeeded", job.Status)
	}
	if gotURL != submitted {
		t.Errorf("pushed url = %q, want the submitted url %q", gotURL, submitted)
	}
	if len(job.Items) != 1 || job.Items[0].URL != submitted {
		t.Errorf("item url = %q, want %q", job.Items[0].URL, submitted)
	}
}

func TestPipelineClampsMaxItems(t *testing.T) {
	// The default cap is 200; a per-job max_items lowers it but cannot raise it.
	// An offset shifts the window so a continued job fetches the next batch.
	cases := []struct {
		maxItems  int
		offset    int
		wantRange string
	}{
		{0, 0, "1-200"},       // unset: the configured cap
		{50, 0, "1-50"},       // below the cap: lowered
		{5000, 0, "1-200"},    // above the cap: clamped to the cap
		{50, 50, "51-100"},    // continued once: the next 50
		{200, 200, "201-400"}, // continued at the default cap
	}
	for _, c := range cases {
		fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
		handler := func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		}
		q, cleanup := testEnv(t, fake, handler)
		id := q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: c.maxItems, Offset: c.offset})
		waitJob(t, q, id)
		if fake.gotRange != c.wantRange {
			t.Errorf("max_items=%d offset=%d: resolve range = %q, want %q", c.maxItems, c.offset, fake.gotRange, c.wantRange)
		}
		cleanup()
	}
}

func TestPipelineSurfacesCap(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	// A resolve that returns the full cap is flagged capped (more may remain).
	t.Run("capped", func(t *testing.T) {
		fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2")}, writeIdx: []int{0, 1}}
		q, cleanup := testEnv(t, fake, handler)
		defer cleanup()
		job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 2}))
		if !job.Capped || job.Cap != 2 {
			t.Errorf("capped=%v cap=%d, want true/2", job.Capped, job.Cap)
		}
	})
	// A resolve below the cap is not flagged.
	t.Run("not capped", func(t *testing.T) {
		fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
		q, cleanup := testEnv(t, fake, handler)
		defer cleanup()
		job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 2}))
		if job.Capped {
			t.Errorf("a sub-cap resolve should not be capped, cap=%d", job.Cap)
		}
	})
}

func TestPipelineResolveFailure(t *testing.T) {
	fake := &fakeRunner{resolveErr: &queue.CodedError{Code: queue.ErrCodeUnsupportedURL}}
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})
	defer cleanup()

	id := q.Enqueue("http://unknown.example/x", queue.Options{})
	job := waitJob(t, q, id)
	if job.Status != queue.JobFailed {
		t.Errorf("status = %s, want failed", job.Status)
	}
	if job.ErrorCode != queue.ErrCodeUnsupportedURL {
		t.Errorf("error code = %q, want %q", job.ErrorCode, queue.ErrCodeUnsupportedURL)
	}
}

func TestPipelineEmptyResolveSucceeds(t *testing.T) {
	fake := &fakeRunner{resolved: nil}
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})
	defer cleanup()
	id := q.Enqueue("http://danbooru/posts?tags=nomatch", queue.Options{})
	job := waitJob(t, q, id)
	if job.Status != queue.JobSucceeded || job.Summary.Total != 0 {
		t.Errorf("empty resolve: status=%s total=%d, want succeeded/0", job.Status, job.Summary.Total)
	}
}

func forumLeaf(id string) gdl.Item {
	return gdl.Item{
		Category: "imgur", Subcategory: "image", ID: id,
		Meta: map[string]any{"category": "imgur", "subcategory": "image", "id": id, "tags": []any{"x"}},
	}
}

func TestPipelineForumDispatchLoose(t *testing.T) {
	// A forum thread whose inline images are externally hosted resolves to
	// Message.Queue handoffs, not files. The pipeline re-resolves deep (-J) and
	// pushes the leaves as loose items instead of reporting "no posts matched".
	fake := &fakeRunner{
		queue:     []gdl.QueueItem{{URL: "https://img.example.com/1.jpg"}, {URL: "https://img.example.com/2.jpg"}},
		category:  "bellazon",
		deepItems: []gdl.Item{forumLeaf("1"), forumLeaf("2")},
		writeIdx:  []int{0, 1},
	}
	var pushes int
	handler := func(w http.ResponseWriter, r *http.Request) {
		pushes++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": pushes})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("https://example.com/main/topic/1132-tennis/", queue.Options{}))

	if job.Status != queue.JobSucceeded || job.Summary.Created != 2 {
		t.Fatalf("status=%s created=%d, want succeeded/2 (the inline images)", job.Status, job.Summary.Created)
	}
	if !fake.gotResolveDeep {
		t.Error("a Queue-only resolve should trigger a deep -J re-resolve")
	}
	if pushes != 2 {
		t.Errorf("pushed %d files, want 2", pushes)
	}
	// The job's site is the dispatcher (the forum), not the leaf image host.
	if job.Site != "bellazon" {
		t.Errorf("site = %q, want bellazon (the dispatcher, not the leaf host)", job.Site)
	}
}

func mangaChapterPage(chID string, num int) gdl.Item {
	return gdl.Item{
		Category: "mangadex", Subcategory: "chapter", ID: chID, Num: num,
		Meta: map[string]any{"category": "mangadex", "subcategory": "chapter",
			"id": chID, "num": float64(num), "title": "Chapter", "tags": []any{"x"}},
	}
}

func TestPipelineMangaTitleExpandsToChapters(t *testing.T) {
	// A manga/comic title (series) resolves to Message.Queue chapters; each is
	// imported as its own cbz and pushed as its own manga, rather than failing or
	// bundling every chapter into one scrambled archive.
	ch1, ch2 := "https://example.com/chapter/aaa", "https://example.com/chapter/bbb"
	fake := &fakeRunner{
		queue:    []gdl.QueueItem{{URL: ch1}, {URL: ch2}},
		category: "mangadex",
		chapterPages: map[string][]gdl.Item{
			ch1: {mangaChapterPage("aaa", 1), mangaChapterPage("aaa", 2)},
			ch2: {mangaChapterPage("bbb", 1), mangaChapterPage("bbb", 2)},
		},
	}
	var cbzCount int
	var entries []int
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		f, fh, _ := r.FormFile("file")
		if !strings.HasSuffix(fh.Filename, ".cbz") {
			t.Errorf("filename = %q, want a .cbz", fh.Filename)
		}
		data, _ := io.ReadAll(f)
		if zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
			entries = append(entries, len(zr.File))
		}
		cbzCount++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": cbzCount})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("https://example.com/title/abc", queue.Options{}))

	if job.Status != queue.JobSucceeded || job.Summary.Created != 2 {
		t.Fatalf("status=%s created=%d, want succeeded/2 (one cbz per chapter)", job.Status, job.Summary.Created)
	}
	if len(job.Items) != 2 {
		t.Fatalf("want 2 chapter items, got %d", len(job.Items))
	}
	if cbzCount != 2 {
		t.Errorf("pushed %d cbz, want 2 (one per chapter)", cbzCount)
	}
	for _, e := range entries {
		if e != 2 {
			t.Errorf("a chapter cbz had %d pages, want 2", e)
		}
	}
	if job.Site != "mangadex" {
		t.Errorf("site = %q, want mangadex", job.Site)
	}
}

// TestPipelineForcedRetryPassesForce checks that a forced retry reaches the
// download pass as force=true, while a bulk initial run does not.
func TestPipelineForcedRetryPassesForce(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001"), danbooruPost("100002")}, writeIdx: []int{0, 1}}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	id := q.Enqueue("http://danbooru/posts", queue.Options{})
	waitJob(t, q, id)
	if fake.gotForce {
		t.Error("bulk initial run passed force=true to Download")
	}

	if err := q.Retry(id, true); err != nil {
		t.Fatalf("forced Retry: %v", err)
	}
	waitJob(t, q, id)
	if !fake.gotForce {
		t.Error("forced retry did not pass force=true to Download")
	}
}

// TestPipelineSinglePostBypassesArchive checks that a job resolving to a single
// post re-downloads past the archive (so re-submitting one post lets monbooru
// merge fresh tags), while a multi-post search keeps the archive.
func TestPipelineSinglePostBypassesArchive(t *testing.T) {
	created := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}

	single := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001")}, writeIdx: []int{0}}
	q, cleanup := testEnv(t, single, created)
	defer cleanup()
	waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if !single.gotForce {
		t.Error("single-post job should bypass the archive (force=true)")
	}

	bulk := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001"), danbooruPost("100002")}, writeIdx: []int{0, 1}}
	q2, cleanup2 := testEnv(t, bulk, created)
	defer cleanup2()
	waitJob(t, q2, q2.Enqueue("http://danbooru/posts", queue.Options{}))
	if bulk.gotForce {
		t.Error("multi-post search should keep the archive (force=false)")
	}
}

func TestProcessRecordsSiteReach(t *testing.T) {
	// A successful resolve reaches the booru, so the site is recorded for the
	// settings "last reached" indicator even when nothing was downloaded.
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}} // no writeIdx: archive-skip, no push
	cfg := config.Default()
	mapper, err := mapping.New(config.NewProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	tracker := sitestate.New()
	proc := New(fake, mapper, monbooru.New(config.NewProvider(cfg)), config.NewProvider(cfg), t.TempDir(), tracker, nil, nil)
	q := queue.New(proc, 1, 100)
	q.Start()
	defer q.Close()

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if job.Status != queue.JobSucceeded {
		t.Fatalf("job status = %s, want succeeded", job.Status)
	}
	if tracker.LastReached("danbooru").IsZero() {
		t.Error("a successful fetch should record the danbooru reach")
	}
	if !tracker.LastReached("e621").IsZero() {
		t.Error("only the resolved site should be recorded")
	}
}

func TestPipelineMangaGalleryBundlesAllPages(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2), mangaPage("654738", 3)},
		writeIdx: []int{0, 1, 2},
	}
	var gotEntries int
	var gotContents []string
	var gotFilename, gotCollection string
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		gotCollection = r.FormValue("collection")
		f, fh, _ := r.FormFile("file")
		gotFilename = fh.Filename
		data, _ := io.ReadAll(f)
		if zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
			gotEntries = len(zr.File)
			for _, zf := range zr.File {
				rc, _ := zf.Open()
				b, _ := io.ReadAll(rc)
				rc.Close()
				gotContents = append(gotContents, string(b))
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	// nhentai is kind=manga, so a gallery URL bundles into one cbz by default.
	id := q.Enqueue("https://example.com/g/654738/", queue.Options{})
	job := waitJob(t, q, id)

	if job.Status != queue.JobSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeCreated || job.Items[0].MonbooruID != 9 {
		t.Fatalf("manga gallery should push one created cbz, got %+v", job.Items)
	}
	if gotEntries != 3 {
		t.Errorf("cbz had %d pages, want all 3", gotEntries)
	}
	// Each page is present and distinct (the bug pushed one page three times).
	for _, want := range []string{"bytes-654738-1", "bytes-654738-2", "bytes-654738-3"} {
		if !slices.Contains(gotContents, want) {
			t.Errorf("cbz missing page %q, got %v", want, gotContents)
		}
	}
	if !strings.HasSuffix(gotFilename, ".cbz") {
		t.Errorf("filename = %q, want a .cbz", gotFilename)
	}
	if gotCollection != "" {
		t.Errorf("cbz mode should not set a collection, got %q", gotCollection)
	}
}

func TestOrderedPagesFallsBackToNoOrdinal(t *testing.T) {
	// Some sources emit the per-file ordinal as `no` instead of `num`; the pages
	// must still bundle in ordinal order, not the lexicographic path order that
	// puts 10 before 2.
	pages := []gdl.Downloaded{
		{Path: "/w/10.jpg", Meta: map[string]any{"no": float64(10)}},
		{Path: "/w/2.jpg", Meta: map[string]any{"no": float64(2)}},
		{Path: "/w/1.jpg", Meta: map[string]any{"no": float64(1)}},
	}
	got := orderedPages(pages)
	want := []string{"/w/1.jpg", "/w/2.jpg", "/w/10.jpg"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderedPages = %v, want %v", got, want)
		}
	}
}

func TestPipelineMangaHonorsFolderOverride(t *testing.T) {
	// The cbz push path must honor a per-job folder override, as the loose-item
	// path already does; without the fix it always used the default folder.
	fake := &fakeRunner{
		resolved: []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2)},
		writeIdx: []int{0, 1},
	}
	var gotFolder string
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		gotFolder = r.FormValue("folder")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("https://example.com/g/654738/", queue.Options{Folder: "manga"}))
	if job.Status != queue.JobSucceeded {
		t.Fatalf("status = %s, want succeeded", job.Status)
	}
	if gotFolder != "manga" {
		t.Errorf("cbz push folder = %q, want the per-job override \"manga\"", gotFolder)
	}
}

func TestPipelineMangaExemptFromCap(t *testing.T) {
	// A manga gallery larger than the cap is one book, so it is fetched whole:
	// the over-cap resolve re-resolves uncapped and the cbz keeps every page.
	fake := &fakeRunner{
		resolved: []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2), mangaPage("654738", 3)},
		writeIdx: []int{0, 1, 2},
	}
	var gotEntries int
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		f, _, _ := r.FormFile("file")
		data, _ := io.ReadAll(f)
		if zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
			gotEntries = len(zr.File)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	// max_items=2 caps the first resolve at 2 of the 3 pages.
	job := waitJob(t, q, q.Enqueue("https://example.com/g/654738/", queue.Options{MaxItems: 2}))

	if job.Status != queue.JobSucceeded || job.Capped {
		t.Errorf("status=%s capped=%v, want succeeded/false (a book is fetched whole)", job.Status, job.Capped)
	}
	if gotEntries != 3 {
		t.Errorf("cbz had %d pages, want all 3", gotEntries)
	}
	if fake.gotRange != "" {
		t.Errorf("last resolve range = %q, want empty (uncapped re-resolve)", fake.gotRange)
	}
}

func TestPipelineMangaIncompleteFails(t *testing.T) {
	// Fewer pages landed than the gallery resolved: a manga is one book, so the
	// job fails rather than pushing a truncated cbz.
	fake := &fakeRunner{
		resolved: []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2), mangaPage("654738", 3)},
		writeIdx: []int{0, 1}, // page 3 never lands
	}
	pushes := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		pushes++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	job := waitJob(t, q, q.Enqueue("https://example.com/g/654738/", queue.Options{}))

	if job.Status != queue.JobFailed {
		t.Errorf("status = %s, want failed (incomplete book not pushed)", job.Status)
	}
	if pushes != 0 {
		t.Errorf("expected no push of a truncated cbz, got %d", pushes)
	}
}

// poolPushHandler records each push's collection fields and answers created
// with a fresh id, for the pool-mode tests.
func poolPushHandler(orders, collections *[]string) http.HandlerFunc {
	nextID := int64(100)
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		*orders = append(*orders, r.FormValue("collection_order"))
		*collections = append(*collections, r.FormValue("collection"))
		nextID++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": nextID})
	}
}

func TestPipelinePool(t *testing.T) {
	// A booru pool pushes each page separately under a shared collection label
	// and order.
	fake := &fakeRunner{
		resolved: []gdl.Item{poolPost("1", 1, "g"), poolPost("2", 2, "g"), poolPost("3", 3, "g")},
		writeIdx: []int{0, 1, 2},
	}
	var orders, collections []string
	q, cleanup := testEnv(t, fake, poolPushHandler(&orders, &collections))
	defer cleanup()

	id := q.Enqueue("http://danbooru/pools/29906", queue.Options{})
	job := waitJob(t, q, id)

	if job.Status != queue.JobSucceeded || job.Summary.Created != 3 {
		t.Errorf("pool: status=%s created=%d, want succeeded/3", job.Status, job.Summary.Created)
	}
	if len(orders) != 3 {
		t.Fatalf("expected 3 pushes, got %d", len(orders))
	}
	for _, o := range []string{"1", "2", "3"} {
		if !slices.Contains(orders, o) {
			t.Errorf("missing collection_order %q in %v", o, orders)
		}
	}
	for _, c := range collections {
		if c != "A Quiet Afternoon" {
			t.Errorf("collection = %q, want the pool name", c)
		}
	}
}

func TestPipelineMoebooruPool(t *testing.T) {
	// moebooru pools carry no pool name and no num; they are still grouped under
	// "pool <id>" and ordered by resolve position.
	fake := &fakeRunner{
		resolved: []gdl.Item{moebooruPoolPost("14823"), moebooruPoolPost("14824"), moebooruPoolPost("14825")},
		writeIdx: []int{0, 1, 2},
	}
	var orders, collections []string
	q, cleanup := testEnv(t, fake, poolPushHandler(&orders, &collections))
	defer cleanup()

	id := q.Enqueue("https://example.com/pool/show/12", queue.Options{})
	job := waitJob(t, q, id)

	if job.Status != queue.JobSucceeded || job.Summary.Created != 3 {
		t.Errorf("status=%s created=%d, want succeeded/3", job.Status, job.Summary.Created)
	}
	for _, c := range collections {
		if c != "pool 12" {
			t.Errorf("collection = %q, want \"pool 12\"", c)
		}
	}
	for _, o := range []string{"1", "2", "3"} {
		if !slices.Contains(orders, o) {
			t.Errorf("missing collection_order %q in %v", o, orders)
		}
	}
}

func TestPipelinePoolExemptFromCap(t *testing.T) {
	// A pool larger than the cap is one work, so it is fetched whole: the
	// over-cap resolve triggers an uncapped re-resolve and every page pushes as
	// a collection item, with no capped flag.
	fake := &fakeRunner{
		resolved: []gdl.Item{poolPost("1", 1, "g"), poolPost("2", 2, "g"), poolPost("3", 3, "g")},
		writeIdx: []int{0, 1, 2},
	}
	var pushes int
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		pushes++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": pushes})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	// max_items=2 caps the first resolve at 2 of the 3 pool pages.
	job := waitJob(t, q, q.Enqueue("http://danbooru/pools/29906", queue.Options{MaxItems: 2}))

	if job.Status != queue.JobSucceeded || job.Summary.Created != 3 {
		t.Errorf("status=%s created=%d, want succeeded/3 (the whole pool)", job.Status, job.Summary.Created)
	}
	if job.Capped {
		t.Error("a pool must not be flagged capped")
	}
	if pushes != 3 {
		t.Errorf("pushed %d pages, want all 3", pushes)
	}
	if fake.gotRange != "" {
		t.Errorf("last resolve range = %q, want empty (uncapped re-resolve)", fake.gotRange)
	}
}

func TestBundleKey(t *testing.T) {
	// A manga gallery's pages share one post id, which keys the bundle item.
	manga := []gdl.Item{{ID: "654738", Meta: map[string]any{"id": "654738"}}}
	if got := bundleKey(manga); got != "gallery:654738" {
		t.Errorf("manga gallery key = %q, want gallery:654738", got)
	}
	if got := bundleKey(nil); got != "gallery" {
		t.Errorf("empty key = %q, want gallery", got)
	}
}

func TestChapterItemsLabelsBySlugWithoutID(t *testing.T) {
	// A chapter whose metadata carries no id is labeled by its URL slug, not the
	// whole URL, so the queue row stays short; one with an id keeps chapter:<id>.
	chapters := []gdl.QueueItem{
		{URL: "https://example.com/comic/a-title/1-first-chapter/"},
		{URL: "https://example.com/comic/a-title/2-second-chapter/", Meta: map[string]any{"id": "abc"}},
	}
	items := chapterItems(chapters)
	if items[0].PostID != "1-first-chapter" {
		t.Errorf("no-id chapter post_id = %q, want the slug 1-first-chapter", items[0].PostID)
	}
	if items[1].PostID != "chapter:abc" {
		t.Errorf("id chapter post_id = %q, want chapter:abc", items[1].PostID)
	}
	// The row still links to the chapter URL.
	if items[0].URL != chapters[0].URL {
		t.Errorf("chapter link = %q, want the chapter URL", items[0].URL)
	}
}

func TestPipelineItemsVisibleDuringDownload(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeRunner{
		resolved:        []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")},
		blockDownload:   true,
		downloadStarted: started,
	}
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})
	defer cleanup()

	id := q.Enqueue("http://danbooru/posts?tags=x", queue.Options{})
	<-started // resolve is done and the download is in flight, not yet complete
	job, err := q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	// The resolved items are already on the job (and pending) so the queue can
	// show progress before the download finishes.
	if len(job.Items) != 3 {
		t.Fatalf("expected 3 items visible during download, got %d", len(job.Items))
	}
	for i, it := range job.Items {
		if it.Status != queue.ItemPending {
			t.Errorf("item %d status = %s, want pending during download", i, it.Status)
		}
	}
	_ = q.Cancel(id)
	waitJob(t, q, id)
}

func TestPipelineItemsAdvanceDuringDownload(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeRunner{
		resolved:        []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")},
		blockDownload:   true,
		downloadStarted: started,
		liveIdx:         []int{0}, // item 0's file lands; the rest are still downloading
	}
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})
	defer cleanup()

	id := q.Enqueue("http://danbooru/posts?tags=x", queue.Options{})
	<-started
	job, err := q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	// The first item advanced to downloaded mid-download; the rest are still
	// pending, so the queue shows real progress before the download returns.
	if job.Items[0].Status != queue.ItemDownloaded {
		t.Errorf("item 0 status = %s, want downloaded", job.Items[0].Status)
	}
	for _, i := range []int{1, 2} {
		if job.Items[i].Status != queue.ItemPending {
			t.Errorf("item %d status = %s, want pending", i, job.Items[i].Status)
		}
	}
	_ = q.Cancel(id)
	waitJob(t, q, id)
}

func TestPipelineCancelMidJob(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeRunner{
		resolved:        []gdl.Item{danbooruPost("1"), danbooruPost("2")},
		blockDownload:   true,
		downloadStarted: started,
	}
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})
	defer cleanup()

	id := q.Enqueue("http://danbooru/posts", queue.Options{})
	<-started // the download is now in flight and blocked
	if err := q.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	job := waitJob(t, q, id)
	if job.Status != queue.JobCanceled {
		t.Errorf("status = %s, want canceled", job.Status)
	}
	// Canceled items are tallied as canceled, not failed, so a deliberate
	// cancel does not read as a batch of errors.
	if job.Summary.Canceled != 2 || job.Summary.Failed != 0 {
		t.Errorf("summary = %+v, want 2 canceled / 0 failed", job.Summary)
	}
}

func TestPipelineWarnsOnFlattenedNativeTags(t *testing.T) {
	// A danbooru (native-category) item that arrives with only the combined
	// tags field - the shape a gallery-dl tag-field rename would produce. The
	// job still succeeds with the tags flattened to general; the pipeline logs
	// a warning so the regression is visible.
	item := gdl.Item{
		Category: "danbooru", Subcategory: "post", ID: "555",
		Meta: map[string]any{
			"category": "danbooru", "id": "555", "subcategory": "post",
			"rating": "g", "tags": "alpha beta",
		},
	}
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotTags []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		_ = json.Unmarshal([]byte(r.FormValue("tags")), &gotTags)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}
	q, cleanup := testEnv(t, fake, handler)
	defer cleanup()

	id := q.Enqueue("http://danbooru/posts/555", queue.Options{})
	job := waitJob(t, q, id)

	if job.Status != queue.JobSucceeded || job.Summary.Created != 1 {
		t.Errorf("flattened-tags job: status=%s created=%d, want succeeded/1", job.Status, job.Summary.Created)
	}
	// The combined list still lands, mapped as general (bare names).
	for _, want := range []string{"alpha", "beta"} {
		if !slices.Contains(gotTags, want) {
			t.Errorf("expected general tag %q in %v", want, gotTags)
		}
	}
}
