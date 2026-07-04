package gdl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/queue"
)

func TestParseResolveItems(t *testing.T) {
	const j = `[[3,"https://cdn.donmai.us/x.jpg",{"category":"danbooru","subcategory":"post","id":11474309,"rating":"g"}]]`
	res, err := parseResolve([]byte(j))
	if err != nil {
		t.Fatalf("parseResolve: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(res.Items))
	}
	it := res.Items[0]
	if it.Category != "danbooru" || it.Subcategory != "post" || it.ID != "11474309" {
		t.Errorf("item fields wrong: %+v", it)
	}
	if it.Meta["rating"] != "g" {
		t.Errorf("meta not carried through: rating=%v", it.Meta["rating"])
	}
}

func TestParseResolvePoolOrder(t *testing.T) {
	const j = `[
		[3,"https://cdn.donmai.us/1.jpg",{"category":"danbooru","subcategory":"pool","id":100001,"num":1}],
		[3,"https://cdn.donmai.us/2.jpg",{"category":"danbooru","subcategory":"pool","id":100002,"num":2}],
		[3,"https://cdn.donmai.us/3.jpg",{"category":"danbooru","subcategory":"pool","id":100003,"num":3}]
	]`
	res, err := parseResolve([]byte(j))
	if err != nil {
		t.Fatalf("parseResolve: %v", err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(res.Items))
	}
	for i, want := range []int{1, 2, 3} {
		if res.Items[i].Num != want {
			t.Errorf("item %d num = %d, want %d", i, res.Items[i].Num, want)
		}
		if res.Items[i].Subcategory != "pool" {
			t.Errorf("item %d subcategory = %q, want pool", i, res.Items[i].Subcategory)
		}
	}
}

func TestParseResolveSurfacesDataJobError(t *testing.T) {
	// gallery-dl reports an extraction error as [-1, {error, message}] while
	// still exiting 0; parseResolve must surface it (classified) instead of
	// returning a misleading empty success.
	const j = `[[-1,{"error":"AuthRequired","message":"'api-key' & 'user-id' needed to access the API ('Missing authentication')"}]]`
	res, err := parseResolve([]byte(j))
	if err == nil {
		t.Fatalf("expected an error, got %d items", len(res.Items))
	}
	var ge *queue.CodedError
	if e, ok := err.(*queue.CodedError); ok {
		ge = e
	}
	if ge == nil || ge.Code != queue.ErrCodeAuthRequired {
		t.Errorf("error = %v, want code %s", err, queue.ErrCodeAuthRequired)
	}
}

func TestFirstPostMeta(t *testing.T) {
	// The refetch path reads the first post's full kwdict from a -j document.
	const j = `[[3,"https://cdn.donmai.us/x.jpg",{"category":"danbooru","id":11474309,"md5":"abc"}]]`
	meta, err := firstPostMeta([]byte(j))
	if err != nil {
		t.Fatalf("firstPostMeta: %v", err)
	}
	if meta["md5"] != "abc" {
		t.Errorf("meta not carried through: md5=%v", meta["md5"])
	}

	// No post at all is a mapping failure, not a silent nil.
	_, err = firstPostMeta([]byte(`[]`))
	var ce *queue.CodedError
	if e, ok := err.(*queue.CodedError); ok {
		ce = e
	}
	if ce == nil || ce.Code != queue.ErrCodeMappingFailed {
		t.Errorf("empty document error = %v, want code %s", err, queue.ErrCodeMappingFailed)
	}

	// A classified extraction error keeps its code instead of flattening to
	// mapping_failed.
	const authErr = `[[-1,{"error":"AuthRequired","message":"'api-key' needed ('Missing authentication')"}]]`
	_, err = firstPostMeta([]byte(authErr))
	ce = nil
	if e, ok := err.(*queue.CodedError); ok {
		ce = e
	}
	if ce == nil || ce.Code != queue.ErrCodeAuthRequired {
		t.Errorf("extraction error = %v, want code %s", err, queue.ErrCodeAuthRequired)
	}
}

func TestParseResolveQueueDispatch(t *testing.T) {
	// A dispatcher URL (a forum thread, a manga title) emits Message.Queue
	// handoffs (type 6) the -j pass lists but does not follow. They land in
	// Queue, not Items, with the parent category captured for routing.
	const j = `[
		[2,{"category":"mangadex","subcategory":"manga"}],
		[6,"https://example.com/chapter/aaa",{"category":"mangadex","subcategory":"manga"}],
		[6,"https://example.com/chapter/bbb",{"category":"mangadex","subcategory":"manga"}]
	]`
	res, err := parseResolve([]byte(j))
	if err != nil {
		t.Fatalf("parseResolve: %v", err)
	}
	if len(res.Items) != 0 {
		t.Errorf("got %d items, want 0 (a dispatcher yields no files)", len(res.Items))
	}
	if len(res.Queue) != 2 {
		t.Fatalf("got %d queue handoffs, want 2", len(res.Queue))
	}
	if res.Queue[0].URL != "https://example.com/chapter/aaa" {
		t.Errorf("queue url = %q, want the chapter url", res.Queue[0].URL)
	}
	if res.Category != "mangadex" {
		t.Errorf("category = %q, want mangadex", res.Category)
	}
}

func TestParseExtractors(t *testing.T) {
	// The combined "Category: <cat> - Subcategory: <sub>" line and an instance
	// extractor's blank category are the two shapes the parser must handle.
	const list = `DanbooruPoolExtractor
Extractor for posts from danbooru pools
Category:  - Subcategory: pool
Example : https://example.com/pools/7659

PixivWorkExtractor
Category: pixiv - Subcategory: work
Example : https://example.com/en/artworks/966412
`
	ex := parseExtractors([]byte(list))
	if len(ex) != 2 {
		t.Fatalf("got %d extractors, want 2", len(ex))
	}
	find := func(cat, sub string) *Extractor {
		for i := range ex {
			if ex[i].Category == cat && ex[i].Subcategory == sub {
				return &ex[i]
			}
		}
		return nil
	}
	if pixiv := find("pixiv", "work"); pixiv == nil {
		t.Error("pixiv/work extractor not parsed")
	} else if pixiv.Example == "" {
		t.Error("pixiv/work extractor has no example URL")
	}
	// An instance extractor (danbooru) prints a blank category; its subcategory
	// and example must still come through rather than being mangled or dropped.
	if pool := find("", "pool"); pool == nil {
		t.Error("blank-category pool extractor not parsed")
	} else if pool.Example == "" {
		t.Error("blank-category pool extractor has no example URL")
	}
}

func TestProbeFromError(t *testing.T) {
	cases := []struct {
		code string
		want ProbeStatus
	}{
		{queue.ErrCodeAuthRequired, ProbeAuthRequired},
		{queue.ErrCodeBlocked, ProbeBlocked},
		{queue.ErrCodeRateLimited, ProbeFailed},
		{queue.ErrCodeDownloadFailed, ProbeFailed},
	}
	for _, tc := range cases {
		got := probeFromError(&queue.CodedError{Code: tc.code, Msg: "detail"})
		if got.Status != tc.want {
			t.Errorf("probeFromError(%s) = %s, want %s", tc.code, got.Status, tc.want)
		}
		if got.Detail != "detail" {
			t.Errorf("probeFromError(%s) dropped the detail: %q", tc.code, got.Detail)
		}
	}
}

func TestConfigArgs(t *testing.T) {
	cfg := config.Default()
	// No config path -> no -c flag.
	cfg.GalleryDL.ConfigPath = ""
	if args := New(cfg, nil, nil, nil).configArgs(); args != nil {
		t.Errorf("empty config path: args = %v, want nil", args)
	}
	// A path that does not exist yet -> still no -c (the stat fails).
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "missing.json")
	if args := New(cfg, nil, nil, nil).configArgs(); args != nil {
		t.Errorf("missing config: args = %v, want nil", args)
	}
	// An existing managed config -> passed via -c.
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gdl.json")
	if err := WriteManagedConfig(cfg, []string{"konachan"}, nil, nil); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	if args := New(cfg, nil, nil, nil).configArgs(); !reflect.DeepEqual(args, []string{"-c", cfg.GalleryDL.ConfigPath}) {
		t.Errorf("configArgs = %v, want [-c %s]", args, cfg.GalleryDL.ConfigPath)
	}
}

func TestResolveOffArgs(t *testing.T) {
	// Each enrichment option gets its own per-site override so the resolve pass
	// skips the extra fetches; empty lists add nothing.
	got := resolveOffArgs([]string{"safebooru", "konachan"}, []string{"danbooru", "e621"}, []string{"safebooru"})
	want := []string{
		"-o", "extractor.safebooru.tags=false", "-o", "extractor.konachan.tags=false",
		"-o", "extractor.danbooru.metadata=false", "-o", "extractor.e621.metadata=false",
		"-o", "extractor.safebooru.notes=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveOffArgs = %v, want %v", got, want)
	}
	if got := resolveOffArgs(nil, nil, nil); got != nil {
		t.Errorf("resolveOffArgs(nil) = %v, want nil", got)
	}
}

func TestDownloadArgs(t *testing.T) {
	// Destination and range precede the URL; the archive is left in place.
	got := downloadArgs("/work", "1-3", "http://x", false, false)
	if want := []string{"-D", "/work", "--range", "1-3", "http://x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("downloadArgs = %v, want %v", got, want)
	}
	// A forced run adds `-o archive=` before the URL so gallery-dl ignores the
	// download-archive and re-fetches already-recorded posts.
	forced := downloadArgs("/work", "", "http://x", true, false)
	if want := []string{"-D", "/work", "-o", "archive=", "http://x"}; !reflect.DeepEqual(forced, want) {
		t.Errorf("forced downloadArgs = %v, want %v", forced, want)
	}
	// A deep (dispatcher) download bounds the child window with --chapter-range.
	deep := downloadArgs("/work", "1-3", "http://x", false, true)
	if want := []string{"-D", "/work", "--chapter-range", "1-3", "http://x"}; !reflect.DeepEqual(deep, want) {
		t.Errorf("deep downloadArgs = %v, want %v", deep, want)
	}
}

func TestReportDownloads(t *testing.T) {
	dir := t.TempDir()
	writeDownloaded(t, dir, "safebooru", 42, "p1.jpg")
	p1 := filepath.Join(dir, "p1.jpg")
	// Results come back in source order: an archive-skip line (`# <path>`) yields
	// a skip entry, a written-file line its sidecar; onFile fires only for the
	// written file, at its position.
	input := "# " + filepath.Join(dir, "p0.jpg") + "\n" + p1 + "\n"
	var streamed []int
	out := reportDownloads(strings.NewReader(input), func(i int, _ Downloaded) { streamed = append(streamed, i) })
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	if !out[0].Skipped {
		t.Errorf("result 0 should be a skip, got %+v", out[0])
	}
	if out[1].Skipped || out[1].Meta["category"] != "safebooru" || out[1].Path != p1 {
		t.Errorf("result 1 = %+v, want the safebooru file at %s", out[1], p1)
	}
	if len(streamed) != 1 || streamed[0] != 1 {
		t.Errorf("onFile positions = %v, want [1]", streamed)
	}
}

// writeDownloaded simulates a gallery-dl download: a file plus its `.json`
// metadata sidecar.
func writeDownloaded(t *testing.T, dir, category string, id int, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"category": category, "id": id}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		stderr string
		want   string
	}{
		{"No suitable extractor found", queue.ErrCodeUnsupportedURL},
		{"unsupported URL", queue.ErrCodeUnsupportedURL},
		{"HTTP 401: missing authentication", queue.ErrCodeAuthRequired},
		// A bare 403 (hotlink/forbidden) is not a missing credential; only a 403
		// that names an auth need is.
		{"403 Forbidden", queue.ErrCodeDownloadFailed},
		{"403 Forbidden: Login Required", queue.ErrCodeAuthRequired},
		// A Cloudflare/bot-protection 403 is a wall, not a missing credential.
		{"ChallengeError: Cloudflare challenge (403 Forbidden)", queue.ErrCodeBlocked},
		{"HTTP 429: too many requests", queue.ErrCodeRateLimited},
		{"rate limit exceeded", queue.ErrCodeRateLimited},
		// DNS / unreachable-host is the downloader's network; a refused connection
		// (the host answered) is not, and stays download_failed.
		{"NameResolutionError: Failed to resolve 'cdnc.example.com' ([Errno -3] Temporary failure in name resolution)", queue.ErrCodeNetworkUnreachable},
		{"ConnectionError: [Errno 101] Network is unreachable", queue.ErrCodeNetworkUnreachable},
		{"connection refused", queue.ErrCodeDownloadFailed},
		{"", queue.ErrCodeDownloadFailed},
	}
	for _, tc := range cases {
		got := classifyError(1, tc.stderr)
		if got.Code != tc.want {
			t.Errorf("classifyError(%q) = %s, want %s", tc.stderr, got.Code, tc.want)
		}
	}
}

func TestWriteManagedConfig(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = "/config/archive.sqlite"
	cfg.GalleryDL.SleepRequest = 1.5
	cfg.GalleryDL.RawConfig = `{"extractor":{"timeout":30,"gelbooru":{"sleep":2}}}`
	cfg.Sites = []config.Site{
		{Name: "gelbooru", APIKey: "K", UserID: "U", Gallery: "art"},
		{Name: "e621", Username: "user", APIKey: "k2", Gallery: "furry"},
		{Name: "danbooru", Username: "solo"},
	}
	flatTag := []string{"gelbooru", "safebooru", "konachan", "yandere"}

	if err := WriteManagedConfig(cfg, flatTag, []string{"danbooru", "e621"}, []string{"gelbooru", "safebooru", "konachan"}); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	data, err := os.ReadFile(cfg.GalleryDL.ConfigPath)
	if err != nil {
		t.Fatalf("read managed config: %v", err)
	}
	var doc struct {
		Extractor map[string]json.RawMessage `json:"extractor"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("managed config is not valid JSON: %v", err)
	}

	asMap := func(key string) map[string]any {
		var m map[string]any
		if raw, ok := doc.Extractor[key]; ok {
			_ = json.Unmarshal(raw, &m)
		}
		return m
	}

	gel := asMap("gelbooru")
	if gel["api-key"] != "K" || gel["user-id"] != "U" || gel["tags"] != true {
		t.Errorf("gelbooru block wrong: %+v", gel)
	}
	if gel["notes"] != true {
		t.Errorf("gelbooru (notes site) should have notes:true, got %+v", gel)
	}
	if gel["sleep"] != float64(2) {
		t.Errorf("raw passthrough did not merge into gelbooru: %+v", gel)
	}
	e621 := asMap("e621")
	// The danbooru/e621 family signs in by HTTP Basic Auth, so the key must also
	// land in "password" or gallery-dl prompts for one and aborts the extraction.
	if e621["username"] != "user" || e621["api-key"] != "k2" || e621["password"] != "k2" {
		t.Errorf("e621 block wrong: %+v", e621)
	}
	if _, hasTags := e621["tags"]; hasTags {
		t.Error("e621 is not a flat-tag family; it should not get tags:true")
	}
	// e621 reads the same include as danbooru for its notes; the commentary
	// token is ignored there.
	if e621["metadata"] != "artist_commentary,notes" {
		t.Errorf("e621 (metadata site) should get the metadata include, got %+v", e621)
	}
	// A username with no key must not be sent alone, or the danbooru family
	// prompts for a password and aborts the extraction.
	dan := asMap("danbooru")
	if _, hasUser := dan["username"]; hasUser {
		t.Error("a username without a key should not be written")
	}
	if dan["metadata"] != "artist_commentary,notes" {
		t.Errorf("danbooru should get the commentary/notes metadata include, got %+v", dan)
	}
	kon := asMap("konachan")
	if kon["tags"] != true || kon["notes"] != true {
		t.Errorf("konachan (flat-tag notes site, no creds) should have tags:true and notes:true, got %+v", kon)
	}
	yan := asMap("yandere")
	if _, hasNotes := yan["notes"]; hasNotes {
		t.Error("yandere was not passed as a notes site; it should not get notes:true")
	}
	// directlink's default filename embeds the host and path; with the directory
	// flattened to the workdir that overflows the name limit on long URLs.
	dl := asMap("directlink")
	if dl["filename"] != "{filename}.{extension}" {
		t.Errorf("directlink should override filename to drop the host/path, got %+v", dl)
	}
	// archive, sleep-request, postprocessors, and the merged top-level raw key.
	var full map[string]any
	_ = json.Unmarshal(data, &full)
	ext := full["extractor"].(map[string]any)
	if ext["archive"] != "/config/archive.sqlite" {
		t.Errorf("archive = %v", ext["archive"])
	}
	if ext["sleep-request"] != 1.5 {
		t.Errorf("sleep-request = %v, want 1.5", ext["sleep-request"])
	}
	if ext["sleep"] != 1.5 {
		t.Errorf("sleep = %v, want 1.5 (throttle file downloads, not just listing)", ext["sleep"])
	}
	if ext["timeout"] != float64(30) {
		t.Errorf("raw top-level extractor.timeout not merged: %v", ext["timeout"])
	}
	if _, ok := ext["postprocessors"]; !ok {
		t.Error("metadata postprocessor missing")
	}
}

func TestWriteManagedConfigCapsDeadHost(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	if err := WriteManagedConfig(cfg, nil, nil, nil); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	data, err := os.ReadFile(cfg.GalleryDL.ConfigPath)
	if err != nil {
		t.Fatalf("read managed config: %v", err)
	}
	var full map[string]any
	if err := json.Unmarshal(data, &full); err != nil {
		t.Fatalf("managed config is not valid JSON: %v", err)
	}
	ext := full["extractor"].(map[string]any)
	if ext["timeout"] != 20.0 {
		t.Errorf("timeout = %v, want 20 (cap a dead host)", ext["timeout"])
	}
	if ext["retries"] != float64(2) {
		t.Errorf("retries = %v, want 2", ext["retries"])
	}
}

func TestWriteManagedConfigRejectsBadRaw(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.RawConfig = "{not json"
	if err := WriteManagedConfig(cfg, nil, nil, nil); err == nil {
		t.Error("expected an error for invalid raw config JSON")
	}
}
