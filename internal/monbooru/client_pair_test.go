package monbooru

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestPairRequestAndStatus(t *testing.T) {
	var gotApp, gotPeer, gotVersion string
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/pair/request":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotApp, _ = body["app"].(string)
			gotPeer, _ = body["peer_token"].(string)
			gotVersion, _ = body["version"].(string)
			_, _ = w.Write([]byte(`{"request_id":"abc","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pair/status":
			_, _ = w.Write([]byte(`{"status":"approved","token":"mb-secret"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	c := testClient(srv.URL, "")

	id, err := c.PairRequest(context.Background(), "ml-secret", "http://ml:8081", "1.2.3", []string{"read", "write"})
	if err != nil || id != "abc" {
		t.Fatalf("PairRequest: id=%q err=%v", id, err)
	}
	if gotApp != "monloader" || gotPeer != "ml-secret" || gotVersion != "1.2.3" {
		t.Errorf("request body: app=%q peer=%q version=%q", gotApp, gotPeer, gotVersion)
	}
	status, token, err := c.PairStatus(context.Background(), "abc")
	if err != nil || status != "approved" || token != "mb-secret" {
		t.Fatalf("PairStatus: status=%q token=%q err=%v", status, token, err)
	}
}

func TestPairRequestRefusesAnAnswerWithNoID(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pair/request" {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		_, _ = w.Write([]byte("<!DOCTYPE html><title>setup</title>"))
	}))

	id, err := testClient(srv.URL, "").PairRequest(context.Background(), "ml-secret", "http://ml:8456", "", []string{"read"})
	if err == nil {
		t.Fatalf("PairRequest returned id=%q and no error; the poll would wait on a request monbooru never filed", id)
	}
}
