package gdl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
)

func mustResolve(t *testing.T, j string) ResolveResult {
	t.Helper()
	res, err := parseResolve([]byte(j))
	if err != nil {
		t.Fatalf("parseResolve: %v", err)
	}
	return res
}

func TestParseResolveItems(t *testing.T) {
	const j = `[[3,"https://cdn.donmai.us/x.jpg",{"category":"danbooru","subcategory":"post","id":11474309,"rating":"g"}]]`
	res := mustResolve(t, j)
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

func TestParseResolveKeepsAnErrorThatEndedTheListing(t *testing.T) {
	const j = `[[3,"https://cdn/a.jpg",{"id":1,"category":"danbooru"}],[-1,{"error":"HttpError","message":"'401 Unauthorized' for 'https://danbooru.donmai.us/posts.json?page=b1'"}]]`
	res, err := parseResolve([]byte(j))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %d, want the row before the error", len(res.Items))
	}
	var ce *outcome.CodedError
	if !errors.As(res.Err, &ce) || ce.Code != outcome.ErrCodeAuthRequired {
		t.Errorf("Err = %v, want %s", res.Err, outcome.ErrCodeAuthRequired)
	}
}

func TestParseResolveKeepsBigIDsExact(t *testing.T) {
	const j = `[[3,"https://64.media.tumblr.com/x.jpg",{"category":"tumblr","id":735284192837419123,"num":2}]]`
	res := mustResolve(t, j)
	if it := res.Items[0]; it.ID != "735284192837419123" || it.Num != 2 {
		t.Errorf("id = %q num = %d, want 735284192837419123 and 2", it.ID, it.Num)
	}
}

func TestParseResolvePoolOrder(t *testing.T) {
	const j = `[
		[3,"https://cdn.donmai.us/1.jpg",{"category":"danbooru","subcategory":"pool","id":100001,"num":1}],
		[3,"https://cdn.donmai.us/2.jpg",{"category":"danbooru","subcategory":"pool","id":100002,"num":2}],
		[3,"https://cdn.donmai.us/3.jpg",{"category":"danbooru","subcategory":"pool","id":100003,"num":3}]
	]`
	res := mustResolve(t, j)
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
	const j = `[[-1,{"error":"AuthRequired","message":"'api-key' & 'user-id' needed to access the API ('Missing authentication')"}]]`
	res, err := parseResolve([]byte(j))
	if err == nil {
		t.Fatalf("expected an error, got %d items", len(res.Items))
	}
	var ge *outcome.CodedError
	if e, ok := err.(*outcome.CodedError); ok {
		ge = e
	}
	if ge == nil || ge.Code != outcome.ErrCodeAuthRequired {
		t.Errorf("error = %v, want code %s", err, outcome.ErrCodeAuthRequired)
	}
}

func TestFirstPostMeta(t *testing.T) {
	const j = `[[3,"https://cdn.donmai.us/x.jpg",{"category":"danbooru","id":11474309,"md5":"abc"}]]`
	meta, err := firstPostMeta([]byte(j))
	if err != nil {
		t.Fatalf("firstPostMeta: %v", err)
	}
	if meta["md5"] != "abc" {
		t.Errorf("meta not carried through: md5=%v", meta["md5"])
	}

	_, err = firstPostMeta([]byte(`[]`))
	var ce *outcome.CodedError
	if e, ok := err.(*outcome.CodedError); ok {
		ce = e
	}
	if ce == nil || ce.Code != outcome.ErrCodeMappingFailed {
		t.Errorf("empty document error = %v, want code %s", err, outcome.ErrCodeMappingFailed)
	}

	const listing = `[[6,"https://mangadex.org/chapter/x",{"category":"mangadex","chapter":1}]]`
	_, err = firstPostMeta([]byte(listing))
	ce = nil
	if e, ok := err.(*outcome.CodedError); ok {
		ce = e
	}
	if ce == nil || ce.Code != outcome.ErrCodeMappingFailed || !strings.Contains(ce.Msg, "lists galleries or chapters") {
		t.Errorf("listing error = %v, want it to say the URL lists galleries", err)
	}

	const authErr = `[[-1,{"error":"AuthRequired","message":"'api-key' needed ('Missing authentication')"}]]`
	_, err = firstPostMeta([]byte(authErr))
	ce = nil
	if e, ok := err.(*outcome.CodedError); ok {
		ce = e
	}
	if ce == nil || ce.Code != outcome.ErrCodeAuthRequired {
		t.Errorf("extraction error = %v, want code %s", err, outcome.ErrCodeAuthRequired)
	}
}

func TestParseResolveQueueDispatch(t *testing.T) {
	const j = `[
		[2,{"category":"mangadex","subcategory":"manga"}],
		[6,"https://example.com/chapter/aaa",{"category":"mangadex","subcategory":"manga"}],
		[6,"https://example.com/chapter/bbb",{"category":"mangadex","subcategory":"manga"}]
	]`
	res := mustResolve(t, j)
	if len(res.Items) != 0 {
		t.Errorf("got %d items, want 0 (a dispatcher yields no files)", len(res.Items))
	}
	if len(res.Queue) != 2 {
		t.Fatalf("got %d queue handoffs, want 2", len(res.Queue))
	}
	if res.Queue[0].URL != "https://example.com/chapter/aaa" {
		t.Errorf("queue url = %q, want the chapter url", res.Queue[0].URL)
	}
	if res.Category != "mangadex" || res.Subcategory != "manga" {
		t.Errorf("category = %q/%q, want mangadex/manga", res.Category, res.Subcategory)
	}
}

func TestParseResolveQueueDropsMalformedURL(t *testing.T) {
	const j = `[
		[6,123,{"category":"mangadex","subcategory":"manga"}],
		[6,null,{"category":"mangadex","subcategory":"manga"}],
		[6,"https://example.com/chapter/aaa",{"category":"mangadex","subcategory":"manga"}]
	]`
	res := mustResolve(t, j)
	if len(res.Queue) != 1 {
		t.Fatalf("got %d queue handoffs, want only the well-formed one", len(res.Queue))
	}
	if res.Queue[0].URL != "https://example.com/chapter/aaa" {
		t.Errorf("queue url = %q, want the chapter url", res.Queue[0].URL)
	}
}

func TestParseExtractors(t *testing.T) {
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
		{outcome.ErrCodeAuthRequired, ProbeAuthRequired},
		{outcome.ErrCodeBlocked, ProbeBlocked},
		{outcome.ErrCodeRateLimited, ProbeRateLimited},
		{outcome.ErrCodeDownloadFailed, ProbeFailed},
	}
	for _, tc := range cases {
		got := ProbeFromError(&outcome.CodedError{Code: tc.code, Msg: "detail"})
		if got.Status != tc.want {
			t.Errorf("ProbeFromError(%s) = %s, want %s", tc.code, got.Status, tc.want)
		}
		if got.Detail != "detail" {
			t.Errorf("ProbeFromError(%s) dropped the detail: %q", tc.code, got.Detail)
		}
	}
}

func TestConfigArgs(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = ""
	if args := New(config.NewProvider(cfg), SiteLists{}).configArgs(); !reflect.DeepEqual(args, []string{"--config-ignore"}) {
		t.Errorf("empty config path: args = %v, want nil", args)
	}
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "missing.json")
	if args := New(config.NewProvider(cfg), SiteLists{}).configArgs(); !reflect.DeepEqual(args, []string{"--config-ignore"}) {
		t.Errorf("missing config: args = %v, want nil", args)
	}
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gdl.json")
	if err := WriteManagedConfig(cfg, SiteLists{FlatTag: []string{"konachan"}}); err != nil {
		t.Fatalf("WriteManagedConfig: %v", err)
	}
	if args := New(config.NewProvider(cfg), SiteLists{}).configArgs(); !reflect.DeepEqual(args, []string{"--config-ignore", "-c", cfg.GalleryDL.ConfigPath}) {
		t.Errorf("configArgs = %v, want [--config-ignore -c %s]", args, cfg.GalleryDL.ConfigPath)
	}
}

func TestResolveOffArgs(t *testing.T) {
	got := resolveOffArgs(SiteLists{FlatTag: []string{"safebooru", "konachan"}, Metadata: []string{"danbooru", "e621"}, Notes: []string{"safebooru"}})
	want := []string{
		"-o", "extractor.safebooru.tags=false", "-o", "extractor.konachan.tags=false",
		"-o", "extractor.danbooru.metadata=false", "-o", "extractor.e621.metadata=false",
		"-o", "extractor.safebooru.notes=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveOffArgs = %v, want %v", got, want)
	}
	if got := resolveOffArgs(SiteLists{}); got != nil {
		t.Errorf("resolveOffArgs(nil) = %v, want nil", got)
	}
}

func TestDownloadArgs(t *testing.T) {
	got := downloadArgs("/work", "1-3", "http://x", ArchiveRead, false)
	if want := []string{"-D", "/work", "--range", "1-3", "--", "http://x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("downloadArgs = %v, want %v", got, want)
	}
	forced := downloadArgs("/work", "", "http://x", ArchiveBypass, false)
	if want := []string{"-D", "/work", "-o", "archive=", "--", "http://x"}; !reflect.DeepEqual(forced, want) {
		t.Errorf("forced downloadArgs = %v, want %v", forced, want)
	}
	deep := downloadArgs("/work", "1-3", "http://x", ArchiveRead, true)
	if want := []string{"-D", "/work", "--chapter-range", "1-3", "--", "http://x"}; !reflect.DeepEqual(deep, want) {
		t.Errorf("deep downloadArgs = %v, want %v", deep, want)
	}
	catchUp := downloadArgs("/work", "1-3", "http://x", ArchiveCatchUp, false)
	if want := []string{"-D", "/work", "--range", "1-3", "--", "http://x"}; !reflect.DeepEqual(catchUp, want) {
		t.Errorf("catch-up downloadArgs = %v, want %v", catchUp, want)
	}
}

func TestPositionRange(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{[]int{1, 2, 3, 4, 6, 9, 10, 11}, "1-4,6,9-11"},
		{[]int{7}, "7"},
		{[]int{1, 3, 5}, "1,3,5"},
		{nil, ""},
	} {
		if got := PositionRange(tc.in); got != tc.want {
			t.Errorf("PositionRange(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func stubGDL(t *testing.T, dir, script string) (string, *config.Config) {
	t.Helper()
	stub := filepath.Join(dir, "gallery-dl")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = stub
	return stub, cfg
}

func TestEveryRunEndsOptionsBeforeTheURL(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	_, cfg := stubGDL(t, dir, "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> "+argv+"; done\necho '[]'\n")
	tool := New(config.NewProvider(cfg), SiteLists{})
	const url = "-o=evil"
	for name, run := range map[string]func(){
		"resolve": func() { _, _ = tool.Resolve(context.Background(), url, "", false) },
		"fetch":   func() { _, _ = tool.FetchMeta(context.Background(), url) },
		"first":   func() { _, _ = tool.FirstPost(context.Background(), url) },
		"probe":   func() { _, _ = tool.Probe(context.Background(), url) },
		"debug":   func() { _, _, _ = tool.DebugLog(context.Background(), url, filepath.Join(dir, "work")) },
	} {
		_ = os.Remove(argv)
		run()
		data, err := os.ReadFile(argv)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		args := strings.Split(strings.TrimSpace(string(data)), "\n")
		if n := len(args); n < 2 || args[n-2] != "--" || args[n-1] != url {
			t.Errorf("%s argv ends %v, want -- then the URL", name, args)
		}
	}
}

func TestReportDownloads(t *testing.T) {
	dir := t.TempDir()
	writeDownloaded(t, dir, "safebooru", 42, "p1.jpg")
	p1 := filepath.Join(dir, "p1.jpg")
	input := "# " + filepath.Join(dir, "p0.jpg") + "\n" + p1 + "\n"
	var streamed []Downloaded
	out, _ := reportDownloads(strings.NewReader(input), dir, func(d Downloaded) { streamed = append(streamed, d) }, nil)
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	if !out[0].Skipped {
		t.Errorf("result 0 should be a skip, got %+v", out[0])
	}
	if out[1].Skipped || out[1].Meta["category"] != "safebooru" || out[1].Path != p1 {
		t.Errorf("result 1 = %+v, want the safebooru file at %s", out[1], p1)
	}
	if len(streamed) != 1 || streamed[0].Path != p1 {
		t.Errorf("onFile saw %v, want only the file at %s", streamed, p1)
	}
}

func TestReportDownloadsReadsASkipsSidecar(t *testing.T) {
	dir := t.TempDir()
	writeDownloaded(t, dir, "danbooru", 7, "p7.png")
	if err := os.Remove(filepath.Join(dir, "p7.png")); err != nil {
		t.Fatal(err)
	}
	out, _ := reportDownloads(strings.NewReader("# "+filepath.Join(dir, "p7.png")+"\n"), dir, nil, nil)
	if len(out) != 1 || !out[0].Skipped || out[0].Meta["id"] != json.Number("7") {
		t.Errorf("got %+v, want a skip carrying post 7's metadata", out)
	}
}

func TestManagedConfigWritesASidecarForASkip(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	data := readManaged(t, cfg, WriteManagedConfig(cfg, SiteLists{}))
	if !strings.Contains(string(data), `"event": "file,skip"`) {
		t.Errorf("managed config = %s, want the metadata postprocessor on skip too", data)
	}
}

func TestAPersonalGalleryDLConfigStaysUnread(t *testing.T) {
	bin, err := exec.LookPath("gallery-dl")
	if err != nil {
		t.Skip("gallery-dl not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	personal := filepath.Join(home, "personal.sqlite3")
	dir := filepath.Join(home, ".config", "gallery-dl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"extractor":{"directlink":{"archive":%q}}}`, personal)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("bytes")) }))
	defer srv.Close()

	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(t.TempDir(), "archive.sqlite")
	cfg.GalleryDL.SleepRequest = 0
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.NewProvider(cfg), SiteLists{}).Download(context.Background(), srv.URL+"/a.png", "", t.TempDir(), ArchiveRead, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(personal); err == nil {
		t.Error("the run wrote the operator's personal archive")
	}
}

func TestIngestableFollowsTheContractList(t *testing.T) {
	for name, want := range map[string]bool{"a.png": true, "A.JPG": true, "b.zip": true, "c.cbz": true, "d.bmp": false, "e": false} {
		if got := Ingestable(name); got != want {
			t.Errorf("Ingestable(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseExtractorInfo(t *testing.T) {
	out := []byte("Category / Subcategory / Basecategory\n  \"danbooru\" / \"post\" / \"Danbooru\"\n\nFilename format (default):\n  \"{id}\"\n")
	if cat, sub := parseExtractorInfo(out); cat != "danbooru" || sub != "post" {
		t.Errorf("parseExtractorInfo = %q, %q", cat, sub)
	}
	if cat, sub := parseExtractorInfo([]byte("[gallery-dl][error] No suitable extractor")); cat != "" || sub != "" {
		t.Errorf("no extractor = %q, %q, want none", cat, sub)
	}
}

type versionRunner struct {
	Runner
	version string
}

func (r versionRunner) Version(context.Context) string { return r.version }

func (r versionRunner) ListExtractors(context.Context) ([]Extractor, error) {
	return nil, errors.New("no listing")
}

func TestARefreshOnADeadContextKeepsTheVersion(t *testing.T) {
	cfg := config.Default()
	root := t.TempDir()
	c := NewCatalog("1.32.1", "1.32.1", false, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Refresh(ctx, versionRunner{}, cfg, root)
	if c.Version() != "1.32.1" {
		t.Errorf("version after a probe on a dead context = %q, want it kept", c.Version())
	}

	c.Refresh(context.Background(), versionRunner{}, cfg, root)
	if c.Version() != "" {
		t.Errorf("version with the binary really gone = %q, want none", c.Version())
	}
	c.Refresh(context.Background(), versionRunner{version: "1.32.2"}, cfg, root)
	if c.Version() != "1.32.2" {
		t.Errorf("version = %q, want the new one", c.Version())
	}
}

func TestARefreshKeepsTheExtractorsOnlyWhileABinaryAnswers(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name    string
		ctx     context.Context
		version string
		want    int
	}{
		{"a dead context says nothing", dead, "", 1},
		{"a binary that answers keeps them", context.Background(), "1.32.2", 1},
		{"a binary that is gone clears them", context.Background(), "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cat := NewCatalog("1.32.1", "1.32.1", false, []Extractor{{Category: "site_a"}}, nil)
			cat.Refresh(c.ctx, versionRunner{version: c.version}, config.Default(), t.TempDir())
			if got := len(cat.Extractors()); got != c.want {
				t.Errorf("extractors after a failed listing = %d, want %d", got, c.want)
			}
		})
	}
}

func TestDownloadKeepsTwoFilesOfOneName(t *testing.T) {
	bin, err := exec.LookPath("gallery-dl")
	if err != nil {
		t.Skip("gallery-dl not installed")
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<a href=%q>a</a> <a href=%q>b</a>", srv.URL+"/a/image.png", srv.URL+"/b/image.png")
	})
	mux.HandleFunc("/a/image.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("first file")) })
	mux.HandleFunc("/b/image.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("the second file")) })

	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(t.TempDir(), "archive.sqlite")
	cfg.GalleryDL.SleepRequest = 0
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatal(err)
	}
	dls, err := New(config.NewProvider(cfg), SiteLists{}).Download(context.Background(), "r:"+srv.URL+"/list", "", t.TempDir(), ArchiveRead, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, d := range dls {
		if d.Skipped {
			t.Errorf("a file of the same name read as skipped: %+v", d)
			continue
		}
		fi, err := os.Stat(d.Path)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, int(fi.Size()))
	}
	slices.Sort(sizes)
	if !slices.Equal(sizes, []int{len("first file"), len("the second file")}) {
		t.Errorf("files on disk have sizes %v", sizes)
	}
}

func TestCatchUpStopsAfterARunOfArchivedPosts(t *testing.T) {
	bin, err := exec.LookPath("gallery-dl")
	if err != nil {
		t.Skip("gallery-dl not installed")
	}
	const known = 200
	var newFetched atomic.Bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	listing := func(w http.ResponseWriter, extra ...string) {
		for i := range known {
			_, _ = fmt.Fprintf(w, "<a href=%q>p</a> ", fmt.Sprintf("%s/known/%d.png", srv.URL, i))
		}
		for _, link := range extra {
			_, _ = fmt.Fprintf(w, "<a href=%q>p</a> ", srv.URL+link)
		}
	}
	mux.HandleFunc("/seed", func(w http.ResponseWriter, r *http.Request) { listing(w) })
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) { listing(w, "/new.png") })
	mux.HandleFunc("/known/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.Path)) })
	mux.HandleFunc("/new.png", func(w http.ResponseWriter, r *http.Request) {
		newFetched.Store(true)
		_, _ = w.Write([]byte("new"))
	})

	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(t.TempDir(), "archive.sqlite")
	cfg.GalleryDL.SleepRequest = 0
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatal(err)
	}
	tool := New(config.NewProvider(cfg), SiteLists{})
	if _, err := tool.Download(context.Background(), "r:"+srv.URL+"/seed", "", t.TempDir(), ArchiveRead, nil, false); err != nil {
		t.Fatal(err)
	}

	var managed map[string]any
	if err := json.Unmarshal(readManaged(t, cfg, nil), &managed); err != nil {
		t.Fatal(err)
	}
	managed["extractor"].(map[string]any)["sleep-skip"] = 0.05
	data, err := json.Marshal(managed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.GalleryDL.ConfigPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	dls, err := tool.Download(context.Background(), "r:"+srv.URL+"/list", "", t.TempDir(), ArchiveCatchUp, nil, false)
	if err != nil {
		t.Fatalf("a caught-up run reads as failed: %v", err)
	}
	if len(dls) < catchUpSkips || len(dls) >= known {
		t.Errorf("reported %d posts, want about %d", len(dls), catchUpSkips)
	}
	for _, d := range dls {
		if !d.Skipped {
			t.Errorf("a known post did not read as skipped: %+v", d)
		}
	}
	if newFetched.Load() {
		t.Error("the run went on past the archived posts")
	}
}

func TestCatchUpReportsAFileThatFailedBeforeTheStop(t *testing.T) {
	bin, err := exec.LookPath("gallery-dl")
	if err != nil {
		t.Skip("gallery-dl not installed")
	}
	const known = 200
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	listing := func(w http.ResponseWriter, extra ...string) {
		for _, link := range extra {
			_, _ = fmt.Fprintf(w, "<a href=%q>p</a> ", srv.URL+link)
		}
		for i := range known {
			_, _ = fmt.Fprintf(w, "<a href=%q>p</a> ", fmt.Sprintf("%s/known/%d.png", srv.URL, i))
		}
	}
	mux.HandleFunc("/seed", func(w http.ResponseWriter, r *http.Request) { listing(w) })
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) { listing(w, "/broken.png") })
	mux.HandleFunc("/known/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.Path)) })
	mux.HandleFunc("/broken.png", http.NotFound)

	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(t.TempDir(), "archive.sqlite")
	cfg.GalleryDL.SleepRequest = 0
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatal(err)
	}
	tool := New(config.NewProvider(cfg), SiteLists{})
	if _, err := tool.Download(context.Background(), "r:"+srv.URL+"/seed", "", t.TempDir(), ArchiveRead, nil, false); err != nil {
		t.Fatal(err)
	}
	dls, err := tool.Download(context.Background(), "r:"+srv.URL+"/list", "", t.TempDir(), ArchiveCatchUp, nil, false)
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeDownloadFailed {
		t.Fatalf("err = %v, want the failed file's %s", err, outcome.ErrCodeDownloadFailed)
	}
	if !CaughtUp(dls) || len(dls) >= known {
		t.Errorf("reported %d posts, want the run stopped after its skips", len(dls))
	}
}

func TestCaughtUpReadsOnlyAFullRunOfSkips(t *testing.T) {
	run := make([]Downloaded, catchUpSkips)
	for i := range run {
		run[i].Skipped = true
	}
	if !CaughtUp(run) {
		t.Error("a full run of skips did not read as caught up")
	}
	if CaughtUp(run[1:]) {
		t.Error("a shorter run read as caught up")
	}
	if CaughtUp(append(run, Downloaded{Path: "/x"})) {
		t.Error("a landed file after the run read as caught up")
	}
}

func TestARunsErrorIsOneClippedLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	bin := filepath.Join(t.TempDir(), "gallery-dl")
	script := "#!/bin/sh\ni=0\nwhile [ $i -lt 1000 ]; do echo \"traceback line $i\" >&2; i=$((i+1)); done\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	tool := New(config.NewProvider(cfg), SiteLists{})
	_, err := tool.Resolve(context.Background(), "https://example.com/x", "", false)
	var ce *outcome.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(ce.Msg, "\n") || len(ce.Msg) > maxErrorMessage+3 {
		t.Errorf("message is %d bytes: %q", len(ce.Msg), ce.Msg)
	}
	probe, _ := tool.Probe(context.Background(), "https://example.com/x")
	if strings.Contains(probe.Detail, "\n") {
		t.Errorf("probe detail is not one line: %q", probe.Detail)
	}
}

func TestReportDownloadsFailsOnALineTooLongToRead(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.jpg")
	second := filepath.Join(dir, "second.jpg")
	for _, p := range []string{first, second} {
		if err := os.WriteFile(p+".json", []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("x", 2<<20)
	out, err := reportDownloads(strings.NewReader(first+"\n"+long+"\n"+second+"\n"), dir, nil, nil)
	if len(out) != 1 {
		t.Errorf("entries = %d, want the one before the long line", len(out))
	}
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeMappingFailed {
		t.Errorf("err = %v, want mapping_failed", err)
	}
}

func TestReportDownloadsCountsARunOfSkips(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept.jpg")
	if err := os.WriteFile(kept+".json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	skip := "# " + filepath.Join(dir, "p.jpg") + "\n"
	var runs []int
	_, _ = reportDownloads(strings.NewReader(skip+skip+kept+"\n"+skip), dir, nil, func(n int) { runs = append(runs, n) })
	if !slices.Equal(runs, []int{1, 2, 1}) {
		t.Errorf("runs = %v, want a downloaded file to end a run", runs)
	}
}

func TestCatchUpKeepsTwoFilesOfOneName(t *testing.T) {
	bin, err := exec.LookPath("gallery-dl")
	if err != nil {
		t.Skip("gallery-dl not installed")
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "<a href=%q>a</a> <a href=%q>b</a>", srv.URL+"/a/image.png", srv.URL+"/b/image.png")
	})
	mux.HandleFunc("/a/image.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("first file")) })
	mux.HandleFunc("/b/image.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("the second file")) })

	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(t.TempDir(), "archive.sqlite")
	cfg.GalleryDL.SleepRequest = 0
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatal(err)
	}
	dls, err := New(config.NewProvider(cfg), SiteLists{}).Download(context.Background(), "r:"+srv.URL+"/list", "", t.TempDir(), ArchiveCatchUp, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dls {
		if d.Skipped {
			t.Errorf("a file of the same name read as skipped: %+v", d)
		}
	}
	if len(dls) != 2 {
		t.Errorf("got %d files, want both", len(dls))
	}
}

func TestReportDownloadsSeparatesAFailureFromASkip(t *testing.T) {
	dir := t.TempDir()
	noSidecar := filepath.Join(dir, "nosidecar.jpg")
	if err := os.WriteFile(noSidecar, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	badSidecar := filepath.Join(dir, "bad.jpg")
	if err := os.WriteFile(badSidecar, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badSidecar+".json", []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := noSidecar + "\n" + badSidecar + "\n"
	out, _ := reportDownloads(strings.NewReader(input), dir, nil, nil)
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	for i, d := range out {
		if d.Skipped {
			t.Errorf("result %d reported as an archive skip: %+v", i, d)
		}
		if d.Err == "" {
			t.Errorf("result %d carries no reason: %+v", i, d)
		}
	}
}

func TestReportDownloadsRefusesAPathOutsideTheJobDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeDownloaded(t, outside, "safebooru", 1, "escaped.jpg")
	escaped := filepath.Join(outside, "escaped.jpg")

	out, _ := reportDownloads(strings.NewReader(escaped+"\n"+filepath.Join(dir, "..", "sneaky.jpg")+"\n"), dir, nil, nil)
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	for i, d := range out {
		if d.Path != "" || d.Err == "" {
			t.Errorf("result %d = %+v, want a refusal with no path", i, d)
		}
	}
	writeDownloaded(t, dir, "safebooru", 2, "kept.jpg")
	in, _ := reportDownloads(strings.NewReader(filepath.Join(dir, "kept.jpg")+"\n"), dir, nil, nil)
	if len(in) != 1 || in[0].Err != "" || in[0].Path == "" {
		t.Errorf("a file inside the job dir = %+v, want it taken", in)
	}
}

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
		{"No suitable extractor found", outcome.ErrCodeUnsupportedURL},
		{"unsupported URL", outcome.ErrCodeUnsupportedURL},
		{"HTTP 401: missing authentication", outcome.ErrCodeAuthRequired},
		{"403 Forbidden", outcome.ErrCodeDownloadFailed},
		{"403 Forbidden: Login Required", outcome.ErrCodeAuthRequired},
		{"ChallengeError: Cloudflare challenge (403 Forbidden)", outcome.ErrCodeBlocked},
		{"HTTP 429: too many requests", outcome.ErrCodeRateLimited},
		{"rate limit exceeded", outcome.ErrCodeRateLimited},
		{"NameResolutionError: Failed to resolve 'cdnc.example.com' ([Errno -3] Temporary failure in name resolution)", outcome.ErrCodeNetworkUnreachable},
		{"ConnectionError: [Errno 101] Network is unreachable", outcome.ErrCodeNetworkUnreachable},
		{"connection refused", outcome.ErrCodeDownloadFailed},
		{"", outcome.ErrCodeDownloadFailed},
		{"[danbooru][warning] tag named captcha_girl\n[download][error] Failed to download x.png", outcome.ErrCodeDownloadFailed},
		{"[danbooru][warning] rate limit near\n[danbooru][error] HttpError: '404 Not Found'", outcome.ErrCodeDownloadFailed},
		{"[downloader.http][warning] Cloudflare challenge\n[download][error] Failed to download x.png", outcome.ErrCodeBlocked},
		{"[gallery-dl][error] Unsupported URL 'http://x'", outcome.ErrCodeUnsupportedURL},
		{"[danbooru][error] HttpError: '404 Not Found' for 'https://x/authentication/captcha.png'", outcome.ErrCodeDownloadFailed},
		{"[downloader.http][warning] '429 Too Many Requests' for 'https://x/a.png' (1/3)\n[download][error] Failed to download a.png", outcome.ErrCodeRateLimited},
	}
	for _, tc := range cases {
		got := classifyError(1, tc.stderr)
		if got.Code != tc.want {
			t.Errorf("classifyError(%q) = %s, want %s", tc.stderr, got.Code, tc.want)
		}
	}
}

func readManaged(t *testing.T, cfg *config.Config, writeErr error) []byte {
	t.Helper()
	if writeErr != nil {
		t.Fatalf("WriteManagedConfig: %v", writeErr)
	}
	data, err := os.ReadFile(cfg.GalleryDL.ConfigPath)
	if err != nil {
		t.Fatalf("read managed config: %v", err)
	}
	return data
}

func readManagedJSON(t *testing.T, cfg *config.Config, writeErr error, doc any) {
	t.Helper()
	if err := json.Unmarshal(readManaged(t, cfg, writeErr), doc); err != nil {
		t.Fatalf("managed config is not valid JSON: %v", err)
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

	data := readManaged(t, cfg, WriteManagedConfig(cfg, SiteLists{FlatTag: flatTag, Metadata: []string{"danbooru", "e621"}, Notes: []string{"gelbooru", "safebooru", "konachan"}}))
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
	if e621["username"] != "user" || e621["api-key"] != "k2" || e621["password"] != "k2" {
		t.Errorf("e621 block wrong: %+v", e621)
	}
	if _, hasTags := e621["tags"]; hasTags {
		t.Error("e621 is not a flat-tag family; it should not get tags:true")
	}
	if e621["metadata"] != "artist_commentary,notes" {
		t.Errorf("e621 (metadata site) should get the metadata include, got %+v", e621)
	}
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
	dl := asMap("directlink")
	if dl["filename"] != "{filename}.{extension}" {
		t.Errorf("directlink should override filename to drop the host/path, got %+v", dl)
	}
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

func TestManagedConfigKeepsTheLoginCacheBesideIt(t *testing.T) {
	bin, err := exec.LookPath("gallery-dl")
	if err != nil {
		t.Skip("gallery-dl not installed")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	cfg.GalleryDL.ConfigPath = filepath.Join(dir, "gallery-dl.json")
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatal(err)
	}
	tool := New(config.NewProvider(cfg), SiteLists{})
	if _, err := tool.run(context.Background(), append(tool.configArgs(), "--cache-clear", "ALL")...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gallery-dl-cache.sqlite3")); err != nil {
		t.Errorf("no cache beside the managed config: %v", err)
	}
	if entries, _ := os.ReadDir(xdg); len(entries) != 0 {
		t.Errorf("the user's cache directory holds %v", entries)
	}
}

func TestDownloadReadsANonASCIIPathWhateverThePipeEncoding(t *testing.T) {
	bin, err := exec.LookPath("gallery-dl")
	if err != nil {
		t.Skip("gallery-dl not installed")
	}
	t.Setenv("PYTHONIOENCODING", "cp1252")
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "<a href=%q>a</a>", srv.URL+"/caf%C3%A9-%E3%81%82.png")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("bytes")) })

	cfg := config.Default()
	cfg.GalleryDL.BinaryPath = bin
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(t.TempDir(), "archive.sqlite")
	cfg.GalleryDL.SleepRequest = 0
	if err := WriteManagedConfig(cfg, SiteLists{}); err != nil {
		t.Fatal(err)
	}
	tool := New(config.NewProvider(cfg), SiteLists{})
	dls, err := tool.Download(context.Background(), "r:"+srv.URL+"/list", "", t.TempDir(), ArchiveRead, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(dls) != 1 || dls[0].Err != "" || !strings.HasSuffix(dls[0].Path, "café-あ.png") {
		t.Errorf("downloads = %+v, want the non-ASCII path read whole", dls)
	}
}

func TestWriteManagedConfigUserAgentNamesTheAccount(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.Sites = []config.Site{
		{Name: "danbooru", Username: "solo", APIKey: "k", UserID: "123"},
		{Name: "aibooru", Username: "solo", APIKey: "k"},
		{Name: "gelbooru", APIKey: "K", UserID: "U"},
	}
	agentSites := map[string][]string{
		"danbooru": {"donmai.us", "danbooru.donmai.us"},
		"aibooru":  {"aibooru.online"},
	}
	var doc struct {
		Extractor struct {
			Danbooru, Aibooru, Gelbooru map[string]any
			Directlink                  map[string]any
		} `json:"extractor"`
	}
	readManagedJSON(t, cfg, WriteManagedConfig(cfg, SiteLists{UserAgent: agentSites}), &doc)
	if got := doc.Extractor.Danbooru["user-agent"]; got != "monloader; user #123" {
		t.Errorf("danbooru user-agent = %v, want the account named", got)
	}
	for site, block := range map[string]map[string]any{"aibooru": doc.Extractor.Aibooru, "gelbooru": doc.Extractor.Gelbooru} {
		if got, ok := block["user-agent"]; ok {
			t.Errorf("%s user-agent = %v, want gallery-dl's default", site, got)
		}
	}
	for host, want := range map[string]string{"donmai.us": "monloader; user #123", "aibooru.online": "+gallery-dl"} {
		block, _ := doc.Extractor.Directlink[host].(map[string]any)
		if block["user-agent"] != want {
			t.Errorf("directlink %s user-agent = %v, want %q", host, block["user-agent"], want)
		}
	}
	if doc.Extractor.Directlink["filename"] != "{filename}.{extension}" {
		t.Errorf("directlink filename lost: %+v", doc.Extractor.Directlink)
	}
}

func TestWriteManagedConfigSiteAgent(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.Sites = []config.Site{
		{Name: "danbooru", UserID: "123", Agent: "mine"},
		{Name: "aibooru", UserID: "123", Agent: "+firefox"},
		{Name: "rule34", APIKey: "K", UserID: "U", Agent: "theirs"},
		{Name: "gelbooru", APIKey: "K", UserID: "U"},
		{Name: "e621", Agent: "+gallery-dl"},
	}
	var doc struct {
		Extractor map[string]any `json:"extractor"`
	}
	readManagedJSON(t, cfg, WriteManagedConfig(cfg, SiteLists{
		UserAgent: map[string][]string{"danbooru": {"cdn.donmai.us"}, "aibooru": {"aibooru.online"}},
		Hosts: map[string][]string{
			"danbooru": {"cdn.donmai.us"}, "aibooru": {"aibooru.online"},
			"rule34": {"wimg.rule34.xxx"}, "gelbooru": {"gelbooru.com"},
		},
	}), &doc)
	block := func(name string) map[string]any {
		b, _ := doc.Extractor[name].(map[string]any)
		return b
	}
	for site, want := range map[string]string{"danbooru": "mine", "aibooru": "+firefox", "rule34": "theirs", "e621": "+gallery-dl"} {
		if got := block(site)["user-agent"]; got != want {
			t.Errorf("%s user-agent = %v, want %q", site, got, want)
		}
	}
	if got, ok := block("gelbooru")["user-agent"]; ok {
		t.Errorf("gelbooru user-agent = %v, want gallery-dl's default", got)
	}
	for host, want := range map[string]string{"donmai.us": "mine", "aibooru.online": "+firefox", "rule34.xxx": "theirs"} {
		hostBlock, _ := block("directlink")[host].(map[string]any)
		if hostBlock["user-agent"] != want {
			t.Errorf("directlink %s user-agent = %v, want %q", host, hostBlock["user-agent"], want)
		}
	}
	if _, ok := block("directlink")["gelbooru.com"]; ok {
		t.Error("a site with no agent of its own should leave its direct links alone")
	}
}

func TestWriteManagedConfigSiteOptionsBeatTheAgent(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.Sites = []config.Site{{Name: "rule34", Agent: "field", Options: `{"user-agent": "options"}`}}
	var doc struct {
		Extractor map[string]any `json:"extractor"`
	}
	readManagedJSON(t, cfg, WriteManagedConfig(cfg, SiteLists{}), &doc)
	block := func(name string) map[string]any {
		b, _ := doc.Extractor[name].(map[string]any)
		return b
	}
	if got := block("rule34")["user-agent"]; got != "options" {
		t.Errorf("user-agent = %v, want the block's options to win", got)
	}
}

func TestWriteManagedConfigCapsDeadHost(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	var full map[string]any
	readManagedJSON(t, cfg, WriteManagedConfig(cfg, SiteLists{}), &full)
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
	if err := WriteManagedConfig(cfg, SiteLists{}); err == nil {
		t.Error("expected an error for invalid raw config JSON")
	}
}

func TestDownloadCanceledMidRunReportsCanceled(t *testing.T) {
	dir := t.TempDir()
	_, cfg := stubGDL(t, dir, "#!/bin/sh\nexec sleep 5\n")
	tool := New(config.NewProvider(cfg), SiteLists{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := tool.Download(ctx, "https://example.com/x", "", filepath.Join(dir, "work"), ArchiveRead, nil, false)
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeCanceled {
		t.Fatalf("canceled download error = %v, want code %s", err, outcome.ErrCodeCanceled)
	}
}

func TestDownloadReturnsAfterAnOverlongLine(t *testing.T) {
	dir := t.TempDir()
	_, cfg := stubGDL(t, dir, "#!/bin/sh\nhead -c 2000000 /dev/zero | tr '\\0' a\necho\nhead -c 2000000 /dev/zero | tr '\\0' b\necho\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New(config.NewProvider(cfg), SiteLists{}).Download(ctx, "https://example.com/x", "", filepath.Join(dir, "work"), ArchiveRead, nil, false)
	if ctx.Err() != nil {
		t.Fatalf("Download hung on output past the scanner's limit: %v", err)
	}
}

func TestDownloadErrorIsTheLastErrorLineClipped(t *testing.T) {
	dir := t.TempDir()
	_, cfg := stubGDL(t, dir, "#!/bin/sh\n"+
		"echo '[danbooru][warning] [Errno 101] Network is unreachable, retrying' >&2\n"+
		"echo '[download][error] Failed to download p1.jpg' >&2\n"+
		"echo '[download][error] Failed to download p2.jpg "+strings.Repeat("x", 400)+"' >&2\n"+
		"echo 'gallery-dl: 2 files failed' >&2\n"+
		"exit 4\n")
	_, err := New(config.NewProvider(cfg), SiteLists{}).Download(context.Background(), "https://example.com/x", "", filepath.Join(dir, "work"), ArchiveRead, nil, false)
	var ce *outcome.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("Download error = %v, want a coded error", err)
	}
	if ce.Code != outcome.ErrCodeNetworkUnreachable {
		t.Errorf("code = %s, want %s from the whole stderr", ce.Code, outcome.ErrCodeNetworkUnreachable)
	}
	if !strings.HasPrefix(ce.Msg, "[download][error] Failed to download p2.jpg") || len(ce.Msg) != 303 {
		t.Errorf("msg = %q (%d bytes), want the last error line clipped to 300", ce.Msg, len(ce.Msg))
	}
}

func TestGalleryDLErrorsCarryNoCredential(t *testing.T) {
	dir := t.TempDir()
	_, cfg := stubGDL(t, dir, "#!/bin/sh\necho \"[gelbooru][error] HttpError: '503 Service Unavailable' for 'https://gelbooru.com/index.php?page=dapi&api_key=SECRETKEY123456789&user_id=424242'\" >&2\nexit 4\n")
	provider := config.NewProvider(cfg)
	tool := New(provider, SiteLists{})

	if err := provider.Update(func(c *config.Config) error {
		c.Sites = []config.Site{{Name: "gelbooru", APIKey: "SECRETKEY123456789", UserID: "424242"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, rerr := tool.Resolve(context.Background(), "https://gelbooru.com/index.php?page=post&s=list", "", false)
	_, derr := tool.Download(context.Background(), "https://gelbooru.com/index.php?page=post&s=list", "", filepath.Join(dir, "work"), ArchiveRead, nil, false)
	probe, _ := tool.Probe(context.Background(), "https://gelbooru.com/index.php?page=post&s=list")
	for name, text := range map[string]string{"resolve": fmt.Sprint(rerr), "download": fmt.Sprint(derr), "probe": probe.Detail} {
		if strings.Contains(text, "SECRETKEY123456789") || strings.Contains(text, "424242") {
			t.Errorf("%s error = %q, want the credentials redacted", name, text)
		}
		if !strings.Contains(text, "<redacted:gelbooru.api_key>") {
			t.Errorf("%s error = %q, want the redaction marker", name, text)
		}
	}
}

func TestWriteManagedConfigOptionsLayers(t *testing.T) {
	cfg := config.Default()
	cfg.GalleryDL.ConfigPath = filepath.Join(t.TempDir(), "gallery-dl.json")
	cfg.GalleryDL.RawConfig = `{"extractor":{"twitter":{"videos":false}}}`
	cfg.Sites = []config.Site{
		{Name: "twitter", Username: "user", Password: "pw", Cookies: "/config/cookies/twitter.txt",
			Options: `{"retweets": true, "text-tweets": true, "headers": {"Authorization": "Bearer X"}}`},
		{Name: "broken", Gallery: "art", Options: `{oops`},
	}
	profileOpts := map[string]map[string]any{
		"twitter": {"retweets": false, "quoted": true, "headers": map[string]any{"Referer": "https://x.com/"}},
	}
	var full map[string]any
	readManagedJSON(t, cfg, WriteManagedConfig(cfg, SiteLists{Options: profileOpts}), &full)
	ext := full["extractor"].(map[string]any)
	tw := ext["twitter"].(map[string]any)
	if tw["username"] != "user" || tw["password"] != "pw" {
		t.Errorf("twitter credentials wrong: %+v", tw)
	}
	if _, hasKey := tw["api-key"]; hasKey {
		t.Errorf("no api key was set, none should be written: %+v", tw)
	}
	if tw["cookies"] != "/config/cookies/twitter.txt" {
		t.Errorf("twitter cookies wrong: %+v", tw)
	}
	if tw["retweets"] != true {
		t.Errorf("instance options should win over profile options: %+v", tw)
	}
	if tw["quoted"] != true || tw["text-tweets"] != true {
		t.Errorf("both options layers should contribute their own keys: %+v", tw)
	}
	if tw["videos"] != false {
		t.Errorf("the raw passthrough should win last: %+v", tw)
	}
	if _, ok := ext["broken"]; ok {
		t.Errorf("a gallery-only site with broken options should write no block: %v", ext["broken"])
	}
	if h := tw["headers"].(map[string]any); h["Referer"] != "https://x.com/" || h["Authorization"] != "Bearer X" {
		t.Errorf("nested options should merge across layers: %+v", h)
	}
	if h := profileOpts["twitter"]["headers"].(map[string]any); len(h) != 1 {
		t.Errorf("the profile's own options must not take the site's keys: %+v", h)
	}
}

func TestHandoffArgsStopOnePastTheWindow(t *testing.T) {
	for rng, want := range map[string][]string{
		"1-200":   {"--chapter-range", "1-201"},
		"201-400": {"--chapter-range", "1-401"},
		"":        nil,
	} {
		if got := handoffArgs(rng); !reflect.DeepEqual(got, want) {
			t.Errorf("handoffArgs(%q) = %v, want %v", rng, got, want)
		}
	}
}
