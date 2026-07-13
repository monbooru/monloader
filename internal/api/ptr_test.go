package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/ptr"
)

// testPTR is the PTRService the api test server wires in; nil by default so
// most tests exercise the "not built in" path. A ptr test sets it, then resets.
var testPTR PTRService

// stubPTR is a scriptable PTRService for the api tests.
type stubPTR struct {
	enabled bool
	status  ptr.Status
	graph   map[string]ptr.TagInfo
}

func (s *stubPTR) Enabled() bool      { return s.enabled }
func (s *stubPTR) Status() ptr.Status { return s.status }
func (s *stubPTR) TagGraph(names []string) (map[string]ptr.TagInfo, error) {
	return s.graph, nil
}

func withPTR(t *testing.T, s PTRService) {
	testPTR = s
	t.Cleanup(func() { testPTR = nil })
}

func TestPTRStatusDisabledWithoutBackend(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "GET", srv.URL+"/api/v1/ptr/status", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["enabled"] != false || body["state"] != "disabled" {
		t.Errorf("status = %v, want disabled", body)
	}
}

func TestPTRStatusReportsState(t *testing.T) {
	withPTR(t, &stubPTR{enabled: true, status: ptr.Status{Enabled: true, State: ptr.StateSyncing, CoveredThrough: 1769558400}})
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "GET", srv.URL+"/api/v1/ptr/status", "", "")
	if resp.StatusCode != http.StatusOK || body["state"] != "syncing" {
		t.Errorf("status = %d %v, want 200 syncing", resp.StatusCode, body)
	}
	if body["covered_through"] != float64(1769558400) {
		t.Errorf("covered_through = %v, want 1769558400", body["covered_through"])
	}
}

func TestPTRTagsMapsBothDirections(t *testing.T) {
	// The store knows the hydrus spelling "blue eyes" as an alias of "blue_eyes"
	// implying "eyes"; a monbooru "blue_eyes" query resolves through it.
	withPTR(t, &stubPTR{enabled: true, graph: map[string]ptr.TagInfo{
		"blue_eyes": {Known: true, Ideal: "blue_eyes", Aliases: []string{"blue eyes"}, Implications: []string{"eyes"}},
	}})
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/ptr/tags", "", `{"tags":["blue_eyes","missing"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", resp.StatusCode, body)
	}
	results, _ := body["results"].(map[string]any)
	be, _ := results["blue_eyes"].(map[string]any)
	if be["known"] != true || be["ideal"] != "blue_eyes" {
		t.Errorf("blue_eyes result = %v, want known with ideal blue_eyes", be)
	}
	impl, _ := be["implications"].([]any)
	if len(impl) != 1 || impl[0] != "eyes" {
		t.Errorf("implications = %v, want [eyes]", impl)
	}
	if miss, _ := results["missing"].(map[string]any); miss["known"] != false {
		t.Errorf("missing = %v, want known=false", miss)
	}
}

func TestPTRTagsDisabled(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/ptr/tags", "", `{"tags":["x"]}`)
	if resp.StatusCode != http.StatusConflict || body["code"] != "ptr_unavailable" {
		t.Errorf("status = %d code=%v, want 409 ptr_unavailable", resp.StatusCode, body["code"])
	}
}

func TestPTRTagsValidation(t *testing.T) {
	withPTR(t, &stubPTR{enabled: true, graph: map[string]ptr.TagInfo{}})
	srv := newTestServer(t, "")
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

func TestLookupPTRBackendLiveWhenEnabled(t *testing.T) {
	withPTR(t, &stubPTR{enabled: true})
	srv := newTestServer(t, "")
	resp, body := doJSON(t, "POST", srv.URL+"/api/v1/lookup", "",
		`{"image_id":1,"backend":"ptr","sha256":"`+strings.Repeat("a", 64)+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("enabled ptr lookup = %d, want 202: %v", resp.StatusCode, body)
	}
}
