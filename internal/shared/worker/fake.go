package worker

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// Fake is a worker for tests: it answers with what its fields hold and
// scores every frame with Score. Its fields are read on each call, so a
// test may change them under Mu.
type Fake struct {
	Mu      sync.Mutex
	Token   string
	Info    Info
	Models  []Model
	Labels  map[string]Labels
	Catalog []CatalogEntry
	// Score answers one frame of one model; nil scores label 0 at 0.9.
	Score func(model string, frame []byte) FrameScores
	// Calls holds every tag call, in order.
	Calls []TagRequest
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	token := f.Token
	f.Mu.Unlock()
	got := BearerToken(r)
	if token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "bad token")
		return
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == PathInfo:
		writeJSON(w, f.Info)
	case r.Method == http.MethodGet && r.URL.Path == PathModels:
		writeJSON(w, f.Models)
	case r.Method == http.MethodGet && r.URL.Path == PathCatalog:
		writeJSON(w, f.Catalog)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, PathModels+"/") && strings.HasSuffix(r.URL.Path, "/labels"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, PathModels+"/"), "/labels")
		l, ok := f.Labels[name]
		if !ok {
			writeError(w, http.StatusNotFound, CodeUnknownModel, "no model "+name)
			return
		}
		if l.ETag != "" && r.Header.Get("If-None-Match") == l.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(w, l)
	case r.Method == http.MethodPost && r.URL.Path == PathTag:
		req, frames, err := ReadTag(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
			return
		}
		f.Calls = append(f.Calls, req)
		var resp TagResponse
		for _, m := range req.Models {
			if !slices.ContainsFunc(f.Models, func(x Model) bool { return x.Name == m }) {
				writeError(w, http.StatusNotFound, CodeUnknownModel, "no model "+m)
				return
			}
			ms := ModelScores{Model: m}
			for _, fr := range frames {
				if f.Score != nil {
					ms.Frames = append(ms.Frames, f.Score(m, fr))
				} else {
					ms.Frames = append(ms.Frames, FrameScores{I: []int32{0}, S: []float32{0.9}})
				}
			}
			resp.Results = append(resp.Results, ms)
		}
		writeJSON(w, resp)
	default:
		http.NotFound(w, r)
	}
}

// writeError answers a refusal in the shape a Client reads.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Error{Error: msg, Code: code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
