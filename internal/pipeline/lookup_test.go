package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/similarity"
	"github.com/monbooru/monloader/internal/sitestate"
)

const (
	testMD5 = "0123456789abcdef0123456789abcdef"
	danURL  = "https://danbooru.donmai.us/posts?tags=md5:" + testMD5
)

func lookupEnv(t *testing.T, fake gdl.Runner, handler http.HandlerFunc, sites []config.Site) *queue.Queue {
	return lookupEnvPTR(t, fake, handler, sites, nil)
}

func lookupEnvPTR(t *testing.T, fake gdl.Runner, handler http.HandlerFunc, sites []config.Site, ptrSvc PTRService) *queue.Queue {
	cfg := config.Default()
	cfg.Sites = sites
	return lookupEnvCfg(t, fake, handler, cfg, ptrSvc, nil)
}

type disabledPTR struct{}

func (disabledPTR) Enabled() bool  { return false }
func (disabledPTR) CaughtUp() bool { return false }
func (disabledPTR) TagsForHash(string) ([]string, bool, error) {
	return nil, false, nil
}

type noSimilarity struct{}

func (noSimilarity) Missing(string) (string, bool) { return "a key", true }
func (noSimilarity) Search(context.Context, string, []byte) (similarity.Result, error) {
	return similarity.Result{}, nil
}

func lookupEnvCfg(t *testing.T, fake gdl.Runner, handler http.HandlerFunc, cfg *config.Config, ptrSvc PTRService, simSvc SimilarityService) *queue.Queue {
	t.Helper()
	return storeEnv(t, fake, handler, cfg, ptrSvc, simSvc, nil)
}

func storeEnv(t *testing.T, fake gdl.Runner, handler http.HandlerFunc, cfg *config.Config, ptrSvc PTRService, simSvc SimilarityService, st *queue.Store) *queue.Queue {
	t.Helper()
	if ptrSvc == nil {
		ptrSvc = disabledPTR{}
	}
	if simSvc == nil {
		simSvc = noSimilarity{}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/galleries" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		handler(w, r)
	}))
	cfg.Monbooru.APIURL = srv.URL
	cfg.Monbooru.APIToken = "tok"
	mapper, err := mapping.New(config.NewProvider(cfg), "")
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	proc := New(fake, mapper, monbooru.New(config.NewProvider(cfg)), config.NewProvider(cfg), t.TempDir(), sitestate.New(), ptrSvc, simSvc)
	if st != nil {
		proc.UseBooks(st)
	}
	q := queue.New(proc, 1, 100)
	q.Start()
	t.Cleanup(func() { q.Close(); srv.Close() })
	return q
}

type captured struct{ path, body string }

func captureReply(resp any) (*captured, http.HandlerFunc) {
	got := &captured{}
	return got, func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path + "?" + r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		reply(w, http.StatusOK, resp)
	}
}

func wantOneEnriched(t *testing.T, job *queue.Job) {
	t.Helper()
	if job.Status != queue.JobSucceeded || len(job.Items) != 1 || job.Items[0].Outcome != outcome.Enriched {
		t.Fatalf("job = %s %+v, want one enriched item", job.Status, job.Items)
	}
}

func wantHashNotFound(t *testing.T, job *queue.Job) {
	t.Helper()
	if job.Items[0].ErrorCode != outcome.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want hash_not_found", job.Items[0])
	}
}

type stubPTR struct {
	enabled bool
	syncing bool
	tags    []string
	found   bool
	err     error
}

func (s *stubPTR) Enabled() bool  { return s.enabled }
func (s *stubPTR) CaughtUp() bool { return s.enabled && !s.syncing }
func (s *stubPTR) TagsForHash(string) ([]string, bool, error) {
	return s.tags, s.found, s.err
}

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

var noMatch = fetchResp{err: &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: "no post metadata in gallery-dl output"}}

func gelbooruLookupPost(id string) map[string]any {
	return map[string]any{
		"category": "gelbooru", "id": id, "subcategory": "tag", "rating": "general",
		"tags_general": []any{"tag_" + id},
	}
}

func TestLookupWalksInOrderAndEnrichesFirstHit(t *testing.T) {
	gelURL := "https://gelbooru.com/index.php?page=post&s=list&tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: noMatch,
		gelURL: {meta: gelbooruLookupPost("999")},
	}}
	got, handler := captureReply(map[string]any{"merge": map[string]any{"tags_added": 1}})
	q := lookupEnv(t, fake, handler, []config.Site{
		{Name: "gelbooru", LookupOrder: 2, APIKey: "k", UserID: "u"},
		{Name: "danbooru", LookupOrder: 1},
	})

	job := waitJob(t, q, q.EnqueueLookup(42, "art", queue.BackendBooru, testMD5, "", false, false))

	if !reflect.DeepEqual(fake.gotFetchURLs, []string{danURL, gelURL}) {
		t.Errorf("walk = %v, want danbooru then gelbooru", fake.gotFetchURLs)
	}
	wantOneEnriched(t, job)
	if job.Site != "gelbooru" {
		t.Errorf("site = %q, want gelbooru", job.Site)
	}
	if job.Items[0].URL != "https://gelbooru.com/index.php?page=post&s=view&id=999" {
		t.Errorf("item url = %q, want the matched post's canonical page", job.Items[0].URL)
	}
	if job.URL != job.Items[0].URL {
		t.Errorf("job url = %q, want the resolved post", job.URL)
	}
	if job.Items[0].PostID != "999" {
		t.Errorf("item post id = %q, want 999", job.Items[0].PostID)
	}
	if got.path != "/api/v1/images/42/enrich?gallery=art" {
		t.Errorf("enrich path = %q", got.path)
	}
	if !strings.Contains(got.body, `"source_md5":"`+testMD5+`"`) || !strings.Contains(got.body, `"verify":true`) ||
		!strings.Contains(got.body, "tag_999") {
		t.Errorf("enrich body wrong: %s", got.body)
	}
}

func TestLookupItemCarriesSHA256(t *testing.T) {
	sha := strings.Repeat("cd", 32)
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{danURL: noMatch}}
	handler := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	q := lookupEnv(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}})

	job := waitJob(t, q, q.EnqueueLookup(6431, "test_gallery", queue.BackendBooru, testMD5, sha, false, false))
	if len(job.Items) != 1 {
		t.Fatalf("items = %+v, want one", job.Items)
	}
	if job.Items[0].SHA256 != sha {
		t.Errorf("item sha256 = %q, want %q so the view link uses /i/<sha256>", job.Items[0].SHA256, sha)
	}
}

func TestLookupSkipsSiteMissingCredential(t *testing.T) {
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: danbooruPost("100005").Meta},
	}}

	q := lookupEnv(t, fake, enrichedHandler, []config.Site{
		{Name: "gelbooru", LookupOrder: 1},
		{Name: "sankaku", LookupOrder: 2},
		{Name: "danbooru", LookupOrder: 3},
	})

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, "", false, false))

	if !reflect.DeepEqual(fake.gotFetchURLs, []string{danURL}) {
		t.Errorf("walk = %v, want only danbooru (others skipped for credentials)", fake.gotFetchURLs)
	}
	wantOneEnriched(t, job)
}

func TestLookupContinuesPastSiteError(t *testing.T) {
	konURL := "https://konachan.com/post?tags=md5:" + testMD5
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {err: &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: "429"}},
		konURL: {meta: map[string]any{"category": "konachan", "id": "77", "rating": "s", "tags_general": []any{"scenery"}}},
	}}
	q := lookupEnv(t, fake, enrichedHandler, []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "konachan", LookupOrder: 2},
	})

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, "", false, false))
	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Enriched || job.Site != "konachan" {
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
		enrichedHandler(w, r)
	}
	q := lookupEnv(t, fake, handler, []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "yandere", LookupOrder: 2},
	})

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendBooru, testMD5, "", false, false))
	it := job.Items[0]
	if it.Outcome != outcome.Failed || it.ErrorCode != outcome.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want failed hash_not_found", it)
	}
	if !strings.Contains(it.Error, "danbooru: no match") || !strings.Contains(it.Error, "yandere: no match") {
		t.Errorf("error trail = %q, want per-site misses", it.Error)
	}
	if reportPath != "/api/v1/images/9/fetch-status" || !strings.Contains(reportBody, `"state":"hash_not_found"`) ||
		!strings.Contains(reportBody, `"backend":"booru"`) {
		t.Errorf("fetch-status report = %q %q, want a hash_not_found state for the booru lookup", reportPath, reportBody)
	}
	if !strings.Contains(reportBody, `"message":"danbooru: no match; iqdb: skipped, needs a key; yandere: no match; saucenao: skipped, needs a key"`) {
		t.Errorf("fetch-status message = %q, want the bare joined trail", reportBody)
	}
}

func TestLookupPTRBackendUnavailable(t *testing.T) {
	fake := &fakeRunner{}
	q := lookupEnvPTR(t, fake, enrichedHandler, nil, &stubPTR{enabled: false})

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendPTR, "", strings.Repeat("a", 64), false, false))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodePTRUnavailable {
		t.Fatalf("items = %+v, want failed ptr_unavailable", job.Items)
	}
	if len(fake.gotFetchURLs) != 0 {
		t.Errorf("a ptr lookup must not touch the booru walk, got %v", fake.gotFetchURLs)
	}
}

func TestLookupPTRBackendSyncing(t *testing.T) {
	fake := &fakeRunner{}
	ptrSvc := &stubPTR{enabled: true, syncing: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, enrichedHandler, nil, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendPTR, "", strings.Repeat("a", 64), false, false))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodePTRSyncing {
		t.Fatalf("items = %+v, want failed ptr_syncing", job.Items)
	}
}

func TestLookupPTRHitEnriches(t *testing.T) {
	fake := &fakeRunner{}
	got, handler := captureReply(map[string]any{"merge": map[string]any{"tags_added": 2}})
	sha := strings.Repeat("cd", 32)
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky", "character:reimu"}}
	q := lookupEnvPTR(t, fake, handler, nil, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(88, "art", queue.BackendPTR, "", sha, false, false))
	wantOneEnriched(t, job)
	if !strings.Contains(got.body, `"source":"ptr"`) || !strings.Contains(got.body, `"verify":false`) ||
		!strings.Contains(got.body, "character:reimu") || strings.Contains(got.body, `"url":"http`) {
		t.Errorf("enrich body wrong: %s", got.body)
	}
}

func TestLookupPTRDropsCategoriesTheOperatorDoesNotSend(t *testing.T) {
	got, handler := captureReply(map[string]any{"merge": map[string]any{"tags_added": 1}})
	cfg := config.Default()
	cfg.Tags.DropCategories = []string{"general"}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky", "character:reimu"}}
	q := lookupEnvCfg(t, &fakeRunner{}, handler, cfg, ptrSvc, nil)

	job := waitJob(t, q, q.EnqueueLookup(88, "art", queue.BackendPTR, "", strings.Repeat("cd", 32), false, false))
	wantOneEnriched(t, job)
	if !strings.Contains(got.body, "character:reimu") || strings.Contains(got.body, "blue_sky") {
		t.Errorf("enrich body = %s, want the general tag dropped", got.body)
	}
}

func TestLookupPTRWithholdsWhatItsRowDrops(t *testing.T) {
	for _, backend := range []string{queue.BackendPTR, queue.BackendAll} {
		var enrich string
		handler := captureHandler("/enrich", func(b string) { enrich = b })
		cfg := config.Default()
		cfg.TagOverrides = []config.TagOverride{{Site: config.SitePTR, From: "species", To: ""}}
		ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"species:cat", "smile"}}
		q := lookupEnvCfg(t, &fakeRunner{}, handler, cfg, ptrSvc, nil)
		wantOneEnriched(t, waitJob(t, q, q.EnqueueLookup(88, "art", backend, "", strings.Repeat("cd", 32), false, false)))
		var body struct {
			Tags         []string `json:"tags"`
			WithheldTags []string `json:"withheld_tags"`
		}
		if err := json.Unmarshal([]byte(enrich), &body); err != nil {
			t.Fatalf("%s enrich body %q: %v", backend, enrich, err)
		}
		if !slices.Equal(body.Tags, []string{"smile"}) || !slices.Equal(body.WithheldTags, []string{"species:cat"}) {
			t.Errorf("%s enrich body = %s, want species:cat withheld", backend, enrich)
		}
	}
}

func TestLookupPTRBlacklistedAppliesNothing(t *testing.T) {
	enriches := 0
	var reportBody string
	report := captureHandler("fetch-status", func(b string) { reportBody = b })
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/enrich") {
			enriches++
		}
		report(w, r)
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky", "bad"}}
	q := lookupEnvPTR(t, &fakeRunner{}, handler, []config.Site{{Name: config.SitePTR, Blacklist: []string{"bad"}}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendPTR, "", strings.Repeat("ef", 32), false, false))
	wantHashNotFound(t, job)
	if enriches != 0 {
		t.Errorf("enriches = %d, want a blacklisted answer to apply nothing", enriches)
	}
	if !strings.Contains(reportBody, `"message":"Public Tag Repository: skipped, blacklisted (ptr rule: bad)"`) {
		t.Errorf("fetch-status report = %q, want the trail to name the rule", reportBody)
	}
}

func TestLookupAllBlacklistedPTRStillRunsTheChain(t *testing.T) {
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{danURL: {meta: gelbooruLookupPost("321")}}}
	var enriches []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enriches = append(enriches, string(b))
		}
		enrichedHandler(w, r)
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"ptr_only_tag", "bad"}}
	q := lookupEnvPTR(t, fake, handler, []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: config.SitePTR, Blacklist: []string{"bad"}},
	}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32), false, false))
	wantOneEnriched(t, job)
	if len(enriches) != 1 || strings.Contains(enriches[0], "ptr_only_tag") {
		t.Errorf("enriches = %v, want the online one alone", enriches)
	}
	if !strings.Contains(job.Items[0].MergeNote, "ptr skipped, blacklisted (ptr rule: bad)") {
		t.Errorf("note = %q, want the skipped PTR named", job.Items[0].MergeNote)
	}
}

func TestLookupPTRRefusesAForeignHash(t *testing.T) {
	fake := &fakeRunner{}
	enriches := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/enrich") {
			enriches++
		}
		w.WriteHeader(http.StatusOK)

		_ = json.NewEncoder(w).Encode(map[string]any{"sha256": strings.Repeat("ab", 32)})
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, handler, nil, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(6, "art", queue.BackendPTR, "", strings.Repeat("cd", 32), false, false))
	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodeHashMismatch {
		t.Fatalf("item = %+v, want a hash_mismatch failure", job.Items)
	}
	if enriches != 0 {
		t.Errorf("enriched %d times, want the tags never written", enriches)
	}
}

func TestLookupPTRMissFails(t *testing.T) {
	fake := &fakeRunner{}
	var reportBody string
	handler := captureHandler("fetch-status", func(b string) { reportBody = b })
	sha := strings.Repeat("ef", 32)
	ptrSvc := &stubPTR{enabled: true, found: false}
	q := lookupEnvPTR(t, fake, handler, nil, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendPTR, "", sha, false, false))
	wantHashNotFound(t, job)
	if !strings.Contains(reportBody, `"state":"hash_not_found"`) ||
		!strings.Contains(reportBody, `"message":"Public Tag Repository: no match"`) {
		t.Errorf("fetch-status report = %q, want hash_not_found with the trail entry", reportBody)
	}
}

func TestLookupPTRWithNoUsableTagsIsAMiss(t *testing.T) {
	for _, backend := range []string{queue.BackendPTR, queue.BackendAll} {
		enriches := 0
		var reportBody string
		report := captureHandler("fetch-status", func(b string) { reportBody = b })
		handler := func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/enrich") {
				enriches++
			}
			report(w, r)
		}
		ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"title:bookkeeping"}}
		q := lookupEnvPTR(t, &fakeRunner{}, handler, nil, ptrSvc)

		job := waitJob(t, q, q.EnqueueLookup(9, "", backend, "", strings.Repeat("ef", 32), false, false))
		wantHashNotFound(t, job)
		if enriches != 0 {
			t.Errorf("%s enriches = %d, want none", backend, enriches)
		}
		if !strings.Contains(reportBody, "Public Tag Repository: no usable tags") {
			t.Errorf("%s fetch-status report = %q, want the trail to say so", backend, reportBody)
		}
	}
}

func TestLookupAllEnrichesBothBackends(t *testing.T) {
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: gelbooruLookupPost("321")},
	}}
	var enriches []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			enriches = append(enriches, string(b))
		}
		enrichedHandler(w, r)
	}
	sha := strings.Repeat("ab", 32)
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, handler, []config.Site{
		{Name: "danbooru", LookupOrder: 1},
		{Name: "gelbooru", LookupOrder: 2, APIKey: "k", UserID: "u"},
	}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, sha, false, false))
	wantOneEnriched(t, job)
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

func TestLookupAllDoubleHitComposesNote(t *testing.T) {
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
		reply(w, http.StatusOK, map[string]any{"merge": merge})
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32), false, false))
	if job.Items[0].Outcome != outcome.Enriched {
		t.Fatalf("item = %+v, want enriched", job.Items[0])
	}
	note := job.Items[0].MergeNote
	if !strings.Contains(note, "ptr +5 tags") || !strings.Contains(note, "gelbooru +2 tags") {
		t.Errorf("merge note = %q, want both backends' deltas", note)
	}
}

func TestALookupRefusedForFlatTagsReportsToMonbooru(t *testing.T) {
	for _, backend := range []string{queue.BackendBooru, queue.BackendAll} {
		t.Run(backend, func(t *testing.T) {
			fake := &fakeRunner{fetchByURL: map[string]fetchResp{
				danURL: {meta: map[string]any{"category": "safebooru", "id": "199", "tags": "an_artist"}},
			}}
			var enriched bool
			var reported string
			q := lookupEnvPTR(t, fake, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "fetch-status") {
					b, _ := io.ReadAll(r.Body)
					reported = string(b)
				} else if strings.HasSuffix(r.URL.Path, "/enrich") {
					enriched = true
				}
				enrichedHandler(w, r)
			}, []config.Site{{Name: "danbooru", LookupOrder: 1}}, nil)

			job := waitJob(t, q, q.EnqueueLookup(7, "", backend, testMD5, strings.Repeat("ab", 32), false, false))
			if job.Items[0].ErrorCode != outcome.ErrCodeMappingFailed {
				t.Errorf("item = %+v, want mapping_failed", job.Items[0])
			}
			if enriched {
				t.Error("a post with only flat tags was sent to monbooru")
			}
			if !strings.Contains(reported, outcome.ErrCodeMappingFailed) {
				t.Errorf("reported %q, want the refusal told to monbooru", reported)
			}
		})
	}
}

func TestLookupAllBooruHitNamesASkippedPTR(t *testing.T) {
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: gelbooruLookupPost("321")},
	}}
	ptrSvc := &stubPTR{enabled: true, syncing: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, enrichedHandler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32), false, false))
	if job.Items[0].Outcome != outcome.Enriched {
		t.Fatalf("item = %+v, want enriched", job.Items[0])
	}
	if note := job.Items[0].MergeNote; !strings.Contains(note, "ptr skipped, still syncing") {
		t.Errorf("merge note = %q, want the skipped ptr named beside the booru's answer", note)
	}
}

func TestLookupAllBooruHitNamesAPTRMiss(t *testing.T) {
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: gelbooruLookupPost("321")},
	}}
	ptrSvc := &stubPTR{enabled: true, found: false}
	q := lookupEnvPTR(t, fake, enrichedHandler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32), false, false))
	if note := job.Items[0].MergeNote; !strings.Contains(note, "ptr no match") {
		t.Errorf("merge note = %q, want the ptr's own answer named", note)
	}
}

func TestLookupAllBooruFailAfterPTRStaysEnriched(t *testing.T) {
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: gelbooruLookupPost("321")},
	}}
	var reportBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
			enrichedHandler(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/enrich") {
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), `"verify":true`) {

				reply(w, http.StatusConflict, map[string]any{"error": "hash mismatch"})
				return
			}
		}
		reply(w, http.StatusOK, map[string]any{"merge": map[string]any{"tags_added": 5}})
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32), false, false))
	if job.Status != queue.JobSucceeded || job.Items[0].Outcome != outcome.Enriched {
		t.Fatalf("job = %s %+v, want the item enriched from the PTR tags", job.Status, job.Items)
	}
	if !strings.Contains(reportBody, "Public Tag Repository: match, tags applied") {
		t.Errorf("fetch-status report = %q, want the PTR match leading the trail", reportBody)
	}
}

func TestLookupAllFullMissMergesTrail(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := captureHandler("fetch-status", func(b string) { reportBody = b })
	sha := strings.Repeat("cd", 32)
	ptrSvc := &stubPTR{enabled: true, found: false}
	q := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendAll, testMD5, sha, false, false))
	wantHashNotFound(t, job)
	if !strings.Contains(reportBody, `"message":"Public Tag Repository: no match; danbooru: no match; iqdb: skipped, needs a key; saucenao: skipped, needs a key"`) {
		t.Errorf("fetch-status report = %q, want the merged trail", reportBody)
	}
}

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
		enrichedHandler(w, r)
	}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendAll, testMD5, strings.Repeat("cd", 32), false, false))
	wantOneEnriched(t, job)
	if len(enriches) != 1 || !strings.Contains(enriches[0], `"source":"ptr"`) {
		t.Fatalf("enriches = %v, want the ptr one alone", enriches)
	}
	if !strings.Contains(reportBody, `"state":"hash_not_found"`) ||
		!strings.Contains(reportBody, `"message":"Public Tag Repository: match, tags applied; danbooru: no match; iqdb: skipped, needs a key; saucenao: skipped, needs a key"`) {
		t.Errorf("fetch-status report = %q, want the ptr match leading the online trail", reportBody)
	}
}

func TestLookupAllWithPTRDisabledWalksBooruOnly(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := captureHandler("fetch-status", func(b string) { reportBody = b })
	q := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}},
		&stubPTR{enabled: false})

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendAll, testMD5, strings.Repeat("ef", 32), false, false))
	wantHashNotFound(t, job)
	if !strings.Contains(reportBody, `"message":"danbooru: no match; iqdb: skipped, needs a key; saucenao: skipped, needs a key"`) ||
		strings.Contains(reportBody, "Public Tag Repository") {
		t.Errorf("fetch-status report = %q, want the booru trail alone", reportBody)
	}
}

func TestLookupAllWithPTRSyncingSkipsIt(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := captureHandler("fetch-status", func(b string) { reportBody = b })
	ptrSvc := &stubPTR{enabled: true, syncing: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvPTR(t, fake, handler, []config.Site{{Name: "danbooru", LookupOrder: 1}}, ptrSvc)

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendAll, testMD5, strings.Repeat("ef", 32), false, false))
	wantHashNotFound(t, job)
	if !strings.Contains(reportBody, `"message":"Public Tag Repository: skipped, still syncing; danbooru: no match; iqdb: skipped, needs a key; saucenao: skipped, needs a key"`) {
		t.Errorf("fetch-status report = %q, want the skipped ptr leading the booru trail", reportBody)
	}
}

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

func okHandler(thumbHits *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			*thumbHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegbytes"))
			return
		}
		enrichedHandler(w, r)
	}
}

func capturePush(id int, fields map[string]*string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		for name, dst := range fields {
			*dst = r.FormValue(name)
		}
		reply(w, http.StatusCreated, map[string]any{"id": id})
	}
}

func countingPush(n *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		*n++
		reply(w, http.StatusCreated, map[string]any{"id": *n})
	}
}

func captureHandler(match string, record func(string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/thumbnail") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegbytes"))
			return
		}
		if strings.Contains(r.URL.Path, match) {
			b, _ := io.ReadAll(r.Body)
			record(string(b))
		}
		enrichedHandler(w, r)
	}
}

func TestLookupItemCarriesTheImageDigest(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name    string
		status  int
		wantSHA string
	}{
		{"monbooru answers", http.StatusOK, sha},
		{"monbooru refuses", http.StatusNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRunner{fetchByURL: map[string]fetchResp{danURL: {meta: danbooruPost("100005").Meta}}}
			handler := func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/4") {
					reply(w, tc.status, map[string]any{"id": 4, "sha256": sha})
					return
				}
				enrichedHandler(w, r)
			}
			q := lookupEnvCfg(t, fake, handler, simChainCfg(), nil, &stubSim{})

			job := waitJob(t, q, q.EnqueueLookup(4, "art", queue.BackendBooru, testMD5, "", false, false))
			wantOneEnriched(t, job)
			if job.Items[0].SHA256 != tc.wantSHA {
				t.Errorf("item sha256 = %q, want %q", job.Items[0].SHA256, tc.wantSHA)
			}
		})
	}
}

func TestLookupChainExactHitFetchesNothing(t *testing.T) {
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL: {meta: danbooruPost("100005").Meta},
	}}
	var thumbHits int
	sim := &stubSim{}
	q := lookupEnvCfg(t, fake, okHandler(&thumbHits), simChainCfg(), nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, "", false, false))
	wantOneEnriched(t, job)
	if thumbHits != 0 || len(sim.calls) != 0 {
		t.Errorf("thumbnail fetches = %d, similarity calls = %v; want none", thumbHits, sim.calls)
	}
}

func TestLookupChainSimilarityHitShortCircuits(t *testing.T) {
	postURL := "https://danbooru.donmai.us/posts/100005"
	postMeta := danbooruPost("100005").Meta
	postMeta["md5"] = "fedcba9876543210fedcba9876543210"
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		danURL:  noMatch,
		postURL: {meta: postMeta},
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
	q := lookupEnvCfg(t, fake, handler, simChainCfg(), nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, "", false, false))
	wantOneEnriched(t, job)
	if job.Items[0].PostID != "danbooru 96%" {
		t.Errorf("item label = %q, want the site and score", job.Items[0].PostID)
	}
	if !strings.Contains(enrichBody, `"similarity":96.4`) {
		t.Errorf("enrich body = %s, want the similarity score", enrichBody)
	}
	if !strings.Contains(enrichBody, `"source_md5":"fedcba9876543210fedcba9876543210"`) {
		t.Errorf("enrich body = %s, want the matched post's md5 claim", enrichBody)
	}
	if job.Items[0].URL != postURL {
		t.Errorf("item url = %q, want the candidate post", job.Items[0].URL)
	}

	if thumbHits != 1 || !reflect.DeepEqual(sim.calls, []string{"iqdb"}) {
		t.Errorf("thumbnail fetches = %d, similarity calls = %v", thumbHits, sim.calls)
	}
	if !reflect.DeepEqual(fake.gotFetchURLs, []string{danURL, postURL}) {
		t.Errorf("fetches = %v, want the exact miss then the candidate", fake.gotFetchURLs)
	}
}

func TestLookupChainCandidatePreference(t *testing.T) {

	gelPost := "https://gelbooru.com/index.php?page=post&s=view&id=999"
	cfg := simChainCfg()
	cfg.Sites[0].LookupOrder = 0
	cfg.Lookup.Iqdb.Order = 0
	fake := &fakeRunner{fetchByURL: map[string]fetchResp{
		gelPost: {meta: gelbooruLookupPost("999")},
	}}
	var thumbHits int
	sim := &stubSim{results: map[string]similarity.Result{
		"saucenao": {Candidates: []similarity.Candidate{
			{Site: "danbooru", URL: "https://danbooru.donmai.us/posts/1", Similarity: 99},
			{Site: "gelbooru", URL: gelPost, Similarity: 91},
			{Site: "yandere", URL: "https://yande.re/post/show/2", Similarity: 42},
		}},
	}}
	q := lookupEnvCfg(t, fake, okHandler(&thumbHits), cfg, nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, "", false, false))
	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Enriched || job.Site != "gelbooru" {
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
	yanPost := "https://yande.re/post/show/2"
	cfg := simChainCfg()
	cfg.Sites[0].LookupOrder = 0
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
	q := lookupEnvCfg(t, fake, okHandler(&thumbHits), cfg, nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, "", false, false))
	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Enriched || job.Site != "yandere" {
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
	q := lookupEnvCfg(t, fake, okHandler(&thumbHits), cfg, nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(1, "", queue.BackendBooru, testMD5, "", false, false))
	it := job.Items[0]
	if it.Outcome != outcome.Failed || it.ErrorCode != outcome.ErrCodeHashNotFound {
		t.Fatalf("item = %+v, want failed hash_not_found", it)
	}
	want := "iqdb: no match, closest candidates: https://anidb.net/anime/9 (71%), " +
		"https://yande.re/post/show/2 (63%), https://danbooru.donmai.us/posts/1 (41%)"
	if !strings.Contains(it.Error, want) {
		t.Errorf("error trail = %q, want %q", it.Error, want)
	}
}

func TestLookupChainRecordsStrongExtraAsSource(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	var enrichBody string
	handler := captureHandler("/enrich", func(b string) { enrichBody = b })
	sim := &stubSim{results: map[string]similarity.Result{
		"iqdb": {Candidates: []similarity.Candidate{
			{Site: "danbooru", URL: "https://danbooru.donmai.us/posts/1", Similarity: 41.2},
		}, Extras: []similarity.Candidate{
			{Site: "anidb.net", URL: "https://anidb.net/anime/9", Similarity: 82.5},
			{Site: "deviantart.com", URL: "https://www.deviantart.com/artist/art/piece-8", Similarity: 96.1},
		}},
	}}
	cfg := simChainCfg()
	cfg.Lookup.Saucenao.Order = 0
	q := lookupEnvCfg(t, fake, handler, cfg, nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(6, "", queue.BackendBooru, testMD5, "", false, false))
	wantOneEnriched(t, job)
	if job.Items[0].PostID != "deviantart.com 96%, source only" || job.Site != "deviantart.com" {
		t.Errorf("label = %q site = %q, want the best extra recorded", job.Items[0].PostID, job.Site)
	}
	if !strings.Contains(enrichBody, `"source":"deviantart.com"`) ||
		!strings.Contains(enrichBody, `"url":"https://www.deviantart.com/artist/art/piece-8"`) ||
		!strings.Contains(enrichBody, `"similarity":96.1`) {
		t.Errorf("enrich body = %s, want the extra's host, url, and score", enrichBody)
	}
}

func TestLookupChainRecordsSourceWhenNoCandidateFetchable(t *testing.T) {

	gelPost := "https://gelbooru.com/index.php?page=post&s=view&id=42"
	cfg := simChainCfg()
	cfg.Lookup.Iqdb.Order = 0
	fake := &fakeRunner{fetchErr: noMatch.err, fetchByURL: map[string]fetchResp{
		gelPost: {err: &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: "429"}},
	}}
	var enrichBody string
	handler := captureHandler("/enrich", func(b string) { enrichBody = b })
	sim := &stubSim{results: map[string]similarity.Result{
		"saucenao": {Candidates: []similarity.Candidate{
			{Site: "sankaku", URL: "https://chan.sankakucomplex.com/post/show/7", Similarity: 95},
			{Site: "gelbooru", URL: gelPost, Similarity: 91},
		}, Extras: []similarity.Candidate{

			{Site: "deviantart.com", URL: "https://www.deviantart.com/artist/art/piece-8", Similarity: 99},
		}},
	}}
	q := lookupEnvCfg(t, fake, handler, cfg, nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(5, "", queue.BackendBooru, testMD5, "", false, false))
	wantOneEnriched(t, job)
	if job.Items[0].PostID != "gelbooru 91%, source only" || job.Site != "gelbooru" {
		t.Errorf("label = %q site = %q, want the preferred candidate recorded", job.Items[0].PostID, job.Site)
	}
	if job.Items[0].URL != gelPost {
		t.Errorf("item url = %q, want the candidate post", job.Items[0].URL)
	}
	gelSearch := "https://gelbooru.com/index.php?page=post&s=list&tags=md5:" + testMD5
	if !reflect.DeepEqual(fake.gotFetchURLs, []string{
		danURL, gelPost, gelSearch,
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
	postURL := "https://danbooru.donmai.us/posts/31"
	cfg := simChainCfg()
	cfg.Lookup.Iqdb.Order = 0
	fake := &fakeRunner{fetchErr: noMatch.err, fetchByURL: map[string]fetchResp{
		postURL: {err: &outcome.CodedError{Code: outcome.ErrCodeBlocked, Msg: "challenge"}},
	}}
	var enriches []string
	handler := captureHandler("/enrich", func(b string) { enriches = append(enriches, b) })
	sim := &stubSim{results: map[string]similarity.Result{
		"saucenao": {Candidates: []similarity.Candidate{{Site: "danbooru", URL: postURL, Similarity: 88}}},
	}}
	ptrSvc := &stubPTR{enabled: true, found: true, tags: []string{"blue_sky"}}
	q := lookupEnvCfg(t, fake, handler, cfg, ptrSvc, sim)

	job := waitJob(t, q, q.EnqueueLookup(7, "", queue.BackendAll, testMD5, strings.Repeat("ab", 32), false, false))
	wantOneEnriched(t, job)
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
	fake := &fakeRunner{fetchErr: noMatch.err}
	var reportBody string
	handler := captureHandler("fetch-status", func(b string) { reportBody = b })
	sim := &stubSim{
		missing: map[string]string{"iqdb": "danbooru api key"},
		errs:    map[string]error{"saucenao": &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: "cooling down"}},
	}
	q := lookupEnvCfg(t, fake, handler, simChainCfg(), nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendBooru, testMD5, "", false, false))
	wantHashNotFound(t, job)
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
			reply(w, http.StatusNotFound, map[string]any{"error": "no thumbnail"})
			return
		}
		if strings.Contains(r.URL.Path, "fetch-status") {
			b, _ := io.ReadAll(r.Body)
			reportBody = string(b)
		}
		enrichedHandler(w, r)
	}
	sim := &stubSim{results: map[string]similarity.Result{}}
	q := lookupEnvCfg(t, fake, handler, simChainCfg(), nil, sim)

	job := waitJob(t, q, q.EnqueueLookup(9, "", queue.BackendBooru, testMD5, "", false, false))
	wantHashNotFound(t, job)
	if len(sim.calls) != 0 {
		t.Errorf("similarity calls = %v, want none without a thumbnail", sim.calls)
	}
	if !strings.Contains(reportBody, `"message":"danbooru: no match; similarity: no thumbnail available; gelbooru: no match"`) {
		t.Errorf("fetch-status report = %q", reportBody)
	}
}

func TestHashImportDownloadsMatchedPost(t *testing.T) {
	post := danbooruPost("100777")
	fake := &fakeRunner{
		fetchByURL: map[string]fetchResp{danURL: {meta: post.Meta}},
		resolved:   []gdl.Item{post},
		writeIdx:   []int{0},
	}
	q := lookupEnv(t, fake, createdHandler, []config.Site{{Name: "danbooru", LookupOrder: 1}})

	job := waitJob(t, q, q.EnqueueHashImport(testMD5, queue.Options{}))
	if job.Status != queue.JobSucceeded || job.Summary.Created != 1 {
		t.Fatalf("job = %s %+v, want one created", job.Status, job.Summary)
	}
	if job.URL != "https://danbooru.donmai.us/posts/100777" {
		t.Errorf("job url = %q, want the matched post's page", job.URL)
	}
	if fake.gotArchive != gdl.ArchiveBypass {
		t.Errorf("hash import should bypass the archive like a single-post job")
	}
}

func TestHashImportFullMissFailsJob(t *testing.T) {
	fake := &fakeRunner{fetchErr: noMatch.err}
	q := lookupEnv(t, fake, enrichedHandler, []config.Site{{Name: "danbooru", LookupOrder: 1}})

	job := waitJob(t, q, q.EnqueueHashImport(testMD5, queue.Options{}))
	if job.Status != queue.JobFailed || job.ErrorCode != outcome.ErrCodeHashNotFound {
		t.Fatalf("job = %s code=%q, want failed hash_not_found", job.Status, job.ErrorCode)
	}
	if job.Subject() != "md5:"+testMD5 {
		t.Errorf("subject = %q, want the md5 form (no url was ever resolved)", job.Subject())
	}
}
