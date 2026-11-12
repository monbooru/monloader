package web

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/web"
)

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func postForm(t *testing.T, ts *httptest.Server, srv *Server, path string, vals url.Values) *http.Response {
	t.Helper()
	vals.Set("_csrf", srv.sessions.CSRFToken("anon"))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.URL+path, vals)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postClosed(t *testing.T, ts *httptest.Server, srv *Server, path string, vals url.Values) *http.Response {
	t.Helper()
	resp := postForm(t, ts, srv, path, vals)
	resp.Body.Close()
	return resp
}

func refusedSave(t *testing.T, resp *http.Response) string {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a refused save answered %d, want the page back", resp.StatusCode)
	}
	if !strings.Contains(body, "flash-err") {
		t.Fatalf("a refused save should carry the error flash, got %q", truncate(body))
	}
	return body
}

func TestEnqueueEmptyURL(t *testing.T) {
	srv, ts := serveWeb(t, "")
	resp := postForm(t, ts, srv, "/", url.Values{"url": {""}})
	if body := readBody(t, resp); !strings.Contains(body, "Enter a URL") {
		t.Errorf("empty url should flash an error, got %q", body)
	}
}

func TestEnqueueRejectsInvalidURL(t *testing.T) {
	srv, ts := serveWeb(t, "")
	for _, bad := range []string{"not a url", "gelbooru.com/post/1", "ftp://x/y", "https://"} {
		body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {bad}}))
		if !strings.Contains(body, "valid") {
			t.Errorf("%q should flash an invalid-url error, got %q", bad, body)
		}
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("invalid urls should not enqueue, queue has %d", total)
	}
}

func TestEnqueueBlockedWhenMonbooruUnconfigured(t *testing.T) {
	srv := newWebServer(t, "", "")
	ts := newTestServer(t, srv)
	body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {"https://example.com/posts/1"}}))
	if !strings.Contains(body, "No monbooru address set") {
		t.Errorf("submit with no monbooru should flash a not-configured error, got %q", body)
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("no job should enqueue when monbooru is unconfigured, queue has %d", total)
	}
}

func TestSettingsNeverEchoesSecrets(t *testing.T) {
	srv := stubbedServer(t, "loginpw")
	const (
		mbToken = "MB-SECRET-TOKEN-zzz"
		siteKey = "SITE-SECRET-KEY-zzz"
		snaoKey = "SAUCENAO-SECRET-KEY-zzz"
	)
	mustUpdate(t, srv, func(c *config.Config) {
		c.Monbooru.APIToken = mbToken
		c.Sites = append(c.Sites, config.Site{Name: "gelbooru", APIKey: siteKey})
		c.Lookup.Saucenao.APIKey = snaoKey
	})
	pwHash := srv.cfg.Current().Auth.PasswordHash

	ts := newTestServer(t, srv)
	id := srv.sessions.Create(7)
	req, _ := http.NewRequest("GET", ts.URL+"/settings", nil)
	req.AddCookie(&http.Cookie{Name: "monloader_session", Value: id})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200 (authenticated)", resp.StatusCode)
	}
	for name, secret := range map[string]string{
		"monbooru token":   mbToken,
		"site api key":     siteKey,
		"saucenao api key": snaoKey,
		"password hash":    pwHash,
	} {
		if strings.Contains(body, secret) {
			t.Errorf("settings page leaked the %s", name)
		}
	}
}

func TestSaveMonbooru(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/monbooru", url.Values{
		"api_url":         {"http://mb2:8080"},
		"web_url":         {"http://booru.example.com"},
		"api_token":       {"newtoken"},
		"default_gallery": {"art"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	c := srv.cfg.Current().Monbooru
	if c.APIURL != "http://mb2:8080" || c.DefaultGallery != "art" || c.WebURL != "http://booru.example.com" {
		t.Errorf("monbooru config not saved: %+v", c)
	}
	if c.APIToken != "tok" {
		t.Errorf("api token must not be settable from the form (pairing only), got %q", c.APIToken)
	}
}

func TestSaveMonbooruWithNoGalleryFieldKeepsTheDefaultGallery(t *testing.T) {
	srv, ts := serveWeb(t, "")
	postClosed(t, ts, srv, "/settings/monbooru", url.Values{"api_url": {"http://mb2:8080"}, "default_gallery": {"art"}})

	postClosed(t, ts, srv, "/settings/monbooru", url.Values{"api_url": {"http://mb3:8080"}, "web_url": {""}})
	c := srv.cfg.Current().Monbooru
	if c.APIURL != "http://mb3:8080" || c.DefaultGallery != "art" {
		t.Errorf("a save with no gallery field: %+v, want the url moved and the gallery kept", c)
	}
	onDisk, err := config.LoadFromFileIn(srv.configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Monbooru.DefaultGallery != "art" {
		t.Errorf("the file's default_gallery = %q, want it kept", onDisk.Monbooru.DefaultGallery)
	}
}

func TestMonbooruWebBase(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	if got := srv.monbooruWebBase(); got != "" {
		t.Errorf("web base without web_url = %q, want none: api_url may not resolve in a browser", got)
	}
	srv.cfg.Current().Monbooru.WebURL = "http://booru.example.com/"
	if got := srv.monbooruWebBase(); got != "http://booru.example.com" {
		t.Errorf("web base with web_url = %q, want http://booru.example.com", got)
	}
}

func TestRefusedMonbooruSaveKeepsABlankedFieldAndTheGalleryChoice(t *testing.T) {
	srv, ts := serveWeb(t, "")
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.WebURL = "http://stored.example" })
	body := refusedSave(t, postForm(t, ts, srv, "/settings/monbooru", url.Values{"api_url": {"nope"}, "web_url": {""}, "default_gallery": {"typed"}}))
	if strings.Contains(body, `value="http://stored.example"`) {
		t.Error("a blanked field came back with the stored value, and the next save would write it")
	}
	if !strings.Contains(body, `value="nope"`) {
		t.Error("the refusal dropped the typed API URL")
	}
}

func TestRefusedMonbooruSaveKeepsTheTypedGalleryChoice(t *testing.T) {
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"main"},{"name":"art"}]`))
	}))
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	ts := newTestServer(t, srv)
	pairMBWith(t, srv, "x")
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.DefaultGallery = "main" })
	body := refusedSave(t, postForm(t, ts, srv, "/settings/monbooru", url.Values{"api_url": {"nope"}, "default_gallery": {"art"}}))
	if !strings.Contains(body, `<option value="art" selected>`) {
		t.Errorf("the refusal dropped the gallery pick: %q", truncate(body))
	}
	if strings.Contains(body, `<option value="main" selected>`) {
		t.Error("the stored gallery came back over the typed pick")
	}
}

func TestSaveMonbooruRefusesNonHTTPURLs(t *testing.T) {
	srv, ts := serveWeb(t, "")
	stored := srv.cfg.Current().Monbooru.APIURL
	for _, tc := range []struct{ field, value string }{
		{"api_url", "notaurl"},
		{"api_url", "not a url at all"},
		{"api_url", "javascript:alert(1)"},
		{"api_url", "ftp://x"},
		{"web_url", "notaurl"},
	} {
		form := url.Values{"api_url": {stored}, "web_url": {""}, "default_gallery": {"kept"}}
		form.Set(tc.field, tc.value)
		body := refusedSave(t, postForm(t, ts, srv, "/settings/monbooru", form))
		if !strings.Contains(body, "must be an http(s) URL") {
			t.Errorf("%s=%q: refusal should name the reason, got %q", tc.field, tc.value, truncate(body))
		}
		if !strings.Contains(body, `value="`+html.EscapeString(tc.value)+`"`) {
			t.Errorf("%s=%q: refusal should re-render what was typed, got %q", tc.field, tc.value, truncate(body))
		}
		if srv.cfg.Current().Monbooru.APIURL != stored || srv.cfg.Current().Monbooru.DefaultGallery == "kept" {
			t.Errorf("%s=%q: the save went through anyway (%+v)", tc.field, tc.value, srv.cfg.Current().Monbooru)
		}
	}
	postClosed(t, ts, srv, "/settings/monbooru", url.Values{"api_url": {""}, "web_url": {""}})
	if srv.cfg.Current().Monbooru.APIURL != "" {
		t.Errorf("clearing the api url should be accepted, got %q", srv.cfg.Current().Monbooru.APIURL)
	}
}

func TestSaveDownloader(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/downloader", url.Values{
		"concurrency":            {"3"},
		"sleep_request":          {"2.5"},
		"max_items_per_job":      {"42"},
		"default_folder":         {"incoming"},
		"history_retention_days": {"3"},
		"success_retention_days": {"1"},
	})
	resp.Body.Close()
	if srv.cfg.Current().Downloader.Concurrency != 3 || srv.cfg.Current().GalleryDL.SleepRequest != 2.5 ||
		srv.cfg.Current().Downloader.MaxItemsPerJob != 42 || srv.cfg.Current().Downloader.DefaultFolder != "incoming" ||
		srv.cfg.Current().Downloader.HistoryRetentionDays != 3 || srv.cfg.Current().Downloader.SuccessRetentionDays != 1 {
		t.Errorf("downloader config not saved: %+v %+v", srv.cfg.Current().Downloader, srv.cfg.Current().GalleryDL.SleepRequest)
	}

	resp = postForm(t, ts, srv, "/settings/downloader", url.Values{
		"history_retention_days": {"0"}, "success_retention_days": {"0"},
	})
	resp.Body.Close()
	if got := srv.cfg.Current().Downloader.HistoryRetentionDays; got != 0 {
		t.Errorf("history_retention_days = %d, want 0 to be accepted as no age limit", got)
	}
	if got := srv.cfg.Current().Downloader.SuccessRetentionDays; got != 0 {
		t.Errorf("success_retention_days = %d, want 0 to be accepted as off", got)
	}

	body := refusedSave(t, postForm(t, ts, srv, "/settings/downloader", url.Values{
		"history_retention_days": {"7"}, "success_retention_days": {"30"},
		"default_folder": {"kept"},
	}))
	if !strings.Contains(body, "must not be longer than") {
		t.Errorf("a succeeded window past the history one should be named, got %q", truncate(body))
	}
	if !strings.Contains(body, `value="kept"`) || !strings.Contains(body, `value="30"`) {
		t.Errorf("the refusal dropped what was typed, got %q", truncate(body))
	}
	if srv.cfg.Current().Downloader.DefaultFolder == "kept" {
		t.Error("the refused save went through anyway")
	}
}

func TestSaveTags(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	_, body := get(t, ts, "/settings")
	for _, want := range []string{`<a href="#rules">Rules</a>`, `name="category_sent" value="general" checked`, `name="category_sent" value="va" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	if strings.Contains(body, `value="rating"`) {
		t.Error("the rating is never dropped, so it must not get a box")
	}

	shown := []string{"general", "artist", "character", "va"}
	postForm(t, ts, srv, "/settings/rules", url.Values{
		"category_shown": shown, "category_sent": {"artist", "character"},
	}).Body.Close()
	if got := srv.cfg.Current().Tags.DropCategories; !slices.Equal(got, []string{"general", "va"}) {
		t.Errorf("drop_categories = %v, want the unticked boxes", got)
	}
	_, body = get(t, ts, "/settings")
	if strings.Contains(body, `name="category_sent" value="general" checked`) || !strings.Contains(body, `name="category_sent" value="artist" checked`) {
		t.Error("the boxes should read the stored list back")
	}

	postForm(t, ts, srv, "/settings/rules", url.Values{"category_shown": {"general", "../x y"}, "category_sent": {"general"}}).Body.Close()
	if got := srv.cfg.Current().Tags.DropCategories; !slices.Equal(got, []string{"va"}) {
		t.Errorf("drop_categories = %v, want a box left out of the form kept and a malformed name refused", got)
	}
	postForm(t, ts, srv, "/settings/rules", url.Values{}).Body.Close()
	if got := srv.cfg.Current().Tags.DropCategories; !slices.Equal(got, []string{"va"}) {
		t.Errorf("drop_categories = %v, want an absent field to keep the stored value", got)
	}
}

func TestTagsSectionFollowsTheFlowOrder(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	_, body := get(t, ts, "/settings")
	section := body[strings.Index(body, `id="rules"`):strings.Index(body, `id="sites"`)]
	strip, blacklist, sent := strings.Index(section, `class="tag-flow"`), strings.Index(section, `id="rules-blacklist"`), strings.Index(section, `name="category_sent"`)
	if strip < 0 || strip > blacklist || blacklist > sent {
		t.Errorf("the section should run strip, blacklist, categories kept: %d, %d, %d", strip, blacklist, sent)
	}
	if strings.Contains(section, "after mapping") || strings.Contains(section, "monsender") {
		t.Error("the section should not place the blacklist after mapping or credit tags to monsender")
	}
}

func TestBlacklistsShowEachOther(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	postForm(t, ts, srv, "/settings/rules", url.Values{"blacklist": {"global_rule\n# global note"}}).Body.Close()
	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{"blacklist": {"# site note\none\ntwo"}}).Body.Close()
	postForm(t, ts, srv, "/settings/sites/e621", url.Values{"blacklist": {"# only a note"}}).Body.Close()

	_, body := get(t, ts, "/settings")
	if !strings.Contains(body, `data-site="newgrounds">newgrounds +2</button>`) {
		t.Errorf("Tags should open the site with its rule count, got %q", truncate(body))
	}
	if !strings.Contains(body, "(blacklist +2)") {
		t.Error("the site's row should show its rule count")
	}
	if strings.Contains(body, `data-site="e621">e621 +`) {
		t.Error("a list of comments adds no rules")
	}

	_, body = get(t, ts, "/settings/sites/danbooru/dialog")
	if !strings.Contains(body, "<code>global_rule</code>") || strings.Contains(body, "global note") {
		t.Errorf("the dialog should show the global rules without their comments, got %q", truncate(body))
	}
}

func TestSiteDialogWarnsOfATargetThatNeverReachesMonbooru(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	postForm(t, ts, srv, "/settings/rules", url.Values{"category_shown": {"meta"}}).Body.Close()
	profile := url.Values{
		"family": {"generic"}, "kind": {"booru"}, "auth": {"none"},
		"cat_from": {"lore", "species"}, "cat_to": {"meta", "general"},
		"rule_from": {"sketch", "lowres", "old", "artist:x"}, "rule_to": {"meta:sketch", "", "new", "General:Y"},
	}
	postForm(t, ts, srv, "/settings/sites/newgrounds", profile).Body.Close()

	_, body := get(t, ts, "/settings/sites/newgrounds/dialog")
	if got := strings.Count(body, "meta is unchecked in Rules, so these tags"); got != 1 {
		t.Errorf("the lore row alone should warn, got %d", got)
	}
	if !strings.Contains(body, `value="general:y"`) {
		t.Error("a rule targeting general should keep its prefix")
	}
	if got := strings.Count(body, "meta is unchecked in Rules, so this tag"); got != 1 {
		t.Errorf("the sketch rule alone should warn, got %d", got)
	}

	profile.Set("post_url_template", "no placeholder")
	body = refusedSave(t, postForm(t, ts, srv, "/settings/sites/newgrounds", profile))
	if !strings.Contains(body, "so this tag never reaches monbooru") {
		t.Error("a refused save should still warn about the rows it hands back")
	}
}

func TestSiteDialogNamesTheTomlOverridesThatWin(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	mustUpdate(t, srv, func(c *config.Config) {
		c.TagOverrides = []config.TagOverride{{Site: "danbooru", From: "species", To: "general"}, {Site: "danbooru", From: "lore"}, {Site: "danbooru", From: "x", To: "not a category"}, {Site: "e621", From: "lore", To: "meta"}}
		c.RatingOverrides = []config.RatingOverride{{Site: "danbooru", From: "s", To: "general"}, {Site: "danbooru", From: "x", To: "nope"}}
	})
	_, body := get(t, ts, "/settings/sites/danbooru/dialog")
	for _, want := range []string{"sends species to general and wins", "drops lore and wins", "sends s to general and wins"} {
		if !strings.Contains(body, want) {
			t.Errorf("the dialog should say %q", want)
		}
	}
	if got := strings.Count(body, "and wins over this map"); got != 3 {
		t.Errorf("only the danbooru overrides the mapper applies should show, got %d", got)
	}
}

type traceRunner struct {
	fakeRunner
	meta map[string]any
	err  error
	urls []string
}

func (s *traceRunner) FirstPost(_ context.Context, url string) (map[string]any, error) {
	s.urls = append(s.urls, url)
	return s.meta, s.err
}

func TestTraceTakesOnlyHTTPURLs(t *testing.T) {
	srv, ts := serveWeb(t, "")
	runner := &traceRunner{meta: map[string]any{"category": "directlink", "domain": "cdn.example", "path": "a", "filename": "x", "extension": "png"}}
	srv.runner = runner
	for _, raw := range []string{"r:file:///etc/passwd", "md5:0123456789abcdef0123456789abcdef", "/etc/passwd"} {
		body := readBody(t, postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {raw}}))
		if !strings.Contains(body, "http(s) post URL") {
			t.Errorf("trace %q = %q, want the refusal", raw, truncate(body))
		}
	}
	if len(runner.urls) != 0 {
		t.Errorf("gallery-dl ran on non-URL input: %v", runner.urls)
	}
	body := readBody(t, postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {"http://cdn.example/a/x.png"}}))
	if !strings.Contains(body, "directlink") || strings.Contains(body, "first post of this URL") {
		t.Errorf("an http file link is the post itself, got %q", truncate(body))
	}
}

func TestTraceUsesThePushWordsForAPostWithNoFile(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	srv.runner = &traceRunner{err: &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: gdl.NoFileMsg}}
	resp := postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {"https://danbooru.donmai.us/posts/5008"}})
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "offers no file") {
		t.Errorf("trace = %q", truncate(string(b)))
	}
}

func TestTraceSaysNoneForASiteThatListsNoRating(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	srv.runner = &traceRunner{meta: map[string]any{"category": "hitomi", "gallery_id": float64(1), "tags": []any{"x"}}}
	resp := postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {"https://hitomi.la/g/1"}})
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `rating:</span> <span class="status-dim">none</span>`) {
		t.Errorf("trace = %q", truncate(string(b)))
	}
}

func TestTraceSaysThePushWouldFailOnFlatTags(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	srv.runner = &traceRunner{meta: map[string]any{"category": "safebooru", "id": float64(199), "rating": "s", "tags": "gel_artist199 smile"}}
	resp := postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {"https://safebooru.org/index.php?page=post&s=view&id=199"}})
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if strings.Contains(body, "Would download") || !strings.Contains(body, "flash-err") || !strings.Contains(body, "Sending fails") {
		t.Errorf("trace = %q", truncate(body))
	}
	if strings.Contains(body, "o-created") {
		t.Errorf("nothing should read as sent: %q", truncate(body))
	}
}

func TestTracePost(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	runner := &traceRunner{meta: map[string]any{
		"category": "danbooru", "id": float64(7), "rating": "s",
		"tags_general": []any{"smile", `<b>`}, "tags_meta": []any{"absurdres"},
	}}
	srv.runner = runner
	postForm(t, ts, srv, "/settings/rules", url.Values{"category_shown": {"meta"}, "category_sent": {}}).Body.Close()
	_, before := srv.queue.List(queue.ListOptions{})

	trace := func(form url.Values) string {
		t.Helper()
		resp := postForm(t, ts, srv, "/settings/rules/trace", form)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	body := trace(url.Values{"url": {" https://danbooru.donmai.us/posts/7 "}})
	if !slices.Equal(runner.urls, []string{"https://danbooru.donmai.us/posts/7"}) {
		t.Errorf("urls = %q", runner.urls)
	}
	for _, want := range []string{"Would download and send 3 tags", "https://danbooru.donmai.us/posts/7", "meta:</span> absurdres", ">meta</td>", `<span class="o-created">smile</span>`, "rating:sensitive", "&lt;b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("the trace should say %q, got %q", want, truncate(body))
		}
	}
	if strings.Contains(body, "<html") {
		t.Error("the trace should answer a fragment")
	}
	if _, after := srv.queue.List(queue.ListOptions{}); after != before {
		t.Errorf("a trace enqueued %d jobs", after-before)
	}

	postForm(t, ts, srv, "/settings/rules", url.Values{"blacklist": {"absurdres"}}).Body.Close()
	body = trace(url.Values{"url": {"https://danbooru.donmai.us/posts/7"}})
	if !strings.Contains(body, "global rule: absurdres") || !strings.Contains(body, `<span class="trace-hit">meta:absurdres</span>`) || !strings.Contains(body, `<span class="status-dim">skipped</span>`) {
		t.Errorf("a blacklisted post should read as skipped, got %q", truncate(body))
	}

	if body = trace(url.Values{"url": {"  "}}); !strings.Contains(body, "Paste a post URL") {
		t.Errorf("a blank URL: %q", truncate(body))
	}

	runner.err = &outcome.CodedError{Code: outcome.ErrCodeBlocked, Msg: "a wall"}
	if body = trace(url.Values{"url": {"https://example.com/x"}}); !strings.Contains(body, "flash-err") || !strings.Contains(body, "blocked: a wall") {
		t.Errorf("a blocked fetch: %q", truncate(body))
	}
	runner.err = &outcome.CodedError{Code: outcome.ErrCodeAuthRequired, Msg: "401"}
	if body = trace(url.Values{"url": {"https://example.com/x"}}); !strings.Contains(body, "auth rejected: 401") {
		t.Errorf("a refused login: %q", truncate(body))
	}
	if body = trace(url.Values{"url": {"https://gelbooru.com/index.php?page=post&s=view&id=1"}}); !strings.Contains(body, "needs API key: 401") {
		t.Errorf("a missing credential should outrank the failure: %q", truncate(body))
	}
	runner.err = errors.New("plain")
	if body = trace(url.Values{"url": {"https://example.com/x"}}); !strings.Contains(body, "failed: plain") {
		t.Errorf("an uncoded failure: %q", truncate(body))
	}
}

func TestSiteDialogNamesTheDefaultAgent(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	srv.cfg.Current().Sites = []config.Site{{Name: "aibooru", UserID: "777"}, {Name: "rule34", UserID: "888"}}
	for site, want := range map[string]string{
		"danbooru": "+gallery-dl",
		"aibooru":  "monloader; user #777",
		"rule34":   "+firefox",
		"e621":     "gallery-dl (by mikf)",
		"pixiv":    "gallery-dl's default",
	} {
		_, body := get(t, ts, "/settings/sites/"+site+"/dialog")
		if !strings.Contains(html.UnescapeString(body), `name="user_agent" value="" placeholder="`+want+`"`) {
			t.Errorf("%s dialog does not offer %q as its default agent", site, want)
		}
	}
}

func TestSiteDialogHasNoTrace(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	_, body := get(t, ts, "/settings/sites/danbooru/dialog")
	if strings.Contains(body, "trace") {
		t.Error("tracing belongs to the Rules section, not the site dialog")
	}
}

func TestSiteDialogMappingTabNamesItsSteps(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	_, body := get(t, ts, "/settings/sites/danbooru/dialog")
	last := -1
	for _, want := range []string{`class="tag-flow"`, "1 Mappings", `name="cat_from"`, `name="rule_from"`, "2 Blacklist", `id="sd-blacklist"`} {
		at := strings.Index(body, want)
		if at <= last {
			t.Errorf("%q out of step order in the mappings tab", want)
		}
		last = at
	}
	if strings.Contains(body, "suppress") || !strings.Contains(body, "(blank = drop)") {
		t.Error("a blank tag rule should read as a drop")
	}
}

func TestSaveTagsBlacklist(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	postForm(t, ts, srv, "/settings/rules", url.Values{
		"blacklist": {"guro\r\n\r\n# my list\r\nfurry -rating:general\r\nva:someone\r\n"},
	}).Body.Close()
	want := []string{"guro", "# my list", "furry -rating:general", "va:someone"}
	if got := srv.cfg.Current().Tags.Blacklist; !slices.Equal(got, want) {
		t.Errorf("blacklist = %q, want %q", got, want)
	}
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, "guro\n# my list\nfurry -rating:general") {
		t.Errorf("the textarea should read the stored rules back, got %q", truncate(body))
	}

	body := refusedSave(t, postForm(t, ts, srv, "/settings/rules", url.Values{
		"category_shown": {"general"}, "blacklist": {"guro\nscore:<0\nmeta:x"},
	}))
	if !strings.Contains(body, "blacklist line 2") || !strings.Contains(body, "nothing was saved") {
		t.Errorf("the refusal should name the line, got %q", truncate(body))
	}
	if !strings.Contains(body, "guro\nscore:") {
		t.Errorf("the refused draft should stay in the box, got %q", truncate(body))
	}
	if strings.Contains(body, `value="general" checked`) {
		t.Error("the refused page should keep the box as it was posted")
	}
	if got := srv.cfg.Current().Tags; !slices.Equal(got.Blacklist, want) || len(got.DropCategories) != 0 {
		t.Errorf("a refused save changed the config: %+v", got)
	}

	postForm(t, ts, srv, "/settings/rules", url.Values{"blacklist": {""}}).Body.Close()
	if got := srv.cfg.Current().Tags.Blacklist; len(got) != 0 {
		t.Errorf("an emptied box should clear the list, got %q", got)
	}
}

func TestSiteBlacklistSavesInTheBlockNotTheProfile(t *testing.T) {
	srv, ts := serveWeb(t, "")
	form := url.Values{"family": {"generic"}, "kind": {"other"}, "auth": {"none"}}
	form.Set("blacklist", "feral\nrating:explicit")
	postForm(t, ts, srv, "/settings/sites/newgrounds", form).Body.Close()
	site := srv.cfg.Current().FindSite("newgrounds")
	if site == nil || !slices.Equal(site.Blacklist, []string{"feral", "rating:explicit"}) {
		t.Fatalf("site = %+v, want the rules in the block", site)
	}
	_, body := get(t, ts, "/settings/sites/newgrounds/dialog")
	if !strings.Contains(body, "feral\nrating:explicit</textarea>") {
		t.Errorf("the dialog should read the rules back, got %q", truncate(body))
	}
	if !strings.Contains(body, "the post still downloads") {
		t.Error("the tag rules hint should say the post still downloads")
	}
	if strings.Contains(body, "&#34;blacklist&#34;") {
		t.Error("the exported profile must not carry the blacklist")
	}

	form.Set("blacklist", "feral\nscore:9")
	body = refusedSave(t, postForm(t, ts, srv, "/settings/sites/newgrounds", form))
	if !strings.Contains(body, "blacklist line 2") || !strings.Contains(body, "feral\nscore:9</textarea>") {
		t.Errorf("the refusal should name the line and keep the draft, got %q", truncate(body))
	}
	if got := srv.cfg.Current().FindSite("newgrounds").Blacklist; !slices.Equal(got, []string{"feral", "rating:explicit"}) {
		t.Errorf("a refused save changed the rules: %q", got)
	}

	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{"gallery": {"art"}}).Body.Close()
	if got := srv.cfg.Current().FindSite("newgrounds").Blacklist; len(got) != 2 {
		t.Errorf("a form without the field should keep the rules, got %q", got)
	}
	postForm(t, ts, srv, "/settings/sites/newgrounds/reset", url.Values{}).Body.Close()
	if srv.cfg.Current().FindSite("newgrounds") != nil {
		t.Error("removing the site should clear its rules with the rest")
	}
}

func TestDroppedCategoryMonbooruDoesNotListKeepsItsBox(t *testing.T) {
	srv := newWebServer(t, "http://127.0.0.1:1", "")
	ts := newTestServer(t, srv)
	mustUpdate(t, srv, func(c *config.Config) { c.Tags.DropCategories = []string{"gone"} })
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, `name="category_sent" value="gone">`) {
		t.Errorf("a stored drop should keep an unticked box, got %q", truncate(body))
	}
}

func TestSaveDownloaderRefusesBadNumbers(t *testing.T) {
	srv, ts := serveWeb(t, "")
	for _, tc := range []struct {
		field, value string
	}{
		{"concurrency", "1.5"},
		{"max_items_per_job", "0"},
		{"history_retention_days", "-1"},
		{"success_retention_days", "-1"},
		{"sleep_request", "soon"},
		{"max_items_per_job", ""},
		{"sleep_request", ""},
	} {
		body := refusedSave(t, postForm(t, ts, srv, "/settings/downloader", url.Values{
			tc.field: {tc.value}, "default_folder": {"kept"},
		}))
		if !strings.Contains(body, "nothing was saved") {
			t.Errorf("%s=%q: refusal should say nothing was saved, got %q", tc.field, tc.value, truncate(body))
		}
		if !strings.Contains(body, `value="kept"`) {
			t.Errorf("%s=%q: the refusal dropped the rest of the section", tc.field, tc.value)
		}
		if srv.cfg.Current().Downloader.DefaultFolder == "kept" {
			t.Errorf("%s=%q: the save went through anyway", tc.field, tc.value)
		}
	}
}

func TestSaveDownloaderDefaultFolder(t *testing.T) {
	srv, ts := serveWeb(t, "")
	postForm(t, ts, srv, "/settings/downloader", url.Values{"default_folder": {"incoming"}}).Body.Close()

	for _, value := range []string{"../../escape", "a/../../b", "/absolute"} {
		body := refusedSave(t, postForm(t, ts, srv, "/settings/downloader", url.Values{"default_folder": {value}}))
		if !strings.Contains(body, "relative path") {
			t.Errorf("default_folder=%q: refusal should name the reason, got %q", value, truncate(body))
		}
		if got := srv.cfg.Current().Downloader.DefaultFolder; got != "incoming" {
			t.Errorf("default_folder=%q: saved anyway as %q", value, got)
		}
	}
	postForm(t, ts, srv, "/settings/downloader", url.Values{"concurrency": {"2"}}).Body.Close()
	if got := srv.cfg.Current().Downloader.DefaultFolder; got != "incoming" {
		t.Errorf("a partial post cleared the default folder: %q", got)
	}
	postForm(t, ts, srv, "/settings/downloader", url.Values{"default_folder": {""}}).Body.Close()
	if got := srv.cfg.Current().Downloader.DefaultFolder; got != "" {
		t.Errorf("clearing the default folder = %q, want empty", got)
	}
}

func TestSaveSite(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/gelbooru", url.Values{
		"api_key": {"K"},
		"user_id": {"U"},
		"gallery": {"art"},
	}).Body.Close()
	site := srv.cfg.Current().FindSite("gelbooru")
	if site == nil || site.APIKey != "K" || site.UserID != "U" || site.Gallery != "art" {
		t.Errorf("site not saved: %+v", site)
	}
	postForm(t, ts, srv, "/settings/sites/gelbooru", url.Values{"gallery": {"newart"}}).Body.Close()
	n := 0
	for _, s := range srv.cfg.Current().Sites {
		if s.Name == "gelbooru" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected 1 gelbooru site after update, got %d", n)
	}
	if srv.cfg.Current().FindSite("gelbooru").Gallery != "newart" {
		t.Errorf("site gallery not updated")
	}
}

func TestSaveSiteLabelAndHostAliases(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"label":      {"Newgrounds"},
		"host_alias": {"art.NGfiles.com", "", "cdn.ngfiles.com"},
	}).Body.Close()
	if site := srv.cfg.Current().FindSite("newgrounds"); site == nil || site.Label != "Newgrounds" {
		t.Errorf("label not saved: %+v", site)
	}
	p, _ := srv.mapper.Lookup("newgrounds")
	if !reflect.DeepEqual(p.Hosts, []string{"art.ngfiles.com", "cdn.ngfiles.com"}) {
		t.Errorf("host aliases = %v, want lowercased with empties dropped", p.Hosts)
	}
	if got := srv.mapper.SourceLabel("art.ngfiles.com"); got != "Newgrounds" {
		t.Errorf("SourceLabel = %q, want the alias resolved to the site label", got)
	}
	_, body := get(t, ts, "/settings/sites/newgrounds/dialog")
	for _, want := range []string{`value="Newgrounds"`, `value="art.ngfiles.com"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog missing %q", want)
		}
	}
}

func TestSaveHostLabels(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/host-labels", url.Values{
		"hostlabel_host":  {"Wiki.Example", "", "dropme.example"},
		"hostlabel_label": {"example wiki", "x", ""},
	})
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "kind=ok") || !strings.Contains(loc, "section=sites") {
		t.Fatalf("save should flash ok on the sites section, got %q", loc)
	}
	want := []config.HostLabel{{Host: "wiki.example", Label: "example wiki"}}
	if got := srv.cfg.Current().HostLabels; !reflect.DeepEqual(got, want) {
		t.Errorf("host labels = %+v, want %+v", got, want)
	}
	if got := srv.mapper.SourceLabel("files.wiki.example"); got != "example wiki" {
		t.Errorf("SourceLabel = %q, want the subdomain matched to its parent row", got)
	}
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, `value="wiki.example"`) {
		t.Error("saved rows should render in the sites section")
	}
	body := refusedSave(t, postForm(t, ts, srv, "/settings/host-labels", url.Values{
		"hostlabel_host": {"good.example", "https://bad.example/x"}, "hostlabel_label": {"good", "bad"},
	}))
	if !strings.Contains(body, "is not a bare hostname") {
		t.Errorf("a URL-shaped host should be refused by name, got %q", truncate(body))
	}
	if !strings.Contains(body, `value="good.example"`) || !strings.Contains(body, `value="https://bad.example/x"`) {
		t.Errorf("the refusal dropped the typed rows, got %q", truncate(body))
	}
	if !reflect.DeepEqual(srv.cfg.Current().HostLabels, want) {
		t.Error("a refused save should leave the table unchanged")
	}
}

func TestAFailedSaveKeepsTheTypedRawConfigAndLookupFields(t *testing.T) {
	srv, ts := serveWeb(t, "")
	if err := os.WriteFile(srv.configPath, []byte("not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	body := refusedSave(t, postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {`{"extractor":{"timeout":30}}`}}))
	if !strings.Contains(body, "Could not save") || !strings.Contains(body, "timeout") {
		t.Errorf("raw_config: %q", truncate(body))
	}
	body = refusedSave(t, postForm(t, ts, srv, "/settings/lookup", url.Values{"min_similarity": {"77"}}))
	if !strings.Contains(body, "Could not save") || !strings.Contains(body, `value="77"`) {
		t.Errorf("lookup: %q", truncate(body))
	}
}

func TestAFailedSiteSaveLeavesItsFilesAlone(t *testing.T) {
	srv, ts := serveWeb(t, "")
	dir := t.TempDir()
	mustUpdate(t, srv, func(c *config.Config) { c.GalleryDL.CookiesDir = dir })
	if err := os.WriteFile(srv.configPath, []byte("not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	body := refusedSave(t, postForm(t, ts, srv, "/settings/sites/newsite", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"cookies_paste": {"# Netscape HTTP Cookie File\nx.example\tTRUE\t/\tFALSE\t0\tn\tv\n"},
	}))
	if !strings.Contains(body, "Could not save") {
		t.Errorf("refusal = %q", truncate(body))
	}
	if srv.mapper.CustomProfile("newsite") {
		t.Error("the profile landed although the save failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "newsite.txt")); !os.IsNotExist(err) {
		t.Errorf("the cookies landed although the save failed: %v", err)
	}
}

func TestAFailedSiteRemoveKeepsItsCookies(t *testing.T) {
	srv, ts := serveWeb(t, "")
	dir := t.TempDir()
	pasted := filepath.Join(dir, "danbooru.txt")
	if err := os.WriteFile(pasted, []byte("# cookies"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, srv, func(c *config.Config) {
		c.GalleryDL.CookiesDir = dir
		c.Sites = []config.Site{{Name: "danbooru", Cookies: pasted}}
	})
	if err := os.WriteFile(srv.configPath, []byte("not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	postForm(t, ts, srv, "/settings/sites/danbooru/reset", url.Values{}).Body.Close()
	if _, err := os.Stat(pasted); err != nil {
		t.Errorf("a failed remove took the cookies: %v", err)
	}
}

func TestResetSite(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/gelbooru", url.Values{
		"api_key": {"K"}, "gallery": {"art"},
		"family": {"gelbooru_v02"}, "kind": {"booru"}, "auth": {"api_required"},
		"post_url_template":   {"https://gelbooru.com/index.php?page=post&s=view&id={id}"},
		"md5_search_template": {"https://gelbooru.com/index.php?page=post&s=list&tags=md5:{md5}"},
		"rule_from":           {"blue_sky"}, "rule_to": {"azure_sky"},
	}).Body.Close()
	if !srv.mapper.CustomProfile("gelbooru") || srv.cfg.Current().FindSite("gelbooru") == nil {
		t.Fatal("site should be configured before reset")
	}
	order := srv.cfg.Current().FindSite("gelbooru").LookupOrder

	resp := postClosed(t, ts, srv, "/settings/sites/gelbooru/reset", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset status = %d, want 303", resp.StatusCode)
	}
	site := srv.cfg.Current().FindSite("gelbooru")
	if site == nil || site.HasUserData() {
		t.Errorf("everything the dialog set should be gone, got %+v", site)
	}
	if site != nil && site.LookupOrder != order {
		t.Errorf("chain position = %d, want %d kept - the lookup section owns it", site.LookupOrder, order)
	}
	if srv.mapper.CustomProfile("gelbooru") {
		t.Error("the user profile should go with the site; its rules rewrite every later push")
	}
	boorus, _, _ := srv.visibleSites()
	if slices.Contains(boorus, "gelbooru") {
		t.Error("the row should leave the sites table")
	}
}

func TestResetSiteDropsTheBlockOfAnUnchainedSite(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{"gallery": {"art"}}).Body.Close()
	postForm(t, ts, srv, "/settings/sites/newgrounds/reset", url.Values{}).Body.Close()
	if got := srv.cfg.Current().FindSite("newgrounds"); got != nil {
		t.Errorf("block = %+v, want it dropped whole", got)
	}
}

func TestResetSiteLeavesTheFileAgreeingWithTheRunningConfig(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{"api_key": {"K"}}).Body.Close()
	postForm(t, ts, srv, "/settings/sites/danbooru/reset", url.Values{}).Body.Close()

	postForm(t, ts, srv, "/settings/downloader", url.Values{"concurrency": {"2"}}).Body.Close()

	onDisk, err := config.LoadFromFileIn(srv.configPath, "")
	if err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
	}
	running, stored := srv.cfg.Current().FindSite("danbooru"), onDisk.FindSite("danbooru")
	if running == nil || stored == nil || !reflect.DeepEqual(*running, *stored) {
		t.Errorf("running = %+v, on disk = %+v, want them to agree", running, stored)
	}
}

func TestRefusedSiteSaveKeepsWhatWasTyped(t *testing.T) {
	srv, ts := serveWeb(t, "")
	draft := func(extra url.Values) url.Values {
		v := url.Values{
			"family": {"danbooru"}, "kind": {"booru"}, "auth": {"none"},
			"example":     {"https://typed.example/posts/1"},
			"host_alias":  {"typed.example"},
			"rule_from":   {"typed_tag"},
			"rule_to":     {"typed_target"},
			"cat_from":    {"typed_suffix"},
			"cat_to":      {"meta"},
			"rating_from": {"typed_rating"},
			"rating_to":   {"general"},
			"label":       {"typed label"},
			"blacklist":   {"typed_rule"},
		}
		maps.Copy(v, extra)
		return v
	}
	for _, tc := range []struct {
		name  string
		extra url.Values
		tab   string
		kept  string
	}{
		{"public options", url.Values{"profile_options": {"{not json"}}, "sd-advanced", "{not json"},
		{"private options", url.Values{"options": {"{not private"}, "options_edit": {"1"}}, "sd-advanced", "{not private"},
		{"cookies", url.Values{"cookies_paste": {"not cookies"}}, "sd-auth-panel", "not cookies"},
		{"post url", url.Values{"post_url_template": {"nope"}}, "sd-profile", "nope"},
		{"user agent", url.Values{"user_agent": {"@firefox"}}, "sd-advanced", `value="@firefox"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := refusedSave(t, postForm(t, ts, srv, "/settings/sites/mysite", draft(tc.extra)))
			for _, want := range []string{"typed.example/posts/1", `value="typed.example"`, "typed_tag", "typed_target", "typed_suffix", "typed_rating", "typed label", "typed_rule", tc.kept} {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal dropped %q", want)
				}
			}
			if !strings.Contains(body, `data-refused-tab="`+tc.tab+`"`) {
				t.Errorf("the dialog should reopen on %s", tc.tab)
			}
		})
	}
}

func TestAStoredCredentialClearsOnItsOwn(t *testing.T) {
	srv, ts := serveWeb(t, "")
	mustUpdate(t, srv, func(c *config.Config) {
		c.Lookup.Saucenao.APIKey = "saucekey"
		c.Sites = []config.Site{{Name: "danbooru", Username: "u", Password: "pw", APIKey: "key", Gallery: "art"}}
	})
	postForm(t, ts, srv, "/settings/lookup/clear-key", url.Values{}).Body.Close()
	if srv.cfg.Current().Lookup.Saucenao.APIKey != "" {
		t.Error("the saucenao key survived its clear")
	}
	postForm(t, ts, srv, "/settings/sites/danbooru/clear/password", url.Values{}).Body.Close()
	site := srv.cfg.Current().FindSite("danbooru")
	if site.Password != "" || site.APIKey != "key" || site.Gallery != "art" || site.Username != "u" {
		t.Errorf("clearing the password: %+v, want only it gone", site)
	}
	postForm(t, ts, srv, "/settings/sites/danbooru/clear/api_key", url.Values{}).Body.Close()
	if site := srv.cfg.Current().FindSite("danbooru"); site.APIKey != "" || site.Gallery != "art" {
		t.Errorf("clearing the api key: %+v", site)
	}
	resp := postClosed(t, ts, srv, "/settings/sites/danbooru/clear/gallery", url.Values{})
	if resp.StatusCode != http.StatusNotFound || srv.cfg.Current().FindSite("danbooru").Gallery != "art" {
		t.Errorf("an unknown field answered %d", resp.StatusCode)
	}
}

func TestSiteRemoveAsksFirstAndTakesThePastedCookies(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pasted := filepath.Join(srv.cfg.Current().GalleryDL.CookiesDir, "danbooru.txt")
	if err := os.MkdirAll(filepath.Dir(pasted), 0o755); err != nil {
		t.Skipf("no cookies dir here: %v", err)
	}
	if err := os.WriteFile(pasted, []byte("# cookies"), 0o600); err != nil {
		t.Skipf("no cookies dir here: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(pasted) })
	mustUpdate(t, srv, func(c *config.Config) {
		c.Sites = []config.Site{{Name: "danbooru", Cookies: pasted}}
	})
	if _, page := get(t, ts, "/settings"); !strings.Contains(page, `action="/settings/sites/danbooru/reset" hx-confirm=`) {
		t.Error("Remove should ask first")
	}
	postForm(t, ts, srv, "/settings/sites/danbooru/reset", url.Values{}).Body.Close()
	if _, err := os.Stat(pasted); !os.IsNotExist(err) {
		t.Errorf("the pasted cookies stayed: %v", err)
	}
}

func TestASaveReachesWhatHoldsItsOwnCopy(t *testing.T) {
	srv, _ := serveWeb(t, "")
	mustUpdate(t, srv, func(c *config.Config) {
		c.GalleryDL.SleepRequest = 7
		c.Lookup.ScheduledDailyBudget = 3
	})
	written, err := os.ReadFile(srv.cfg.Current().GalleryDL.ConfigPath)
	if err != nil {
		t.Fatalf("the managed config was not rewritten: %v", err)
	}
	if !strings.Contains(string(written), `"sleep-request": 7`) {
		t.Errorf("managed config = %s, want the saved sleep", written)
	}
	if limit, _, _ := srv.queue.LookupBudget(); limit != 3 {
		t.Errorf("lookup budget = %d, want the saved 3", limit)
	}
}

func TestRawConfigIsWithheldUntilRevealed(t *testing.T) {
	srv, ts := serveWeb(t, "")
	const secret = `{"extractor":{"pixiv":{"refresh-token":"tok-secret"}}}`
	postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {secret}}).Body.Close()

	_, page := get(t, ts, "/settings")
	if strings.Contains(page, "tok-secret") {
		t.Error("the settings page echoes raw_config")
	}
	if !strings.Contains(page, `id="raw-config-reveal"`) {
		t.Error("a stored raw_config should offer a reveal")
	}

	postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {""}, "raw_config_edit": {"0"}}).Body.Close()
	if srv.cfg.Current().GalleryDL.RawConfig != secret {
		t.Error("a blank save with no reveal must leave it")
	}
	if _, body := get(t, ts, "/settings/raw"); !strings.Contains(body, "tok-secret") {
		t.Errorf("the reveal should answer the stored value, got %q", truncate(body))
	}
	postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {""}, "raw_config_edit": {"1"}}).Body.Close()
	if srv.cfg.Current().GalleryDL.RawConfig != "" {
		t.Error("a blank save after a reveal should clear it")
	}
}

func TestSaveRawValidAndInvalid(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {`{"extractor":{"timeout":30}}`}}).Body.Close()
	if srv.cfg.Current().GalleryDL.RawConfig == "" {
		t.Error("valid raw config should be saved")
	}
	broken := `{"extractor":{"timeout":45,"retries":5`
	resp := postForm(t, ts, srv, "/settings/raw", url.Values{"raw_config": {broken}})
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("invalid raw config answered %d, want the page back", resp.StatusCode)
	}
	if !strings.Contains(body, "must be a JSON object") {
		t.Errorf("refusal should name the reason, got %q", truncate(body))
	}
	if !strings.Contains(body, html.EscapeString(broken)) {
		t.Errorf("refusal should re-render the submitted block, got %q", truncate(body))
	}
	if srv.cfg.Current().GalleryDL.RawConfig != `{"extractor":{"timeout":30}}` {
		t.Error("invalid raw config should not have overwritten the saved value")
	}
	if !strings.Contains(body, `<body data-canonical-url="/settings#advanced">`) {
		t.Errorf("a refusal should name the settings page as its address, not the POST path, got %q", truncate(body))
	}
}

func TestAFlashPageNamesItsAddressWithoutTheFlash(t *testing.T) {
	_, ts := serveWeb(t, "")
	for path, want := range map[string]string{
		"/settings?kind=ok&section=sites&msg=Saved.": `<body data-canonical-url="/settings#sites">`,
		"/ptr?kind=err&msg=nope":                     `<body data-canonical-url="/ptr">`,
		"/settings":                                  "<body>",
	} {
		if _, body := get(t, ts, path); !strings.Contains(body, want) {
			t.Errorf("%s should carry %s", path, want)
		}
	}
}

func TestTestMonbooruButton(t *testing.T) {
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	ts := newTestServer(t, srv)

	resp := postForm(t, ts, srv, "/settings/monbooru/test", url.Values{"api_url": {mb.URL}})
	if body := readBody(t, resp); !strings.Contains(body, "flash-ok") {
		t.Errorf("reachable monbooru test should flash ok, body=%q", body)
	}
}

func TestTestMonbooruSendsTheTokenOnlyToTheSavedAddress(t *testing.T) {
	var gotAuth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()
	mb := monbooruStub()
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	storeCfg(srv, func(c *config.Config) { c.Monbooru.APIToken = "stored-token-value" })
	ts := newTestServer(t, srv)

	resp := postForm(t, ts, srv, "/settings/monbooru/test", url.Values{"api_url": {other.URL}})
	body := readBody(t, resp)
	if gotAuth != "" {
		t.Errorf("a typed address received Authorization %q", gotAuth)
	}
	if !strings.Contains(body, "only sent to the saved address") {
		t.Errorf("body = %q, want the failure to say why", body)
	}
}

func TestTestSiteButton(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/sites/danbooru/test", url.Values{})
	got := readBody(t, resp)
	if !strings.Contains(got, `flash-ok">ok<`) {
		t.Errorf("site probe should report ok in the row state cell, got %q", got)
	}
	if !strings.Contains(got, "site-last") || !strings.Contains(got, "just now") {
		t.Errorf("a successful probe should show the last-reached time, got %q", got)
	}
	if srv.siteState.LastReached("danbooru").IsZero() {
		t.Error("a successful probe should record the danbooru reach")
	}

	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{"gallery": {"art"}}).Body.Close()
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, "last ok") {
		t.Error("settings should show the last-reached note after a successful probe")
	}

	resp2 := postForm(t, ts, srv, "/settings/sites/nhentai/test", url.Values{})
	if got := readBody(t, resp2); !strings.Contains(got, `flash-ok">ok<`) {
		t.Errorf("profile example should let the probe run, got %q", got)
	}
}

func TestTestSiteStateWords(t *testing.T) {
	srv, ts := serveWeb(t, "")

	probe := func(site string, res gdl.ProbeResult) string {
		srv.runner = fakeRunner{probe: res}
		return readBody(t, postForm(t, ts, srv, "/settings/sites/"+site+"/test", url.Values{}))
	}

	if got := probe("e926", gdl.ProbeResult{Status: gdl.ProbeBlocked, Detail: "Cloudflare challenge (403 Forbidden)"}); !strings.Contains(got, ">blocked<") {
		t.Errorf("cloudflare probe should read blocked, got %q", got)
	}
	if got := probe("e926", gdl.ProbeResult{Status: gdl.ProbeRateLimited, Detail: "429 Too Many Requests"}); !strings.Contains(got, `flash-warn`) || !strings.Contains(got, ">rate limited<") {
		t.Errorf("a rate-limited probe should read as the lookup Test does, got %q", got)
	}
	if got := probe("sankaku", gdl.ProbeResult{Status: gdl.ProbeFailed, Detail: "invalid id"}); !strings.Contains(got, "needs cookies") {
		t.Errorf("cookies site without cookies should read needs cookies, got %q", got)
	}
	if got := probe("gelbooru", gdl.ProbeResult{Status: gdl.ProbeAuthRequired, Detail: "missing authentication"}); !strings.Contains(got, "needs API key") {
		t.Errorf("api-key site without key should read needs api key, got %q", got)
	}
	postForm(t, ts, srv, "/settings/sites/gelbooru", url.Values{"api_key": {"WRONGKEY"}}).Body.Close()
	if got := probe("gelbooru", gdl.ProbeResult{Status: gdl.ProbeAuthRequired, Detail: "401 Unauthorized"}); !strings.Contains(got, "auth rejected") {
		t.Errorf("api-key site with a rejected key should read auth rejected, got %q", got)
	}
}

func TestQueueActions(t *testing.T) {
	srv, ts := serveWeb(t, "")

	id := srv.queue.Enqueue("http://x", queue.Options{})
	resp := postClosed(t, ts, srv, "/queue/"+itoa(id)+"/retry", url.Values{})
	if resp.StatusCode != 200 {
		t.Errorf("retry status = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("DELETE", ts.URL+"/queue/"+itoa(id), nil)
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("cancel status = %d", resp2.StatusCode)
	}
}

func TestStaleCancelKeepsFinishedJob(t *testing.T) {
	srv, ts := serveWeb(t, "")

	id := finishJob(t, srv, "http://x")

	resp := postClosed(t, ts, srv, "/queue/"+itoa(id)+"/cancel", url.Values{})
	if resp.StatusCode != 200 {
		t.Fatalf("stale cancel status = %d", resp.StatusCode)
	}
	if _, err := srv.queue.Get(id); err != nil {
		t.Errorf("finished job gone after a stale cancel: %v", err)
	}
	if body := readBody(t, resp); strings.Contains(body, "no longer in the queue") {
		t.Errorf("a stale cancel should re-render the rows, not report the job gone: %q", truncate(body))
	}
}

func TestRowActionOnUnknownJobReportsGone(t *testing.T) {
	srv, ts := serveWeb(t, "")
	for _, path := range []string{"/queue/99999/retry", "/queue/99999/continue", "/queue/99999/continue-all", "/queue/99999/cancel"} {
		resp := postForm(t, ts, srv, path, url.Values{})
		body := readBody(t, resp)
		if !strings.Contains(body, "no longer in the queue") {
			t.Errorf("%s on an unknown id = %q, want the gone notice", path, truncate(body))
		}
		if resp.Header.Get("HX-Retarget") != "#add-flash" {
			t.Errorf("%s should retarget the notice to the add bar, got %q", path, resp.Header.Get("HX-Retarget"))
		}
	}
}

func TestContinueOnUncappedJobReportsRefusal(t *testing.T) {
	srv, ts := serveWeb(t, "")

	id := finishJob(t, srv, "http://x")

	resp := postForm(t, ts, srv, "/queue/"+itoa(id)+"/continue", url.Values{})
	defer resp.Body.Close()
	if got := resp.Header.Get("HX-Retarget"); got != "#add-flash" {
		t.Errorf("HX-Retarget = %q, want #add-flash", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no more posts to fetch") {
		t.Errorf("body = %q, want the refusal", body)
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 1 {
		t.Errorf("refused continue queued a job: %d jobs", total)
	}
}

func TestForceDownloadRetry(t *testing.T) {
	srv, ts := serveWeb(t, "")

	id := finishJob(t, srv, "http://x")

	resp := postClosed(t, ts, srv, "/queue/"+itoa(id)+"/retry?force=1", url.Values{})
	if resp.StatusCode != 200 {
		t.Fatalf("force download status = %d", resp.StatusCode)
	}
	job, err := srv.queue.Get(id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !job.Force {
		t.Error("force download did not set job.Force")
	}
}

func loginClient(t *testing.T, ts *httptest.Server, srv *Server, password string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var sid string
	for _, c := range resp.Cookies() {
		if c.Name == "monloader_session" {
			sid = c.Value
		}
	}
	if sid == "" {
		t.Fatal("login did not set a session cookie")
	}
	return client, srv.sessions.CSRFToken(sid)
}

func TestAuthSetInitialPassword(t *testing.T) {
	srv, ts := serveWeb(t, "")

	if body := readBody(t, postForm(t, ts, srv, "/settings/auth/password", url.Values{"new_password": {""}})); !strings.Contains(body, "required") {
		t.Errorf("empty new password should error, got %q", body)
	}
	body := readBody(t, postForm(t, ts, srv, "/settings/auth/password", url.Values{"new_password": {"hunter2"}}))
	if !srv.cfg.Current().Auth.EnablePassword || srv.cfg.Current().Auth.PasswordHash == "" {
		t.Fatalf("setting a password should enable auth, got %+v", srv.cfg.Current().Auth)
	}
	if !strings.Contains(body, "Password updated") || !strings.Contains(body, "auth-password-section") {
		t.Errorf("expected a flash and the OOB section, got %q", body)
	}
}

func TestSettingFirstPasswordKeepsTheSetterLoggedIn(t *testing.T) {
	srv, ts := serveWeb(t, "")
	resp := postClosed(t, ts, srv, "/settings/auth/password", url.Values{"new_password": {"hunter2"}})
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "monloader_session" {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie handed to the setter")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", ts.URL+"/queue", nil)
	req.AddCookie(sess)
	got, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Errorf("the setter's next page answered %d, want 200", got.StatusCode)
	}
}

func TestAuthPasswordChangeRemove(t *testing.T) {
	srv, ts := serveWeb(t, "secret")
	client, csrf := loginClient(t, ts, srv, "secret")
	post := func(path string, vals url.Values) string {
		vals.Set("_csrf", csrf)
		resp, err := client.PostForm(ts.URL+path, vals)
		if err != nil {
			t.Fatal(err)
		}
		return readBody(t, resp)
	}

	initialHash := srv.cfg.Current().Auth.PasswordHash
	if body := post("/settings/auth/password", url.Values{"current_password": {"wrong"}, "new_password": {"x"}}); !strings.Contains(body, "incorrect") {
		t.Errorf("a wrong current password should be rejected, got %q", body)
	}
	if srv.cfg.Current().Auth.PasswordHash != initialHash {
		t.Error("the hash should be unchanged after a rejected change")
	}
	post("/settings/auth/password", url.Values{"current_password": {"secret"}, "new_password": {"newsecret"}})
	if srv.cfg.Current().Auth.PasswordHash == initialHash {
		t.Error("the hash should change after a confirmed change")
	}
	post("/settings/auth/remove-password", url.Values{"current_password": {"newsecret"}})
	if srv.cfg.Current().Auth.EnablePassword || srv.cfg.Current().Auth.PasswordHash != "" {
		t.Errorf("remove should disable auth, got %+v", srv.cfg.Current().Auth)
	}
}

func TestAPasswordChangeSignsOutEveryOtherSession(t *testing.T) {
	srv, ts := serveWeb(t, "secret")
	client, csrf := loginClient(t, ts, srv, "secret")
	other, _ := loginClient(t, ts, srv, "secret")

	resp, err := client.PostForm(ts.URL+"/settings/auth/password", url.Values{"_csrf": {csrf}, "current_password": {"secret"}, "new_password": {"newsecret"}})
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, resp)

	noRedirect := func(c *http.Client) int {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Get(ts.URL + "/settings")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := noRedirect(client); code != http.StatusOK {
		t.Errorf("the session that changed the password got %d, want it kept", code)
	}
	if code := noRedirect(other); code != http.StatusSeeOther {
		t.Errorf("another session got %d after the change, want a redirect to login", code)
	}
}

func TestLoginRefusesAnOversizedBody(t *testing.T) {
	_, ts := serveWeb(t, "secret")
	resp, err := http.Post(ts.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader("password="+strings.Repeat("a", maxFormBytes+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSeeOther {
		t.Errorf("an oversized login body got %d", resp.StatusCode)
	}
}

func TestAuthPasswordChangeChecksAHashLeftWithAuthOff(t *testing.T) {
	srv, ts := serveWeb(t, "secret")
	mustUpdate(t, srv, func(c *config.Config) {
		c.Auth.EnablePassword = false
	})
	hash := srv.cfg.Current().Auth.PasswordHash

	if _, body := get(t, ts, "/settings"); !strings.Contains(body, `id="pw-current"`) {
		t.Error("the settings page should ask for the current password while a hash is set")
	}
	for _, current := range []string{"", "wrong"} {
		body := readBody(t, postForm(t, ts, srv, "/settings/auth/password", url.Values{"current_password": {current}, "new_password": {"x"}}))
		if !strings.Contains(body, "incorrect") {
			t.Errorf("current password %q should be rejected, got %q", current, body)
		}
	}
	if got := srv.cfg.Current().Auth; got.PasswordHash != hash || got.EnablePassword {
		t.Fatalf("a rejected change should leave the hash and the flag alone, got %+v", got)
	}
	readBody(t, postForm(t, ts, srv, "/settings/auth/password", url.Values{"current_password": {"secret"}, "new_password": {"newsecret"}}))
	if got := srv.cfg.Current().Auth; got.PasswordHash == hash || !got.EnablePassword {
		t.Errorf("a confirmed change should replace the hash and turn auth on, got %+v", got)
	}
}

func TestAuthTokenCreate(t *testing.T) {
	srv, ts := serveWeb(t, "")

	resp := postForm(t, ts, srv, "/settings/auth/tokens", url.Values{"name": {"ci"}})
	if trig := resp.Header.Get("HX-Trigger"); trig != "token-created" {
		t.Errorf("a created token should trigger token-created to reset the form, got %q", trig)
	}
	body := readBody(t, resp)
	toks := srv.cfg.Current().Auth.Tokens
	if len(toks) != 1 || toks[0].Name != "ci" {
		t.Fatalf("want one token named ci, got %+v", toks)
	}
	m := regexp.MustCompile(`value="([a-f0-9]{32})"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("response should reveal a 32-hex secret once, got %q", body)
	}
	if apitoken.Hash(m[1]) != toks[0].TokenHash {
		t.Error("revealed secret does not match the stored hash")
	}
}

func TestNotFound(t *testing.T) {
	_, ts := serveWeb(t, "")

	if code, body := get(t, ts, "/no-such-page"); code != http.StatusNotFound {
		t.Errorf("GET /no-such-page = %d, want 404", code)
	} else if !strings.Contains(body, `class="mascot-page-code">404<`) || !strings.Contains(body, "Back to monloader") || !strings.Contains(body, `src="/static/mascot-lost.png"`) {
		t.Errorf("themed 404 page expected, got %q", body)
	}

	if code, body := get(t, ts, "/api/v1/no-such-endpoint"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/no-such-endpoint = %d, want 404", code)
	} else if !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("JSON 404 expected for an API path, got %q", body)
	}

	if code, _ := get(t, ts, "/"); code != http.StatusOK {
		t.Errorf("GET / = %d, want 200 (add screen still resolves)", code)
	}
}

func TestAuthTokenPrivileges(t *testing.T) {
	srv, ts := serveWeb(t, "")

	readBody(t, postForm(t, ts, srv, "/settings/auth/tokens", url.Values{"name": {"ci"}}))
	id := srv.cfg.Current().Auth.Tokens[0].ID
	saved := readBody(t, postForm(t, ts, srv, "/settings/auth/tokens/"+id+"/privileges", url.Values{"scope": {"read"}}))
	if want := `<span id="token-scopes-` + id + `" hx-swap-oob="true">read</span>`; !strings.Contains(saved, want) || !strings.Contains(saved, "Token privileges saved.") {
		t.Errorf("save answered %q, want the row's scopes swapped and the notice", saved)
	}
	got := srv.cfg.Current().Auth.Tokens[0].Scopes
	if len(got) != 1 || got[0] != config.ScopeRead {
		t.Fatalf("scopes = %v, want [read]", got)
	}

	status, body := get(t, ts, "/settings/auth/tokens/"+id+"/privileges")
	if status != http.StatusOK {
		t.Fatalf("privileges dialog = %d, want 200", status)
	}
	if !strings.Contains(body, `value="read" checked`) || strings.Contains(body, `value="write" checked`) {
		t.Errorf("dialog should check exactly the token's scopes, got %q", body)
	}
	if strings.Contains(body, "disabled") || !strings.Contains(body, ">Save<") {
		t.Errorf("an ordinary token's scopes should be editable, got %q", body)
	}
	if status, _ := get(t, ts, "/settings/auth/tokens/nosuchtoken/privileges"); status != http.StatusNotFound {
		t.Errorf("unknown token = %d, want 404", status)
	}

	mustUpdate(t, srv, func(c *config.Config) {
		c.Auth.Tokens[0].Paired = "monsender"
	})
	_, body = get(t, ts, "/settings/auth/tokens/"+id+"/privileges")
	if !strings.Contains(body, "disabled") || !strings.Contains(body, "Managed by a pairing") {
		t.Errorf("a paired token's scopes should render read-only, got %q", body)
	}
}

func revokeToken(srv *Server, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("DELETE", "/settings/auth/tokens/"+id, nil)
	req.SetPathValue("id", id)
	return record(srv.settingsTokenRevoke, req)
}

func TestRevokingTheMonbooruRowRemovesThePairing(t *testing.T) {
	var teardown string
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pair/remove" {
			teardown = r.Header.Get("Authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mb.Close()
	srv := newWebServer(t, mb.URL, "")
	pairMBWith(t, srv, "peer-secret")
	mustUpdate(t, srv, func(c *config.Config) { c.Monbooru.Paused = true })

	w := revokeToken(srv, srv.cfg.Current().Auth.Tokens.Paired("monbooru").ID)

	if c := srv.cfg.Current(); srv.hasPairedToken("monbooru") || c.Monbooru.APIToken != "" || c.Monbooru.Paused {
		t.Errorf("the monbooru link outlived its token: %+v", c.Monbooru)
	}
	if teardown != "Bearer peer-secret" {
		t.Errorf("monbooru was called back with %q", teardown)
	}
	body := w.Body.String()
	if !strings.Contains(body, "monbooru pairing removed") || !strings.Contains(body, `id="monbooru-pair" hx-swap-oob="true"`) {
		t.Errorf("the flash or the refreshed card is missing: %q", body)
	}
}

func TestRevokingTheMonbooruRowWarnsWhenUnreachable(t *testing.T) {
	srv := newWebServer(t, "http://ml", "")
	pairMBWith(t, srv, "peer-secret")

	w := revokeToken(srv, srv.cfg.Current().Auth.Tokens.Paired("monbooru").ID)

	if srv.hasPairedToken("monbooru") {
		t.Error("local pairing not removed")
	}
	if !strings.Contains(w.Body.String(), `flash-warn`) || !strings.Contains(w.Body.String(), "remove the pairing in monbooru too") {
		t.Errorf("an unreachable monbooru should warn, got %q", w.Body.String())
	}
}

func TestRevokingAnAppRowLeavesTheMonbooruLink(t *testing.T) {
	srv := newWebServer(t, "http://ml", "")
	pairMBWith(t, srv, "peer-secret")
	tok, _ := apitoken.New("monsender (paired)", config.AllScopes)
	tok.Paired = "monsender"
	addToken(t, srv, tok)

	w := revokeToken(srv, tok.ID)

	if srv.hasPairedToken("monsender") {
		t.Error("monsender's token kept")
	}
	if !srv.hasPairedToken("monbooru") || srv.cfg.Current().Monbooru.APIToken != "peer-secret" {
		t.Error("revoking an app's token touched the monbooru link")
	}
	if !strings.Contains(w.Body.String(), "Token revoked.") || strings.Contains(w.Body.String(), "monbooru-pair") {
		t.Errorf("got %q", w.Body.String())
	}
}

func TestRevokingAnUnknownToken(t *testing.T) {
	srv := stubbedServer(t, "")
	if w := revokeToken(srv, "nope"); !strings.Contains(w.Body.String(), "Token not found.") {
		t.Errorf("got %q", w.Body.String())
	}
}

func TestUnmanagedPairedTokenRevocable(t *testing.T) {
	srv := stubbedServer(t, "")
	tok, _ := apitoken.New("monsender-dev (paired)", config.AllScopes)
	tok.Paired = "monsender-dev"
	addToken(t, srv, tok)
	managed, _ := apitoken.New("monsender (paired)", config.AllScopes)
	managed.Paired = "monsender"
	addToken(t, srv, managed)

	page := httptest.NewRecorder()
	srv.render(page, "webui/auth_tokens", map[string]any{"Tokens": srv.tokenRows()})
	if !strings.Contains(page.Body.String(), `hx-delete="/settings/auth/tokens/`+tok.ID+`"`) {
		t.Errorf("no Revoke for a pairing with no panel: %s", page.Body)
	}
	if strings.Contains(page.Body.String(), `hx-delete="/settings/auth/tokens/`+managed.ID+`"`) || !strings.Contains(page.Body.String(), "via pairing") {
		t.Errorf("monsender's token should read via pairing: %s", page.Body)
	}

	req := httptest.NewRequest("DELETE", "/settings/auth/tokens/"+tok.ID, nil)
	req.SetPathValue("id", tok.ID)
	record(srv.settingsTokenRevoke, req)

	if srv.cfg.Current().Auth.Tokens.ByID(tok.ID) != nil {
		t.Errorf("token kept: %v", srv.cfg.Current().Auth.Tokens)
	}
}

func TestPairedTokenPrivilegesLocked(t *testing.T) {
	srv := stubbedServer(t, "")
	tok, _ := apitoken.New("monbooru (paired)", config.AllScopes)
	tok.Paired = "monbooru"
	addToken(t, srv, tok)

	form := url.Values{"scope": {config.ScopeRead}}
	req := httptest.NewRequest("POST", "/settings/auth/tokens/"+tok.ID+"/privileges", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", tok.ID)
	w := record(srv.settingsTokenPrivilegesPost, req)

	if got := strings.Join(srv.cfg.Current().Auth.Tokens[0].Scopes, " "); got != strings.Join(config.AllScopes, " ") {
		t.Fatalf("paired token scopes changed to %q; want them unchanged (manage via pairing)", got)
	}
	if !strings.Contains(w.Body.String(), "pairing") {
		t.Errorf("expected a pairing hint, got %q", w.Body.String())
	}
}

func TestClearQueueButton(t *testing.T) {
	srv, ts := serveWeb(t, "")
	for _, path := range []string{"/queue/clear", "/queue/clear-succeeded"} {
		resp := postClosed(t, ts, srv, path, url.Values{})
		if resp.StatusCode != 200 {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestStatsRSS(t *testing.T) {
	srv := stubbedServer(t, "")
	st := srv.gatherStats()
	if st.Mem.RSS == 0 {
		t.Skip("RSS unavailable (non-Linux)")
	}

	if st.Mem.RSS < st.Mem.HeapAlloc {
		t.Errorf("RSS %d should be >= live heap %d", st.Mem.RSS, st.Mem.HeapAlloc)
	}
}

func TestLoginFlow(t *testing.T) {
	srv, ts := serveWeb(t, "secret")

	if code, body := get(t, ts, "/login"); code != 200 || !strings.Contains(body, "password") {
		t.Fatalf("login page: code=%d", code)
	}
	jar := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := jar.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {"secret"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct password status = %d, want 303", resp.StatusCode)
	}
	wrong, _ := jar.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {"wrong"}})
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password status = %d, want 401", wrong.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "monloader_session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("no session cookie set on login")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/queue", nil)
	req.AddCookie(cookie)
	resp, err := jar.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("authenticated /queue status = %d, want 200", resp.StatusCode)
	}
}

func TestALoginUnderAHugeSessionLifetimeStaysLoggedIn(t *testing.T) {
	srv, ts := serveWeb(t, "secret")
	mustUpdate(t, srv, func(c *config.Config) {
		c.Auth.SessionLifetimeDays = 200000
	})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/queue", nil)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	page, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Errorf("/queue after the login = %d, want 200: the session expired at once", page.StatusCode)
	}
}

func TestSessionCookiesKeepTheirFlags(t *testing.T) {
	srv, ts := serveWeb(t, "secret")
	jar := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, form url.Values, cookies ...*http.Cookie) *http.Cookie {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		req.Header.Set("X-Forwarded-Proto", "https")
		resp, err := jar.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == "monloader_session" {
				return c
			}
		}
		t.Fatalf("%s set no session cookie", path)
		return nil
	}
	in := post("/login", url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {"secret"}})
	if !in.Secure || !in.HttpOnly || in.SameSite != http.SameSiteLaxMode {
		t.Errorf("login cookie = %+v, want Secure, HttpOnly, SameSite=Lax", in)
	}
	out := post("/logout", url.Values{"_csrf": {srv.sessions.CSRFToken(in.Value)}}, in)
	if out.MaxAge >= 0 || !out.Secure || !out.HttpOnly || out.SameSite != http.SameSiteLaxMode {
		t.Errorf("logout cookie = %+v, want an expiry with the login cookie's flags", out)
	}
}

func TestLoginBacksOffAfterAFailure(t *testing.T) {
	srv, ts := serveWeb(t, "secret")
	jar := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login := func(password string) (*http.Response, string) {
		resp, err := jar.PostForm(ts.URL+"/login", url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {password}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	if resp, _ := login("wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", resp.StatusCode)
	}
	resp, body := login("secret")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("an immediate retry status = %d, want 429", resp.StatusCode)
	}
	if !strings.Contains(body, "Wait 1 second before trying again.") {
		t.Errorf("the refusal does not name the second left: %s", body)
	}
	srv.loginRL.Fail("127.0.0.1")
	srv.loginRL.Fail("127.0.0.1")
	if _, body := login("secret"); !strings.Contains(body, "Wait 4 seconds before trying again.") {
		t.Errorf("after three failures the refusal does not name the four seconds left: %s", body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "monloader_session" {
			t.Error("a refused retry issued a session cookie")
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAddBarHashImport(t *testing.T) {
	srv, ts := serveWeb(t, "")

	for _, target := range []string{
		"0123456789ABCDEF0123456789abcdef",
		"md5:0123456789abcdef0123456789abcdef",
		"MD5:0123456789ABCDEF0123456789ABCDEF",
	} {
		postForm(t, ts, srv, "/", url.Values{"url": {target}}).Body.Close()
	}
	jobs, total := srv.queue.List(queue.ListOptions{})
	if total != 3 {
		t.Fatalf("want 3 hash-import jobs, got %d", total)
	}
	for _, j := range jobs {
		if j.Kind != queue.KindHashImport || j.MD5 != "0123456789abcdef0123456789abcdef" {
			t.Errorf("job kind=%s md5=%q, want a hash import on the lower-cased md5", j.Kind, j.MD5)
		}
	}
}

func TestAddBarSHA256Rejected(t *testing.T) {
	srv, ts := serveWeb(t, "")

	sha := strings.Repeat("a", 64)
	body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {sha}}))

	if !strings.Contains(body, "which is off") || !strings.Contains(body, "PTR page") {
		t.Errorf("a sha256 paste should point at the ptr page, got %q", body)
	}
	upper := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {"SHA256:" + strings.ToUpper(sha)}}))
	if !strings.Contains(upper, "PTR page") {
		t.Errorf("an upper-case sha256: paste should get the same hint, got %q", upper)
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("a sha256 must not enqueue, queue has %d", total)
	}
}

func TestAddBarSHA256HintFollowsSyncState(t *testing.T) {
	for _, tc := range []struct {
		state ptr.State
		want  string
	}{
		{ptr.StateSyncing, "finished syncing"},
		{ptr.StateReady, "not for importing here"},
	} {
		srv, ts := serveEngine(t, &stubEngine{status: ptr.Status{Enabled: true, State: tc.state, Queryable: tc.state == ptr.StateReady}})
		body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {strings.Repeat("a", 64)}}))
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s hint = %q, want it to mention %q", tc.state, body, tc.want)
		}
	}
}

func TestAddBarMD5RejectedWhenNoLookupSite(t *testing.T) {
	srv, ts := serveWeb(t, "")

	mustUpdate(t, srv, func(c *config.Config) {
		c.Sites = []config.Site{{Name: "danbooru", LookupOrder: 0}}
	})

	body := readBody(t, postForm(t, ts, srv, "/", url.Values{"url": {"0123456789abcdef0123456789abcdef"}}))
	if !strings.Contains(body, "MD5 lookup is off") {
		t.Errorf("md5 import with no lookup site should flash the off hint, got %q", body)
	}
	if _, total := srv.queue.List(queue.ListOptions{}); total != 0 {
		t.Errorf("nothing should enqueue when md5 lookup is off, queue has %d", total)
	}
}

func TestAddBarShowsLookupStatus(t *testing.T) {
	srv, ts := serveWeb(t, "")

	if _, body := get(t, ts, "/"); strings.Contains(body, "lookup-status") {
		t.Errorf("a working chain should show no lookup line, got %q", truncate(body))
	}
	mustUpdate(t, srv, func(c *config.Config) {
		for i := range c.Sites {
			c.Sites[i].LookupOrder = 0
		}
	})
	if _, body := get(t, ts, "/queue"); !strings.Contains(body, "MD5 lookup is off") {
		t.Errorf("an empty chain should say MD5 lookup is off, got %q", truncate(body))
	}
}

func TestSaveSiteIgnoresStaleLookupOrder(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/gelbooru", url.Values{"api_key": {"k"}, "lookup_order": {"9"}}).Body.Close()
	site := srv.cfg.Current().FindSite("gelbooru")
	if site.APIKey != "k" || site.LookupOrder != 4 {
		t.Errorf("gelbooru = %+v, want the key saved and the seeded order 4 kept", site)
	}
}

func TestSaveLookupChain(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/lookup/chain", url.Values{
		"order-iqdb":     {"1"},
		"order-danbooru": {"2"},
		"order-yandere":  {"3"},
		"order-saucenao": {""},
	}).Body.Close()
	cfg := srv.cfg.Current()
	if cfg.Lookup.Iqdb.Order != 1 || cfg.Lookup.Saucenao.Order != 0 {
		t.Errorf("service orders = %+v, want iqdb 1 and saucenao cleared", cfg.Lookup)
	}
	if cfg.FindSite("danbooru").LookupOrder != 2 || cfg.FindSite("yandere").LookupOrder != 3 {
		t.Errorf("site orders wrong: %+v", cfg.Sites)
	}

	resp := postForm(t, ts, srv, "/settings/lookup/chain", url.Values{"order-nhentai": {"1"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "does+not+support+MD5+lookup") {
		t.Errorf("order on a templateless site should be refused, got redirect %q", loc)
	}
	if s := srv.cfg.Current().FindSite("nhentai"); s != nil && s.LookupOrder != 0 {
		t.Errorf("nhentai should not have kept an order: %+v", s)
	}
}

func TestSaveLookup(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/lookup", url.Values{"min_similarity": {"90"}}).Body.Close()
	if got := srv.cfg.Current().Lookup.MinSimilarity; got != 90 {
		t.Errorf("min_similarity = %d, want 90", got)
	}
	body := refusedSave(t, postForm(t, ts, srv, "/settings/lookup", url.Values{"min_similarity": {"150"}}))
	if !strings.Contains(body, "percentage from 1 to 100") || srv.cfg.Current().Lookup.MinSimilarity != 90 {
		t.Errorf("an out-of-range floor should be refused, got %q and %d", truncate(body), srv.cfg.Current().Lookup.MinSimilarity)
	}

	postForm(t, ts, srv, "/settings/lookup", url.Values{"api_key": {"sk-1"}}).Body.Close()
	postForm(t, ts, srv, "/settings/lookup", url.Values{"api_key": {""}, "min_similarity": {"85"}}).Body.Close()
	if cfg := srv.cfg.Current(); cfg.Lookup.Saucenao.APIKey != "sk-1" || cfg.Lookup.MinSimilarity != 85 {
		t.Errorf("lookup = %+v, want the key kept and the floor updated", cfg.Lookup)
	}
}

type stubSim struct {
	missing map[string]string
	res     similarity.Result
	err     error
}

func (s *stubSim) Missing(service string) (string, bool) {
	label, ok := s.missing[service]
	return label, ok
}

func (s *stubSim) Search(context.Context, string, []byte) (similarity.Result, error) {
	return s.res, s.err
}

func (s *stubSim) LimitedUntil(service string) time.Time { return time.Time{} }

type stubMetaRunner struct {
	fakeRunner
	err  error
	urls []string
}

func (s *stubMetaRunner) FetchMeta(_ context.Context, url string) (map[string]any, error) {
	s.urls = append(s.urls, url)
	return nil, s.err
}

func TestLookupPanelRenders(t *testing.T) {
	srv := stubbedServer(t, "")
	srv.sim = &stubSim{missing: map[string]string{"iqdb": "danbooru API key", "saucenao": "API key"}}
	ts := newTestServer(t, srv)

	_, body := get(t, ts, "/settings")
	for _, want := range []string{
		"exact md5", "similarity",
		"needs danbooru API key", "needs API key",
		"uses the danbooru site credentials", "API key: not set",
		"Min similarity", "chain-dialog", "saucenao-dialog",
		`id="chain-on"`, `id="chain-off"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	if strings.Contains(body, "se-field-lookup") || strings.Contains(body, `name="lookup_order"`) {
		t.Error("the site dialog should have dropped its lookup-order field")
	}
	js, err := web.FS.ReadFile("static/main.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `"order-"`) {
		t.Error(`main.js should build the order-<name> fields saveLookupChain reads`)
	}
}

func TestLookupTestButton(t *testing.T) {
	srv, ts := serveWeb(t, "")

	post := func(service string) string {
		req, _ := http.NewRequest("POST", ts.URL+"/settings/lookup/test/"+service, nil)
		req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return readBody(t, resp)
	}

	srv.sim = &stubSim{missing: map[string]string{"saucenao": "API key"}}
	if body := post("saucenao"); !strings.Contains(body, "needs API key") {
		t.Errorf("keyless saucenao test = %q", body)
	}
	srv.sim = &stubSim{res: similarity.Result{DailyRemaining: 94}}
	if body := post("saucenao"); !strings.Contains(body, "ok, 94 queries left today") {
		t.Errorf("saucenao test = %q, want the quota note", body)
	}
	srv.sim = &stubSim{res: similarity.Result{DailyRemaining: -1}}
	if body := post("iqdb"); !strings.Contains(body, ">ok<") {
		t.Errorf("iqdb test = %q, want a bare ok", body)
	}
	srv.sim = &stubSim{err: &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: "cooling down"}}
	if body := post("iqdb"); !strings.Contains(body, "rate limited") {
		t.Errorf("limited test = %q", body)
	}
	mr := &stubMetaRunner{err: &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: "no post metadata"}}
	srv.runner = mr
	if body := post("danbooru"); !strings.Contains(body, ">ok<") {
		t.Errorf("danbooru md5 test = %q, want ok on a clean miss", body)
	}
	sum := md5.Sum(similarity.ProbeImage())
	if len(mr.urls) != 1 || !strings.Contains(mr.urls[0], hex.EncodeToString(sum[:])) {
		t.Errorf("fetches = %v, want one search for the probe md5", mr.urls)
	}
	srv.runner = &stubMetaRunner{err: &outcome.CodedError{Code: outcome.ErrCodeAuthRequired, Msg: "401"}}
	if body := post("danbooru"); !strings.Contains(body, "auth rejected") {
		t.Errorf("danbooru md5 test = %q, want auth rejected", body)
	}
	if body := post("gelbooru"); !strings.Contains(body, "needs API key") {
		t.Errorf("keyless gelbooru test = %q", body)
	}
	if resp, err := http.Post(ts.URL+"/settings/lookup/test/nosuch", "", nil); err == nil {
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
			t.Errorf("unknown service = %d, want a refusal", resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestSiteSearch(t *testing.T) {
	srv, ts := serveWeb(t, "")

	_, body := get(t, ts, "/settings/sites/search?q=twit")
	if !strings.Contains(body, `data-site="twitter"`) || !strings.Contains(body, ">Add<") {
		t.Errorf("twitter should be offered for adding, got %q", body)
	}
	if _, dlg := get(t, ts, "/settings/sites/twitter/dialog"); !strings.Contains(dlg, `value="cookies" selected`) {
		t.Errorf("the supportedsites Cookies row should seed the cookies auth kind, got %q", dlg)
	}
	if !strings.Contains(body, "Twitter") {
		t.Errorf("the display name should render, got %q", body)
	}

	if _, body := get(t, ts, "/settings/sites/search?q=Twitt"); !strings.Contains(body, `data-site="twitter"`) {
		t.Errorf("a display-name match should find the site, got %q", body)
	}

	if _, body := get(t, ts, "/settings/sites/search?q=aibooru"); !strings.Contains(body, `data-site="aibooru"`) {
		t.Errorf("a profiled instance should be findable, got %q", body)
	}

	postForm(t, ts, srv, "/settings/sites/twitter", url.Values{"gallery": {"art"}}).Body.Close()
	if _, body := get(t, ts, "/settings/sites/search?q=twitter"); !strings.Contains(body, "disabled") {
		t.Errorf("a configured site's add button should be disabled, got %q", body)
	}

	if _, body := get(t, ts, "/settings/sites/search?q="); strings.TrimSpace(body) != "" {
		t.Errorf("an empty query should render nothing, got %q", body)
	}

	if _, body := get(t, ts, "/settings/sites/search?q=zzzznothing"); !strings.Contains(body, "No supported site matches") {
		t.Errorf("a miss should say so, got %q", body)
	}
}

func TestSiteProfileEditor(t *testing.T) {
	srv, ts := serveWeb(t, "")

	_, body := get(t, ts, "/settings/sites/danbooru/dialog")
	if !strings.Contains(body, `value="danbooru" selected`) {
		t.Errorf("the danbooru family should be selected, got %q", body)
	}
	if strings.Contains(body, "Reset profile") {
		t.Error("a shipped profile should offer no reset")
	}

	_, body = get(t, ts, "/settings/sites/twitter/dialog")
	for _, want := range []string{`value="generic" selected`, `value="other" selected`, `value="cookies" selected`} {
		if !strings.Contains(body, want) {
			t.Errorf("twitter form missing seed %q", want)
		}
	}

	resp := postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"username_password"},
		"gallery":     {"art"},
		"rating_from": {"e", "t", ""}, "rating_to": {"general", "sensitive", "general"},
		"rule_from": {"Newgrounds Logo", ""}, "rule_to": {"", ""},
	})
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "kind=ok") {
		t.Fatalf("dialog save should flash ok, got %q", loc)
	}
	if !srv.mapper.CustomProfile("newgrounds") {
		t.Fatal("the saved profile should be live")
	}
	p, _ := srv.mapper.Lookup("newgrounds")
	if p.RatingOverrides["e"] != "general" || p.RatingOverrides["t"] != "sensitive" {
		t.Errorf("rating map not saved: %+v", p.RatingOverrides)
	}
	if to, ok := p.TagRules["newgrounds_logo"]; !ok || to != "" {
		t.Errorf("the rule spelling should be normalized and suppress, got %+v", p.TagRules)
	}
	if got := srv.cfg.Current().FindSite("newgrounds"); got == nil || got.Gallery != "art" {
		t.Errorf("the site block should save in the same pass, got %+v", got)
	}
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, "custom profile") {
		t.Error("the sites table should mark the custom profile")
	}
	if _, body := get(t, ts, "/settings/sites/newgrounds/dialog"); !strings.Contains(body, "Reset profile") {
		t.Error("a custom profile should offer reset")
	}

	body = refusedSave(t, postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{
		"family": {"generic"}, "post_url_template": {"https://x.example/posts"},
	}))
	if !strings.Contains(body, "at least one {placeholder}") {
		t.Errorf("a template without a placeholder should be refused, got %q", truncate(body))
	}
	if !strings.Contains(body, `value="https://x.example/posts"`) {
		t.Errorf("the refused dialog dropped what was typed, got %q", truncate(body))
	}

	postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{"gallery": {"art2"}}).Body.Close()
	if !srv.mapper.CustomProfile("newgrounds") || srv.cfg.Current().FindSite("newgrounds").Gallery != "art2" {
		t.Error("a profile-less save should keep the profile and update the block")
	}

	postForm(t, ts, srv, "/settings/sites/newgrounds/profile/reset", url.Values{}).Body.Close()
	if srv.mapper.CustomProfile("newgrounds") {
		t.Error("reset should revert the category")
	}
}

func TestSiteDialogWithholdsTheStoredCredentials(t *testing.T) {
	srv, ts := serveWeb(t, "")
	postForm(t, ts, srv, "/settings/sites/twitter", url.Values{
		"username": {"me"}, "password": {"PW-SECRET-zzz"}, "api_key": {"KEY-SECRET-zzz"},
	}).Body.Close()
	site := srv.cfg.Current().FindSite("twitter")
	if site == nil || site.Password != "PW-SECRET-zzz" || site.APIKey != "KEY-SECRET-zzz" {
		t.Fatalf("site not saved: %+v", site)
	}

	_, body := get(t, ts, "/settings/sites/twitter/dialog")
	for _, secret := range []string{"PW-SECRET-zzz", "KEY-SECRET-zzz"} {
		if strings.Contains(body, secret) {
			t.Errorf("the dialog echoed %s", secret)
		}
	}
	if n := strings.Count(body, "(set - leave blank to keep)"); n != 2 {
		t.Errorf("set/unset markers = %d, want both credentials marked set", n)
	}
}

func TestSaveSiteRefusalWritesNothing(t *testing.T) {
	srv, ts := serveWeb(t, "")

	for _, tc := range []struct {
		name  string
		extra url.Values
	}{
		{"a paste that is not a cookies export", url.Values{"cookies_paste": {"just some text"}}},
		{"site options that are not JSON", url.Values{"options": {"{not json"}}},
		{"a user agent gallery-dl would open a browser for", url.Values{"user_agent": {"browser"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{
				"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
				"rule_from": {"scenery"}, "rule_to": {"renamed_by_a_failed_save"},
			}
			for k, v := range tc.extra {
				form[k] = v
			}
			body := refusedSave(t, postForm(t, ts, srv, "/settings/sites/newgrounds", form))
			if !strings.Contains(body, `value="renamed_by_a_failed_save"`) {
				t.Errorf("the refused dialog dropped what was typed, got %q", truncate(body))
			}
			if srv.mapper.CustomProfile("newgrounds") {
				t.Error("a refused save wrote the mapping profile")
			}
			if p, _ := srv.mapper.Lookup("newgrounds"); p.TagRules["scenery"] != "" {
				t.Errorf("a refused save left a live tag rule: %+v", p.TagRules)
			}
		})
	}
}

func TestSaveSiteRefusesStarInTagRuleKey(t *testing.T) {
	srv, ts := serveWeb(t, "")

	body := refusedSave(t, postForm(t, ts, srv, "/settings/sites/newgrounds", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"rule_from": {"*_(va)"}, "rule_to": {"va:*"},
	}))
	for _, want := range []string{`data-refused-tab="sd-mapping"`, `value="*_(va)"`, `value="va:*"`, "names one exact tag"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refused dialog lacks %q, got %q", want, truncate(body))
		}
	}
	if srv.mapper.CustomProfile("newgrounds") {
		t.Error("a refused save wrote the mapping profile")
	}
}

func TestSiteDialogExportTab(t *testing.T) {
	srv, ts := serveWeb(t, "")

	_, body := get(t, ts, "/settings/sites/danbooru/dialog")
	if !strings.Contains(body, `id="sd-export-json"`) {
		t.Fatalf("export textarea missing, got %q", body)
	}
	if !strings.Contains(body, `&#34;post_url_template&#34;: &#34;https://danbooru.donmai.us/posts/{id}&#34;`) {
		t.Error("the export payload should carry the shipped profile")
	}
	if strings.Contains(body, `class="exp-add"`) {
		t.Error("a stock profile should show no changed lines")
	}

	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"family": {"danbooru"}, "kind": {"booru"}, "auth": {"cookies"},
		"post_url_template":   {"https://danbooru.donmai.us/posts/{id}"},
		"md5_search_template": {"https://danbooru.donmai.us/posts?tags=md5:{md5}"},
		"rule_from":           {"engrish"}, "rule_to": {""},
	}).Body.Close()
	_, body = get(t, ts, "/settings/sites/danbooru/dialog")
	if !strings.Contains(body, `<span class="exp-add">  &#34;auth&#34;: &#34;cookies&#34;,</span>`) {
		t.Error("the new auth value should read added")
	}
	if !strings.Contains(body, `<span class="exp-del">  &#34;auth&#34;: &#34;api_optional&#34;,</span>`) {
		t.Error("the displaced shipped auth should read struck")
	}
	if !strings.Contains(body, `exp-del">  &#34;example&#34;`) {
		t.Error("the dropped shipped example should read struck")
	}
	if !strings.Contains(body, "tag_rules") {
		t.Error("the export should carry the added tag rule")
	}
	if strings.Contains(body, `id="sd-export-json" hidden readonly>`) &&
		strings.Contains(strings.SplitN(strings.SplitN(body, `id="sd-export-json"`, 2)[1], "</textarea>", 2)[0], "api_optional") {
		t.Error("the copy payload should not carry displaced built-in values")
	}
}

func TestSiteDialogExportCommaShift(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/yandere", url.Values{
		"family": {"moebooru"}, "kind": {"booru"}, "auth": {"none"},
		"post_url_template":   {"https://yande.re/post/show/{id}"},
		"md5_search_template": {"https://yande.re/post?tags=md5:{md5}"},
		"example":             {"https://yande.re/post/show/1211519"},
		"host_alias":          {"files.yande.re"},
	}).Body.Close()
	_, body := get(t, ts, "/settings/sites/yandere/dialog")
	if !strings.Contains(body, `<span class="exp-add">  &#34;hosts&#34;: [</span>`) {
		t.Fatalf("the added host list should read added, got %q", body)
	}
	if strings.Contains(body, `exp-add">  &#34;example&#34;`) || strings.Contains(body, `exp-del">  &#34;example&#34;`) {
		t.Error("an unchanged example should read as shipped")
	}
}

func TestSiteDialogExportChangedLastKey(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/yandere", url.Values{
		"family": {"moebooru"}, "kind": {"booru"}, "auth": {"none"},
		"post_url_template":   {"https://yande.re/post/show/{id}"},
		"md5_search_template": {"https://yande.re/post?tags=md5:{md5}"},
		"example":             {"https://yande.re/post/show/999999"},
		"host_alias":          {"files.yande.re"},
	}).Body.Close()
	_, body := get(t, ts, "/settings/sites/yandere/dialog")

	added := strings.Index(body, `<span class="exp-add">  &#34;example&#34;: &#34;https://yande.re/post/show/999999&#34;`)
	struck := strings.Index(body, `<span class="exp-del">  &#34;example&#34;`)
	hosts := strings.Index(body, `<span class="exp-add">  &#34;hosts&#34;: [`)
	if added < 0 || struck < 0 || hosts < 0 {
		t.Fatalf("expected an added example, a struck example and an added host list, got %q", body)
	}
	if added > struck || struck > hosts {
		t.Errorf("the struck example should sit between its replacement and the added hosts, got add=%d del=%d hosts=%d", added, struck, hosts)
	}
}

func TestSaveSitePasswordAndOptions(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/twitter", url.Values{
		"username": {"me"}, "password": {"PW-SECRET-zzz"},
		"options": {`{"access-token": "OPT-SECRET-zzz"}`}, "options_edit": {"1"},
	}).Body.Close()
	site := srv.cfg.Current().FindSite("twitter")
	if site == nil || site.Password != "PW-SECRET-zzz" || site.Options != `{"access-token": "OPT-SECRET-zzz"}` {
		t.Fatalf("site not saved: %+v", site)
	}
	postForm(t, ts, srv, "/settings/sites/twitter", url.Values{"password": {""}, "options": {""}}).Body.Close()
	if got := srv.cfg.Current().FindSite("twitter").Password; got != "PW-SECRET-zzz" {
		t.Errorf("a blank password should keep the stored one, got %q", got)
	}
	if got := srv.cfg.Current().FindSite("twitter").Options; got != `{"access-token": "OPT-SECRET-zzz"}` {
		t.Errorf("an unrevealed options field should keep the stored value, got %q", got)
	}
	if _, body := get(t, ts, "/settings/sites/twitter/dialog"); strings.Contains(body, "OPT-SECRET-zzz") {
		t.Error("the dialog echoed the stored private options")
	}
	if _, body := get(t, ts, "/settings/sites/twitter/options"); !strings.Contains(body, "OPT-SECRET-zzz") {
		t.Error("the reveal should answer with the stored options")
	}
	if _, body := get(t, ts, "/settings"); strings.Contains(body, "PW-SECRET-zzz") {
		t.Error("the settings page leaked the password")
	}
	if _, body := get(t, ts, "/settings"); !strings.Contains(body, `>cookies !</span>`) {
		t.Error("the twitter row should carry the seeded cookies login label")
	}
	body := refusedSave(t, postForm(t, ts, srv, "/settings/sites/twitter", url.Values{"options": {`{broken`}, "options_edit": {"1"}}))
	if !strings.Contains(body, "site options must be a JSON object") {
		t.Errorf("broken options should be refused, got %q", truncate(body))
	}
	if got := srv.cfg.Current().FindSite("twitter").Options; got != `{"access-token": "OPT-SECRET-zzz"}` {
		t.Errorf("a refused save should keep the stored options, got %q", got)
	}
}

func TestSaveSiteUserAgent(t *testing.T) {
	srv, ts := serveWeb(t, "")
	managedAgent := func() any {
		t.Helper()
		data, err := os.ReadFile(srv.cfg.Current().GalleryDL.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Extractor map[string]any `json:"extractor"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		block, _ := doc.Extractor["rule34"].(map[string]any)
		return block["user-agent"]
	}

	postForm(t, ts, srv, "/settings/sites/rule34", url.Values{"user_agent": {"  my agent/1.0  "}}).Body.Close()
	if site := srv.cfg.Current().FindSite("rule34"); site == nil || site.Agent != "my agent/1.0" {
		t.Fatalf("site = %+v, want the agent trimmed and saved", site)
	}
	if got := managedAgent(); got != "my agent/1.0" {
		t.Errorf("managed user-agent = %v, want the saved one", got)
	}
	postForm(t, ts, srv, "/settings/sites/rule34", url.Values{"gallery": {"art"}}).Body.Close()
	if got := srv.cfg.Current().FindSite("rule34").Agent; got != "my agent/1.0" {
		t.Errorf("a form without the field left the agent %q", got)
	}
	postForm(t, ts, srv, "/settings/sites/rule34", url.Values{"user_agent": {""}}).Body.Close()
	if got := srv.cfg.Current().FindSite("rule34").Agent; got != "" {
		t.Errorf("a blanked agent left %q", got)
	}
	if got := managedAgent(); got != nil {
		t.Errorf("managed user-agent = %v, want none after the clear", got)
	}
}

func TestSaveSiteClearsBlankedPlainCredentials(t *testing.T) {
	srv, ts := serveWeb(t, "")

	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"username": {"audituser"}, "user_id": {"77"}, "api_key": {"KEY-SECRET"},
	}).Body.Close()
	site := srv.cfg.Current().FindSite("danbooru")
	if site == nil || site.Username != "audituser" || site.UserID != "77" || site.APIKey != "KEY-SECRET" {
		t.Fatalf("site not saved: %+v", site)
	}

	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"username": {""}, "user_id": {""}, "api_key": {""},
	}).Body.Close()
	site = srv.cfg.Current().FindSite("danbooru")
	if site.Username != "" || site.UserID != "" {
		t.Errorf("blanked username / user id = %q / %q, want both cleared", site.Username, site.UserID)
	}
	if site.APIKey != "KEY-SECRET" {
		t.Errorf("a blank api key should keep the stored one, got %q", site.APIKey)
	}

	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"username": {"back"}, "gallery": {"art"}, "label": {"Danbooru"},
		"options": {`{"a":1}`}, "options_edit": {"1"},
	}).Body.Close()
	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{"username": {"back"}}).Body.Close()
	site = srv.cfg.Current().FindSite("danbooru")
	if site.APIKey != "KEY-SECRET" || site.Gallery != "art" || site.Label != "Danbooru" || site.Options != `{"a":1}` {
		t.Errorf("an unposted field was cleared: %+v", site)
	}
	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"username": {"back"}, "options": {""}, "options_edit": {"0"},
	}).Body.Close()
	if site = srv.cfg.Current().FindSite("danbooru"); site.Options != `{"a":1}` {
		t.Errorf("an unrevealed blank field cleared the options: %q", site.Options)
	}
	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"username": {"back"}, "options": {`{"b":2}`}, "options_edit": {"0"},
	}).Body.Close()
	if site = srv.cfg.Current().FindSite("danbooru"); site.Options != `{"b":2}` {
		t.Errorf("options typed without reveal were dropped: %q", site.Options)
	}
	postForm(t, ts, srv, "/settings/sites/danbooru", url.Values{
		"username": {""}, "gallery": {""}, "label": {""},
		"options": {""}, "options_edit": {"1"},
	}).Body.Close()
	site = srv.cfg.Current().FindSite("danbooru")
	if site.Gallery != "" || site.Label != "" || site.Options != "" {
		t.Errorf("blanked fields the form carried should clear: %+v", site)
	}
}

func TestSaveSiteCookiesPaste(t *testing.T) {
	srv, ts := serveWeb(t, "")
	dir := t.TempDir()
	mustUpdate(t, srv, func(c *config.Config) { c.GalleryDL.CookiesDir = dir })
	paste := "# Netscape HTTP Cookie File\n.x.com\tTRUE\t/\tTRUE\t1785167999\tauth_token\tSECRET\n#HttpOnly_.x.com\tTRUE\t/\tTRUE\t1785167999\tct0\tSECRET2\n"
	postForm(t, ts, srv, "/settings/sites/twitter", url.Values{"cookies_paste": {paste}}).Body.Close()

	want := dir + "/twitter.txt"
	if got := srv.cfg.Current().FindSite("twitter").Cookies; got != want {
		t.Fatalf("cookies path = %q, want %q", got, want)
	}
	data, err := os.ReadFile(want)
	if err != nil || string(data) != paste {
		t.Fatalf("cookies file not written verbatim: %v", err)
	}
	if info, _ := os.Stat(want); info.Mode().Perm() != 0o600 {
		t.Errorf("cookies file mode = %v, want 0600", info.Mode().Perm())
	}

	if _, body := get(t, ts, "/settings/sites/twitter/dialog"); !strings.Contains(body, "2 cookies") {
		t.Error("the dialog should show the cookie count")
	}
	if _, body := get(t, ts, "/settings"); strings.Contains(body, "SECRET") {
		t.Error("cookie contents must never render")
	}

	refused := refusedSave(t, postForm(t, ts, srv, "/settings/sites/twitter", url.Values{"cookies_paste": {"just some text"}}))
	if !strings.Contains(refused, "not a Netscape cookies.txt export") {
		t.Errorf("a non-cookies paste should be refused, got %q", truncate(refused))
	}
	if !strings.Contains(refused, "just some text") {
		t.Errorf("the refusal dropped the paste, got %q", truncate(refused))
	}
	resp := postClosed(t, ts, srv, "/settings/sites/bad%20name", url.Values{"cookies_paste": {paste}})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("an invalid name should be refused, got %q", loc)
	}
}

func TestLookupChainSiteSurfacing(t *testing.T) {
	srv, ts := serveWeb(t, "")

	_, body := get(t, ts, "/settings")
	if !strings.Contains(body, `data-site="danbooru"`) || !strings.Contains(body, ">Edit site<") {
		t.Error("a chained site should carry the edit-site shortcut")
	}
	if n := strings.Count(body, `data-site="danbooru"`); n < 2 {
		t.Errorf("iqdb should carry a danbooru edit shortcut, found %d danbooru buttons", n)
	}

	mustUpdate(t, srv, func(c *config.Config) {
		c.Sites = append(c.Sites, config.Site{Name: "nhentai", LookupOrder: 9})
	})
	_, body = get(t, ts, "/settings")
	if !strings.Contains(body, "no md5 template") {
		t.Error("an ordered templateless site should show as skipped")
	}
	if !strings.Contains(body, `id="chain-off"`) || !strings.Contains(body, `data-name="nhentai"`) {
		t.Error("the skipped site should be draggable in the order dialog")
	}

	postForm(t, ts, srv, "/settings/sites/szurubooru", url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"md5_search_template": {"https://booru.example/posts?tags=md5:{md5}"},
	}).Body.Close()
	_, body = get(t, ts, "/settings")
	if !strings.Contains(body, `data-name="szurubooru"`) {
		t.Error("a user-profiled md5-capable site should appear in the order dialog")
	}
}

func TestSettingsSiteGroups(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})

	_, body := get(t, ts, "/settings")
	if !strings.Contains(body, "No sites added") {
		t.Error("a fresh install should show the empty-state hint")
	}
	if strings.Contains(body, `id="state-danbooru"`) {
		t.Error("a bare seeded lookup block should not surface a row")
	}

	postForm(t, ts, srv, "/settings/sites/gelbooru", url.Values{"gallery": {"art"}}).Body.Close()
	postForm(t, ts, srv, "/settings/sites/nhentai", url.Values{"gallery": {"manga"}}).Body.Close()
	postForm(t, ts, srv, "/settings/sites/twitter", url.Values{"cookies": {"/config/cookies/twitter.txt"}}).Body.Close()
	_, body = get(t, ts, "/settings")
	for _, want := range []string{
		">Boorus<", "Manga / comics", "Other sites", "<th>Action</th>",
		`class="edit-site"`, `id="site-edit-dialog"`,
		`id="state-gelbooru"`, `id="state-nhentai"`, `id="state-twitter"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}
}

func ptrSiteForm(blacklist string) url.Values {
	return url.Values{
		"family": {"generic"}, "kind": {"other"}, "auth": {"none"},
		"cat_from": {"studio"}, "cat_to": {"artist"},
		"rule_from": {"old_name"}, "rule_to": {"new_name"},
		"blacklist": {blacklist},
	}
}

func TestPTRRowSitsInSitesWithItsOwnDialog(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})

	_, page := get(t, ts, "/settings")
	if !strings.Contains(page, ">Public Tag Repository<") || !strings.Contains(page, `id="state-ptr"`) || !strings.Contains(page, `data-site="ptr"`) {
		t.Error("the PTR row should show on a fresh config")
	}
	if strings.Contains(page, "/settings/sites/ptr/test") || strings.Contains(page, "/settings/sites/ptr/reset") {
		t.Error("the PTR row has no gallery-dl test, and nothing to remove yet")
	}

	_, dialog := get(t, ts, "/settings/sites/ptr/dialog")
	for _, want := range []string{`id="sd-mapping"`, `id="sd-blacklist"`, "hydrus namespace", `id="sd-auth"`, `id="sd-family"`} {
		if !strings.Contains(dialog, want) {
			t.Errorf("the PTR dialog is missing %q", want)
		}
	}
	for _, not := range []string{`class="dialog-tab`, `id="sd-advanced"`, `id="sd-profile"`, `name="gallery"`, "from Rules", `id="sd-mapping" hidden`} {
		if strings.Contains(dialog, not) {
			t.Errorf("the PTR dialog should not carry %q", not)
		}
	}
	_, other := get(t, ts, "/settings/sites/danbooru/dialog")
	if !strings.Contains(other, `id="sd-advanced"`) || strings.Contains(other, "hydrus namespace") {
		t.Error("a booru's dialog should keep its tabs and its own wording")
	}

	postForm(t, ts, srv, "/settings/sites/ptr", ptrSiteForm("bad")).Body.Close()
	if got := srv.mapper.MapPTR([]string{"studio:s", "old_name"}); !slices.Equal(got, []string{"artist:s", "new_name"}) {
		t.Errorf("MapPTR = %v, want the saved map and rule", got)
	}
	if _, hit := srv.mapper.PTRBlacklisted([]string{"bad"}); !hit {
		t.Error("the saved blacklist should apply")
	}

	_, page = get(t, ts, "/settings")
	if n := strings.Count(page, `id="state-ptr"`); n != 1 {
		t.Errorf("the PTR row shows %d times, want it out of the site groups", n)
	}
	if !strings.Contains(page, "/settings/sites/ptr/reset") {
		t.Error("a configured PTR row should offer Remove")
	}
	if !strings.Contains(page, `data-site="ptr">ptr +1`) {
		t.Error("Rules should list the PTR's own blacklist")
	}

	raw, err := os.ReadFile(srv.cfg.Current().GalleryDL.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var managed struct {
		Extractor map[string]any `json:"extractor"`
	}
	if err := json.Unmarshal(raw, &managed); err != nil {
		t.Fatal(err)
	}
	if _, present := managed.Extractor[config.SitePTR]; present {
		t.Error("the PTR must not reach gallery-dl's managed config")
	}
	if _, listed := get(t, ts, "/settings/sites/search?q=ptr"); strings.Contains(listed, `data-site="ptr"`) {
		t.Error("the add-a-site search should not offer the PTR")
	}

	postForm(t, ts, srv, "/settings/sites/ptr/reset", url.Values{}).Body.Close()
	if got := srv.mapper.MapPTR([]string{"studio:s"}); !slices.Equal(got, []string{"copyright:s"}) {
		t.Errorf("after Remove MapPTR = %v, want the table's category", got)
	}
	if _, hit := srv.mapper.PTRBlacklisted([]string{"bad"}); hit {
		t.Error("Remove should clear the blacklist")
	}
	_, page = get(t, ts, "/settings")
	if !strings.Contains(page, `id="state-ptr"`) {
		t.Error("the PTR row stays after Remove")
	}
}

func TestPTRRefusedBlacklistReopensItsDialog(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	body := readBody(t, postForm(t, ts, srv, "/settings/sites/ptr", ptrSiteForm("score:<0")))
	if !strings.Contains(body, `blacklist line 1`) || !strings.Contains(body, `id="sd-auth"`) || !strings.Contains(body, "score:&lt;0") {
		t.Error("a refused PTR save should hand back its dialog and draft")
	}
	if strings.Contains(body, `class="dialog-tab`) {
		t.Error("the refused PTR dialog should stay tabless")
	}
}

func TestSiteTestButtonsTargetTheirRowForADottedName(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	postForm(t, ts, srv, "/settings/sites/mastodon.social", url.Values{"gallery": {"art"}}).Body.Close()

	_, body := get(t, ts, "/settings")
	if !strings.Contains(body, `hx-post="/settings/sites/mastodon.social/test" hx-target="previous .site-state"`) {
		t.Error("the dotted site's Test button should target its own row's state cell")
	}
	if strings.Contains(body, `hx-target="#state-`) || strings.Contains(body, `hx-target="#lk-state-`) {
		t.Error("a Test button still targets a state cell by id, which a dot in the name breaks")
	}
}

func TestTestMonbooruFailure(t *testing.T) {
	dead := monbooruStub()
	deadURL := dead.URL
	dead.Close()
	srv, ts := serveProc(t, noopProc{})
	resp := postForm(t, ts, srv, "/settings/monbooru/test", map[string][]string{"api_url": {deadURL}})
	if body := readBody(t, resp); !strings.Contains(body, "flash-err") {
		t.Errorf("unreachable monbooru test should flash err, body=%q", body)
	}
}

func TestLookupQueryableSkipsWhatTheWalkSkips(t *testing.T) {
	srv := stubbedServer(t, "")
	for _, src := range srv.mapper.LookupChain() {
		got := srv.mapper.SourceQueryable(src, srv.sim.Missing)
		switch src.Name {
		case "danbooru":
			if !got {
				t.Error("danbooru needs no credential for an md5 search but reads unqueryable")
			}
		case "iqdb", "saucenao":
			if got {
				t.Errorf("%s with no credential reads queryable", src.Name)
			}
		}
	}
}

func TestPausedBannerPointsAtAControlBothPagesReach(t *testing.T) {
	srv, ts := serveProc(t, noopProc{})
	srv.queue.Pause()
	for _, path := range []string{"/", "/queue"} {
		_, body := get(t, ts, path)
		if !strings.Contains(body, `id="queue-paused-banner"`) || !strings.Contains(body, "[resume] on the") {
			t.Errorf("%s: the paused banner should point at the queue page's resume control", path)
		}
	}
}

const hostileText = `<script>alert(1)</script><img src=x onerror=alert(1)>"'`

type hostileProc struct{}

func (hostileProc) Process(_ context.Context, job *queue.Job) error {
	job.SetSite(hostileText)
	job.SetItems([]queue.Item{
		{PostID: hostileText, URL: "javascript:alert(1)"},
		{PostID: "2", URL: "https://example.com/" + hostileText},
	})
	for i := range 2 {
		job.UpdateItem(i, func(it *queue.Item) {
			it.Status = queue.ItemFailed
			it.Outcome = outcome.Failed
			it.ErrorCode = hostileText
			it.Error = hostileText
			it.TagWarnings = []string{hostileText}
			it.MergeNote = hostileText
		})
	}
	return nil
}

func TestHostileDataRendersInert(t *testing.T) {
	queueSrv, queueTS := serveProc(t, hostileProc{})
	id := queueSrv.queue.Enqueue("javascript:alert(1)//"+hostileText, queue.Options{})
	waitFor(t, func() bool {
		snap, err := queueSrv.queue.Get(id)
		return err == nil && snap.Status == queue.JobFailed
	})

	traceSrv, traceTS := serveWeb(t, "")
	pairMB(t, traceSrv)
	traceSrv.runner = &traceRunner{meta: map[string]any{
		"category": hostileText, "permalink": "javascript:alert(1)", "rating": hostileText,
		"tags_general": []any{hostileText}, "tags_artist": hostileText,
	}}

	store := contribStoreForWeb(t)
	contribSrv, contribTS := serveEngine(t, &stubEngine{status: ptr.Status{Enabled: true, State: ptr.StateReady}, hasKey: true, contrib: store})
	contribSrv.cfg.Current().Monbooru.WebURL = "javascript:alert(1)"
	staged, _, err := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: hostileText, Tag2: hostileText, Reason: hostileText, Origin: hostileText})
	if err != nil {
		t.Fatal(err)
	}
	mustFail(t, store, staged, hostileText)
	logged, _, err := store.Stage(ptr.ContribItem{Kind: ptr.ContribParent, Tag: hostileText, Tag2: "x", Reason: hostileText})
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimIDs([]int64{logged})
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimIDs = %v, %v", items, err)
	}
	if _, err := store.Complete(items[0]); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		body func() string
	}{
		{"queue rows", func() string { _, body := get(t, queueTS, "/internal/queue-rows"); return body }},
		{"queue items", func() string {
			_, body := get(t, queueTS, "/internal/queue-rows/"+strconv.FormatInt(id, 10)+"/items")
			return body
		}},
		{"trace", func() string {
			resp := postForm(t, traceTS, traceSrv, "/settings/rules/trace", url.Values{"url": {"https://example.com/posts/1"}})
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return string(b)
		}},
		{"contributions", func() string { _, body := get(t, contribTS, "/internal/ptr-contrib"); return body }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body()
			if !strings.Contains(body, "&lt;script&gt;alert(1)") {
				t.Errorf("the hostile text never rendered: %s", truncate(body))
			}
			for _, active := range []string{"<script>alert", "<img src=x", `href="javascript:`, `src="javascript:`} {
				if strings.Contains(body, active) {
					t.Errorf("rendered %q active: %s", active, truncate(body))
				}
			}
		})
	}
}

func TestSavingASiteWithNothingSetSaysSo(t *testing.T) {
	srv := newWebServer(t, "", "")
	ts := newTestServer(t, srv)
	flash := func(vals url.Values) string {
		resp := postClosed(t, ts, srv, "/settings/sites/danbooru", vals)
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return loc.Query().Get("msg")
	}
	if got := flash(url.Values{}); !strings.Contains(got, "Nothing to save for danbooru") {
		t.Errorf("empty save flashed %q", got)
	}
	if got := flash(url.Values{"label": {"Dan"}}); got != "Site danbooru saved." {
		t.Errorf("save with a label flashed %q", got)
	}
}

func TestSHA256HintNamesThePTRState(t *testing.T) {
	for _, tc := range []struct {
		st   ptr.Status
		want string
	}{
		{ptr.Status{}, "which is off"},
		{ptr.Status{Enabled: true, State: ptr.StateSyncing}, "finished syncing"},
		{ptr.Status{Enabled: true, State: ptr.StateError}, "stopped on an error"},
		{ptr.Status{Enabled: true, State: ptr.StatePaused}, "is paused"},
		{ptr.Status{Enabled: true, State: ptr.StateError, Queryable: true}, "not for importing here"},
	} {
		if got := sha256AddHint(tc.st); !strings.Contains(got, tc.want) {
			t.Errorf("state %q queryable %v: %q, want %q", tc.st.State, tc.st.Queryable, got, tc.want)
		}
	}
}

func TestTheFlowStripEndsInTheGallery(t *testing.T) {
	srv, ts := serveWeb(t, "")
	pairMB(t, srv)
	_, body := get(t, ts, "/settings")
	strip := body[strings.Index(body, `class="tag-flow"`):]
	strip = strip[:strings.Index(strip, "</ol>")]
	last := -1
	for _, want := range []string{"3 categories kept", `class="flow-gate">push<`, "4 aliases · 5 rejected · 6 implications", "in monbooru, per gallery", `class="flow-gate">image<`} {
		at := strings.Index(strip, want)
		if at <= last {
			t.Errorf("%q out of order in the strip", want)
		}
		last = at
	}

	_, dialog := get(t, ts, "/settings/sites/danbooru/dialog")
	if strings.Contains(dialog, "flow-gallery") || !strings.Contains(dialog, `class="flow-gate">monbooru<`) {
		t.Error("the site dialog's strip should end at the monbooru gate")
	}
}

func TestTraceEndsInTheGalleryColumn(t *testing.T) {
	var asked []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tags/resolve" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Names []string `json:"names"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = body.Names
		_, _ = w.Write([]byte(`{"results":[
		 {"name":"white_background","status":"alias","resolves_to":"simple_background","implies":[]},
		 {"name":"watermark","status":"rejected","implies":[]},
		 {"name":"rating:sensitive","status":"exists","implies":[]}]}`))
	}))
	t.Cleanup(stub.Close)
	srv, ts := serveWeb(t, "")
	srv.runner = &traceRunner{meta: map[string]any{"category": "danbooru", "id": float64(7), "rating": "s",
		"tags_general": "white_background watermark"}}
	storeCfg(srv, func(c *config.Config) { c.Monbooru.APIToken = "" })

	body := readBody(t, postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {"https://danbooru.donmai.us/posts/7"}}))
	if !strings.Contains(body, "monbooru not paired, the trace stops at the push") || strings.Contains(body, "4-6") {
		t.Errorf("unpaired trace = %q, want the verdict to stop at the push with no gallery column", truncate(body))
	}

	pairMB(t, srv)
	storeCfg(srv, func(c *config.Config) {
		c.Monbooru.APIURL = stub.URL
		c.Sites = []config.Site{{Name: "danbooru", Gallery: "art"}}
	})
	body = readBody(t, postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {"https://danbooru.donmai.us/posts/7"}}))
	for _, want := range []string{"<th>4-6 art</th>", "-&gt; simple_background", `<span class="o-failed">rejected</span>`, `<span class="o-created">kept</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("paired trace lacks %q: %q", want, truncate(body))
		}
	}
	if !slices.Equal(asked, []string{"rating:sensitive", "watermark", "white_background"}) {
		t.Errorf("monbooru was asked %v", asked)
	}
}

func TestTraceLeavesTheGalleryColumnBlankWithoutAnAnswer(t *testing.T) {
	stub := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(stub.Close)
	srv, ts := serveWeb(t, "")
	srv.runner = &traceRunner{meta: map[string]any{"category": "danbooru", "id": float64(7), "rating": "s",
		"tags_general": "white_background watermark"}}
	pairMB(t, srv)
	storeCfg(srv, func(c *config.Config) { c.Monbooru.APIURL = stub.URL })

	body := readBody(t, postForm(t, ts, srv, "/settings/rules/trace", url.Values{"url": {"https://danbooru.donmai.us/posts/7"}}))
	if !strings.Contains(body, "The gallery column is blank: monbooru v1.24.0 or newer fills the gallery column") {
		t.Errorf("trace = %q, want the reason in the verdict", truncate(body))
	}
	if strings.Contains(body, "refused") || strings.Contains(body, outcome.ErrCodeMonbooruRejected) {
		t.Errorf("trace = %q, want blank gallery cells and no error code", truncate(body))
	}
}
