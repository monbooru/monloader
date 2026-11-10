package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/sitestate"
)

type fakeRunner struct {
	resolved    []gdl.Item
	resolveErr  error
	writeIdx    []int
	dlErr       error
	ext         string
	extByIdx    map[int]string
	queue       []gdl.QueueItem
	category    string
	subcategory string
	childQueue  map[string][]gdl.QueueItem

	chapterResolves int
	deepItems       []gdl.Item
	gotResolveDeep  bool
	chapterPages    map[string][]gdl.Item
	shortStream     int
	errIdx          []int
	dlExt           string
	extractorSub    string

	failIdx []int

	skipMeta bool
	extra    []gdl.Item

	blockResolve   bool
	resolveStarted chan struct{}
	blockFetch     bool
	fetchStarted   chan struct{}

	blockDownload   bool
	downloadStarted chan struct{}
	liveIdx         []int

	listErr    error
	stopAfter  int
	gotArchive gdl.Archive
	gotRange   string

	honorRangeSet  bool
	gotDownloadRng string
	downloadCalls  int
	sidecarAdds    map[string]any

	emptyOnUncapped bool

	fetchMeta    map[string]any
	fetchErr     error
	fetchByURL   map[string]fetchResp
	gotFetchURLs []string
}

type fetchResp struct {
	meta map[string]any
	err  error
}

func (f *fakeRunner) Resolve(ctx context.Context, url, rng string, deep bool) (gdl.ResolveResult, error) {
	if f.blockResolve {
		close(f.resolveStarted)
		<-ctx.Done()
		return gdl.ResolveResult{}, errors.New("signal: killed")
	}
	if deep {
		f.gotResolveDeep = true
		return gdl.ResolveResult{Items: capItems(f.deepItems, rng), Category: f.category}, nil
	}
	if handoffs, ok := f.childQueue[url]; ok {
		return gdl.ResolveResult{Queue: handoffs}, nil
	}
	if pages, ok := f.chapterPages[url]; ok {
		f.chapterResolves++
		return gdl.ResolveResult{Items: pages}, nil
	}
	f.gotRange = rng
	if f.emptyOnUncapped && rng == "" {
		return gdl.ResolveResult{Category: f.category}, nil
	}
	if f.resolveErr != nil {
		return gdl.ResolveResult{}, f.resolveErr
	}
	cat := f.category
	if cat == "" && len(f.resolved) > 0 {
		cat = f.resolved[0].Category
	}
	return gdl.ResolveResult{Items: capItems(f.resolved, rng), Queue: f.queue, Category: cat, Subcategory: f.subcategory, Err: f.listErr}, nil
}

func (f *fakeRunner) FirstPost(ctx context.Context, url string) (map[string]any, error) {
	return f.FetchMeta(ctx, url)
}

func (f *fakeRunner) FetchMeta(ctx context.Context, url string) (map[string]any, error) {
	if f.blockFetch {
		close(f.fetchStarted)
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}
	f.gotFetchURLs = append(f.gotFetchURLs, url)
	if r, ok := f.fetchByURL[url]; ok {
		return r.meta, r.err
	}
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.fetchMeta, nil
}

func (f *fakeRunner) Download(ctx context.Context, url, rng, workDir string, archive gdl.Archive, onFile func(gdl.Downloaded), deep bool) ([]gdl.Downloaded, error) {
	f.gotArchive = archive
	f.gotDownloadRng = rng
	f.downloadCalls++

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
	src := f.resolved
	if deep {
		src = f.deepItems
	}
	if f.blockDownload {
		for _, i := range f.liveIdx {
			if onFile != nil {
				it := src[i]
				onFile(gdl.Downloaded{Meta: it.Meta})
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
	items := append(capItems(src, rng), f.extra...)
	if f.honorRangeSet {
		items = atPositions(src, rng)
	}
	out := make([]gdl.Downloaded, len(items))
	for i, it := range items {
		if f.shortStream > 0 && i >= f.shortStream {
			break
		}
		if slices.Contains(f.errIdx, i) {
			out[i] = gdl.Downloaded{Err: "the downloaded file has no readable metadata sidecar"}
			continue
		}
		if !write[i] && i < len(items)-len(f.extra) {
			out[i] = gdl.Downloaded{Skipped: true}
			if f.skipMeta {
				out[i].Meta = it.Meta
			}
			continue
		}

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
		if f.sidecarAdds != nil {
			meta = maps.Clone(meta)
			maps.Copy(meta, f.sidecarAdds)
		}
		d := gdl.Downloaded{Path: path, Meta: meta}
		if onFile != nil {
			onFile(d)
		}
		out[i] = d
	}
	if f.shortStream > 0 && f.shortStream < len(out) {
		out = out[:f.shortStream]
	}
	if f.stopAfter > 0 {
		out = out[:f.stopAfter]
	}
	for _, i := range slices.Backward(slices.Sorted(slices.Values(f.failIdx))) {
		out = slices.Delete(out, i, i+1)
	}
	return out, f.dlErr
}

func (f *fakeRunner) ListExtractors(ctx context.Context) ([]gdl.Extractor, error) { return nil, nil }
func (f *fakeRunner) Probe(ctx context.Context, url string) (gdl.ProbeResult, error) {
	return gdl.ProbeResult{}, nil
}
func (f *fakeRunner) DebugLog(ctx context.Context, url, workRoot string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeRunner) Version(ctx context.Context) string { return "fake" }

func (f *fakeRunner) SetSiteLists(gdl.SiteLists) {}

func (f *fakeRunner) ExtractorInfo(context.Context, string) (string, string) {
	return f.category, f.extractorSub
}

func atPositions(items []gdl.Item, rng string) []gdl.Item {
	if rng == "" {
		return items
	}
	var out []gdl.Item
	for _, part := range strings.Split(rng, ",") {
		lo, hi, isSpan := strings.Cut(part, "-")
		if !isSpan {
			hi = lo
		}
		from, _ := strconv.Atoi(lo)
		to, _ := strconv.Atoi(hi)
		for pos := from; pos <= to && pos <= len(items); pos++ {
			out = append(out, items[pos-1])
		}
	}
	return out
}

func rangeUpper(rng string) int {
	_, hi, ok := strings.Cut(rng, "-")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(hi)
	return n
}

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

func multiAssetPost(id string, num int) gdl.Item {
	return gdl.Item{
		Category: "artstation", Subcategory: "artwork", ID: id, Num: num,
		Meta: map[string]any{
			"category": "artstation", "id": id, "subcategory": "artwork",
			"num": float64(num), "tags": []any{"art"},
		},
	}
}

func moebooruPoolPost(id string) gdl.Item {
	return gdl.Item{
		Category: "yandere", Subcategory: "pool", ID: id,
		Meta: map[string]any{
			"category": "yandere", "id": id, "subcategory": "pool",
			"rating": "s", "pool": float64(12), "tags_general": []any{"comic"},
		},
	}
}

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

func createdHandler(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusCreated, map[string]any{"id": 1})
}

func enrichedHandler(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]any{})
}

func reply(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func startQueue(t *testing.T, fake gdl.Runner, cfg *config.Config, dir string, tracker *sitestate.Tracker) *queue.Queue {
	t.Helper()
	provider := config.NewProvider(cfg)
	mapper, err := mapping.New(provider, dir)
	if err != nil {
		t.Fatal(err)
	}
	q := queue.New(New(fake, mapper, monbooru.New(provider), provider, t.TempDir(), tracker, disabledPTR{}, noSimilarity{}), 1, 100)
	q.Start()
	t.Cleanup(q.Close)
	return q
}

func testEnv(t *testing.T, fake gdl.Runner, handler http.HandlerFunc) *queue.Queue {
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

	got, handler := captureReply(map[string]any{"merge": map[string]any{"tags_added": 2}, "verified": true})
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://danbooru.donmai.us/posts/100001"))

	if job.Status != queue.JobSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
	wantOneEnriched(t, job)
	if job.Summary.Enriched != 1 || job.Summary.Total != 1 {
		t.Errorf("summary = %+v, want 1 enriched / 1 total", job.Summary)
	}
	if job.Site != "danbooru" {
		t.Errorf("site = %q, want the refetched post's category", job.Site)
	}
	if got.path != "/api/v1/images/42/enrich?gallery=art" {
		t.Errorf("enrich path = %q, want /api/v1/images/42/enrich?gallery=art", got.path)
	}
	if !strings.Contains(got.body, `"verify":true`) ||
		!strings.Contains(got.body, `"source_md5":"abc123def456"`) ||
		!strings.Contains(got.body, "tag_100001") {
		t.Errorf("enrich body wrong: %s", got.body)
	}
}

func TestPipelineEnrichWithoutMergeSaysNoNewTags(t *testing.T) {
	fake := &fakeRunner{fetchMeta: danbooruPost("100001").Meta}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://danbooru.donmai.us/posts/100001"))
	wantOneEnriched(t, job)
	if job.Items[0].MergeNote != "no new tags" {
		t.Errorf("merge note = %q, want %q", job.Items[0].MergeNote, "no new tags")
	}
}

func TestPipelineMetadataHashMismatchFails(t *testing.T) {
	fake := &fakeRunner{fetchMeta: danbooruPost("100002").Meta}
	handler := func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusConflict, map[string]string{"error": "changed", "code": "hash_mismatch"})
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.EnqueueMetadata(7, "art", "https://danbooru.donmai.us/posts/100002"))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodeHashMismatch {
		t.Fatalf("items = %+v, want one failed with hash_mismatch", job.Items)
	}
}

func TestPipelineMetadataFetchFailureReported(t *testing.T) {
	fake := &fakeRunner{fetchErr: &outcome.CodedError{Code: outcome.ErrCodeUnsupportedURL, Msg: "no suitable extractor"}}
	got, handler := captureReply(map[string]any{})
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://example.com/x"))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodeUnsupportedURL {
		t.Fatalf("items = %+v, want one failed with unsupported_url", job.Items)
	}
	if got.path != "/api/v1/images/42/fetch-status?gallery=art" {
		t.Errorf("report path = %q, want /api/v1/images/42/fetch-status?gallery=art", got.path)
	}
	if !strings.Contains(got.body, `"state":"unsupported_url"`) || !strings.Contains(got.body, `"kind":"metadata"`) ||
		strings.Contains(got.body, "backend") {
		t.Errorf("report body = %q, want the unsupported_url state of a metadata job", got.body)
	}
}

func TestPipelineDroppedJobReported(t *testing.T) {
	var mu sync.Mutex
	var paths, bodies []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, string(b))
		mu.Unlock()
		enrichedHandler(w, r)
	}
	q := testEnv(t, &fakeRunner{}, handler)
	q.Pause()

	q.Enqueue("https://example.com/x", queue.Options{})
	q.CancelPending()
	mu.Lock()
	reported := len(paths)
	mu.Unlock()
	if reported != 0 {
		t.Fatalf("reported %d paths, want none for a plain download", reported)
	}

	_ = q.CancelLive(q.EnqueueLookup(42, "art", queue.BackendBooru, testMD5, "", true, true))
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/api/v1/images/42/fetch-status" {
		t.Fatalf("paths = %v, want one fetch-status report for image 42", paths)
	}
	if !strings.Contains(bodies[0], `"state":"canceled"`) || !strings.Contains(bodies[0], `"kind":"lookup"`) ||
		!strings.Contains(bodies[0], `"backend":"booru"`) {
		t.Errorf("report body = %q, want the canceled state of a booru lookup", bodies[0])
	}
}

func TestPipelineMixedOutcomes(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{danbooruPost("100001"), danbooruPost("100002"), danbooruPost("100003"), danbooruPost("100004")},
		writeIdx: []int{0, 1, 3},
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		_, fh, _ := r.FormFile("file")
		switch {
		case strings.HasPrefix(fh.Filename, "100001"):
			reply(w, http.StatusCreated, map[string]any{"id": 11})
		case strings.HasPrefix(fh.Filename, "100002"):
			reply(w, http.StatusOK, map[string]any{"image": map[string]any{"id": 22}, "alias_added": true})
		case strings.HasPrefix(fh.Filename, "100004"):
			reply(w, http.StatusInternalServerError, map[string]string{"error": "boom", "code": "internal_error"})
		default:
			reply(w, http.StatusCreated, map[string]any{"id": 99})
		}
	}
	q := testEnv(t, fake, handler)

	id := q.Enqueue("http://danbooru/posts", queue.Options{})
	job := waitJob(t, q, id)

	if job.Status != queue.JobPartial {
		t.Errorf("status = %s, want partial", job.Status)
	}
	want := queue.Summary{Created: 1, Duplicate: 1, Skipped: 1, Archived: 1, Failed: 1, Total: 4}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	check := []struct {
		idx       int
		outcome   outcome.Outcome
		monbooru  int64
		errorCode string
	}{
		{0, outcome.Created, 11, ""},
		{1, outcome.Duplicate, 22, ""},
		{2, outcome.SkippedArchive, 0, ""},
		{3, outcome.Failed, 0, outcome.ErrCodeMonbooruRejected},
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
	if job.Items[0].SHA256 == "" {
		t.Error("created item should carry a sha256")
	}
}

func TestUnreadableSidecarFailsRatherThanReadingAsAnArchiveSkip(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{
			{ID: "100001", Category: "danbooru", Meta: map[string]any{"id": float64(100001), "category": "danbooru", "extension": "jpg"}},
			{ID: "100002", Category: "danbooru", Meta: map[string]any{"id": float64(100002), "category": "danbooru", "extension": "jpg"}},
		},
		writeIdx: []int{0},
		errIdx:   []int{1},
	}
	q := testEnv(t, fake, createdHandler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts", queue.Options{}))
	it := job.Items[1]
	if it.Outcome != outcome.Failed || it.ErrorCode != outcome.ErrCodeMappingFailed {
		t.Errorf("item 1 = %s / %q, want failed / %s", it.Outcome, it.ErrorCode, outcome.ErrCodeMappingFailed)
	}
	if job.Summary.Archived != 0 || job.Summary.Failed != 1 {
		t.Errorf("summary = %+v, want it counted as a failure, not an archive skip", job.Summary)
	}
}

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
	fake := &fakeRunner{
		resolved: []gdl.Item{articleAsset("image"), articleAsset("audio")},
		writeIdx: []int{0, 1},
		extByIdx: map[int]string{1: ".ogg"},
	}
	var pushes int
	handler := countingPush(&pushes)
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("https://example.com/wiki/some-article", queue.Options{}))

	if job.Status != queue.JobSucceeded {
		t.Errorf("status = %s, want succeeded (an unsupported file is skipped, not failed)", job.Status)
	}
	want := queue.Summary{Created: 1, Skipped: 1, Total: 2}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	if job.Items[0].Outcome != outcome.Created {
		t.Errorf("image outcome = %s, want created", job.Items[0].Outcome)
	}
	if job.Items[1].Outcome != outcome.SkippedUnsupported {
		t.Errorf("audio outcome = %s, want skipped_unsupported", job.Items[1].Outcome)
	}
	if pushes != 1 {
		t.Errorf("pushed %d files, want 1 (the audio is not pushed)", pushes)
	}
}

func TestPipelinePushesAVIFAndJPEGXL(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{articleAsset("avif"), articleAsset("jxl")},
		writeIdx: []int{0, 1},
		extByIdx: map[int]string{0: ".avif", 1: ".jxl"},
	}
	var pushes int
	handler := countingPush(&pushes)
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("https://example.com/wiki/some-article", queue.Options{}))

	if want := (queue.Summary{Created: 2, Total: 2}); job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	if pushes != 2 {
		t.Errorf("pushed %d files, want both", pushes)
	}
}

func TestPipelinePartialDownloadStream(t *testing.T) {
	fake := &fakeRunner{
		resolved:    []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")},
		writeIdx:    []int{0, 1, 2},
		shortStream: 2,
		dlErr:       &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed},
	}
	q := testEnv(t, fake, createdHandler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{}))

	if job.Status != queue.JobPartial {
		t.Errorf("status = %s, want partial", job.Status)
	}
	want := queue.Summary{Created: 2, Failed: 1, Total: 3}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	for _, i := range []int{0, 1} {
		if job.Items[i].Outcome != outcome.Created {
			t.Errorf("item %d outcome = %s, want created", i, job.Items[i].Outcome)
		}
	}
	if job.Items[2].Outcome != outcome.Failed || job.Items[2].ErrorCode != outcome.ErrCodeDownloadFailed {
		t.Errorf("tail item = %+v, want failed/download_failed", job.Items[2])
	}
}

func TestArchiveSkipsStaySkippedWhenAnotherFileFails(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")},
		writeIdx: []int{0},
		dlErr:    &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "HttpError: 404"},
	}
	q := testEnv(t, fake, createdHandler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{}))

	want := queue.Summary{Created: 1, Skipped: 2, Archived: 2, Total: 3}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	for _, i := range []int{1, 2} {
		if job.Items[i].Outcome != outcome.SkippedArchive {
			t.Errorf("item %d outcome = %s, want skipped_archive", i, job.Items[i].Outcome)
		}
	}
}

func TestFailedFileKeepsLaterOutcomesOnTheirPosts(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{danbooruPost("5"), danbooruPost("4"), danbooruPost("3"), danbooruPost("2"), danbooruPost("1")},
		writeIdx: []int{0, 1, 2, 3, 4},
		failIdx:  []int{2},
		dlErr:    &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "Failed to download danbooru_3.png"},
	}
	var pushed []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		for _, fh := range r.MultipartForm.File {
			pushed = append(pushed, fh[0].Filename)
		}
		createdHandler(w, r)
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{}))

	for i, want := range []outcome.Outcome{outcome.Created, outcome.Created, outcome.Failed, outcome.Created, outcome.Created} {
		if job.Items[i].Outcome != want {
			t.Errorf("item %d (post %s) outcome = %s, want %s", i, job.Items[i].PostID, job.Items[i].Outcome, want)
		}
	}
	if want := []string{"5.jpg", "4.jpg", "2.jpg", "1.jpg"}; !slices.Equal(pushed, want) {
		t.Errorf("pushed %v, want %v", pushed, want)
	}
}

func TestArchiveSkipsPairByPostAroundAFailedFile(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{danbooruPost("4"), danbooruPost("3"), danbooruPost("2"), danbooruPost("1")},
		writeIdx: []int{3},
		failIdx:  []int{1},
		skipMeta: true,
		dlErr:    &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "Failed to download danbooru_3.png"},
	}
	q := testEnv(t, fake, createdHandler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{}))

	for i, want := range []outcome.Outcome{outcome.SkippedArchive, outcome.Failed, outcome.SkippedArchive, outcome.Created} {
		if job.Items[i].Outcome != want {
			t.Errorf("item %d (post %s) outcome = %s, want %s", i, job.Items[i].PostID, job.Items[i].Outcome, want)
		}
	}
}

func TestDownloadsPastAShortResolveArePushed(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{danbooruPost("3"), danbooruPost("2")},
		extra:    []gdl.Item{danbooruPost("1")},
		writeIdx: []int{0, 1, 2},
	}
	var pushes int
	q := testEnv(t, fake, countingPush(&pushes))

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{}))

	if len(job.Items) != 3 || job.Items[2].PostID != "1" || job.Items[2].Outcome != outcome.Created {
		t.Fatalf("items = %+v, want the unlisted post 1 appended and pushed", job.Items)
	}
	if pushes != 3 || job.Summary.Total != 3 {
		t.Errorf("pushes = %d, summary = %+v, want all three", pushes, job.Summary)
	}
}

func TestBundleWithAnUnusableFileFailsWithItsReason(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2)},
		writeIdx: []int{0},
		errIdx:   []int{1},
	}
	q := testEnv(t, fake, createdHandler)

	job := waitJob(t, q, q.Enqueue("https://nhentai.net/g/654738/", queue.Options{}))

	it := job.Items[0]
	if it.ErrorCode != outcome.ErrCodeMappingFailed || !strings.Contains(it.Error, "no readable metadata sidecar") {
		t.Errorf("bundle = %s / %q, want failed with the entry's reason", it.ErrorCode, it.Error)
	}
}

func TestPipelineRecordsTagWarnings(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
	handler := func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusCreated, map[string]any{
			"image":        map[string]any{"id": 5},
			"tag_warnings": []string{"rejected:foo"},
		})
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if len(job.Items) != 1 || len(job.Items[0].TagWarnings) != 1 || job.Items[0].TagWarnings[0] != "rejected:foo" {
		t.Errorf("item tag warnings = %v, want [rejected:foo]", job.Items[0].TagWarnings)
	}
}

func TestPipelineEnrichRecordsTagWarnings(t *testing.T) {
	meta := danbooruPost("100001").Meta
	meta["md5"] = "abc123def456"
	fake := &fakeRunner{fetchMeta: meta}
	handler := func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]any{
			"merge":        map[string]any{"tags_added": 4},
			"tag_warnings": []string{"tag copyright:toolong: invalid tag name"},
		})
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://danbooru.donmai.us/posts/100001"))
	if len(job.Items) != 1 || len(job.Items[0].TagWarnings) != 1 {
		t.Fatalf("item tag warnings = %v, want the one monbooru refused", job.Items[0].TagWarnings)
	}
}

func TestPipelineMultiImagePostKeepsEveryImage(t *testing.T) {
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
		reply(w, http.StatusCreated, map[string]any{"id": pushes})
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("https://example.com/artwork/2x3LaB", queue.Options{}))
	if job.Status != queue.JobSucceeded || job.Summary.Created != 3 {
		t.Fatalf("status=%s created=%d, want succeeded/3 (one per asset)", job.Status, job.Summary.Created)
	}
	if pushes != 3 || len(seen) != 3 {
		t.Errorf("expected 3 distinct files pushed, got %d pushes / %d distinct", pushes, len(seen))
	}
}

func TestPipelineGenericSourceUsesSubmittedURL(t *testing.T) {
	item := gdl.Item{
		Category: "desuarchive", Subcategory: "thread", Num: 288484266,
		Meta: map[string]any{
			"category": "desuarchive", "subcategory": "thread",
			"num": "288484266", "tags": []any{"x"},
		},
	}
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotURL string
	handler := capturePush(1, map[string]*string{"url": &gotURL})
	q := testEnv(t, fake, handler)

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

func TestPipelinePushesClientMetadata(t *testing.T) {
	item := directlinkItem()
	item.Meta["domain"] = "cdn.example"
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotTags, gotClientTags, gotSource, gotVia, gotCommentary string
	handler := capturePush(1, map[string]*string{
		"tags": &gotTags, "client_tags": &gotClientTags, "source": &gotSource, "via": &gotVia, "commentary": &gotCommentary,
	})
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("https://cdn.example/file.png", queue.Options{
		Tags:       []string{"Meta:From Alice", "photo"},
		Commentary: "shared by alice",
		Source:     "discord",
		Via:        "my-sender",
	}))
	if job.Status != queue.JobSucceeded {
		t.Fatalf("status = %s, want succeeded", job.Status)
	}
	if !strings.Contains(gotClientTags, "meta:from_alice") || !strings.Contains(gotClientTags, "photo") {
		t.Errorf("pushed client_tags = %s, want the client's normalized tags", gotClientTags)
	}
	if strings.Contains(gotTags, "photo") {
		t.Errorf("pushed tags = %s, want the client's kept apart from the source's", gotTags)
	}
	if gotSource != "discord" || gotVia != "my-sender" || gotCommentary != "shared by alice" {
		t.Errorf("pushed = {source:%q via:%q commentary:%q}, want the client's", gotSource, gotVia, gotCommentary)
	}
}

func TestPushCarriesPostMD5(t *testing.T) {
	item := danbooruPost("100001")
	item.Meta["md5"] = "0123456789abcdef0123456789abcdef"
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotMD5 string
	q := testEnv(t, fake, capturePush(1, map[string]*string{"md5": &gotMD5}))

	waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/100001", queue.Options{}))
	if gotMD5 != "0123456789abcdef0123456789abcdef" {
		t.Errorf("pushed md5 = %q, want the post's md5", gotMD5)
	}
}

func TestPipelineKeepsTheSitesOwnLabel(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001")}, writeIdx: []int{0}}
	var gotSource, gotClientTags string
	handler := capturePush(1, map[string]*string{"source": &gotSource, "client_tags": &gotClientTags})
	q := testEnv(t, fake, handler)

	waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/100001", queue.Options{
		Tags: []string{"meta:from_alice"}, Source: "discord",
	}))
	if gotSource != "danbooru" {
		t.Errorf("pushed source = %q, want danbooru", gotSource)
	}
	if !strings.Contains(gotClientTags, "meta:from_alice") {
		t.Errorf("pushed client_tags = %s, want the client's tag", gotClientTags)
	}
}

func TestPipelineClampsMaxItems(t *testing.T) {

	cases := []struct {
		maxItems  int
		offset    int
		wantRange string
		wantProbe string
	}{
		{0, 0, "1-200", "1-201"},
		{50, 0, "1-50", "1-51"},
		{5000, 0, "1-200", "1-201"},
		{50, 50, "51-100", "51-101"},
		{200, 200, "201-400", "201-401"},
	}
	for _, c := range cases {
		fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
		q := testEnv(t, fake, createdHandler)
		waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: c.maxItems, Offset: c.offset}))
		if fake.gotRange != c.wantProbe || fake.gotDownloadRng != c.wantRange {
			t.Errorf("max_items=%d offset=%d: resolve %q download %q, want %q and %q", c.maxItems, c.offset, fake.gotRange, fake.gotDownloadRng, c.wantProbe, c.wantRange)
		}
	}
}

func TestPipelineSurfacesCap(t *testing.T) {
	t.Run("capped", func(t *testing.T) {
		fake := &fakeRunner{resolved: searchRows(3), writeIdx: []int{0, 1}}
		q := testEnv(t, fake, createdHandler)
		job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 2}))
		if !job.Capped || job.Cap != 2 || job.Summary.Total != 2 {
			t.Errorf("capped=%v cap=%d total=%d, want true/2/2", job.Capped, job.Cap, job.Summary.Total)
		}
	})
	t.Run("exactly a window", func(t *testing.T) {
		fake := &fakeRunner{resolved: searchRows(2), writeIdx: []int{0, 1}}
		q := testEnv(t, fake, createdHandler)
		job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 2}))
		if job.Capped {
			t.Errorf("a search with exactly a window of posts should not be capped, cap=%d", job.Cap)
		}
	})
	t.Run("not capped", func(t *testing.T) {
		fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
		q := testEnv(t, fake, createdHandler)
		job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 2}))
		if job.Capped {
			t.Errorf("a sub-cap resolve should not be capped, cap=%d", job.Cap)
		}
	})
}

func TestPipelineAPostWithMoreFilesThanTheWindowIsCapped(t *testing.T) {
	files := []gdl.Item{danbooruPost("1"), danbooruPost("1")}
	for i := range files {
		files[i].Category, files[i].Num = "kemono", i+1
	}
	fake := &fakeRunner{resolved: files, writeIdx: []int{0}}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://kemono/post/1", queue.Options{MaxItems: 1}))
	if !job.Capped {
		t.Error("a post with a second file past the window should be capped")
	}
}

func TestPipelineResolveFailure(t *testing.T) {
	fake := &fakeRunner{resolveErr: &outcome.CodedError{Code: outcome.ErrCodeUnsupportedURL}}
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})

	job := waitJob(t, q, q.Enqueue("http://unknown.example/x", queue.Options{}))
	if job.Status != queue.JobFailed {
		t.Errorf("status = %s, want failed", job.Status)
	}
	if job.ErrorCode != outcome.ErrCodeUnsupportedURL {
		t.Errorf("error code = %q, want %q", job.ErrorCode, outcome.ErrCodeUnsupportedURL)
	}
}

func TestPipelineEmptyResolveSucceeds(t *testing.T) {
	fake := &fakeRunner{resolved: nil}
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=nomatch", queue.Options{}))
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
	fake := &fakeRunner{
		queue:     []gdl.QueueItem{{URL: "https://img.example.com/1.jpg"}, {URL: "https://img.example.com/2.jpg"}},
		category:  "bellazon",
		deepItems: []gdl.Item{forumLeaf("1"), forumLeaf("2")},
		writeIdx:  []int{0, 1},
	}
	var pushes int
	handler := countingPush(&pushes)
	q := testEnv(t, fake, handler)

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
	if job.Site != "bellazon" {
		t.Errorf("site = %q, want bellazon (the dispatcher, not the leaf host)", job.Site)
	}
}

func TestPipelineDispatchCapCountsTheWindowsOffset(t *testing.T) {
	children := make([]gdl.QueueItem, 3)
	for i := range children {
		children[i] = gdl.QueueItem{URL: "https://img.example.com/" + strconv.Itoa(i) + ".jpg"}
	}
	for _, tc := range []struct {
		name       string
		offset     int
		wantCapped bool
	}{
		{"first window with more past it", 0, true},
		{"last window", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRunner{queue: children, category: "bellazon", deepItems: []gdl.Item{forumLeaf("1"), forumLeaf("2")}, writeIdx: []int{0, 1}}
			q := testEnv(t, fake, createdHandler)
			job := waitJob(t, q, q.Enqueue("https://example.com/main/topic/1132-tennis/", queue.Options{Offset: tc.offset, MaxItems: 2}))
			if job.Capped != tc.wantCapped {
				t.Errorf("capped = %v, want %v", job.Capped, tc.wantCapped)
			}
		})
	}
}

var listingAuth401 = &outcome.CodedError{Code: outcome.ErrCodeAuthRequired, Msg: "HttpError: '401 Unauthorized' for 'https://danbooru.donmai.us/posts.json?page=b100'"}

func searchRows(n int) []gdl.Item {
	rows := make([]gdl.Item, n)
	for i := range rows {
		rows[i] = danbooruPost(strconv.Itoa(1000 - i))
	}
	return rows
}

func TestPipelineAListingCutShortIsPartialAndContinuesFromTheCut(t *testing.T) {
	fake := &fakeRunner{resolved: searchRows(2), writeIdx: []int{0, 1}, listErr: listingAuth401, dlErr: listingAuth401}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 5}))
	if job.Status != queue.JobPartial || job.ErrorCode != outcome.ErrCodeAuthRequired || job.Summary.Created != 2 {
		t.Fatalf("status=%s code=%q summary=%+v", job.Status, job.ErrorCode, job.Summary)
	}
	if !job.Capped || job.Cap != 5 {
		t.Errorf("capped=%v cap=%d, want the row to keep its next window at the same size", job.Capped, job.Cap)
	}
	next, err := q.Continue(job.ID)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	fake.resolved, fake.listErr, fake.dlErr = nil, nil, nil
	waitJob(t, q, next)
	if fake.gotRange != "3-8" {
		t.Errorf("next window's resolve = %q, want 3-8, one past 3-7", fake.gotRange)
	}
}

func TestPipelineAChainStopsAtAListingCutShort(t *testing.T) {
	fake := &fakeRunner{resolved: searchRows(2), writeIdx: []int{0, 1}, listErr: listingAuth401, dlErr: listingAuth401}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 5, Auto: true}))
	if job.Status != queue.JobPartial {
		t.Fatalf("status = %s", job.Status)
	}
	all, _ := q.List(queue.ListOptions{})
	for _, j := range all {
		if j.ID != job.ID {
			t.Errorf("the chain queued window %d past a listing that errored", j.ID)
		}
	}
}

func TestPipelineACleanDownloadOverrulesTheListingError(t *testing.T) {
	fake := &fakeRunner{resolved: searchRows(2), writeIdx: []int{0, 1}, listErr: listingAuth401}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 5}))
	if job.Status != queue.JobSucceeded || job.Capped || job.ErrorCode != "" {
		t.Fatalf("status=%s capped=%v code=%q", job.Status, job.Capped, job.ErrorCode)
	}
}

func TestPipelineARunFailureWithEveryRowPairedCutsTheWindow(t *testing.T) {
	fake := &fakeRunner{resolved: searchRows(2), writeIdx: []int{0, 1}, dlErr: listingAuth401}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 5}))
	if job.Status != queue.JobPartial || !job.Capped || job.ErrorCode != outcome.ErrCodeAuthRequired {
		t.Fatalf("status=%s capped=%v code=%q", job.Status, job.Capped, job.ErrorCode)
	}
}

func TestPipelineAFailedRowCarriesTheRunFailure(t *testing.T) {
	fake := &fakeRunner{resolved: searchRows(2), writeIdx: []int{0, 1}, failIdx: []int{1}, dlErr: listingAuth401}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{MaxItems: 5}))
	if job.Status != queue.JobPartial || job.Capped || job.ErrorCode != "" || job.Summary.Failed != 1 {
		t.Fatalf("status=%s capped=%v code=%q summary=%+v", job.Status, job.Capped, job.ErrorCode, job.Summary)
	}
}

func TestPipelineAFailingChildBesideSiblingsReadsPartial(t *testing.T) {
	fake := &fakeRunner{
		category: "bellazon", subcategory: "thread",
		queue:     []gdl.QueueItem{{URL: "https://img.example.com/1.jpg"}, {URL: "https://img.example.com/2.jpg"}},
		deepItems: []gdl.Item{forumLeaf("1"), forumLeaf("2")},
		writeIdx:  []int{0, 1},
		dlErr:     listingAuth401,
	}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("https://example.com/main/topic/1132-tennis/", queue.Options{MaxItems: 5}))
	if job.Status != queue.JobPartial || job.Capped || job.ErrorCode != outcome.ErrCodeAuthRequired {
		t.Fatalf("status=%s capped=%v code=%q", job.Status, job.Capped, job.ErrorCode)
	}
}

func TestPipelineAHandoffThatYieldsNothingFailsWithTheListingError(t *testing.T) {
	fake := &fakeRunner{
		category: "bellazon", subcategory: "thread",
		queue:   []gdl.QueueItem{{URL: "https://img.example.com/1.jpg"}},
		listErr: listingAuth401,
	}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("https://example.com/main/topic/1132-tennis/", queue.Options{MaxItems: 5}))
	if job.Status != queue.JobFailed || job.ErrorCode != outcome.ErrCodeAuthRequired {
		t.Fatalf("status=%s code=%q", job.Status, job.ErrorCode)
	}
}

func TestPipelineACheckThatReachedKnownPostsIgnoresALaterListingError(t *testing.T) {
	fake := &fakeRunner{resolved: searchRows(5), skipMeta: true, listErr: listingAuth401, dlErr: listingAuth401}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{Watch: 7, MaxItems: 30}))
	if job.Status != queue.JobSucceeded || job.Capped || job.ErrorCode != "" {
		t.Fatalf("status=%s capped=%v code=%q", job.Status, job.Capped, job.ErrorCode)
	}
}

func TestPipelineACatchUpStopKeepsItsFailedFile(t *testing.T) {

	fake := &fakeRunner{
		resolved: searchRows(30), failIdx: []int{0}, skipMeta: true, stopAfter: 21,
		dlErr: &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "[download][error] Failed to download 1000.jpg"},
	}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=x", queue.Options{Watch: 7, MaxItems: 30}))
	if job.Status != queue.JobPartial || job.Capped || job.ErrorCode != "" {
		t.Fatalf("status=%s capped=%v code=%q", job.Status, job.Capped, job.ErrorCode)
	}
	if job.Items[0].ErrorCode != outcome.ErrCodeDownloadFailed || job.Summary.Failed != 1 || job.Summary.Archived != 29 {
		t.Errorf("first=%+v summary=%+v, want the failed post failed and the rest archived", job.Items[0], job.Summary)
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
		reply(w, http.StatusCreated, map[string]any{"id": cbzCount})
	}
	q := testEnv(t, fake, handler)

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

func TestPipelineChapterRefusesANonHTTPLink(t *testing.T) {
	good := "https://example.com/chapter/aaa"
	fake := &fakeRunner{
		queue:        []gdl.QueueItem{{URL: "--write-log=/config/x"}, {URL: good}},
		category:     "mangadex",
		chapterPages: map[string][]gdl.Item{good: {mangaChapterPage("aaa", 1)}},
	}
	q := testEnv(t, fake, createdHandler)

	job := waitJob(t, q, q.Enqueue("https://example.com/title/abc", queue.Options{}))

	if len(job.Items) != 2 || job.Items[0].ErrorCode != outcome.ErrCodeUnsupportedURL {
		t.Fatalf("items = %+v, want the first refused as %s", job.Items, outcome.ErrCodeUnsupportedURL)
	}
	if job.Items[1].Outcome != outcome.Created {
		t.Errorf("the http chapter = %+v, want it imported", job.Items[1])
	}
}

func TestPipelineMangaTitleWindowsAdvance(t *testing.T) {
	var chapters []gdl.QueueItem
	pages := map[string][]gdl.Item{}
	for _, id := range []string{"aaa", "bbb", "ccc", "ddd", "eee"} {
		url := "https://example.com/chapter/" + id
		chapters = append(chapters, gdl.QueueItem{URL: url})
		pages[url] = []gdl.Item{mangaChapterPage(id, 1)}
	}
	fake := &fakeRunner{queue: chapters, category: "mangadex", chapterPages: pages}
	q := testEnv(t, fake, createdHandler)

	for _, c := range []struct {
		offset int
		want   []string
		capped bool
	}{
		{offset: 0, want: []string{"aaa", "bbb"}, capped: true},
		{offset: 2, want: []string{"ccc", "ddd"}, capped: true},
		{offset: 4, want: []string{"eee"}, capped: false},
	} {
		job := waitJob(t, q, q.Enqueue("https://example.com/title/abc",
			queue.Options{MaxItems: 2, Offset: c.offset}))
		var got []string
		for _, it := range job.Items {
			got = append(got, it.PostID)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("offset %d imported %v, want %v", c.offset, got, c.want)
		}
		if job.Capped != c.capped {
			t.Errorf("offset %d capped = %v, want %v", c.offset, job.Capped, c.capped)
		}
	}
}

func TestPipelineForcedRetryPassesForce(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001"), danbooruPost("100002")}, writeIdx: []int{0, 1}}
	q := testEnv(t, fake, createdHandler)

	id := q.Enqueue("http://danbooru/posts", queue.Options{})
	waitJob(t, q, id)
	if fake.gotArchive == gdl.ArchiveBypass {
		t.Error("bulk initial run bypassed the archive")
	}

	if err := q.Retry(id, true); err != nil {
		t.Fatalf("forced Retry: %v", err)
	}
	waitJob(t, q, id)
	if fake.gotArchive != gdl.ArchiveBypass {
		t.Error("forced retry did not bypass the archive")
	}
}

func TestPipelineSinglePostBypassesArchive(t *testing.T) {
	single := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001")}, writeIdx: []int{0}}
	q := testEnv(t, single, createdHandler)
	waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if single.gotArchive != gdl.ArchiveBypass {
		t.Error("single-post job should bypass the archive")
	}

	bulk := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001"), danbooruPost("100002")}, writeIdx: []int{0, 1}}
	q2 := testEnv(t, bulk, createdHandler)
	waitJob(t, q2, q2.Enqueue("http://danbooru/posts", queue.Options{}))
	if bulk.gotArchive == gdl.ArchiveBypass {
		t.Error("multi-post search should keep the archive")
	}
}

func TestPipelineWatchCheckArchiveMode(t *testing.T) {
	unprofiled := func(id string) gdl.Item {
		it := danbooruPost(id)
		it.Category, it.Meta["category"] = "unprofiledsite", "unprofiledsite"
		return it
	}
	for _, tc := range []struct {
		name     string
		resolved []gdl.Item
		want     gdl.Archive
	}{
		{"booru search", []gdl.Item{danbooruPost("100001"), danbooruPost("100002")}, gdl.ArchiveCatchUp},
		{"one result stays on the archive", []gdl.Item{danbooruPost("100001")}, gdl.ArchiveCatchUp},
		{"pool", []gdl.Item{poolPost("1", 1, "g"), poolPost("2", 2, "g")}, gdl.ArchiveRead},
		{"unprofiled site", []gdl.Item{unprofiled("100001"), unprofiled("100002")}, gdl.ArchiveRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := make([]int, len(tc.resolved))
			for i := range idx {
				idx[i] = i
			}
			fake := &fakeRunner{resolved: tc.resolved, writeIdx: idx}
			q := testEnv(t, fake, createdHandler)
			waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=a", queue.Options{Watch: 1}))
			if fake.gotArchive != tc.want {
				t.Errorf("archive mode = %v, want %v", fake.gotArchive, tc.want)
			}
		})
	}
}

func TestPipelineWatchCheckRefusesManga(t *testing.T) {
	gallery := &fakeRunner{resolved: []gdl.Item{mangaPage("1", 1), mangaPage("1", 2)}, writeIdx: []int{0, 1}}
	title := &fakeRunner{
		queue:       []gdl.QueueItem{{URL: "https://mangadex.org/chapter/a"}},
		category:    "mangadex",
		subcategory: "manga",
	}
	for name, tc := range map[string]struct {
		fake *fakeRunner
		site string
	}{"gallery": {gallery, "nhentai"}, "title": {title, "mangadex"}} {
		t.Run(name, func(t *testing.T) {
			q := testEnv(t, tc.fake, createdHandler)
			job := waitJob(t, q, q.Enqueue("https://manga.example/x", queue.Options{Watch: 1}))
			if job.Status != queue.JobFailed || job.ErrorCode != outcome.ErrCodeUnsupportedURL {
				t.Errorf("watch check of a manga %s = %s %s, want failed unsupported_url", name, job.Status, job.ErrorCode)
			}
			if !strings.Contains(job.Error, "only a manga listing can be watched") {
				t.Errorf("error = %q, want it to say what can be watched", job.Error)
			}
			if job.Site != tc.site {
				t.Errorf("site = %q, want %q", job.Site, tc.site)
			}
		})
	}
}

func mangaListing() *fakeRunner {
	a, b := "https://e-hentai.org/g/1/aaaaaaaaaa/", "https://e-hentai.org/g/2/bbbbbbbbbb/"
	return &fakeRunner{
		queue:       []gdl.QueueItem{{URL: a}, {URL: b}},
		category:    "exhentai",
		subcategory: "search",
		chapterPages: map[string][]gdl.Item{
			a: {mangaPage("1", 1), mangaPage("1", 2)},
			b: {mangaPage("2", 1), mangaPage("2", 2)},
		},
	}
}

func mangaListingEnv(t *testing.T, fake *fakeRunner, handler http.HandlerFunc) (*queue.Queue, *queue.Store) {
	t.Helper()
	st, err := queue.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return storeEnv(t, fake, handler, config.Default(), nil, nil, st), st
}

const mangaSearchURL = "https://e-hentai.org/?f_search=a"

func TestPipelineWatchCheckOfAMangaListingSkipsWhatLanded(t *testing.T) {
	fake := mangaListing()
	q, _ := mangaListingEnv(t, fake, createdHandler)

	first := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1, MaxItems: 2}))
	if first.Status != queue.JobSucceeded || first.Summary.Created != 2 || fake.downloadCalls != 2 {
		t.Fatalf("first check = %s created=%d downloads=%d, want both galleries landed", first.Status, first.Summary.Created, fake.downloadCalls)
	}
	if first.Site != "exhentai" || first.Subcategory != "search" {
		t.Errorf("first check labeled %s/%s, want exhentai/search", first.Site, first.Subcategory)
	}

	second := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1, MaxItems: 2}))
	if second.Status != queue.JobSucceeded || second.Summary.Archived != 2 || second.Summary.Created != 0 {
		t.Errorf("second check = %s %+v, want both galleries archive-skipped", second.Status, second.Summary)
	}
	if fake.downloadCalls != 2 {
		t.Errorf("downloads = %d, want no request for a landed gallery", fake.downloadCalls)
	}
}

func TestPipelineAListingCutShortMarksItsMangaWindow(t *testing.T) {
	fake := &fakeRunner{
		category: "mangadex", subcategory: "search",
		queue:   []gdl.QueueItem{{URL: "https://mangadex.org/chapter/c1"}},
		listErr: listingAuth401,
		chapterPages: map[string][]gdl.Item{
			"https://mangadex.org/chapter/c1": {mangaChapterPage("c1", 1)},
		},
	}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("https://mangadex.org/search?q=x", queue.Options{MaxItems: 5}))
	if job.Status != queue.JobPartial || !job.Capped || job.ErrorCode != outcome.ErrCodeAuthRequired {
		t.Fatalf("status=%s capped=%v code=%q items=%+v", job.Status, job.Capped, job.ErrorCode, job.Items)
	}
}

func TestPipelineWatchCheckRetriesABookThatNeverLanded(t *testing.T) {
	fake := mangaListing()
	var refuse atomic.Bool
	refuse.Store(true)
	q, _ := mangaListingEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		createdHandler(w, r)
	})

	first := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1}))
	if first.Summary.Failed != 2 {
		t.Fatalf("first check = %+v, want both pushes failed", first.Summary)
	}
	refuse.Store(false)
	second := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1, Force: true, ForceTo: 2}))
	if second.Summary.Created != 2 || fake.downloadCalls != 4 {
		t.Errorf("forced check = %+v downloads=%d, want both fetched again", second.Summary, fake.downloadCalls)
	}
}

func TestPipelineABookThatYieldsNoPagesFails(t *testing.T) {
	fake := mangaListing()
	fake.chapterPages[fake.queue[0].URL] = nil
	q, _ := mangaListingEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{}))
	if job.Status != queue.JobPartial || job.Items[0].ErrorCode != outcome.ErrCodeDownloadFailed || job.Items[0].Outcome != outcome.Failed {
		t.Errorf("status=%s first=%+v, want the empty book failed and never read as archived", job.Status, job.Items[0])
	}
}

func TestPipelineMangaListingForceRespectsWhoAsked(t *testing.T) {
	for name, tc := range map[string]struct {
		opts      queue.Options
		downloads int
	}{
		"a forced check":    {queue.Options{Watch: 1, Force: true, ForceTo: 2}, 2},
		"an operator force": {queue.Options{Watch: 1, Force: true}, 4},
		"a paste":           {queue.Options{}, 4},
	} {
		t.Run(name, func(t *testing.T) {
			fake := mangaListing()
			q, _ := mangaListingEnv(t, fake, createdHandler)
			waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1}))
			waitJob(t, q, q.Enqueue(mangaSearchURL, tc.opts))
			if fake.downloadCalls != tc.downloads {
				t.Errorf("downloads = %d, want %d", fake.downloadCalls, tc.downloads)
			}
		})
	}
}

func TestPipelineFailsAListedTitleRatherThanBundlingIt(t *testing.T) {
	const child = "https://allporncomic.com/porncomic/x/"
	for name, opts := range map[string]queue.Options{"a paste": {}, "a watch check": {Watch: 1}} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRunner{
				queue:       []gdl.QueueItem{{URL: child}},
				category:    "allporncomic",
				subcategory: "tag",
				childQueue:  map[string][]gdl.QueueItem{child: {{URL: child + "chapter-1/"}, {URL: child + "chapter-2/"}}},
			}
			q, st := mangaListingEnv(t, fake, createdHandler)
			job := waitJob(t, q, q.Enqueue("https://allporncomic.com/porncomic-tag/x/", opts))
			if job.Items[0].ErrorCode != outcome.ErrCodeUnsupportedURL || fake.downloadCalls != 0 {
				t.Errorf("item = %+v downloads=%d, want unsupported_url and nothing fetched", job.Items[0], fake.downloadCalls)
			}
			if landed, _ := st.BookLanded(child); landed {
				t.Error("a refused title was recorded as landed")
			}
		})
	}
}

func TestPipelineWatchCheckJudgesABlacklistedBookWithoutRequests(t *testing.T) {
	fake := mangaListing()
	st, err := queue.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Tags.Blacklist = []string{"tag_1"}
	q := storeEnv(t, fake, createdHandler, cfg, nil, nil, st)

	first := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1}))
	if first.Summary.Blacklisted != 2 || fake.chapterResolves != 2 {
		t.Fatalf("first check = %+v resolves=%d, want both resolved and blacklisted", first.Summary, fake.chapterResolves)
	}
	second := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1}))
	if second.Summary.Blacklisted != 2 || fake.chapterResolves != 2 {
		t.Errorf("second check = %+v resolves=%d, want both blacklisted with no request", second.Summary, fake.chapterResolves)
	}
	if second.Items[0].MergeNote == "" {
		t.Error("a remembered book's row does not name the rule")
	}

	cfg.Tags.Blacklist = nil
	third := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1}))
	if third.Summary.Created != 2 || fake.downloadCalls != 2 {
		t.Errorf("after removing the rule = %+v downloads=%d, want both landed", third.Summary, fake.downloadCalls)
	}
}

func TestPipelineListedGalleryWithOneHandoffIsStillABook(t *testing.T) {
	const child = "https://e-hentai.org/g/1/aaaaaaaaaa/"
	fake := &fakeRunner{
		queue:        []gdl.QueueItem{{URL: child}},
		category:     "exhentai",
		subcategory:  "search",
		childQueue:   map[string][]gdl.QueueItem{child: {{URL: "https://hitomi.la/galleries/1.html"}}},
		chapterPages: map[string][]gdl.Item{child: {mangaPage("1", 1), mangaPage("1", 2)}},
	}
	q, st := mangaListingEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue(mangaSearchURL, queue.Options{Watch: 1}))
	if job.Summary.Created != 1 {
		t.Errorf("summary = %+v, want the gallery landed", job.Summary)
	}
	if landed, _ := st.BookLanded(child); !landed {
		t.Error("the gallery was not recorded as landed")
	}
}

func TestPipelinePastedGalleryCountsAsLanded(t *testing.T) {
	const gallery = "https://e-hentai.org/g/1/aaaaaaaaaa/"
	fake := &fakeRunner{resolved: []gdl.Item{mangaPage("1", 1), mangaPage("1", 2)}, writeIdx: []int{0, 1}}
	q, st := mangaListingEnv(t, fake, createdHandler)
	waitJob(t, q, q.Enqueue(gallery, queue.Options{}))
	if landed, err := st.BookLanded(gallery); !landed || err != nil {
		t.Errorf("after the paste = %v, %v, want landed", landed, err)
	}
}

func TestProcessRecordsSiteReach(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}}
	cfg := config.Default()
	mb := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer mb.Close()
	cfg.Monbooru.APIURL = mb.URL
	tracker := sitestate.New()
	q := startQueue(t, fake, cfg, "", tracker)

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
	var gotContents, gotNames []string
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
				gotNames = append(gotNames, zf.Name)
			}
		}
		reply(w, http.StatusCreated, map[string]any{"id": 9})
	}
	q := testEnv(t, fake, handler)

	id := q.Enqueue("https://example.com/g/654738/", queue.Options{})
	job := waitJob(t, q, id)

	if job.Status != queue.JobSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Created || job.Items[0].MonbooruID != 9 {
		t.Fatalf("manga gallery should push one created cbz, got %+v", job.Items)
	}
	if gotEntries != 3 {
		t.Errorf("cbz had %d pages, want all 3", gotEntries)
	}

	if want := []string{"bytes-654738-1", "bytes-654738-2", "bytes-654738-3"}; !slices.Equal(gotContents, want) {
		t.Errorf("cbz pages = %v, want %v in that order", gotContents, want)
	}
	if !slices.IsSorted(gotNames) {
		t.Errorf("cbz entry names = %v, want them in reading order", gotNames)
	}
	if !strings.HasSuffix(gotFilename, ".cbz") {
		t.Errorf("filename = %q, want a .cbz", gotFilename)
	}
	if gotCollection != "" {
		t.Errorf("cbz mode should not set a collection, got %q", gotCollection)
	}
}

func TestPipelineMangaGalleryFoldsItsTitleIntoTheFilename(t *testing.T) {
	page := mangaPage("654738", 1)
	page.Meta["title"] = "Fate/Zero: Part 1?"
	fake := &fakeRunner{resolved: []gdl.Item{page}, writeIdx: []int{0}}
	var gotFilename string
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		if _, fh, err := r.FormFile("file"); err == nil {
			gotFilename = fh.Filename
		}
		reply(w, http.StatusCreated, map[string]any{"id": 9})
	}
	q := testEnv(t, fake, handler)

	waitJob(t, q, q.Enqueue("https://example.com/g/654738/", queue.Options{}))

	if want := "Fate_Zero_ Part 1_.cbz"; gotFilename != want {
		t.Errorf("filename = %q, want %q", gotFilename, want)
	}
}

func TestOrderedPagesFallsBackToNoOrdinal(t *testing.T) {
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

func TestOrderedPagesFallsBackToChapterPage(t *testing.T) {
	pages := []gdl.Downloaded{
		{Path: "/w/c001_1000.jpg", Meta: map[string]any{"page": float64(1000)}},
		{Path: "/w/c001_999.jpg", Meta: map[string]any{"page": float64(999)}},
		{Path: "/w/c001_001.jpg", Meta: map[string]any{"page": float64(1)}},
	}
	got := orderedPages(pages)
	want := []string{"/w/c001_001.jpg", "/w/c001_999.jpg", "/w/c001_1000.jpg"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderedPages = %v, want %v", got, want)
		}
	}
}

func TestOrderedPagesKeepStreamOrderWithNoOrdinal(t *testing.T) {
	pages := []gdl.Downloaded{
		{Path: "/w/p2.jpg", Meta: map[string]any{}},
		{Path: "/w/p10.jpg", Meta: map[string]any{}},
		{Path: "/w/p1.jpg", Meta: map[string]any{}},
	}
	want := []string{"/w/p2.jpg", "/w/p10.jpg", "/w/p1.jpg"}
	if got := orderedPages(pages); !slices.Equal(got, want) {
		t.Errorf("orderedPages = %v, want the order they landed in", got)
	}
}

func TestPipelineMangaHonorsFolderOverride(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2)},
		writeIdx: []int{0, 1},
	}
	var gotFolder string
	handler := capturePush(1, map[string]*string{"folder": &gotFolder})
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("https://example.com/g/654738/", queue.Options{Folder: "manga"}))
	if job.Status != queue.JobSucceeded {
		t.Fatalf("status = %s, want succeeded", job.Status)
	}
	if gotFolder != "manga" {
		t.Errorf("cbz push folder = %q, want the per-job override \"manga\"", gotFolder)
	}
}

func TestPipelineDefaultPushLeavesTheFolderToMonbooru(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{directlinkItem()}, writeIdx: []int{0}}
	gotFolder := "unset"
	q := testEnv(t, fake, capturePush(1, map[string]*string{"folder": &gotFolder}))

	job := waitJob(t, q, q.Enqueue("https://cdn.example/file.png", queue.Options{}))
	if job.Status != queue.JobSucceeded {
		t.Fatalf("status = %s, want succeeded", job.Status)
	}
	if gotFolder != "" {
		t.Errorf("push folder = %q, want none so monbooru's received-files folder applies", gotFolder)
	}
}

func TestPipelineMangaExemptFromCap(t *testing.T) {
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
		reply(w, http.StatusCreated, map[string]any{"id": 1})
	}
	q := testEnv(t, fake, handler)

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
	fake := &fakeRunner{
		resolved: []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2), mangaPage("654738", 3)},
		writeIdx: []int{0, 1},
	}
	pushes := 0
	q := testEnv(t, fake, countingPush(&pushes))

	job := waitJob(t, q, q.Enqueue("https://example.com/g/654738/", queue.Options{}))

	if job.Status != queue.JobFailed {
		t.Errorf("status = %s, want failed (incomplete book not pushed)", job.Status)
	}
	if pushes != 0 {
		t.Errorf("expected no push of a truncated cbz, got %d", pushes)
	}
}

func TestPipelineMangaShortReresolveFails(t *testing.T) {
	fake := &fakeRunner{
		resolved:        []gdl.Item{mangaPage("654738", 1), mangaPage("654738", 2), mangaPage("654738", 3)},
		writeIdx:        []int{0, 1, 2},
		emptyOnUncapped: true,
	}
	pushes := 0
	q := testEnv(t, fake, countingPush(&pushes))

	job := waitJob(t, q, q.Enqueue("https://example.com/g/654738/", queue.Options{MaxItems: 2}))

	if job.Status != queue.JobFailed {
		t.Errorf("status = %s, want failed (a partial book is not bundled)", job.Status)
	}
	if job.Capped {
		t.Error("an unconfirmed gallery must not read as a capped import")
	}
	if pushes != 0 {
		t.Errorf("expected no push of a truncated cbz, got %d", pushes)
	}
}

func poolPushHandler(orders, collections *[]string) http.HandlerFunc {
	nextID := int64(100)
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		*orders = append(*orders, r.FormValue("collection_order"))
		*collections = append(*collections, r.FormValue("collection"))
		nextID++
		reply(w, http.StatusCreated, map[string]any{"id": nextID})
	}
}

func TestPipelinePool(t *testing.T) {
	fake := &fakeRunner{
		resolved: []gdl.Item{poolPost("1", 1, "g"), poolPost("2", 2, "g"), poolPost("3", 3, "g")},
		writeIdx: []int{0, 1, 2},
	}
	var orders, collections []string
	q := testEnv(t, fake, poolPushHandler(&orders, &collections))

	job := waitJob(t, q, q.Enqueue("http://danbooru/pools/29906", queue.Options{}))

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
	fake := &fakeRunner{
		resolved: []gdl.Item{moebooruPoolPost("14823"), moebooruPoolPost("14824"), moebooruPoolPost("14825")},
		writeIdx: []int{0, 1, 2},
	}
	var orders, collections []string
	q := testEnv(t, fake, poolPushHandler(&orders, &collections))

	job := waitJob(t, q, q.Enqueue("https://example.com/pool/show/12", queue.Options{}))

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
	fake := &fakeRunner{
		resolved: []gdl.Item{poolPost("1", 1, "g"), poolPost("2", 2, "g"), poolPost("3", 3, "g")},
		writeIdx: []int{0, 1, 2},
	}
	var pushes int
	handler := countingPush(&pushes)
	q := testEnv(t, fake, handler)

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

func TestPipelinePoolShortReresolveKeepsWindow(t *testing.T) {
	fake := &fakeRunner{
		resolved:        []gdl.Item{poolPost("1", 1, "g"), poolPost("2", 2, "g"), poolPost("3", 3, "g")},
		writeIdx:        []int{0, 1, 2},
		emptyOnUncapped: true,
	}
	var pushes int
	handler := countingPush(&pushes)
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/pools/29906", queue.Options{MaxItems: 2}))

	if job.Status != queue.JobSucceeded || job.Summary.Created != 2 {
		t.Errorf("status=%s created=%d, want succeeded/2 (the capped window)", job.Status, job.Summary.Created)
	}
	if pushes != 2 {
		t.Errorf("pushed %d pages, want the 2 the capped resolve found", pushes)
	}
	if !job.Capped || job.Cap != 2 {
		t.Errorf("capped=%v cap=%d, want true/2 (the window is a partial import)", job.Capped, job.Cap)
	}
	if _, err := q.Continue(job.ID); err != nil {
		t.Errorf("Continue on the partial window: %v", err)
	}
}

func TestBundleKey(t *testing.T) {
	manga := []gdl.Item{{ID: "654738", Meta: map[string]any{"id": "654738"}}}
	if got := bundleKey(manga); got != "gallery:654738" {
		t.Errorf("manga gallery key = %q, want gallery:654738", got)
	}
	if got := bundleKey(nil); got != "gallery" {
		t.Errorf("empty key = %q, want gallery", got)
	}
}

func TestChapterItemsLabelsBySlugWithoutID(t *testing.T) {
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
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})

	id := q.Enqueue("http://danbooru/posts?tags=x", queue.Options{})
	<-started
	job, err := q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Items) != 3 {
		t.Fatalf("expected 3 items visible during download, got %d", len(job.Items))
	}
	for i, it := range job.Items {
		if it.Status != queue.ItemPending {
			t.Errorf("item %d status = %s, want pending during download", i, it.Status)
		}
	}
	_ = q.CancelSeries(id)
	waitJob(t, q, id)
}

func TestPipelineItemsAdvanceDuringDownload(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeRunner{
		resolved:        []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")},
		blockDownload:   true,
		downloadStarted: started,
		liveIdx:         []int{0},
	}
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})

	id := q.Enqueue("http://danbooru/posts?tags=x", queue.Options{})
	<-started
	job, err := q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Items[0].Status != queue.ItemDownloaded {
		t.Errorf("item 0 status = %s, want downloaded", job.Items[0].Status)
	}
	for _, i := range []int{1, 2} {
		if job.Items[i].Status != queue.ItemPending {
			t.Errorf("item %d status = %s, want pending", i, job.Items[i].Status)
		}
	}
	_ = q.CancelSeries(id)
	waitJob(t, q, id)
}

func TestPipelineCancelMidJob(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeRunner{
		resolved:        []gdl.Item{danbooruPost("1"), danbooruPost("2")},
		blockDownload:   true,
		downloadStarted: started,
	}
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})

	id := q.Enqueue("http://danbooru/posts", queue.Options{})
	<-started
	if err := q.CancelSeries(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	job := waitJob(t, q, id)
	if job.Status != queue.JobCanceled {
		t.Errorf("status = %s, want canceled", job.Status)
	}
	if job.Summary.Canceled != 2 || job.Summary.Failed != 0 {
		t.Errorf("summary = %+v, want 2 canceled / 0 failed", job.Summary)
	}
}

func TestPipelineCancelMidResolve(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeRunner{blockResolve: true, resolveStarted: started}
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {})

	id := q.Enqueue("http://danbooru/posts", queue.Options{})
	<-started
	if err := q.CancelSeries(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	job := waitJob(t, q, id)
	if job.Status != queue.JobCanceled {
		t.Errorf("status = %s, want canceled", job.Status)
	}
}

func TestPipelineCancelMidMetadataFetch(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeRunner{blockFetch: true, fetchStarted: started}
	var reported string
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-status") {
			reported = r.URL.Path
		}
		w.WriteHeader(http.StatusOK)
	})

	id := q.EnqueueMetadata(42, "art", "https://danbooru.donmai.us/posts/100001")
	<-started
	if err := q.CancelSeries(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	job := waitJob(t, q, id)
	if job.Status != queue.JobCanceled {
		t.Errorf("status = %s, want canceled", job.Status)
	}
	if job.Summary.Canceled != 1 || job.Summary.Failed != 0 {
		t.Errorf("summary = %+v, want 1 canceled / 0 failed", job.Summary)
	}
	if reported != "" {
		t.Errorf("fetch status reported to %q, want no report on a cancel", reported)
	}
}

func TestADownloadWithMonbooruDownFetchesNothing(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001")}, writeIdx: []int{0}}
	cfg := config.Default()
	down := httptest.NewServer(http.NotFoundHandler())
	cfg.Monbooru.APIURL = down.URL
	down.Close()
	q := startQueue(t, fake, cfg, "", sitestate.New())

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/100001", queue.Options{}))

	if job.Status != queue.JobFailed || job.ErrorCode != outcome.ErrCodeMonbooruUnreachable {
		t.Errorf("status=%s code=%s, want failed monbooru_unreachable", job.Status, job.ErrorCode)
	}
	if fake.downloadCalls != 0 {
		t.Errorf("gallery-dl ran %d downloads, each recording its posts in the archive", fake.downloadCalls)
	}
}

func TestAPostOfferingNoFileFails(t *testing.T) {
	fake := &fakeRunner{category: "danbooru", extractorSub: "post"}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/5008", queue.Options{}))
	if job.Status != queue.JobFailed || job.ErrorCode != outcome.ErrCodeDownloadFailed || !strings.Contains(job.Error, "no file") {
		t.Errorf("status=%s code=%s error=%q, want a failure naming the missing file", job.Status, job.ErrorCode, job.Error)
	}
}

func TestASearchWithNoPostsStillSucceeds(t *testing.T) {
	fake := &fakeRunner{category: "danbooru", extractorSub: "tag"}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("http://danbooru/posts?tags=none", queue.Options{}))
	if job.Status != queue.JobSucceeded {
		t.Errorf("status=%s, want an empty search to succeed", job.Status)
	}
}

func TestPipelineFailsAPostWithOnlyFlatTags(t *testing.T) {

	flat := gdl.Item{
		Category: "safebooru", Subcategory: "post", ID: "199",
		Meta: map[string]any{"category": "safebooru", "id": "199", "subcategory": "post", "rating": "s", "tags": "an_artist a_character"},
	}
	good := gdl.Item{
		Category: "safebooru", Subcategory: "post", ID: "121",
		Meta: map[string]any{"category": "safebooru", "id": "121", "subcategory": "post", "rating": "s", "tags": "an_artist", "tags_artist": "an_artist"},
	}
	fake := &fakeRunner{resolved: []gdl.Item{flat, good}, writeIdx: []int{0, 1}}
	var pushed [][]string
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		var tags []string
		_ = json.Unmarshal([]byte(r.FormValue("tags")), &tags)
		pushed = append(pushed, tags)
		reply(w, http.StatusCreated, map[string]any{"id": 1})
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("http://safebooru/index.php?tags=x", queue.Options{}))

	if job.Status != queue.JobPartial || job.Items[0].ErrorCode != outcome.ErrCodeMappingFailed {
		t.Errorf("status=%s items=%+v, want partial with the flat post failing mapping_failed", job.Status, job.Items)
	}
	if job.Items[1].Outcome != outcome.Created {
		t.Errorf("the post with its categories: %+v, want created", job.Items[1])
	}
	if len(pushed) != 1 || !slices.Contains(pushed[0], "artist:an_artist") {
		t.Errorf("pushed %v, want only the categorized post", pushed)
	}
}

func TestPipelineKeepsAPostWithNoTagsAtAll(t *testing.T) {
	item := gdl.Item{
		Category: "safebooru", Subcategory: "post", ID: "7",
		Meta: map[string]any{"category": "safebooru", "id": "7", "subcategory": "post", "rating": "s", "tags": ""},
	}
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusCreated, map[string]any{"id": 1})
	})
	job := waitJob(t, q, q.Enqueue("http://safebooru/index.php?id=7", queue.Options{}))
	if job.Summary.Created != 1 {
		t.Errorf("summary = %+v, want the tagless post created", job.Summary)
	}
}

func TestMetadataRefetchFailsAPostWithOnlyFlatTags(t *testing.T) {
	fake := &fakeRunner{fetchMeta: map[string]any{"category": "safebooru", "id": "199", "tags": "an_artist"}}
	var enriched bool
	var reported string
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reported = string(b)
		} else if r.Method == http.MethodPost {
			enriched = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://safebooru.org/index.php?id=199"))

	if enriched {
		t.Error("a post with only flat tags was sent to monbooru")
	}
	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodeMappingFailed {
		t.Errorf("items = %+v, want mapping_failed", job.Items)
	}
	if !strings.Contains(reported, string(outcome.ErrCodeMappingFailed)) {
		t.Errorf("reported %q, want the failure told to monbooru", reported)
	}
}

func genericPost(id string, num int) gdl.Item {
	return gdl.Item{Category: "imgsite", ID: id, Num: num, Meta: map[string]any{"category": "imgsite", "id": id, "num": json.Number(strconv.Itoa(num))}}
}

func TestMetadataRefetchOfAListingWithNoMD5Fails(t *testing.T) {
	fake := &fakeRunner{fetchMeta: genericPost("1", 1).Meta, resolved: []gdl.Item{genericPost("1", 1), genericPost("2", 1)}}
	var enriched bool
	var reported string
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reported = string(b)
		} else if r.Method == http.MethodPost {
			enriched = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})

	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://imgsite.example/search?q=x"))

	if enriched {
		t.Error("the first post of a listing was merged into the image")
	}
	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodeUnsupportedURL {
		t.Errorf("items = %+v, want unsupported_url", job.Items)
	}
	if !strings.Contains(reported, "more than one post") {
		t.Errorf("reported %q, want the failure told to monbooru", reported)
	}
}

func TestMetadataRefetchOfOnePostWithSeveralFilesEnriches(t *testing.T) {
	fake := &fakeRunner{fetchMeta: genericPost("1", 1).Meta, resolved: []gdl.Item{genericPost("1", 1), genericPost("1", 2)}}
	q := testEnv(t, fake, enrichedHandler)
	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://imgsite.example/post/1"))
	wantOneEnriched(t, job)
}

func TestMetadataRefetchOfAPostWithAnMD5SkipsTheCheck(t *testing.T) {
	meta := genericPost("1", 1).Meta
	meta["md5"] = "abc123"
	fake := &fakeRunner{fetchMeta: meta, resolved: []gdl.Item{genericPost("1", 1), genericPost("2", 1)}}
	q := testEnv(t, fake, enrichedHandler)
	job := waitJob(t, q, q.EnqueueMetadata(42, "art", "https://imgsite.example/search?q=x"))
	wantOneEnriched(t, job)
}

func TestBuildCBZFilePadsPastThreeDigits(t *testing.T) {
	dir := t.TempDir()
	pages := make([]string, 1001)
	for i := range pages {
		p := filepath.Join(dir, "p"+strconv.Itoa(i)+".jpg")
		if err := os.WriteFile(p, []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		pages[i] = p
	}
	dest := filepath.Join(dir, "book.cbz")
	if err := buildCBZFile(pages, dest); err != nil {
		t.Fatalf("buildCBZFile: %v", err)
	}

	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := make([]string, len(zr.File))
	for i, f := range zr.File {
		names[i] = f.Name
	}
	if !slices.IsSorted(names) {
		t.Error("entry names do not sort in reading order")
	}
	if names[0] != "0001.jpg" || names[len(names)-1] != "1001.jpg" {
		t.Errorf("entry names %q..%q, want 0001.jpg..1001.jpg", names[0], names[len(names)-1])
	}
}

func TestProcessRecordsTheExtractorSubcategory(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001")}, writeIdx: []int{0}}
	q := testEnv(t, fake, createdHandler)
	job := waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/100001", queue.Options{}))
	if job.Subcategory != "post" {
		t.Errorf("subcategory = %q, want the resolved extractor's post", job.Subcategory)
	}
}

func TestARuleNamingAnUnconfirmedCategoryHoldsTheJob(t *testing.T) {
	var pushes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/api/v1/galleries":
			http.Error(w, "busy", http.StatusServiceUnavailable)
		default:
			pushes++
			createdHandler(w, r)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Monbooru.APIURL, cfg.Monbooru.APIToken = srv.URL, "tok"
	cfg.Tags.Blacklist = []string{"unconfirmed_category:x"}
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
	q := startQueue(t, fake, cfg, "", sitestate.New())

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if job.Status != queue.JobFailed || job.ErrorCode != outcome.ErrCodeMonbooruUnreachable {
		t.Errorf("status=%s code=%s, want failed monbooru_unreachable", job.Status, job.ErrorCode)
	}
	if pushes != 0 || fake.downloadCalls != 0 {
		t.Errorf("%d pushes, %d downloads, want the job held before either", pushes, fake.downloadCalls)
	}
}

func TestAnEarlyFetchReportCarriesTheBareMessage(t *testing.T) {
	var reported struct {
		State   string `json:"state"`
		Message string `json:"message"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/galleries":
			http.Error(w, "busy", http.StatusServiceUnavailable)
		case strings.HasSuffix(r.URL.Path, "/fetch-status"):
			_ = json.NewDecoder(r.Body).Decode(&reported)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Monbooru.APIURL, cfg.Monbooru.APIToken = srv.URL, "tok"
	cfg.Tags.Blacklist = []string{"unconfirmed_category:x"}
	q := startQueue(t, &fakeRunner{}, cfg, "", sitestate.New())

	waitJob(t, q, q.EnqueueMetadata(42, "art", "https://danbooru.donmai.us/posts/1"))
	if reported.State != outcome.ErrCodeMonbooruUnreachable {
		t.Errorf("state = %q", reported.State)
	}
	if reported.Message == "" || strings.HasPrefix(reported.Message, outcome.ErrCodeMonbooruUnreachable) {
		t.Errorf("message = %q, want it without the code the state field carries", reported.Message)
	}
}

func TestAPushWarnsOfACategoryMonbooruDoesNotList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "danbooru.json"), []byte(`{"family":"danbooru","auth":"none",
		"post_url_template":"https://danbooru.donmai.us/posts/{id}","category_overrides":{"artist":"gone_cat"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/api/v1/galleries":
			_, _ = w.Write([]byte(`[{"name":"default"}]`))
		case "/api/v1/categories":
			_, _ = w.Write([]byte(`[{"name":"other_cat"}]`))
		default:
			createdHandler(w, r)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Monbooru.APIURL, cfg.Monbooru.APIToken = srv.URL, "tok"
	item := danbooruPost("1")
	item.Meta["tags_artist"] = []any{"someone"}
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	q := startQueue(t, fake, cfg, dir, sitestate.New())

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Created || len(job.Items[0].TagWarnings) != 1 || !strings.Contains(job.Items[0].TagWarnings[0], "gone_cat") {
		t.Errorf("items = %+v, want a created item warning of gone_cat", job.Items)
	}
}

func goneCatEnv(t *testing.T, site, kind string, fake gdl.Runner, handler http.HandlerFunc) *queue.Queue {
	t.Helper()
	dir := t.TempDir()
	profile := fmt.Sprintf(`{"family":"generic","kind":%q,"auth":"none","category_overrides":{"artist":"gone_cat"}}`, kind)
	if err := os.WriteFile(filepath.Join(dir, site+".json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/api/v1/galleries":
			_, _ = w.Write([]byte(`[{"name":"default"}]`))
		case "/api/v1/categories":
			_, _ = w.Write([]byte(`[{"name":"other_cat"}]`))
		default:
			handler(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Monbooru.APIURL, cfg.Monbooru.APIToken = srv.URL, "tok"
	q := startQueue(t, fake, cfg, dir, sitestate.New())
	return q
}

func goneCatPost(site string, num int) gdl.Item {
	return gdl.Item{Category: site, Subcategory: "gallery", ID: "1", Num: num, Meta: map[string]any{
		"category": site, "subcategory": "gallery", "id": "1", "num": json.Number(strconv.Itoa(num)),
		"tags_artist": []any{"someone"}, "tags_general": []any{"plain"},
	}}
}

func wantGoneCatWarning(t *testing.T, job *queue.Job) {
	t.Helper()
	if len(job.Items) != 1 || !slices.ContainsFunc(job.Items[0].TagWarnings, func(w string) bool { return strings.Contains(w, "gone_cat") }) {
		t.Errorf("items = %+v, want a warning naming gone_cat", job.Items)
	}
}

func TestABookWarnsOfACategoryMonbooruDoesNotList(t *testing.T) {
	pages := []gdl.Item{goneCatPost("gonebook", 1), goneCatPost("gonebook", 2)}
	q := goneCatEnv(t, "gonebook", "manga", &fakeRunner{resolved: pages, writeIdx: []int{0, 1}}, createdHandler)
	job := waitJob(t, q, q.Enqueue("https://gonebook.example/g/1", queue.Options{}))
	wantGoneCatWarning(t, job)
	if n := len(job.Items[0].TagWarnings); n != 1 {
		t.Errorf("warnings = %v, want one per target however many pages named it", job.Items[0].TagWarnings)
	}
}

func TestAnEnrichWarnsOfACategoryMonbooruDoesNotList(t *testing.T) {
	q := goneCatEnv(t, "gonesite", "booru", &fakeRunner{fetchMeta: goneCatPost("gonesite", 1).Meta}, enrichedHandler)
	wantGoneCatWarning(t, waitJob(t, q, q.EnqueueMetadata(42, "art", "https://gonesite.example/post/1")))
}

func TestAReplaceKeepsItsTagWarnings(t *testing.T) {
	cap := &replaceCapture{}
	fake := &fakeRunner{resolved: []gdl.Item{goneCatPost("gonesite", 1)}, writeIdx: []int{0}}
	q := goneCatEnv(t, "gonesite", "booru", fake, replaceStub(t, cap, http.StatusOK,
		map[string]any{"replaced": true, "tag_warnings": []string{"tag x: refused"}}))
	job := waitJob(t, q, q.EnqueueReplace(42, "art", "https://gonesite.example/post/1"))
	wantGoneCatWarning(t, job)
	if len(job.Items) == 1 && !slices.Contains(job.Items[0].TagWarnings, "tag x: refused") {
		t.Errorf("warnings = %v, want monbooru's too", job.Items[0].TagWarnings)
	}
}

func withheldEnv(t *testing.T, site, kind string, fake gdl.Runner, handler http.HandlerFunc) *queue.Queue {
	t.Helper()
	dir := t.TempDir()
	profile := fmt.Sprintf(`{"family":"generic","kind":%q,"auth":"none","tag_rules":{"dropped_tag":""}}`, kind)
	if err := os.WriteFile(filepath.Join(dir, site+".json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/api/v1/galleries":
			_, _ = w.Write([]byte(`[]`))
		default:
			handler(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Monbooru.APIURL, cfg.Monbooru.APIToken = srv.URL, "tok"
	q := startQueue(t, fake, cfg, dir, sitestate.New())
	return q
}

func wantWithheld(t *testing.T, raw string) {
	t.Helper()
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("withheld_tags = %q: %v", raw, err)
	}
	if !slices.Equal(got, []string{"dropped_tag"}) {
		t.Errorf("withheld_tags = %v, want dropped_tag once", got)
	}
}

func TestAPushNamesWhatARuleWithheld(t *testing.T) {
	item := gdl.Item{Category: "mysite", Subcategory: "post", ID: "1",
		Meta: map[string]any{"category": "mysite", "id": "1", "subcategory": "post", "tags": []any{"dropped_tag", "kept"}}}
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var withheld string
	q := withheldEnv(t, "mysite", "booru", fake, capturePush(1, map[string]*string{"withheld_tags": &withheld}))

	waitJob(t, q, q.Enqueue("http://mysite/1", queue.Options{}))
	wantWithheld(t, withheld)
}

func TestAnEnrichNamesWhatARuleWithheld(t *testing.T) {
	fake := &fakeRunner{fetchMeta: map[string]any{"category": "mysite", "id": "1", "tags": []any{"dropped_tag", "kept"}}}
	var body struct {
		WithheldTags []string `json:"withheld_tags"`
	}
	q := withheldEnv(t, "mysite", "booru", fake, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		enrichedHandler(w, r)
	})

	waitJob(t, q, q.EnqueueMetadata(42, "art", "http://mysite/1"))
	if !slices.Equal(body.WithheldTags, []string{"dropped_tag"}) {
		t.Errorf("withheld_tags = %v, want dropped_tag", body.WithheldTags)
	}
}

func TestABookNamesWhatARuleWithheldOnce(t *testing.T) {
	page := func(num int) gdl.Item {
		return gdl.Item{Category: "mybook", Subcategory: "gallery", ID: "9", Num: num,
			Meta: map[string]any{"category": "mybook", "subcategory": "gallery", "id": "9", "num": float64(num), "title": "Book", "tags": []any{"dropped_tag", "kept"}}}
	}
	fake := &fakeRunner{resolved: []gdl.Item{page(1), page(2), page(3)}, writeIdx: []int{0, 1, 2}}
	var withheld string
	q := withheldEnv(t, "mybook", "manga", fake, capturePush(1, map[string]*string{"withheld_tags": &withheld}))

	job := waitJob(t, q, q.Enqueue("http://mybook/g/9", queue.Options{}))
	if job.Summary.Created != 1 {
		t.Fatalf("summary = %+v, items %+v, want one book", job.Summary, job.Items)
	}
	wantWithheld(t, withheld)
}

func TestPipelineLearnsCustomCategoriesBeforeMapping(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "danbooru.json"), []byte(`{"family":"danbooru","auth":"none",
		"post_url_template":"https://danbooru.donmai.us/posts/{id}",
		"tag_rules":{"artist:soandso_(va)":"pipeline_va:soandso"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	item := danbooruPost("1")
	item.Meta["tags_artist"] = []any{"soandso_(va)"}
	var pushed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/galleries":
			_, _ = w.Write([]byte(`[{"name":"default"}]`))
		case "/api/v1/categories":
			_, _ = w.Write([]byte(`[{"name":"pipeline_va"}]`))
		default:
			_ = r.ParseMultipartForm(32 << 20)
			pushed = r.Form["tags"]
			createdHandler(w, r)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Monbooru.APIURL, cfg.Monbooru.APIToken = srv.URL, "tok"
	q := startQueue(t, &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}, cfg, dir, sitestate.New())

	job := waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/1", queue.Options{}))
	if job.Status != queue.JobSucceeded {
		t.Fatalf("job = %s %+v", job.Status, job.Items)
	}
	if !strings.Contains(strings.Join(pushed, " "), "pipeline_va:soandso") || strings.Contains(strings.Join(pushed, " "), "artist:pipeline_va") {
		t.Errorf("pushed tags = %v, want the tag moved to the custom category", pushed)
	}
}

func TestPipelineFailsWhenAnUnresolvedRuleHasOnlyAPartialCategoryList(t *testing.T) {

	if os.Getenv("MONLOADER_TEST_FRESH_CATEGORIES") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
		cmd.Env = append(os.Environ(), "MONLOADER_TEST_FRESH_CATEGORIES=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	var pushes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/galleries":
			_, _ = w.Write([]byte(`[{"name":"default"},{"name":"art"}]`))
		case r.URL.Path == "/api/v1/categories" && r.URL.Query().Get("gallery") == "art":
			http.Error(w, "down", http.StatusServiceUnavailable)
		case r.URL.Path == "/api/v1/categories":
			_, _ = w.Write([]byte(`[{"name":"partial_cat"}]`))
		default:
			pushes.Add(1)
			createdHandler(w, r)
		}
	}))
	defer srv.Close()
	run := func(rule string) *queue.Job {
		cfg := config.Default()
		cfg.Monbooru.APIURL, cfg.Monbooru.APIToken = srv.URL, "tok"
		cfg.Tags.Blacklist = []string{rule}
		q := startQueue(t, &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}, cfg, "", sitestate.New())
		return waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/1", queue.Options{}))
	}
	job := run("silent_gallery_cat:x")
	if job.Status != queue.JobFailed || job.ErrorCode != outcome.ErrCodeMonbooruUnreachable {
		t.Fatalf("job = %s %s, want it held on the missing list", job.Status, job.ErrorCode)
	}
	if pushes.Load() != 0 {
		t.Errorf("pushes = %d, want none", pushes.Load())
	}
	if job := run("partial_cat:x"); job.Status != queue.JobSucceeded {
		t.Errorf("a rule the answering gallery resolves should run: %s %s", job.Status, job.Error)
	}
}

func blacklistEnv(t *testing.T, fake *fakeRunner, rules ...string) (*queue.Queue, *int) {
	t.Helper()
	cfg := config.Default()
	cfg.Tags.Blacklist = rules
	pushes := new(int)
	q := lookupEnvCfg(t, fake, func(w http.ResponseWriter, r *http.Request) {
		*pushes++
		createdHandler(w, r)
	}, cfg, nil, nil)
	return q, pushes
}

func TestPipelineBlacklistSkipsBeforeDownload(t *testing.T) {
	fake := &fakeRunner{honorRangeSet: true, writeIdx: []int{0, 1, 2},
		resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3"), danbooruPost("4"), danbooruPost("5")}}
	q, pushes := blacklistEnv(t, fake, "tag_2", "tag_4")

	job := waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts?tags=a", queue.Options{}))
	if want := "1,3,5"; fake.gotDownloadRng != want {
		t.Errorf("download range = %q, want the kept positions %q", fake.gotDownloadRng, want)
	}
	if job.Status != queue.JobSucceeded || *pushes != 3 {
		t.Errorf("job = %s, pushes = %d, want succeeded with 3", job.Status, *pushes)
	}
	want := queue.Summary{Created: 3, Skipped: 2, Blacklisted: 2, Total: 5}
	if job.Summary != want {
		t.Errorf("summary = %+v, want %+v", job.Summary, want)
	}
	for i, wantOutcome := range []outcome.Outcome{outcome.Created, outcome.SkippedBlacklist, outcome.Created, outcome.SkippedBlacklist, outcome.Created} {
		if job.Items[i].Outcome != wantOutcome {
			t.Errorf("item %d = %s, want %s", i, job.Items[i].Outcome, wantOutcome)
		}
	}
	if got := job.Items[1].MergeNote; got != "global rule: tag_2" {
		t.Errorf("note = %q, want the rule", got)
	}
}

func TestPipelineBlacklistKeepsTheRangeWithNothingExcluded(t *testing.T) {
	fake := &fakeRunner{honorRangeSet: true, writeIdx: []int{0}, resolved: []gdl.Item{danbooruPost("1")}}
	q, _ := blacklistEnv(t, fake, "nothing_has_this")
	waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/1", queue.Options{}))
	if want := "1-200"; fake.gotDownloadRng != want {
		t.Errorf("download range = %q, want the plain window %q", fake.gotDownloadRng, want)
	}
}

func TestPipelineBlacklistPositionsCountFromTheWindow(t *testing.T) {
	fake := &fakeRunner{writeIdx: []int{0, 1},
		resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")}}
	q, _ := blacklistEnv(t, fake, "tag_2")
	waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts?tags=a", queue.Options{MaxItems: 3}))
	if got, want := fake.gotDownloadRng, "1,3"; got != want {
		t.Errorf("first window range = %q, want %q", got, want)
	}
	if got, ok := keptRange("41-43", 40, 3, map[int]string{1: "r"}); !ok || got != "41,43" {
		t.Errorf("later window = %q %v, want 41,43", got, ok)
	}
	if got, ok := keptRange("", 40, 3, map[int]string{0: "r"}); !ok || got != "2-3" {
		t.Errorf("a whole re-resolve counts from 1, got %q %v", got, ok)
	}
	if got, ok := keptRange("", 0, 1, map[int]string{0: "r"}); ok || got != "" {
		t.Errorf("nothing kept = %q %v", got, ok)
	}
}

func TestPipelineAKeptRowTheShiftedRangeMissedFails(t *testing.T) {
	fake := &fakeRunner{writeIdx: []int{0, 1}, honorRangeSet: true, shortStream: 1,
		resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2"), danbooruPost("3")}}
	q, _ := blacklistEnv(t, fake, "tag_2")
	job := waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts?tags=a", queue.Options{MaxItems: 3}))
	if got := job.Items[2]; got.ErrorCode != outcome.ErrCodeDownloadFailed || got.Outcome != outcome.Failed {
		t.Errorf("row = %+v, want it failed and not read as archived", got)
	}
	if job.Summary.Archived != 0 {
		t.Errorf("summary = %+v", job.Summary)
	}
}

func TestPipelineBlacklistEverythingRunsNoDownload(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2")}}
	q, pushes := blacklistEnv(t, fake, "tag_*")
	job := waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts?tags=a", queue.Options{}))
	if fake.downloadCalls != 0 || *pushes != 0 {
		t.Errorf("downloads = %d, pushes = %d, want none", fake.downloadCalls, *pushes)
	}
	if job.Status != queue.JobSucceeded || job.Summary.Blacklisted != 2 || job.Summary.Skipped != 2 || job.Summary.Archived != 0 {
		t.Errorf("job = %s %+v", job.Status, job.Summary)
	}
}

func TestPipelineBlacklistRechecksTheSidecar(t *testing.T) {
	gelbooru := gdl.Item{Category: "gelbooru", Subcategory: "post", ID: "1", Meta: map[string]any{
		"category": "gelbooru", "id": "1", "subcategory": "post", "rating": "g", "tags": "blue_sky",
	}}
	fake := &fakeRunner{resolved: []gdl.Item{gelbooru}, writeIdx: []int{0},
		sidecarAdds: map[string]any{"tags_artist": []any{"paruperu"}, "tags_general": []any{"blue_sky"}}}
	cfg := config.Default()
	cfg.Tags.Blacklist = []string{"artist:paruperu"}
	pushes := 0
	q := lookupEnvCfg(t, fake, func(w http.ResponseWriter, r *http.Request) { pushes++; createdHandler(w, r) }, cfg, nil, nil)

	job := waitJob(t, q, q.Enqueue("https://gelbooru.com/index.php?page=post&s=view&id=1", queue.Options{}))
	if fake.downloadCalls != 1 || pushes != 0 {
		t.Errorf("downloads = %d, pushes = %d, want the file fetched and not pushed", fake.downloadCalls, pushes)
	}
	if job.Items[0].Outcome != outcome.SkippedBlacklist || job.Items[0].MergeNote != "global rule: artist:paruperu" {
		t.Errorf("item = %+v", job.Items[0])
	}
}

func TestPipelineBlacklistLeavesAFileThatLandedAnywayUnpushed(t *testing.T) {

	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2")}, writeIdx: []int{0, 1}}
	q, pushes := blacklistEnv(t, fake, "tag_1")
	job := waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts?tags=a", queue.Options{}))
	if *pushes != 1 || job.Items[0].Outcome != outcome.SkippedBlacklist || job.Items[1].Outcome != outcome.Created {
		t.Errorf("pushes = %d, items = %+v", *pushes, job.Items)
	}
}

func TestPipelineBlacklistDoesNotShiftItemsWithNoID(t *testing.T) {
	noID := func(tag string, num int) gdl.Item {
		return gdl.Item{Category: "wackybooru", Subcategory: "post", Num: num, Meta: map[string]any{
			"category": "wackybooru", "subcategory": "post", "tags": []any{tag},
		}}
	}
	fake := &fakeRunner{honorRangeSet: true, writeIdx: []int{0, 1},
		resolved: []gdl.Item{noID("first", 1), noID("bad", 2), noID("third", 3)}}
	q, pushes := blacklistEnv(t, fake, "bad")
	job := waitJob(t, q, q.Enqueue("https://wacky.example/posts", queue.Options{}))
	if *pushes != 2 {
		t.Errorf("pushes = %d, want 2", *pushes)
	}
	for i, want := range []outcome.Outcome{outcome.Created, outcome.SkippedBlacklist, outcome.Created} {
		if job.Items[i].Outcome != want {
			t.Errorf("item %d = %s, want %s", i, job.Items[i].Outcome, want)
		}
	}
}

func TestPipelineBlacklistSkipsAWholeBook(t *testing.T) {
	pages := []gdl.Item{mangaPage("9", 1), mangaPage("9", 2)}
	fake := &fakeRunner{resolved: pages, writeIdx: []int{0, 1}}
	q, pushes := blacklistEnv(t, fake, "tag_1")
	job := waitJob(t, q, q.Enqueue("https://nhentai.net/g/9/", queue.Options{}))
	if fake.downloadCalls != 0 || *pushes != 0 {
		t.Errorf("downloads = %d, pushes = %d, want none", fake.downloadCalls, *pushes)
	}
	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.SkippedBlacklist {
		t.Errorf("items = %+v, want the book skipped as one", job.Items)
	}
}

func TestPipelineBlacklistSkipsAChapterOnItsOwn(t *testing.T) {
	good := "https://mangadex.org/chapter/good"
	bad := "https://mangadex.org/chapter/bad"
	page := func(tag string) gdl.Item {
		return gdl.Item{Category: "mangadex", Subcategory: "chapter", ID: tag, Num: 1, Meta: map[string]any{
			"category": "mangadex", "id": tag, "num": float64(1), "tags": []any{tag},
		}}
	}
	fake := &fakeRunner{category: "mangadex",
		queue:        []gdl.QueueItem{{URL: good, Meta: map[string]any{"category": "mangadex"}}, {URL: bad, Meta: map[string]any{"category": "mangadex"}}},
		chapterPages: map[string][]gdl.Item{good: {page("fine")}, bad: {page("nope")}}}
	q, pushes := blacklistEnv(t, fake, "nope")
	job := waitJob(t, q, q.Enqueue("https://mangadex.org/title/x", queue.Options{}))
	if *pushes != 1 || job.Items[0].Outcome != outcome.Created || job.Items[1].Outcome != outcome.SkippedBlacklist {
		t.Errorf("pushes = %d, items = %+v", *pushes, job.Items)
	}
}

func TestPipelineBlacklistRechecksADeepPass(t *testing.T) {
	leaf := func(id, tag string) gdl.Item {
		return gdl.Item{Category: "imagehost", ID: id, Meta: map[string]any{"category": "imagehost", "id": id, "tags": []any{tag}}}
	}
	fake := &fakeRunner{category: "forum", queue: []gdl.QueueItem{{URL: "https://x/1", Meta: map[string]any{"category": "forum"}}},
		deepItems: []gdl.Item{leaf("1", "fine"), leaf("2", "nope")}, writeIdx: []int{0, 1}}
	q, pushes := blacklistEnv(t, fake, "nope")
	job := waitJob(t, q, q.Enqueue("https://forum.example/thread/1", queue.Options{}))
	if strings.Contains(fake.gotDownloadRng, ",") {
		t.Errorf("a deep download range = %q must stay a plain chapter window", fake.gotDownloadRng)
	}
	if *pushes != 1 || job.Items[1].Outcome != outcome.SkippedBlacklist {
		t.Errorf("pushes = %d, items = %+v", *pushes, job.Items)
	}
}

func TestPipelineBlacklistStaysOffARefetch(t *testing.T) {
	fake := &fakeRunner{fetchMeta: map[string]any{"category": "danbooru", "id": "1", "rating": "g", "tags_general": []any{"nope"}}}
	cfg := config.Default()
	cfg.Tags.Blacklist = []string{"nope"}
	q := lookupEnvCfg(t, fake, enrichedHandler, cfg, nil, nil)
	job := waitJob(t, q, q.EnqueueMetadata(5, "art", "https://danbooru.donmai.us/posts/1"))
	wantOneEnriched(t, job)
}

func TestPipelineRecordsWhatWasSentAndHeld(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1")}, writeIdx: []int{0}}
	handler := func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusCreated, map[string]any{
			"image":         map[string]any{"id": 5},
			"rejected_tags": []string{"watermark"},
		})
	}
	q := testEnv(t, fake, handler)

	job := waitJob(t, q, q.Enqueue("http://danbooru/posts/1", queue.Options{}))
	if len(job.Items) != 1 {
		t.Fatalf("items = %v", job.Items)
	}
	it := job.Items[0]
	if it.TagsSent == 0 || len(it.Held) != 1 || it.Held[0] != "watermark (rejected by the gallery)" {
		t.Errorf("item sent %d held %v, want the pushed count and the gallery's rejection phrased", it.TagsSent, it.Held)
	}
	if heldTags([]string{"meta:highres"}, nil)[0] != "meta:highres (held by your rules)" {
		t.Error("a withheld tag should read as held by the rules")
	}
}
