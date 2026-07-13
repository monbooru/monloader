package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/mapping"
	"github.com/leqwin/monloader/internal/monbooru"
	"github.com/leqwin/monloader/internal/queue"
	"github.com/leqwin/monloader/internal/similarity"
	"github.com/leqwin/monloader/internal/sitestate"
)

const testMD5 = "0123456789abcdef0123456789abcdef"

// lookupEnv is testEnv with a caller-chosen [[sites]] list, so a test can
// shape the lookup walk. ptrSvc backs the PTR lookup backend (nil = absent).
func lookupEnv(t *testing.T, fake gdl.Runner, handler http.HandlerFunc, sites []config.Site) (*queue.Queue, func()) {
	return lookupEnvPTR(t, fake, handler, sites, nil)
}

func lookupEnvPTR(t *testing.T, fake gdl.Runner, handler http.HandlerFunc, sites []config.Site, ptrSvc PTRService) (*queue.Queue, func()) {
	cfg := config.Default()
	cfg.Sites = sites
	return lookupEnvCfg(t, fake, handler, cfg, ptrSvc, nil)
}

// lookupEnvCfg takes the whole config (so a test can shape the [lookup]
// section) plus the two lookup service stubs.
func lookupEnvCfg(t *testing.T, fake gdl.Runner, handler http.HandlerFunc, cfg *config.Config, ptrSvc PTRService, simSvc SimilarityService) (*queue.Queue, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	cfg.Monbooru.APIURL = srv.URL
	cfg.Monbooru.APIToken = "tok"
	mapper, err := mapping.New(config.NewProvider(cfg))
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	proc := New(fake, mapper, monbooru.New(config.NewProvider(cfg)), config.NewProvider(cfg), t.TempDir(), sitestate.New(), ptrSvc, simSvc)
	q := queue.New(proc, 1, 100)
	q.Start()
	cleanup := func() { q.Close(); srv.Close() }
	return q, cleanup
}

// stubPTR is a PTRService for the PTR-lookup tests: it returns scripted tags.
type stubPTR struct {
	enabled bool
	tags    []string
	found   bool
	err     error
}

func (s *stubPTR) Enabled() bool { return s.enabled }
func (s *stubPTR) TagsForHash(string) ([]string, bool, error) {
	return s.tags, s.found, s.err
}

// stubSim is a SimilarityService returning scripted candidates per service.
type stubSim struct {
	missing map[string]string
	results map[string]similarity.Result
	errs    map[string]error
	calls   []string
}

func (s *stubSim) Missing(service string) (string, bool) {
	label, ok := s.missing[service]
	return label, ok
}

func (s *stubSim) Search(_ context.Context, service string, _ []byte) (similarity.Result, error) {
	s.calls = append(s.calls, service)
	if err := s.errs[service]; err != nil {
		return similarity.Result{}, err
	}
	return s.results[service], nil
}

// noMatch is what FetchMeta returns for a search that resolved zero posts.
var noMatch = fetchResp{err: &queue.CodedError{Code: queue.ErrCodeMappingFailed, Msg: "no post metadata in gallery-dl output"}}

func gelbooruLookupPost(id string) map[string]any {
	return map[string]any{
		"category": "gelbooru", "id": id, "subcategory": "tag", "rating": "general",
		"tags_general": []any{"tag_" + id},
	}
}

func TestLookupWalksInOrderAndEnrichesFirstHit(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	gelURL := "https://gelbooru.com/index.php?page=post&s=list&tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: noMatch,
		gelURL: {meta: gelbooruLookupPost("999")},
	}}
	var gotPath, gotBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"merge": map[string]any{"tags_added": 1}})
	}
	q, cleanup := lookupEnv(t, fake, handler, []config.Site{
		{Name: "gelbooru", LookupOrder: 2, APIKey: "k", UserID: "u"},
		{Name: "danbooru", LookupOrder: 1},
	})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(42, "art", queue.BackendBooru, testMD5, ""))

	if !reflect.DeepEqual(fake.gotFetchURLs, []string{danURL, gelURL}) {
		t.Errorf("walk = %v, want danbooru then gelbooru", fake.gotFetchURLs)
	}
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want one enriched item", job.Status, job.Items)
	}
	if job.Site != "gelbooru" {
		t.Errorf("site = %q, want gelbooru", job.Site)
	}
	if job.Items[0].URL != "https://gelbooru.com/index.php?page=post&s=view&id=999" {
		t.Errorf("item url = %q, want the matched post's canonical page", job.Items[0].URL)
	}
	if gotPath != "/api/v1/images/42/enrich?gallery=art" {
		t.Errorf("enrich path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"source_md5":"`+testMD5+`"`) || !strings.Contains(gotBody, `"verify":true`) ||
		!strings.Contains(gotBody, "tag_999") {
		t.Errorf("enrich body wrong: %s", gotBody)
	}
}

func TestLookupSkipsSiteMissingCredential(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: danbooruPost("100005").Meta},
	}}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	// gelbooru leads the order but has no api key; sankaku has no cookies.
	q, cleanup := lookupEnv(t, fake, handler, []config.Site{
		{Name: "gelbooru", LookupOrder: 1},
		{Name: "sankaku", LookupOrder: 2},
		{Name: "danbooru", LookupOrder: 3},
	})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, ""))

	if !reflect.DeepEqual(fake.gotFetchURLs, []string{danURL}) {
		t.Errorf("walk = %v, want only danbooru (others skipped for credentials)", fake.gotFetchURLs)
	}
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("items = %+v, want one enriched", job.Items)
	}
}

func TestLookupContinuesPastSiteError(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	konURL := "https://konachan.com/post?tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {err: &queue.CodedError{Code: queue.ErrCodeRateLimited, Msg: "429"}},
		konURL: {meta: map[string]any{"category": "konachan", "id": "77", "rating": "s", "tags_general": []any{"scenery"}}},
	}}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	q, cleanup := lookupEnv(t, fake, handler, []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "konachan", LookupOrder: 2},
	})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, ""))
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched || job.Site != "konachan" {
		t.Fatalf("job = %+v, want konachan enrich despite the danbooru error", job.Items)
	}
}

func TestLookupFullMissFailsWithTrail(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportPath, reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-status") {
			reportPath = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
	q, cleanup := lookupEnv(t, fake, handler, []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "yandere", LookupOrder: 2},
	})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendBooru, testMD5, ""))
	it := job.Items[0]
	if it.Outcome != queue.OutcomeFailed || it.ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want failed hash_not_found", it)
	}
	if !strings.Contains(it.Error, "danbooru: no match") || !strings.Contains(it.Error, "yandere: no match") {
		t.Errorf("error trail = %q, want per-site misses", it.Error)
	}
	if reportPath != "/api/v1/images/9/fetch-status" || !strings.Contains(reportBody, `"state":"hash_not_found"`) {
		t.Errorf("fetch-status report = %q %q, want a hash_not_found state", reportPath, reportBody)
	}
	// The reported message is the bare "; "-joined trail - no code prefix, no
	// prose wrapper - so monbooru can split it back into a list.
	if !strings.Contains(reportBody, `"message":"danbooru: no match; yandere: no match"`) {
		t.Errorf("fetch-status message = %q, want the bare joined trail", reportBody)
	}
}

func TestLookupPTRBackendUnavailable(t *testing.T) {
	fake := &fakeRunner{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
	// A disabled PTR service (and a nil one) both fail ptr_unavailable.
	q, cleanup := lookupEnvPTR(t, fake, handler, nil, &stubPTR{enabled: false})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendPTR, "", strings.Repeat("a", 64)))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != queue.ErrCodePTRUnavailable {
		t.Fatalf("items = %+v, want failed ptr_unavailable", job.Items)
	}
	if len(fake.gotFetchURLs) != 0 {
		t.Errorf("a ptr lookup must not touch the booru walk, got %v", fake.gotFetchURLs)
	}
}

func TestLookupPTRHitEnriches(t *testing.T) {
	fake := &fakeRunner{}
	var gotBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"merge": map[string]any{"tags_added": 2}})
	}
	sha := strings.Repeat("cd", 32)
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky", "character:reimu"}}
	q, cleanup := lookupEnvPTR(t, fake, handler, nil, ptrSvc)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(88, "art", queue.BackendPTR, "", sha))
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want one enriched", job.Status, job.Items)
	}
	// The enrich carries the mapped tags, source ptr, no url, and verify false.
	if !strings.Contains(gotBody, `"source":"ptr"`) || !strings.Contains(gotBody, `"verify":false`) ||
		!strings.Contains(gotBody, "character:reimu") || strings.Contains(gotBody, `"url":"http`) {
		t.Errorf("enrich body wrong: %s", gotBody)
	}
}

func TestLookupPTRMissFails(t *testing.T) {
	fake := &fakeRunner{}
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
	sha := strings.Repeat("ef", 32)
	ptrSvc := &stubPTR{enabled: true, found: false}
	q, cleanup := lookupEnvPTR(t, fake, handler, nil, ptrSvc)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendPTR, "", sha))
	if job.Items[0].ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want hash_not_found", job.Items[0])
	}
	if !strings.Contains(reportBody, `"state":"hash_not_found"`) ||
		!strings.Contains(reportBody, `"message":"Public Tag Repository: no match"`) {
		t.Errorf("fetch-status report = %q, want hash_not_found with the trail entry", reportBody)
	}
}

// The all backend runs both lookups: a double hit enriches twice in one job -
// tags from the PTR, tags plus the post source from the matched site.
func TestLookupAllEnrichesBothBackends(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: gelbooruLookupPost("321")},
	}}
	var enriches []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enriches = append(enriches, string(b))
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	sha := strings.Repeat("ab", 32)
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q, cleanup := lookupEnvPTR(t, fake, handler, []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "gelbooru", LookupOrder: 2, APIKey: "k", UserID: "u"},
	}, ptrSvc)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, sha))
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want one enriched item", job.Status, job.Items)
	}
	// The PTR hit must not stop the booru walk, and the first booru hit must:
	// gelbooru sits next in the order and is never queried.
	if !reflect.DeepEqual(fake.gotFetchURLs, []string{danURL}) {
		t.Errorf("walk = %v, want the danbooru hit alone (later sites skipped)", fake.gotFetchURLs)
	}
	if len(enriches) != 2 {
		t.Fatalf("enrich calls = %d, want the ptr and the booru one", len(enriches))
	}
	if !strings.Contains(enriches[0], `"source":"ptr"`) || !strings.Contains(enriches[0], `"verify":false`) {
		t.Errorf("first enrich should be the unverified ptr one, got %s", enriches[0])
	}
	if !strings.Contains(enriches[1], `"verify":true`) || !strings.Contains(enriches[1], "tag_321") {
		t.Errorf("second enrich should be the verified booru one, got %s", enriches[1])
	}
}

// A double hit records one item whose note names both backends, so neither
// enrich's outcome overwrites the other's.
func TestLookupAllDoubleHitComposesNote(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: gelbooruLookupPost("321")},
	}}
	handler := func(w http.ResponseWriter, r *http.Request) {
		merge := map[string]any{"tags_added": 2}
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), `"source":"ptr"`) {
				merge = map[string]any{"tags_added": 5}
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"merge": merge})
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q, cleanup := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32)))
	if job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("item = %+v, want enriched", job.Items[0])
	}
	note := job.Items[0].MergeNote
	if !strings.Contains(note, "ptr +5 tags") || !strings.Contains(note, "gelbooru +2 tags") {
		t.Errorf("merge note = %q, want both backends' deltas", note)
	}
}

// The PTR tags land even when the booru post can no longer be enriched: the
// item stays enriched and the trail leads with the PTR match rather than the
// online failure monbooru's enrich handler recorded.
func TestLookupAllBooruFailAfterPTRStaysEnriched(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: gelbooruLookupPost("321")},
	}}
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), `"verify":true`) {
				// The booru post no longer serves this file.
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "hash mismatch"})
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"merge": map[string]any{"tags_added": 5}})
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q, cleanup := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32)))
	if job.Status != queue.JobSucceeded || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want the item enriched from the PTR tags", job.Status, job.Items)
	}
	if !strings.Contains(reportBody, "Public Tag Repository: match, tags applied") {
		t.Errorf("fetch-status report = %q, want the PTR match leading the trail", reportBody)
	}
}

// A full miss on the all backend reports one trail covering everything
// searched, the PTR entry first.
func TestLookupAllFullMissMergesTrail(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
	sha := strings.Repeat("cd", 32)
	ptrSvc := &stubPTR{enabled: true, found: false}
	q, cleanup := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendAll, testMD5, sha))
	if job.Items[0].ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want hash_not_found", job.Items[0])
	}
	if !strings.Contains(reportBody, `"message":"Public Tag Repository: no match; danbooru: no match"`) {
		t.Errorf("fetch-status report = %q, want the merged trail", reportBody)
	}
}

// A PTR hit with a full online miss still reports the trail: the PTR leaves
// no source URL, so the operator should see what was searched and missed.
func TestLookupAllPTROnlyHitReportsOnlineTrail(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var enriches []string
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enriches = append(enriches, string(b))
		}
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q, cleanup := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendAll, testMD5, strings.Repeat("cd", 32)))
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want one enriched item", job.Status, job.Items)
	}
	if len(enriches) != 1 || !strings.Contains(enriches[0], `"source":"ptr"`) {
		t.Fatalf("enriches = %v, want the ptr one alone", enriches)
	}
	if !strings.Contains(reportBody, `"state":"hash_not_found"`) ||
		!strings.Contains(reportBody, `"message":"Public Tag Repository: match, tags applied; danbooru: no match"`) {
		t.Errorf("fetch-status report = %q, want the ptr match leading the online trail", reportBody)
	}
}

// With the PTR off, the all backend degrades to the booru walk alone instead
// of refusing like the ptr backend does.
func TestLookupAllWithPTRDisabledWalksBooruOnly(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
	q, cleanup := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}},
		&stubPTR{enabled: false})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendAll, testMD5, strings.Repeat("ef", 32)))
	if job.Items[0].ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want hash_not_found", job.Items[0])
	}
	if !strings.Contains(reportBody, `"message":"danbooru: no match"`) ||
		strings.Contains(reportBody, "Public Tag Repository") {
		t.Errorf("fetch-status report = %q, want the booru trail alone", reportBody)
	}
}

// simChainCfg is a chain of danbooru (exact, 1), iqdb (2), saucenao (3),
// gelbooru (exact, 4, keyed) - the seeded default's shape, trimmed.
func simChainCfg() *config.Config {
	cfg := config.Default()
	cfg.Sites = []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "gelbooru", LookupOrder: 4, APIKey: "k", UserID: "u"},
	}
	cfg.Lookup.Iqdb.Order = 2
	cfg.Lookup.Saucenao.Order = 3
	return cfg
}

// okHandler answers every monbooru call with an empty 200 while counting
// thumbnail fetches.
func okHandler(thumbHits *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			*thumbHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegbytes"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
}

func TestLookupChainExactHitFetchesNothing(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: danbooruPost("100005").Meta},
	}}
	var thumbHits int
	sim := &stubSim{}
	q, cleanup := lookupEnvCfg(t, fake, okHandler(&thumbHits), simChainCfg(), nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, ""))
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("items = %+v, want one enriched", job.Items)
	}
	// The chain resolved on the first exact source: no thumbnail left the LAN
	// and no similarity service was asked.
	if thumbHits != 0 || len(sim.calls) != 0 {
		t.Errorf("thumbnail fetches = %d, similarity calls = %v; want none", thumbHits, sim.calls)
	}
}

func TestLookupChainSimilarityHitShortCircuits(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	postURL := "https://danbooru.donmai.us/posts/100005"
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL:  noMatch,
		postURL: {meta: danbooruPost("100005").Meta},
	}}
	var thumbHits int
	var enrichBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enrichBody = string(b)
		}
		okHandler(&thumbHits)(w, r)
	}
	sim := &stubSim{results: map[string]similarity.Result{
		"iqdb": {Candidates: []similarity.Candidate{{Site: "danbooru", URL: postURL, Similarity: 96.4}}},
	}}
	q, cleanup := lookupEnvCfg(t, fake, handler, simChainCfg(), nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, ""))
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("items = %+v, want one enriched", job.Items)
	}
	// The row names how the match was found; the item links the matched post.
	if job.Items[0].PostID != "danbooru 96%" {
		t.Errorf("item label = %q, want the site and score", job.Items[0].PostID)
	}
	// The enrich carries the score so monbooru marks the origin
	// similarity-matched.
	if !strings.Contains(enrichBody, `"similarity":96.4`) {
		t.Errorf("enrich body = %s, want the similarity score", enrichBody)
	}
	if job.Items[0].URL != postURL {
		t.Errorf("item url = %q, want the candidate post", job.Items[0].URL)
	}
	// One thumbnail fetch, the iqdb hit ends the chain before saucenao or the
	// gelbooru exact search.
	if thumbHits != 1 || !reflect.DeepEqual(sim.calls, []string{"iqdb"}) {
		t.Errorf("thumbnail fetches = %d, similarity calls = %v", thumbHits, sim.calls)
	}
	if !reflect.DeepEqual(fake.gotFetchURLs, []string{danURL, postURL}) {
		t.Errorf("fetches = %v, want the exact miss then the candidate", fake.gotFetchURLs)
	}
}

func TestLookupChainCandidatePreference(t *testing.T) {
	// saucenao offers the same image on danbooru (unranked here) at a higher
	// score and on gelbooru (ranked in the chain): the operator's ranking
	// wins over the raw score.
	gelPost := "https://gelbooru.com/index.php?page=post&s=view&id=999"
	cfg := simChainCfg()
	cfg.Sites[0].LookupOrder = 0 // danbooru keeps its block but leaves the chain
	cfg.Lookup.Iqdb.Order = 0
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		gelPost: {meta: gelbooruLookupPost("999")},
	}}
	var thumbHits int
	sim := &stubSim{results: map[string]similarity.Result{
		"saucenao": {Candidates: []similarity.Candidate{
			{Site: "danbooru", URL: "https://danbooru.donmai.us/posts/1", Similarity: 99},
			{Site: "gelbooru", URL: gelPost, Similarity: 91},
			{Site: "yandere", URL: "https://yande.re/post/show/2", Similarity: 42}, // under the floor
		}},
	}}
	q, cleanup := lookupEnvCfg(t, fake, okHandler(&thumbHits), cfg, nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, ""))
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched || job.Site != "gelbooru" {
		t.Fatalf("job = %+v site=%q, want the ranked gelbooru candidate enriched", job.Items, job.Site)
	}
	if job.Items[0].PostID != "gelbooru 91%" {
		t.Errorf("item label = %q", job.Items[0].PostID)
	}
	if !reflect.DeepEqual(fake.gotFetchURLs, []string{gelPost}) {
		t.Errorf("fetches = %v, want only the preferred candidate", fake.gotFetchURLs)
	}
}

func TestLookupChainUnrankedCandidatesFallBackToScore(t *testing.T) {
	// No candidate sits on a ranked site: the most confident one wins.
	yanPost := "https://yande.re/post/show/2"
	cfg := simChainCfg()
	cfg.Sites[0].LookupOrder = 0 // danbooru keeps its block but leaves the chain
	cfg.Lookup.Iqdb.Order = 0
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		yanPost: {meta: map[string]any{"category": "yandere", "id": "2", "rating": "s", "tags_general": []any{"scenery"}}},
	}}
	var thumbHits int
	sim := &stubSim{results: map[string]similarity.Result{
		"saucenao": {Candidates: []similarity.Candidate{
			{Site: "konachan", URL: "https://konachan.com/post/show/5", Similarity: 88},
			{Site: "yandere", URL: yanPost, Similarity: 96},
		}},
	}}
	q, cleanup := lookupEnvCfg(t, fake, okHandler(&thumbHits), cfg, nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, ""))
	if len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched || job.Site != "yandere" {
		t.Fatalf("job = %+v site=%q, want the most confident candidate enriched", job.Items, job.Site)
	}
	if job.Items[0].PostID != "yandere 96%" {
		t.Errorf("item label = %q", job.Items[0].PostID)
	}
	if !reflect.DeepEqual(fake.gotFetchURLs, []string{yanPost}) {
		t.Errorf("fetches = %v, want only the preferred candidate", fake.gotFetchURLs)
	}
}

func TestLookupChainClosestCandidatesOnTrail(t *testing.T) {
	// Nothing clears the floor: the miss entry lists the closest candidates
	// with their scores - deduped, best first, and including a service's
	// extras (hits outside the curated sites the walk cannot fetch).
	fake := &fakeRunner{fetchErr: noMatch.err}
	var thumbHits int
	sim := &stubSim{results: map[string]similarity.Result{
		"iqdb": {Candidates: []similarity.Candidate{
			{Site: "danbooru", URL: "https://danbooru.donmai.us/posts/1", Similarity: 41.2},
			{Site: "yandere", URL: "https://yande.re/post/show/2", Similarity: 62.8},
			{Site: "danbooru", URL: "https://danbooru.donmai.us/posts/1", Similarity: 41.2},
		}, Extras: []similarity.Candidate{
			{Site: "anidb.net", URL: "https://anidb.net/anime/9", Similarity: 71.4},
		}},
	}}
	cfg := simChainCfg()
	cfg.Lookup.Saucenao.Order = 0
	q, cleanup := lookupEnvCfg(t, fake, okHandler(&thumbHits), cfg, nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, ""))
	it := job.Items[0]
	if it.Outcome != queue.OutcomeFailed || it.ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want failed hash_not_found", it)
	}
	want := "iqdb: no match, closest candidates: https://anidb.net/anime/9 (71%), " +
		"https://yande.re/post/show/2 (63%), https://danbooru.donmai.us/posts/1 (41%)"
	if !strings.Contains(it.Error, want) {
		t.Errorf("error trail = %q, want %q", it.Error, want)
	}
}

func TestLookupChainRecordsStrongExtraAsSource(t *testing.T) {
	// The only hit clearing the floor lives outside the curated sites: no
	// tags can be fetched from it, but it is still recorded as the image's
	// source, its host standing in for the site label.
	fake := &fakeRunner{fetchErr: noMatch.err}
	var enrichBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegbytes"))
			return
		}
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enrichBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	sim := &stubSim{results: map[string]similarity.Result{
		"iqdb": {Candidates: []similarity.Candidate{
			{Site: "danbooru", URL: "https://danbooru.donmai.us/posts/1", Similarity: 41.2},
		}, Extras: []similarity.Candidate{
			{Site: "anidb.net", URL: "https://anidb.net/anime/9", Similarity: 82.5},
			{Site: "pixiv.net", URL: "https://www.pixiv.net/artworks/8", Similarity: 96.1},
		}},
	}}
	cfg := simChainCfg()
	cfg.Lookup.Saucenao.Order = 0
	q, cleanup := lookupEnvCfg(t, fake, handler, cfg, nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(6, "", queue.BackendBooru, testMD5, ""))
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want one enriched item", job.Status, job.Items)
	}
	if job.Items[0].PostID != "pixiv.net 96%, source only" || job.Site != "pixiv.net" {
		t.Errorf("label = %q site = %q, want the best extra recorded", job.Items[0].PostID, job.Site)
	}
	if !strings.Contains(enrichBody, `"source":"pixiv.net"`) ||
		!strings.Contains(enrichBody, `"url":"https://www.pixiv.net/artworks/8"`) ||
		!strings.Contains(enrichBody, `"similarity":96.1`) {
		t.Errorf("enrich body = %s, want the extra's host, url, and score", enrichBody)
	}
}

func TestLookupChainRecordsSourceWhenNoCandidateFetchable(t *testing.T) {
	// saucenao finds the image on ranked gelbooru (fetch fails) and unranked
	// sankaku (needs cookies): no metadata anywhere, but the preferred
	// candidate is still recorded as the image's source, with no tags and no
	// verify.
	gelPost := "https://gelbooru.com/index.php?page=post&s=view&id=42"
	cfg := simChainCfg()
	cfg.Lookup.Iqdb.Order = 0
	fake := &fakeRunner{fetchErr: noMatch.err, fetchByURL: map[string]fetchResp{
		gelPost: {err: &queue.CodedError{Code: queue.ErrCodeRateLimited, Msg: "429"}},
	}}
	var enrichBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegbytes"))
			return
		}
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enrichBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	sim := &stubSim{results: map[string]similarity.Result{
		"saucenao": {Candidates: []similarity.Candidate{
			{Site: "sankaku", URL: "https://chan.sankakucomplex.com/post/show/7", Similarity: 95},
			{Site: "gelbooru", URL: gelPost, Similarity: 91},
		}, Extras: []similarity.Candidate{
			// A stronger non-curated hit must not outrank a curated candidate.
			{Site: "pixiv.net", URL: "https://www.pixiv.net/artworks/8", Similarity: 99},
		}},
	}}
	q, cleanup := lookupEnvCfg(t, fake, handler, cfg, nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(5, "", queue.BackendBooru, testMD5, ""))
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want one enriched item", job.Status, job.Items)
	}
	if job.Items[0].PostID != "gelbooru 91%, source only" || job.Site != "gelbooru" {
		t.Errorf("label = %q site = %q, want the preferred candidate recorded", job.Items[0].PostID, job.Site)
	}
	if job.Items[0].URL != gelPost {
		t.Errorf("item url = %q, want the candidate post", job.Items[0].URL)
	}
	// The exact sources after the service still ran before settling for the
	// source-only record.
	gelSearch := "https://gelbooru.com/index.php?page=post&s=list&tags=md5:" + testMD5
	if !reflect.DeepEqual(fake.gotFetchURLs, []string{
		"https://danbooru.donmai.us/posts?tags=md5:" + testMD5, gelPost, gelSearch,
	}) {
		t.Errorf("fetches = %v, want the full chain before the fallback", fake.gotFetchURLs)
	}
	if !strings.Contains(enrichBody, `"source":"gelbooru"`) || !strings.Contains(enrichBody, `"url":"https://gelbooru.com/`) ||
		!strings.Contains(enrichBody, `"verify":false`) || !strings.Contains(enrichBody, `"tags":null`) ||
		!strings.Contains(enrichBody, `"similarity":91`) {
		t.Errorf("enrich body = %s, want a tagless source-only payload with the score", enrichBody)
	}
}

func TestLookupAllPTRHitStillRecordsSimilaritySource(t *testing.T) {
	// The PTR answers tags while the chain's only candidate cannot be fetched:
	// both enriches land - tags from the PTR, then the source-only record.
	postURL := "https://danbooru.donmai.us/posts/31"
	cfg := simChainCfg()
	cfg.Lookup.Iqdb.Order = 0
	fake := &fakeRunner{fetchErr: noMatch.err, fetchByURL: map[string]fetchResp{
		postURL: {err: &queue.CodedError{Code: queue.ErrCodeBlocked, Msg: "challenge"}},
	}}
	var enriches []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegbytes"))
			return
		}
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enriches = append(enriches, string(b))
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	sim := &stubSim{results: map[string]similarity.Result{
		"saucenao": {Candidates: []similarity.Candidate{{Site: "danbooru", URL: postURL, Similarity: 88}}},
	}}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q, cleanup := lookupEnvCfg(t, fake, handler, cfg, ptrSvc, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32)))
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != queue.OutcomeEnriched {
		t.Fatalf("job = %s %+v, want one enriched item", job.Status, job.Items)
	}
	if job.Items[0].PostID != "danbooru 88%, source only" {
		t.Errorf("label = %q", job.Items[0].PostID)
	}
	if len(enriches) != 2 {
		t.Fatalf("enrich calls = %d, want the ptr one then the source-only one", len(enriches))
	}
	if !strings.Contains(enriches[0], `"source":"ptr"`) || !strings.Contains(enriches[0], "blue_sky") {
		t.Errorf("first enrich should carry the ptr tags, got %s", enriches[0])
	}
	if !strings.Contains(enriches[1], `"source":"danbooru"`) || !strings.Contains(enriches[1], `"tags":null`) {
		t.Errorf("second enrich should be the tagless source record, got %s", enriches[1])
	}
}

func TestLookupChainFallsBackToWalkAndMergesTrail(t *testing.T) {
	// iqdb is unconfigured, saucenao rate-limited, the exact searches miss: the
	// trail reads every source in chain order.
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegbytes"))
			return
		}
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	sim := &stubSim{
		missing: map[string]string{"iqdb": "danbooru api key"},
		errs:    map[string]error{"saucenao": &queue.CodedError{Code: queue.ErrCodeRateLimited, Msg: "cooling down"}},
	}
	q, cleanup := lookupEnvCfg(t, fake, handler, simChainCfg(), nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendBooru, testMD5, ""))
	if job.Items[0].ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want hash_not_found", job.Items[0])
	}
	want := `"message":"danbooru: no match; iqdb: skipped, needs danbooru api key; saucenao: rate_limited; gelbooru: no match"`
	if !strings.Contains(reportBody, want) {
		t.Errorf("fetch-status report = %q, want the chain-ordered trail", reportBody)
	}
}

func TestLookupChainThumbnailUnavailableSkipsSimilarity(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "no thumbnail"})
			return
		}
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	sim := &stubSim{results: map[string]similarity.Result{}}
	q, cleanup := lookupEnvCfg(t, fake, handler, simChainCfg(), nil, sim)
	defer cleanup()

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendBooru, testMD5, ""))
	if job.Items[0].ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want hash_not_found", job.Items[0])
	}
	// One trail entry covers both similarity sources; no service was queried
	// without bytes; the exact searches still ran.
	if len(sim.calls) != 0 {
		t.Errorf("similarity calls = %v, want none without a thumbnail", sim.calls)
	}
	if !strings.Contains(reportBody, `"message":"danbooru: no match; similarity: no thumbnail available; gelbooru: no match"`) {
		t.Errorf("fetch-status report = %q", reportBody)
	}
}

func TestHashImportDownloadsMatchedPost(t *testing.T) {
	danURL := "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
	post := danbooruPost("100777")
	fake := &fakeRunner{
		fetchByURL: map[string]fetchResp{danURL: {meta: post.Meta}},
		resolved:   []gdl.Item{post},
		writeIdx:   []int{0},
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 55})
	}
	q, cleanup := lookupEnv(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueHashImport(testMD5, queue.Options{}))
	if job.Status != queue.JobSucceeded || job.Summary.Created != 1 {
		t.Fatalf("job = %s %+v, want one created", job.Status, job.Summary)
	}
	if job.URL != "https://danbooru.donmai.us/posts/100777" {
		t.Errorf("job url = %q, want the matched post's page", job.URL)
	}
	// The download resolved the post URL the walk produced, forced past the
	// archive like any single-post submit.
	if !fake.gotForce {
		t.Errorf("hash import should bypass the archive like a single-post job")
	}
}

func TestHashImportFullMissFailsJob(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
	q, cleanup := lookupEnv(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}})
	defer cleanup()

	job := waitJob(t, q, q.EnqueueHashImport(testMD5, queue.Options{}))
	if job.Status != queue.JobFailed || job.ErrorCode != queue.ErrCodeHashNotFound {
		t.Fatalf("job = %s code=%q, want failed hash_not_found", job.Status, job.ErrorCode)
	}
	if job.Subject() != "md5:"+testMD5 {
		t.Errorf("subject = %q, want the md5 form (no url was ever resolved)", job.Subject())
	}
}
