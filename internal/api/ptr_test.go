package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func disabledPTR() *stubPTR {
	return &stubPTR{status: ptr.Status{State: ptr.StateDisabled}}
}

type stubPTR struct {
	enabled       bool
	status        ptr.Status
	graph         map[string]ptr.TagInfo
	hasKey        bool
	createdKey    string
	createErr     error
	syncing       bool
	provisional   bool
	refreshBanned bool
	refreshErr    error
	onRefresh     func()
	contrib       *ptr.ContribStore
	tagFilter     *ptr.TagFilter
	ideals        map[string]map[string]bool
	raws          map[string][]string
	siblings      map[[2]string]bool
	parents       map[[2]string]bool
	idealTags     map[string]string
	onLookup      func()
	lookups       int
	searchOut     []ptr.TagMatch
	searchTrunc   bool
	searched      []ptr.TagRange
}

func (s *stubPTR) Provisional() bool               { return s.provisional }
func (s *stubPTR) CaughtUp() bool                  { return s.enabled && !s.syncing }
func (s *stubPTR) Contrib() *ptr.ContribStore      { return s.contrib }
func (s *stubPTR) TagFilterCached() *ptr.TagFilter { return s.tagFilter }
func (s *stubPTR) HashHasIdeal(h, tag string) (bool, error) {
	return s.ideals[h][tag], nil
}
func (s *stubPTR) HashHasIdeals(h string, tags []string) (map[string]bool, error) {
	out := make(map[string]bool, len(tags))
	for _, tag := range tags {
		out[tag] = s.ideals[h][tag]
	}
	return out, nil
}
func (s *stubPTR) HashHasRaw(h, tag string) (bool, error) {
	for _, r := range s.raws[h] {
		if r == tag {
			return true, nil
		}
	}
	return false, nil
}
func (s *stubPTR) RawTagsForHash(h string) ([]string, error) { return s.raws[h], nil }
func (s *stubPTR) TagsForHash(h string) ([]string, bool, error) {
	s.lookups++
	if s.onLookup != nil {
		s.onLookup()
	}
	tags, ok := s.raws[h]
	return tags, ok, nil
}
func (s *stubPTR) IdealTag(tag string) (string, bool, error) {
	ideal, ok := s.idealTags[tag]
	return ideal, ok, nil
}
func (s *stubPTR) SiblingCurrent(bad, good string) (bool, error) {
	return s.siblings[[2]string{bad, good}], nil
}
func (s *stubPTR) ParentCurrent(child, parent string) (bool, error) {
	return s.parents[[2]string{child, parent}], nil
}
func (s *stubPTR) ParentEdgeCovered(child, parent string) (bool, error) {
	ci := s.idealTags[child]
	if ci == "" {
		ci = child
	}
	pi := s.idealTags[parent]
	if pi == "" {
		pi = parent
	}

	for spelling, ideal := range s.idealTags {
		if ideal == ci && s.parents[[2]string{spelling, pi}] {
			return true, nil
		}
	}
	return s.parents[[2]string{ci, pi}], nil
}

func (s *stubPTR) Enabled() bool        { return s.enabled }
func (s *stubPTR) HasPersonalKey() bool { return s.hasKey }
func (s *stubPTR) CreateContribAccount(context.Context) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	s.hasKey = true
	return s.createdKey, nil
}
func (s *stubPTR) RefreshAccount(context.Context) (*ptr.Account, error) {
	if s.onRefresh != nil {
		s.onRefresh()
	}
	if s.refreshErr != nil {
		return nil, s.refreshErr
	}
	return &ptr.Account{Banned: s.refreshBanned}, nil
}
func (s *stubPTR) Status() ptr.Status { return s.status }
func (s *stubPTR) TagGraph(_ context.Context, names []string) (map[string]ptr.TagInfo, error) {
	return s.graph, nil
}
func (s *stubPTR) SearchTags(_ context.Context, ranges []ptr.TagRange, _ int) ([]ptr.TagMatch, bool, error) {
	s.searched = append(s.searched, ranges...)
	return s.searchOut, s.searchTrunc, nil
}

func contribStore(t *testing.T) *ptr.ContribStore {
	t.Helper()
	store, err := ptr.OpenContribStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPTRStatusDisabledWithoutBackend(t *testing.T) {
	srv := newTestServer(t, "")
	body := wantJSON(t, "GET", srv.URL+"/api/v1/ptr/status", "", http.StatusOK)
	if body["enabled"] != false || body["state"] != "disabled" {
		t.Errorf("status = %v, want disabled", body)
	}
}

func TestPTRStatusReportsState(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, status: ptr.Status{Enabled: true, State: ptr.StateSyncing, CoveredThrough: 1769558400}})
	resp, body := doJSON(t, "GET", srv.URL+"/api/v1/ptr/status", "", "")
	if resp.StatusCode != http.StatusOK || body["state"] != "syncing" {
		t.Errorf("status = %d %v, want 200 syncing", resp.StatusCode, body)
	}
	if body["covered_through"] != float64(1769558400) {
		t.Errorf("covered_through = %v, want 1769558400", body["covered_through"])
	}
	if q, ok := body["queryable"]; !ok || q != false {
		t.Errorf("queryable = %v (present %v), want false spelled out", q, ok)
	}
}

func TestPTRTagsMapsBothDirections(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, graph: map[string]ptr.TagInfo{
		"blue_eyes": {Known: true, Ideal: "blue_eyes", Aliases: []string{"blue eyes"}, Implications: []string{"eyes"}, ImpliedBy: []string{"character:asuka"}},
	}})
	body := wantJSON(t, "POST", srv.URL+"/api/v1/ptr/tags", `{"tags":["blue_eyes","missing"]}`, http.StatusOK)
	results, _ := body["results"].(map[string]any)
	be, _ := results["blue_eyes"].(map[string]any)
	if be["known"] != true || be["ideal"] != "blue_eyes" {
		t.Errorf("blue_eyes result = %v, want known with ideal blue_eyes", be)
	}
	impl, _ := be["implications"].([]any)
	if len(impl) != 1 || impl[0] != "eyes" {
		t.Errorf("implications = %v, want [eyes]", impl)
	}
	impBy, _ := be["implied_by"].([]any)
	if len(impBy) != 1 || impBy[0] != "character:asuka" {
		t.Errorf("implied_by = %v, want [character:asuka]", impBy)
	}
	if miss, _ := results["missing"].(map[string]any); miss["known"] != false {
		t.Errorf("missing = %v, want known=false", miss)
	}
}

func TestPTRTagSearchScansEveryNamespaceSpelling(t *testing.T) {
	stub := &stubPTR{enabled: true, searchOut: []ptr.TagMatch{
		{Ideal: "character:samus aran (metroid)", Matched: []string{"character:samus aran (metroid)"}},
		{Ideal: "character:samus aran", Matched: []string{"character:samus aran", "character:samus_aran"}, Aliases: 8, Implications: 1, ImpliedBy: 2},
		{Ideal: "title:samus arandom", Matched: []string{"title:samus arandom"}, Aliases: 3},
	}}
	srv := newPTRServer(t, "", stub)
	body := wantJSON(t, "GET", srv.URL+"/api/v1/ptr/tags/search?q=character:samus_aran", "", http.StatusOK)
	var prefixes []string
	for _, r := range stub.searched {
		if len(r.Contains) > 0 {
			t.Errorf("prefix search asked for a substring scan: %+v", r)
		}
		prefixes = append(prefixes, r.Prefix)
	}
	want := []string{"character:samus_aran", "character:samus aran"}
	if !reflect.DeepEqual(prefixes, want) {
		t.Errorf("scanned %v, want %v", prefixes, want)
	}
	clusters, _ := body["clusters"].([]any)
	if len(clusters) != 2 {
		t.Fatalf("clusters = %v, want the two routable ones", clusters)
	}
	first, _ := clusters[0].(map[string]any)
	if first["ideal"] != "character:samus_aran" || first["aliases"] != float64(8) {
		t.Errorf("first cluster = %v, want the connected samus_aran", first)
	}
	matched, _ := first["matched"].([]any)
	if len(matched) != 1 || matched[0] != "character:samus_aran" {
		t.Errorf("matched = %v, want the two spellings deduped by monbooru form", matched)
	}
	second, _ := clusters[1].(map[string]any)
	if second["ideal"] != "character:samus_aran_(metroid)" {
		t.Errorf("second cluster = %v, want the orphan below the connected one", second)
	}
}

func TestPTRTagSearchContainsNeedsANamespace(t *testing.T) {
	stub := &stubPTR{enabled: true}
	srv := newPTRServer(t, "", stub)

	wantCode(t, "GET", srv.URL+"/api/v1/ptr/tags/search?q=samus&mode=contains", "", http.StatusBadRequest, "namespace_required")
	if len(stub.searched) != 0 {
		t.Errorf("refused query still scanned %v", stub.searched)
	}

	if resp, body := doJSON(t, "GET", srv.URL+"/api/v1/ptr/tags/search?q=character:samus_aran&mode=contains", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("namespaced contains = %d %v", resp.StatusCode, body)
	}
	if len(stub.searched) != 1 || stub.searched[0].Prefix != "character:" ||
		!reflect.DeepEqual(stub.searched[0].Contains, []string{"samus_aran", "samus aran"}) {
		t.Errorf("scanned %+v, want one character: range carrying both spellings", stub.searched)
	}
}

func TestPTRTagSearchValidation(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true})
	for _, tc := range []struct{ name, query, code string }{
		{"empty q", "?q=", "invalid_request"},
		{"limit zero", "?q=samus&limit=0", "invalid_request"},
		{"limit over the cap", "?q=samus&limit=51", "invalid_request"},
		{"limit not a number", "?q=samus&limit=lots", "invalid_request"},
		{"unknown mode", "?q=character:mika&mode=substring", "invalid_request"},
	} {
		resp, body := doJSON(t, "GET", srv.URL+"/api/v1/ptr/tags/search"+tc.query, "", "")
		if resp.StatusCode != http.StatusBadRequest || body["code"] != tc.code {
			t.Errorf("%s = %d %v, want 400 %s", tc.name, resp.StatusCode, body, tc.code)
		}
	}
}

func TestPTRTagSearchFoldsClustersSharingAName(t *testing.T) {
	stub := &stubPTR{enabled: true, searchOut: []ptr.TagMatch{
		{Ideal: "character:hatsune miku (nt)", Matched: []string{"character:hatsune miku (nt)"}, Aliases: 0},
		{Ideal: "character:hatsune_miku_(nt)", Matched: []string{"character:hatsune_miku_(nt)"}, Aliases: 2},
	}}
	srv := newPTRServer(t, "", stub)
	body := wantJSON(t, "GET", srv.URL+"/api/v1/ptr/tags/search?q=character:hatsune_miku", "", http.StatusOK)
	clusters, _ := body["clusters"].([]any)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %v, want the two spellings folded into one row", clusters)
	}
	row, _ := clusters[0].(map[string]any)
	if row["ideal"] != "character:hatsune_miku_(nt)" || row["aliases"] != float64(2) {
		t.Errorf("folded row = %v, want the name once with the larger count", row)
	}
	if matched, _ := row["matched"].([]any); len(matched) != 1 {
		t.Errorf("matched = %v, want the one monbooru spelling", matched)
	}
	if body["truncated"] != false {
		t.Errorf("truncated = %v, want false; folding is not a cut", body["truncated"])
	}
}

func TestPTRTagSearchLimitReportsTruncation(t *testing.T) {
	stub := &stubPTR{enabled: true, searchOut: []ptr.TagMatch{
		{Ideal: "character:samus", Matched: []string{"character:samus"}},
		{Ideal: "character:samus fox", Matched: []string{"character:samus fox"}},
	}}
	srv := newPTRServer(t, "", stub)
	body := wantJSON(t, "GET", srv.URL+"/api/v1/ptr/tags/search?q=character:samus&limit=1", "", http.StatusOK)
	if clusters, _ := body["clusters"].([]any); len(clusters) != 1 {
		t.Errorf("clusters = %v, want the limit applied", clusters)
	}
	if body["truncated"] != true {
		t.Errorf("truncated = %v, want true when the limit cut the answer", body["truncated"])
	}
}

func TestPTRTagsDropsSelfAlias(t *testing.T) {

	srv := newPTRServer(t, "", &stubPTR{enabled: true, graph: map[string]ptr.TagInfo{
		"blue_eyes": {Known: true, Ideal: "blue_eyes", Aliases: []string{"blue eyes", "blue eye"}},
	}})
	body := wantJSON(t, "POST", srv.URL+"/api/v1/ptr/tags", `{"tags":["blue_eyes"]}`, http.StatusOK)
	be := body["results"].(map[string]any)["blue_eyes"].(map[string]any)
	for _, a := range be["aliases"].([]any) {
		if a == "blue_eyes" {
			t.Errorf("aliases %v list the queried tag itself", be["aliases"])
		}
	}
}

func TestPTRTagsPrefersConnectedSpelling(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, graph: map[string]ptr.TagInfo{
		"one_female": {Known: true, Ideal: "one_female"},
		"one female": {Known: true, Ideal: "1 female", Aliases: []string{"1 girl"}},
		"lone_tag":   {Known: true, Ideal: "lone_tag"},
	}})
	body := wantJSON(t, "POST", srv.URL+"/api/v1/ptr/tags", `{"tags":["one_female","lone_tag"]}`, http.StatusOK)
	results, _ := body["results"].(map[string]any)
	of, _ := results["one_female"].(map[string]any)
	if of["known"] != true || of["ideal"] != "1_female" {
		t.Errorf("one_female result = %v, want the connected spelling's ideal 1_female", of)
	}
	if aliases, _ := of["aliases"].([]any); len(aliases) != 1 || aliases[0] != "1_girl" {
		t.Errorf("one_female aliases = %v, want [1_girl]", of["aliases"])
	}

	if lt, _ := results["lone_tag"].(map[string]any); lt["known"] != true || lt["ideal"] != "lone_tag" {
		t.Errorf("lone_tag result = %v, want known orphan", lt)
	}
}

func TestPTRTagsDroppedNamespaceIsNotKnown(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, graph: map[string]ptr.TagInfo{
		"title:some_title": {Known: true, Ideal: "title:some_title"},
		"title:some title": {Known: true, Ideal: "title:some title"},
	}})
	body := wantJSON(t, "POST", srv.URL+"/api/v1/ptr/tags", `{"tags":["title:some_title"]}`, http.StatusOK)
	got, _ := body["results"].(map[string]any)["title:some_title"].(map[string]any)
	if got["known"] != false {
		t.Errorf("dropped-namespace tag = %v, want known=false", got)
	}
}

func TestPTRTagsDisabled(t *testing.T) {
	srv := newTestServer(t, "")
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/tags", `{"tags":["x"]}`, http.StatusConflict, "ptr_unavailable")
}

func TestPTRTagsValidation(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, graph: map[string]ptr.TagInfo{}})
	for _, tc := range []struct {
		name, body string
	}{
		{"empty", `{"tags":[]}`},
		{"too many", `{"tags":[` + strings.Repeat(`"x",`, 500) + `"y"]}`},
		{"bad json", `{`},
	} {
		resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/ptr/tags", "", tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
	}
}

func TestPTRLookupBatch(t *testing.T) {
	hit := strings.Repeat("a", 64)
	miss := strings.Repeat("b", 64)
	srv := newPTRServer(t, "", &stubPTR{enabled: true, raws: map[string][]string{
		hit: {"blue eyes", "creator:some artist"},
	}, status: ptr.Status{Progress: ptr.Progress{UpdateIndex: 4812}}})

	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/ptr/lookup", "",
		`{"gallery":"g","scheduled":true,"images":[{"image_id":7,"sha256":"`+hit+`"},{"image_id":8,"sha256":"`+miss+`"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", resp.StatusCode, body)
	}
	if body["index"] != float64(4812) {
		t.Errorf("index = %v, want the store cursor 4812", body["index"])
	}
	results, _ := body["results"].(map[string]any)
	if _, present := results[miss]; present {
		t.Errorf("results = %v, want the unmatched hash absent", results)
	}
	tags, _ := results[hit].([]any)
	if len(tags) != 2 || tags[0] != "artist:some_artist" || tags[1] != "blue_eyes" {
		t.Errorf("tags = %v, want the hydrus spellings mapped to monbooru form", tags)
	}

	_, list := doJSON(t, "GET", srv.URL+"/api/v1/queue", "", "")
	jobs, _ := list["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("history holds %d jobs, want the sweep filed", len(jobs))
	}
	job, _ := jobs[0].(map[string]any)
	items, _ := job["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want one per matched image", items)
	}
	it, _ := items[0].(map[string]any)
	if it["monbooru_id"] != float64(7) || it["outcome"] != "matched" {
		t.Errorf("item = %v, want the answered image marked matched, not enriched", it)
	}
}

func TestPTRLookupBatchDropsUncheckedCategories(t *testing.T) {
	hit := strings.Repeat("a", 64)
	cfg := testCfg("")
	cfg.Tags.DropCategories = []string{"artist"}
	srv, _ := serveCfgTracked(t, cfg, &stubPTR{enabled: true, raws: map[string][]string{
		hit: {"blue eyes", "creator:some artist"},
	}})

	_, body := doJSON(t, "POST", srv.URL+"/api/v1/ptr/lookup", "", `{"images":[{"image_id":7,"sha256":"`+hit+`"}]}`)
	results, _ := body["results"].(map[string]any)
	tags, _ := results[hit].([]any)
	if len(tags) != 1 || tags[0] != "blue_eyes" {
		t.Errorf("tags = %v, want the artist tag dropped as a queued lookup drops it", tags)
	}
}

func TestPTRLookupBatchNamesWhatItHeldBack(t *testing.T) {
	hit := strings.Repeat("a", 64)
	cfg := testCfg("")
	cfg.Tags.DropCategories = []string{"artist"}
	mapper, err := mapping.New(config.NewProvider(cfg), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := mapper.SaveUserProfile(config.SitePTR, mapping.Profile{
		Family: mapping.FamilyGeneric, Kind: mapping.KindOther, Auth: mapping.AuthNone,
		CategoryOverrides: map[string]string{"species": ""},
	}); err != nil {
		t.Fatal(err)
	}
	q := queue.New(fakeProc{}, 1, 100)
	t.Cleanup(q.Close)
	h := New(q, nil, mapper, config.NewProvider(cfg), nil, "", nil, &stubPTR{enabled: true, raws: map[string][]string{
		hit: {"blue eyes", "species:cat", "creator:some artist"},
	}}, Host{})

	w := httptest.NewRecorder()
	h.ptrLookup(w, httptest.NewRequest("POST", "/api/v1/ptr/lookup", strings.NewReader(`{"images":[{"image_id":7,"sha256":"`+hit+`"}]}`)))
	var out contract.PTRLookupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.Results[hit]; !slices.Equal(got, []string{"blue_eyes"}) {
		t.Errorf("results = %v, want the tag that lands", got)
	}
	if got := out.WithheldTags[hit]; !slices.Equal(got, []string{"species:cat"}) {
		t.Errorf("withheld tags = %v, want the row's dropped category named", got)
	}
	if !slices.Equal(out.WithheldCategories, []string{"artist"}) {
		t.Errorf("withheld categories = %v, want Keep categories named", out.WithheldCategories)
	}
}

func TestPTRLookupBatchLeavesOutAHashWithNoUsableTags(t *testing.T) {
	sha := strings.Repeat("a", 64)
	srv := newPTRServer(t, "", &stubPTR{enabled: true, raws: map[string][]string{sha: {"title:bookkeeping"}}})
	out := postOK(t, srv.URL+"/api/v1/ptr/lookup", `{"images":[{"image_id":1,"sha256":"`+sha+`"}]}`, "lookup")
	if results, _ := out["results"].(map[string]any); len(results) != 0 {
		t.Errorf("results = %v, want the hash absent", results)
	}
}

func TestPTRLookupBatchLeavesOutABlacklistedAnswer(t *testing.T) {
	hit, skipped := strings.Repeat("a", 64), strings.Repeat("b", 64)
	cfg := testCfg("")
	cfg.Sites = append(cfg.Sites, config.Site{Name: config.SitePTR, Blacklist: []string{"bad"}})
	srv, _ := serveCfgTracked(t, cfg, &stubPTR{enabled: true, raws: map[string][]string{
		hit:     {"blue eyes"},
		skipped: {"blue eyes", "bad"},
	}})

	_, body := doJSON(t, "POST", srv.URL+"/api/v1/ptr/lookup", "",
		`{"images":[{"image_id":7,"sha256":"`+hit+`"},{"image_id":8,"sha256":"`+skipped+`"}]}`)
	results, _ := body["results"].(map[string]any)
	if _, present := results[skipped]; present {
		t.Errorf("results = %v, want the blacklisted hash absent", results)
	}
	if _, present := results[hit]; !present {
		t.Errorf("results = %v, want the other hash answered", results)
	}
}

func TestPTRLookupStopsWhenTheCallerGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &stubPTR{enabled: true, onLookup: cancel}
	srv := newPTRServer(t, "", stub)

	var images []string
	for i := 0; i < 20; i++ {
		images = append(images, fmt.Sprintf(`{"image_id":%d,"sha256":"%s"}`, i+1, strings.Repeat("a", 63)+strconv.Itoa(i%10)))
	}
	body := `{"images":[` + strings.Join(images, ",") + `]}`
	req := httptest.NewRequest("POST", "/api/v1/ptr/lookup", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiTestToken)
	srv.Config.Handler.ServeHTTP(httptest.NewRecorder(), req)

	if stub.lookups != 1 {
		t.Errorf("walked %d images, want the batch to stop at the first boundary after the cancel", stub.lookups)
	}
}

func TestPTRLookupRefusals(t *testing.T) {
	sha := strings.Repeat("a", 64)
	srv := newTestServer(t, "")
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/lookup", `{"images":[{"sha256":"`+sha+`"}]}`, http.StatusConflict, "ptr_unavailable")

	srv = newPTRServer(t, "", &stubPTR{enabled: true, syncing: true})
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/lookup", `{"images":[{"sha256":"`+sha+`"}]}`, http.StatusConflict, "ptr_syncing")

	srv = newPTRServer(t, "", &stubPTR{enabled: true})
	for _, tc := range []struct {
		name, body string
	}{
		{"empty", `{"images":[]}`},
		{"too many", `{"images":[` + strings.Repeat(`{"sha256":"`+sha+`"},`, 100) + `{"sha256":"` + sha + `"}]}`},
		{"malformed hash", `{"images":[{"sha256":"abc"}]}`},
		{"bad json", `{`},
		{"missing image_id", `{"images":[{"sha256":"` + sha + `"}]}`},
		{"negative image_id", `{"images":[{"image_id":-1,"sha256":"` + sha + `"}]}`},
	} {
		resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/ptr/lookup", "", tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
	}
}

func TestLookupPTRBackendLiveWhenEnabled(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true})
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":1,"backend":"ptr","sha256":"`+strings.Repeat("a", 64)+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("enabled ptr lookup = %d, want 202: %v", resp.StatusCode, body)
	}
}

func TestPTRReadsRefusedWhileSyncing(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, syncing: true, graph: map[string]ptr.TagInfo{}})
	sha := strings.Repeat("a", 64)

	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":1,"backend":"ptr","sha256":"`+sha+`"}`)
	if resp.StatusCode != http.StatusConflict || body["code"] != "ptr_syncing" {
		t.Errorf("ptr lookup while syncing = %d %v, want 409 ptr_syncing", resp.StatusCode, body)
	}
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/tags", `{"tags":["x"]}`, http.StatusConflict, "ptr_syncing")
	wantCode(t, "GET", srv.URL+"/api/v1/ptr/tags/search?q=x", "", http.StatusConflict, "ptr_syncing")
	resp, body = doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":1,"backend":"all","md5":"0123456789abcdef0123456789abcdef","sha256":"`+sha+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("all lookup while syncing = %d %v, want 202", resp.StatusCode, body)
	}
}

func TestPTRAccountCreateEndpoint(t *testing.T) {
	srv := newTestServer(t, "")
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/account", "", http.StatusConflict, "ptr_unavailable")

	srv2 := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true})
	wantCode(t, "POST", srv2.URL+"/api/v1/ptr/account", "", http.StatusConflict, "account_exists")

	srv3, _ := serveSavingConfig(t, testCfg(""), &stubPTR{enabled: true, createErr: &ptr.ServerError{Status: 400, Message: "no accounts available"}})
	wantCode(t, "POST", srv3.URL+"/api/v1/ptr/account", "", http.StatusServiceUnavailable, "ptr_velocity")

	srv5, provider := serveSavingConfig(t, testCfg(""), &stubPTR{enabled: true, createdKey: "cafe1234"})
	resp, body := doJSON(t, "POST", srv5.URL+"/api/v1/ptr/account", "", "")
	if resp.StatusCode != http.StatusOK || body["created"] != true {
		t.Errorf("create = %d %v, want 200 created", resp.StatusCode, body)
	}
	if got := provider.Current().PTR.AccessKey; got != "cafe1234" {
		t.Errorf("saved access key = %q, want the created one", got)
	}
}

func TestPTRStatusCarriesContrib(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, status: ptr.Status{
		Enabled: true, State: ptr.StateReady,
		Contrib: &ptr.ContribStatus{Account: true, Banned: false},
	}})
	body := wantJSON(t, "GET", srv.URL+"/api/v1/ptr/status", "", http.StatusOK)
	contrib, ok := body["contrib"].(map[string]any)
	if !ok || contrib["account"] != true || contrib["banned"] != false {
		t.Errorf("contrib = %v, want account true", body["contrib"])
	}
}

func TestPTRContribPreview(t *testing.T) {
	h := h64("aa")
	store := contribStore(t)

	if _, _, err := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "waiting tag", Hash: rawHash("aa")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingPetition, Tag: "hat", Hash: rawHash("aa"), Reason: "wrong"}); err != nil {
		t.Fatal(err)
	}
	srv := newPTRServer(t, "", &stubPTR{
		enabled: true,
		contrib: store,
		tagFilter: ptr.NewTagFilter(map[string]int{
			"artist request": 1,
		}),
		ideals: map[string]map[string]bool{h: {"blonde hair": true}},
		raws:   map[string][]string{h: {"hat", "smile", "species:fox", "booru:danbooru", "blond hair", "headwear"}},
		idealTags: map[string]string{
			"blond hair":  "blonde hair",
			"blonde hair": "blonde hair",
			"headwear":    "headwear",
			"hat ribbon":  "hat ribbon",
		},
	})
	body := `{"sha256":"` + h + `","tags":["blonde_hair","new_tag","hat_ribbon","waiting_tag","year:2024","artist_request","species:fox"],"implied":["headwear"]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", body, "preview")
	toAdd := out["to_add"].([]any)
	byTag := map[string]map[string]any{}
	for _, e := range toAdd {
		m := e.(map[string]any)
		byTag[m["tag"].(string)] = m
	}
	wantStatus := map[string]string{
		"blonde_hair":    "known",
		"new_tag":        "new",
		"hat_ribbon":     "new",
		"waiting_tag":    "unsent",
		"year:2024":      "ineligible",
		"artist_request": "filtered",
		"species:fox":    "known",
	}
	for tag, want := range wantStatus {
		if got := byTag[tag]["status"]; got != want {
			t.Errorf("to_add[%s].status = %v, want %s", tag, got, want)
		}
	}
	if byTag["blonde_hair"]["ptr"] != "blonde hair" {
		t.Errorf("ptr spelling = %v, want the exact PTR form", byTag["blonde_hair"]["ptr"])
	}

	if byTag["new_tag"]["unknown_tag"] != true {
		t.Errorf("new_tag.unknown_tag = %v, want true", byTag["new_tag"]["unknown_tag"])
	}
	if _, ok := byTag["hat_ribbon"]["unknown_tag"]; ok {
		t.Errorf("hat_ribbon carries unknown_tag = %v, want absent", byTag["hat_ribbon"]["unknown_tag"])
	}

	ptrOnly := out["ptr_only"].([]any)
	byPTROnly := map[string]map[string]any{}
	for _, e := range ptrOnly {
		m := e.(map[string]any)
		byPTROnly[m["tag"].(string)] = m
	}
	if _, ok := byPTROnly["species:fox"]; ok {
		t.Error("submitted species:fox offered for petition")
	}
	if _, ok := byPTROnly["blond_hair"]; ok {
		t.Error("alias spelling of a submitted tag offered for petition")
	}
	if _, ok := byPTROnly["headwear"]; ok {
		t.Error("implied context tag offered for petition")
	}
	if _, ok := byTag["headwear"]; ok {
		t.Error("implied context tag offered as an add")
	}
	if m := byPTROnly["hat"]; m == nil || m["petitionable"] != false {
		t.Errorf("hat = %v, want petitionable false (petition staged)", m)
	}
	if m := byPTROnly["smile"]; m == nil || m["petitionable"] != true {
		t.Errorf("smile = %v, want petitionable true", m)
	}
}

func TestPTRContribPreviewRedundantSiblingNotOffered(t *testing.T) {

	h := h64("cc")
	srv := newPTRServer(t, "", &stubPTR{
		enabled: true,
		ideals:  map[string]map[string]bool{h: {}},
		raws:    map[string][]string{h: {"catt", "character:cat", "=_="}},
		idealTags: map[string]string{
			"catt":          "character:cat",
			"character:cat": "character:cat",
		},
	})
	body := `{"sha256":"` + h + `","tags":[]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", body, "preview")
	byPTROnly := map[string]map[string]any{}
	for _, e := range out["ptr_only"].([]any) {
		m := e.(map[string]any)
		byPTROnly[m["tag"].(string)] = m
	}
	if _, ok := byPTROnly["catt"]; ok {
		t.Error("redundant bad-sibling catt offered for petition")
	}
	if m := byPTROnly["=_="]; m == nil || m["petitionable"] != true {
		t.Errorf("=_= = %v, want offered (sole carrier of its ideal)", m)
	}
}

func TestPTRContribPreviewPulledIdealNotOffered(t *testing.T) {
	h := h64("cf")
	ideal := `character:mizuno "sailor mercury" ami`
	srv := newPTRServer(t, "", &stubPTR{
		enabled:   true,
		ideals:    map[string]map[string]bool{h: {}},
		raws:      map[string][]string{h: {"sailor mercury"}},
		idealTags: map[string]string{"sailor mercury": ideal},
	})

	body := `{"sha256":"` + h + `","tags":["character:mizuno_sailor_mercury_ami"]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", body, "preview")
	for _, e := range out["ptr_only"].([]any) {
		if m := e.(map[string]any); m["tag"] == "sailor_mercury" {
			t.Errorf("pulled bad-sibling still offered for petition: %v", m)
		}
	}
}

func TestPTRContribPreviewCoCarrierBadSiblingsOffered(t *testing.T) {
	h := h64("cd")
	srv := newPTRServer(t, "", &stubPTR{
		enabled: true,
		ideals:  map[string]map[string]bool{h: {}},
		raws:    map[string][]string{h: {"clr1", "clr2"}},
		idealTags: map[string]string{
			"clr1": "color",
			"clr2": "color",
		},
	})
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", `{"sha256":"`+h+`","tags":[]}`, "preview")
	offered := map[string]bool{}
	for _, e := range out["ptr_only"].([]any) {
		m := e.(map[string]any)
		offered[m["tag"].(string)] = m["petitionable"] == true
	}
	if !offered["clr1"] || !offered["clr2"] {
		t.Errorf("co-carrier bad siblings = %v, want both offered", offered)
	}
}

func TestPTRContribPreviewDropsUnpetitionableRaws(t *testing.T) {
	h := h64("ce")
	srv := newPTRServer(t, "", &stubPTR{
		enabled: true,
		ideals:  map[string]map[string]bool{h: {}},
		raws:    map[string][]string{h: {"pixiv", "rating:safe", "smile"}},
		idealTags: map[string]string{
			"pixiv":       "source:pixiv",
			"rating:safe": "rating:safe",
		},
	})
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", `{"sha256":"`+h+`","tags":[]}`, "preview")
	offered := map[string]bool{}
	for _, e := range out["ptr_only"].([]any) {
		offered[e.(map[string]any)["tag"].(string)] = true
	}
	if offered["pixiv"] {
		t.Error("pixiv offered though its ideal is unrepresentable")
	}
	if offered["rating:safe"] {
		t.Error("rating mapping offered though the add side refuses it")
	}
	if !offered["smile"] {
		t.Error("a normal raw should still be offered")
	}
}

func TestPTRContribPreviewLossyPulledTagIsKnown(t *testing.T) {

	h := h64("bb")
	srv := newPTRServer(t, "", &stubPTR{
		enabled: true,
		ideals:  map[string]map[string]bool{h: {}},
		raws:    map[string][]string{h: {"series:girls' frontline 2: exilium"}},
	})
	body := `{"sha256":"` + h + `","tags":["copyright:girls_frontline_2:_exilium"]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", body, "preview")
	toAdd := out["to_add"].([]any)
	if len(toAdd) != 1 {
		t.Fatalf("to_add = %v, want one entry", toAdd)
	}
	if st := toAdd[0].(map[string]any)["status"]; st != "known" {
		t.Errorf("pulled lossy tag status = %v, want known (already on the PTR)", st)
	}
	if po := out["ptr_only"].([]any); len(po) != 0 {
		t.Errorf("ptr_only = %v, want empty (the tag is carried)", po)
	}
}

func TestPTRContribPreviewWidenedSpellingExactKnown(t *testing.T) {
	h := h64("bc")
	srv := newPTRServer(t, "", &stubPTR{
		enabled: true,
		ideals:  map[string]map[string]bool{h: {"series:girls' frontline 2: exilium": true}},
	})
	body := `{"sha256":"` + h + `","tags":["copyright:girls'_frontline_2:_exilium"]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", body, "preview")
	toAdd := out["to_add"].([]any)
	if len(toAdd) != 1 {
		t.Fatalf("to_add = %v, want one entry", toAdd)
	}
	entry := toAdd[0].(map[string]any)
	if entry["status"] != "known" || entry["ptr"] != "series:girls' frontline 2: exilium" {
		t.Errorf("widened pulled tag = %v, want known with the raw spelling", entry)
	}
}

func TestPTRContribPreviewUserApostropheSendsIntact(t *testing.T) {
	h := h64("bd")
	srv := newPTRServer(t, "", &stubPTR{enabled: true, ideals: map[string]map[string]bool{h: {}}})
	body := `{"sha256":"` + h + `","tags":["copyright:girls'_frontline"]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", body, "preview")
	toAdd := out["to_add"].([]any)
	if len(toAdd) != 1 {
		t.Fatalf("to_add = %v, want one entry", toAdd)
	}
	entry := toAdd[0].(map[string]any)
	if entry["status"] != "new" || entry["ptr"] != "series:girls' frontline" {
		t.Errorf("user apostrophe tag = %v, want new with the apostrophe intact", entry)
	}
}

func TestPTRContribPreviewGates(t *testing.T) {
	srv := newTestServer(t, "")
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/contrib/preview", `{"sha256":"`+h64("aa")+`","tags":[]}`, http.StatusConflict, "ptr_unavailable")

	srv2 := newPTRServer(t, "", &stubPTR{enabled: true, syncing: true, provisional: true})
	resp, out := doJSON(t, "POST", srv2.URL+"/api/v1/ptr/contrib/preview", "", `{"sha256":"`+h64("aa")+`","tags":["x"]}`)
	if resp.StatusCode != http.StatusOK || out["provisional"] != true {
		t.Errorf("syncing = %d %v, want provisional true", resp.StatusCode, out)
	}

	wantCode(t, "POST", srv2.URL+"/api/v1/ptr/contrib/preview", `{"sha256":"nothex","tags":[]}`, http.StatusBadRequest, "")
}

func h64(seed string) string {
	return seed + strings.Repeat("0", 64-len(seed))
}

func rawHash(seed string) []byte {
	raw, _ := hex.DecodeString(h64(seed))
	return raw
}

func TestPTRContribPreviewReadScope(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BaseURL = "http://localhost:8081"
	cfg.Auth.Tokens = []apitoken.Token{{ID: "ro", Name: "ro", TokenHash: apitoken.Hash("ro-token"), Scopes: []string{config.ScopeRead}}}
	srv, _ := serveCfgTracked(t, cfg, &stubPTR{enabled: true})
	resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/preview", "ro-token", `{"sha256":"`+h64("aa")+`","tags":["x"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("preview with read-only token = %d, want 200", resp.StatusCode)
	}
}

func TestPTRContribStageResults(t *testing.T) {
	store := contribStore(t)

	sibID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "blond hair", Tag2: "blonde hair", Reason: "misspelling"})
	claimed, _ := store.ClaimIDs([]int64{sibID})
	_, _ = store.Complete(claimed[0])

	h := h64("aa")
	srv := newPTRServer(t, "", &stubPTR{
		enabled: true, hasKey: true, contrib: store,
		status:   ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}},
		ideals:   map[string]map[string]bool{h: {"known tag": true}},
		raws:     map[string][]string{h: {"hat"}},
		siblings: map[[2]string]bool{{"good tag", "bad tag"}: true, {"artist:held b", "artist:held a"}: true},
	})
	body := `{"commit": false, "origin": "image 42", "items": [
		{"kind": "mapping_add", "sha256": "` + h + `", "tag": "new_tag"},
		{"kind": "mapping_add", "sha256": "` + h + `", "tag": "known_tag"},
		{"kind": "mapping_add", "sha256": "` + h + `", "tag": "year:2024"},
		{"kind": "mapping_add", "sha256": "` + h + `", "tag": "new_tag"},
		{"kind": "mapping_petition", "sha256": "` + h + `", "tag": "hat"},
		{"kind": "mapping_petition", "sha256": "` + h + `", "tag": "hat", "reason": "wrong"},
		{"kind": "mapping_petition", "sha256": "` + h + `", "tag": "smile", "reason": "wrong"},
		{"kind": "sibling", "bad": "bad_tag", "good": "good_tag", "reason": "misspelling"},
		{"kind": "sibling", "bad": "blond_hair", "good": "blonde_hair", "reason": "misspelling"},
		{"kind": "parent", "child": "miko", "parent": "shrine_maiden", "reason": "canonical"},
		{"kind": "sibling", "bad": "foo_bar", "good": "foo bar", "reason": "same"},
		{"kind": "parent", "child": "zed", "parent": "zed", "reason": "same"},
		{"kind": "sibling", "bad": "artist:held_a", "good": "artist:held_b", "reason": "reversed"}
	]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	results := out["results"].([]any)
	want := []string{
		"staged",
		"already_known",
		"ineligible",
		"duplicate",
		"invalid_reason",
		"staged",
		"not_on_ptr",
		"conflict",
		"already_suggested",
		"staged",
		"ineligible",
		"ineligible",
		"conflict",
	}
	for i, w := range want {
		got := results[i].(map[string]any)["result"]
		if got != w {
			t.Errorf("results[%d] = %v, want %s", i, got, w)
		}
	}
	if _, ok := out["job_id"]; ok {
		t.Error("commit:false returned a job_id")
	}
}

func TestPTRContribStageCommitGates(t *testing.T) {
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: false, status: ptr.Status{Enabled: true}})
	body := `{"items": [{"kind": "mapping_add", "sha256": "` + h64("aa") + `", "tag": "x"}]}`
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/contrib", body, http.StatusConflict, "ptr_account_required")

	srv2 := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, refreshBanned: true,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}}})
	wantCode(t, "POST", srv2.URL+"/api/v1/ptr/contrib", body, http.StatusConflict, "ptr_banned")
}

func TestPTRContribStageCommitReturnsJob(t *testing.T) {
	store := contribStore(t)
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}}})
	body := `{"origin": "image 7", "items": [{"kind": "mapping_add", "sha256": "` + h64("aa") + `", "tag": "fresh_tag"}]}`
	out := wantJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib", body, http.StatusAccepted)
	if _, ok := out["job_id"].(float64); !ok {
		t.Errorf("job_id missing: %v", out)
	}
}

func TestContribStageStagesNothingForACancelledRequest(t *testing.T) {
	store := contribStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store, onRefresh: cancel,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}}})
	body := `{"items": [{"kind": "mapping_add", "sha256": "` + h64("aa") + `", "tag": "fresh_tag"}]}`
	r := httptest.NewRequestWithContext(ctx, "POST", "/api/v1/ptr/contrib", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+apiTestToken)
	r.Header.Set("Content-Type", "application/json")
	srv.Config.Handler.ServeHTTP(httptest.NewRecorder(), r)

	if unsent, _, err := store.Counts(); err != nil || unsent != 0 {
		t.Errorf("unsent = %d (err %v), want nothing staged for a request whose sender left", unsent, err)
	}
}

func TestPTRContribCommittedAddDedup(t *testing.T) {
	store := contribStore(t)

	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "fresh tag", Hash: rawHash("aa")})
	claimed, _ := store.ClaimIDs([]int64{id})
	_, _ = store.Complete(claimed[0])

	h := h64("aa")
	stub := &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}}}
	srv := newPTRServer(t, "", stub)

	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", `{"sha256":"`+h+`","tags":["fresh_tag"]}`, "preview")
	entry := out["to_add"].([]any)[0].(map[string]any)
	if entry["status"] != "known" {
		t.Errorf("committed-but-unsynced add status = %v, want known", entry["status"])
	}

	body := `{"commit": false, "items": [{"kind": "mapping_add", "sha256": "` + h + `", "tag": "fresh_tag"}]}`
	out = postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	if got := out["results"].([]any)[0].(map[string]any)["result"]; got != "already_known" {
		t.Errorf("re-stage of a committed add = %v, want already_known", got)
	}

	stub.status.CoveredThrough = time.Now().Unix() + 10
	out = postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage after coverage")
	if got := out["results"].([]any)[0].(map[string]any)["result"]; got != "staged" {
		t.Errorf("re-stage after coverage = %v, want staged", got)
	}
}

func TestPTRContribRefusesAnAddTheRepositoryDidNotTake(t *testing.T) {
	store := contribStore(t)
	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "dropped tag", Hash: rawHash("aa")})
	claimed, _ := store.ClaimIDs([]int64{id})
	logID, _ := store.Complete(claimed[0])
	if err := store.SetOutcome(logID, ptr.OutcomeNotTaken); err != nil {
		t.Fatal(err)
	}

	h := h64("aa")
	stub := &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, CoveredThrough: time.Now().Unix() + 10, Contrib: &ptr.ContribStatus{Account: true}}}
	srv := newPTRServer(t, "", stub)

	out := postOK(t, srv.URL+"/api/v1/ptr/contrib/preview", `{"sha256":"`+h+`","tags":["dropped_tag"]}`, "preview")
	entry := out["to_add"].([]any)[0].(map[string]any)
	if entry["status"] != "ineligible" || entry["note"] == nil {
		t.Errorf("preview of a dropped add = %v, want ineligible with a note", entry)
	}

	body := `{"commit": false, "items": [{"kind": "mapping_add", "sha256": "` + h + `", "tag": "dropped_tag"}]}`
	out = postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	if got := out["results"].([]any)[0].(map[string]any)["result"]; got != "ineligible" {
		t.Errorf("re-stage of a dropped add = %v, want ineligible", got)
	}
}

func TestPTRContribPetitionRawForms(t *testing.T) {
	store := contribStore(t)
	h := h64("aa")
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}},
		raws:   map[string][]string{h: {"under_scored", "rating:questionable", "heart symbol"}},
	})
	body := `{"commit": false, "items": [
		{"kind": "mapping_petition", "sha256": "` + h + `", "tag": "under_scored", "reason": "wrong"},
		{"kind": "mapping_petition", "sha256": "` + h + `", "tag": "rating:questionable", "reason": "wrong"},
		{"kind": "mapping_petition", "sha256": "` + h + `", "tag": "heart_symbol", "reason": "wrong"}
	]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	for i, want := range []string{"staged", "staged", "staged"} {
		if got := out["results"].([]any)[i].(map[string]any)["result"]; got != want {
			t.Errorf("results[%d] = %v, want %s", i, got, want)
		}
	}
	unsent, _ := store.Unsent()
	got := []string{unsent[0].Tag, unsent[1].Tag, unsent[2].Tag}
	want := []string{"under_scored", "rating:questionable", "heart symbol"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unsent[%d].Tag = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPTRContribPetitionSecondaryNamespace(t *testing.T) {
	store := contribStore(t)
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status:  ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}},
		parents: map[[2]string]bool{{"johnny", "artist:john doe"}: true},
	})
	body := `{"commit": false, "items": [
		{"kind": "parent_petition", "child": "johnny", "parent": "artist:john_doe", "reason": "wrong"}
	]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	if got := out["results"].([]any)[0].(map[string]any)["result"]; got != "staged" {
		t.Fatalf("result = %v, want staged", got)
	}
	unsent, _ := store.Unsent()
	if len(unsent) != 1 || unsent[0].Tag2 != "artist:john doe" {
		t.Errorf("unsent = %+v, want the secondary-namespace raw", unsent)
	}
}

func TestPTRContribPetitionRawPairForms(t *testing.T) {
	store := contribStore(t)

	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status:   ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}},
		siblings: map[[2]string]bool{{"marisa_kirisame", "character:kirisame marisa"}: true},
		parents:  map[[2]string]bool{{"koakuma", "series:touhou"}: true},
	})
	body := `{"commit": false, "items": [
		{"kind": "sibling_petition", "bad": "marisa_kirisame", "good": "character:kirisame_marisa", "reason": "wrong"},
		{"kind": "parent_petition", "child": "koakuma", "parent": "copyright:touhou", "reason": "wrong"},
		{"kind": "sibling_petition", "bad": "not_an_alias", "good": "character:kirisame_marisa", "reason": "wrong"}
	]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	results := out["results"].([]any)
	for i, want := range []string{"staged", "staged", "not_on_ptr"} {
		if got := results[i].(map[string]any)["result"]; got != want {
			t.Errorf("results[%d] = %v, want %s", i, got, want)
		}
	}
	unsent, _ := store.Unsent()
	if len(unsent) != 2 {
		t.Fatalf("unsent rows = %d, want 2", len(unsent))
	}
	if unsent[0].Tag != "marisa_kirisame" || unsent[0].Tag2 != "character:kirisame marisa" {
		t.Errorf("sibling petition raw = %q -> %q", unsent[0].Tag, unsent[0].Tag2)
	}
	if unsent[1].Tag != "koakuma" || unsent[1].Tag2 != "series:touhou" {
		t.Errorf("parent petition raw = %q => %q", unsent[1].Tag, unsent[1].Tag2)
	}
}

func TestPTRContribSuggestKnownRawForms(t *testing.T) {
	store := contribStore(t)

	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status:   ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}},
		siblings: map[[2]string]bool{{"marisa_kirisame", "character:kirisame marisa"}: true},
	})
	body := `{"commit": false, "items": [
		{"kind": "sibling", "bad": "marisa_kirisame", "good": "character:kirisame_marisa", "reason": "misspelling"}
	]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	if got := out["results"].([]any)[0].(map[string]any)["result"]; got != "already_known" {
		t.Errorf("suggest of a raw-current alias = %v, want already_known", got)
	}
}

func TestPTRContribLedgerAndRescind(t *testing.T) {
	store := contribStore(t)
	id1, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: rawHash("aa"), Origin: "image 42"})
	_, _, _ = store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "x", Tag2: "y", Reason: "r"})
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}}})

	out := wantJSON(t, "GET", srv.URL+"/api/v1/ptr/contrib", "", http.StatusOK)
	if unsent := out["unsent"].([]any); len(unsent) != 2 {
		t.Errorf("unsent = %d, want 2", len(unsent))
	}

	wantCode(t, "DELETE", srv.URL+"/api/v1/ptr/contrib/"+strconv.FormatInt(id1, 10), "", http.StatusNoContent, "")
	resp, out := doJSON(t, "DELETE", srv.URL+"/api/v1/ptr/contrib", "", "")
	if resp.StatusCode != http.StatusOK || out["rescinded"] != float64(1) {
		t.Errorf("rescind all = %d %v", resp.StatusCode, out)
	}
}

func TestPTRContribLogRescind(t *testing.T) {
	store := contribStore(t)
	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: rawHash("aa")})
	sibID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "x", Tag2: "y", Reason: "r"})
	claimed, _ := store.ClaimIDs([]int64{addID, sibID})
	addLogID, _ := store.Complete(claimed[0])
	sibLogID, _ := store.Complete(claimed[1])

	h := h64("aa")
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}},
		raws:   map[string][]string{h: {"a tag"}}})

	wantJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/log/"+strconv.FormatInt(addLogID, 10)+"/rescind", "", http.StatusAccepted)
	row, _ := store.LogRow(addLogID)
	if row.Outcome != ptr.OutcomeApplied {
		t.Errorf("outcome = %q, want applied until the petition is sent", row.Outcome)
	}
	unsent, _ := store.Unsent()
	found := false
	for _, it := range unsent {
		if it.Kind == ptr.ContribMappingPetition && it.Reason == "rescinding my own earlier add" {
			found = true
		}
	}
	if !found {
		t.Errorf("no fixed-reason petition staged: %+v", unsent)
	}

	wantCode(t, "POST", srv.URL+"/api/v1/ptr/contrib/log/"+strconv.FormatInt(sibLogID, 10)+"/rescind", "", http.StatusConflict, "not_rescindable")
}

func TestPTRContribReconfirmFailedResends(t *testing.T) {
	store := contribStore(t)
	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "waiting tag", Hash: rawHash("aa")})
	claimed, _ := store.ClaimIDs([]int64{id})
	if err := store.FailItem(claimed[0].ID, "boom"); err != nil {
		t.Fatal(err)
	}
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}}})

	body := `{"commit": false, "items": [{"kind":"mapping_add","sha256":"` + h64("aa") + `","tag":"waiting_tag"}]}`
	out := postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")
	if got := out["results"].([]any)[0].(map[string]any)["result"]; got != "staged" {
		t.Errorf("re-confirm of a failed item = %v, want staged", got)
	}
	if unsent, _ := store.Unsent(); len(unsent) != 1 {
		t.Errorf("rows = %d, want the single row reused", len(unsent))
	}
}

func TestPTRContribReconfirmTakesTheNewReason(t *testing.T) {
	store := contribStore(t)
	id, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "bad tag", Tag2: "good tag", Reason: "old"})
	claimed, _ := store.ClaimIDs([]int64{id})
	if err := store.FailItem(claimed[0].ID, "boom"); err != nil {
		t.Fatal(err)
	}
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}}})

	body := `{"commit": false, "items": [{"kind":"sibling","bad":"bad_tag","good":"good_tag","reason":"new"}]}`
	postOK(t, srv.URL+"/api/v1/ptr/contrib", body, "stage")

	unsent, err := store.Unsent()
	if err != nil || len(unsent) != 1 || unsent[0].Reason != "new" {
		t.Errorf("unsent = %+v, %v, want the new reason on the single row", unsent, err)
	}
}

func TestPTRContribLogRescindRetriableAfterClear(t *testing.T) {
	store := contribStore(t)
	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: rawHash("aa")})
	claimed, _ := store.ClaimIDs([]int64{addID})
	addLogID, _ := store.Complete(claimed[0])

	h := h64("aa")
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}},
		raws:   map[string][]string{h: {"a tag"}}})
	rescind := func() int {
		resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/log/"+strconv.FormatInt(addLogID, 10)+"/rescind", "", "")
		return resp.StatusCode
	}

	if code := rescind(); code != http.StatusAccepted {
		t.Fatalf("first rescind = %d, want 202", code)
	}

	if code := rescind(); code != http.StatusConflict {
		t.Errorf("second rescind while pending = %d, want 409", code)
	}

	for _, it := range mustUnsentItems(t, store) {
		if it.Kind == ptr.ContribMappingPetition {
			_, _ = store.Rescind(it.ID)
		}
	}
	if code := rescind(); code != http.StatusAccepted {
		t.Errorf("rescind after clear = %d, want 202", code)
	}
}

func mustUnsentItems(t *testing.T, store *ptr.ContribStore) []ptr.ContribItem {
	t.Helper()
	items, err := store.Unsent()
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestPTRContribLogRescindBeforeSync(t *testing.T) {
	store := contribStore(t)
	addID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribMappingAdd, Tag: "a tag", Hash: rawHash("aa")})
	claimed, _ := store.ClaimIDs([]int64{addID})
	addLogID, _ := store.Complete(claimed[0])

	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}}})

	wantJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/log/"+strconv.FormatInt(addLogID, 10)+"/rescind", "", http.StatusAccepted)
	if unsent := mustUnsentItems(t, store); len(unsent) != 1 || unsent[0].Kind != ptr.ContribMappingPetition {
		t.Errorf("unsent = %+v, want the petition staged", unsent)
	}
}

func TestPTRContribCommitAlreadyRunning(t *testing.T) {
	store := contribStore(t)
	_, _, _ = store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "x", Tag2: "y", Reason: "r"})
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateReady, Contrib: &ptr.ContribStatus{Account: true}}})

	if resp, _ := doJSON(t, "POST", srv.URL+"/api/v1/queue/pause", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatal("pause failed")
	}
	wantJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/commit", "", http.StatusAccepted)
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/contrib/commit", "", http.StatusConflict, "already_running")
}

func TestPTRContribPairPreview(t *testing.T) {
	store := contribStore(t)

	sibID, _, _ := store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "pend a", Tag2: "pend b", Reason: "r"})
	claimed, _ := store.ClaimIDs([]int64{sibID})
	_, _ = store.Complete(claimed[0])

	srv := newPTRServer(t, "", &stubPTR{
		enabled: true, hasKey: true, contrib: store,
		status:   ptr.Status{Enabled: true, Contrib: &ptr.ContribStatus{Account: true}},
		siblings: map[[2]string]bool{{"live a", "live b"}: true, {"rev b", "rev a"}: true, {"gender_swap", "gender bender"}: true, {"artist:held b", "artist:held a"}: true},
		parents: map[[2]string]bool{{"child x", "parent y"}: true, {"rev parent", "rev child"}: true, {"artist:held d", "artist:held c"}: true,

			{"sole_female", "1_female"}: true,

			{"gray_hair", "hair"}: true},

		idealTags: map[string]string{"cov a": "cov ideal", "cov b": "cov ideal",
			"orph_a": "orph_a", "orph a": "cov ideal",
			"1girl": "1_female", "1 girl": "1_female", "sole_female": "sole_female",
			"gray_hair": "grey_hair", "grey_hair": "grey_hair", "hair": "hair"},
	})

	cases := []struct {
		kind, a, b, wantDir string
	}{
		{"sibling", "fresh_a", "fresh_b", "suggest"},
		{"sibling", "foo_bar", "foo bar", "ineligible"},
		{"parent", "zed", "zed", "ineligible"},
		{"sibling", "artist:held_a", "artist:held_b", "conflict"},
		{"parent", "artist:held_c", "artist:held_d", "conflict"},
		{"sibling", "live a", "live b", "petition"},
		{"sibling", "pend a", "pend b", "pending"},
		{"sibling", "rev a", "rev b", "conflict"},
		{"parent", "child x", "parent y", "petition"},
		{"parent", "new_child", "new_parent", "suggest"},
		{"parent", "rev child", "rev parent", "conflict"},
		{"parent", "sole_female", "1girl", "covered"},
		{"parent", "grey_hair", "hair", "covered"},
		{"sibling", "year:2024", "fresh_b", "ineligible"},

		{"sibling", "gender_swap", "gender_bender", "petition"},
		{"sibling", "cov_a", "cov_b", "covered"},
		{"sibling", "orph_a", "cov_b", "covered"},
	}
	for _, tc := range cases {
		body := `{"kind":"` + tc.kind + `","a":"` + tc.a + `","b":"` + tc.b + `"}`
		resp, out := doJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/pair-preview", "", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s/%s = %d", tc.kind, tc.a, tc.b, resp.StatusCode)
		}
		if out["direction"] != tc.wantDir {
			t.Errorf("%s %s/%s direction = %v, want %s", tc.kind, tc.a, tc.b, out["direction"], tc.wantDir)
		}
	}

	for _, body := range []string{
		`{"kind":"sibling","a":"character:mika"}`,
		`{"kind":"sibling","a":"","b":"fresh_b"}`,
	} {
		resp, out := doJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/pair-preview", "", body)
		if resp.StatusCode != http.StatusBadRequest || out["code"] != "invalid_request" {
			t.Errorf("%s = %d %v, want 400 invalid_request", body, resp.StatusCode, out)
		}
	}
}

func TestPTRContribCommitRefusedWhileSyncing(t *testing.T) {
	store := contribStore(t)
	_, _, _ = store.Stage(ptr.ContribItem{Kind: ptr.ContribSibling, Tag: "x", Tag2: "y", Reason: "r"})
	srv := newPTRServer(t, "", &stubPTR{enabled: true, hasKey: true, syncing: true, contrib: store,
		status: ptr.Status{Enabled: true, State: ptr.StateSyncing, Contrib: &ptr.ContribStatus{Account: true}}})

	body := `{"items": [{"kind": "mapping_add", "sha256": "` + h64("aa") + `", "tag": "t"}]}`
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/contrib", body, http.StatusConflict, "ptr_syncing")
	wantCode(t, "POST", srv.URL+"/api/v1/ptr/contrib/commit", "", http.StatusConflict, "ptr_syncing")
}

func TestContribPreviewRequiresTags(t *testing.T) {
	h := strings.Repeat("ab", 32)
	srv := newPTRServer(t, "", &stubPTR{enabled: true, status: ptr.Status{Enabled: true, State: ptr.StateReady}})

	resp, out := doJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/preview", "", `{"sha256":"`+h+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", resp.StatusCode, out)
	}
	if out["code"] != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", out["code"])
	}

	resp2, _ := doJSON(t, "POST", srv.URL+"/api/v1/ptr/contrib/preview", "", `{"sha256":"`+h+`","tags":[]}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status = %d for an empty list, want 200", resp2.StatusCode)
	}
}
