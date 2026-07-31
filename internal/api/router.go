package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/sitestate"
)

// PTRService is the PTR surface the API exposes: capability + progress for the
// status endpoint, and the tag-graph query. A nil service means the PTR is not
// built into the run (status reports disabled).
type PTRService interface {
	Status() ptr.Status
	TagGraph(names []string) (map[string]ptr.TagInfo, error)
	Enabled() bool
	HasPersonalKey() bool
	CreateContribAccount(ctx context.Context) (string, error)
	Syncing() bool
	Provisional() bool
	CaughtUp() bool
	Contrib() *ptr.ContribStore
	TagFilterCached() *ptr.TagFilter
	HashHasIdeal(hashHex, tag string) (bool, error)
	HashHasIdeals(hashHex string, tags []string) (map[string]bool, error)
	HashHasRaw(hashHex, tag string) (bool, error)
	RawTagsForHash(hashHex string) ([]string, error)
	IdealTag(tag string) (string, bool, error)
	SiblingCurrent(bad, good string) (bool, error)
	ParentCurrent(child, parent string) (bool, error)
	ParentEdgeCovered(child, parent string) (bool, error)
	RefreshAccount(ctx context.Context) (*ptr.Account, error)
}

// PairHandlers are the extension pairing handshake's three handlers. They live
// in the web layer, which owns the pending-request store and the operator's
// approval screen, but they are declared and mounted here with everything else
// so the handshake a client must speak first is in the reference too.
type PairHandlers struct {
	Request  http.HandlerFunc
	Status   http.HandlerFunc
	Teardown http.HandlerFunc
}

// Handler serves monloader's own /api/v1/ surface.
type Handler struct {
	queue      *queue.Queue
	runner     gdl.Runner
	mapper     *mapping.Mapper
	cfg        *config.Provider
	extractors []gdl.Extractor
	supported  map[string]gdl.SupportedSite
	version    string
	gdlVersion string
	siteState  *sitestate.Tracker
	ptr        PTRService
	pair       PairHandlers
	// saveConfig persists a config mutation through the owner's save
	// path (the web layer's updateConfig); nil in tests that never
	// persist.
	saveConfig func(func(*config.Config) error) error
}

// New builds the API handler. extractors is the cached --list-extractors
// result; version and gdlVersion feed /health; siteState is the shared "last
// reached" tracker the test probe records into; ptr backs the PTR endpoints
// (nil when the PTR is not built into the run); pair carries the web layer's
// pairing handlers.
func New(q *queue.Queue, runner gdl.Runner, mapper *mapping.Mapper, cfg *config.Provider, extractors []gdl.Extractor, supported map[string]gdl.SupportedSite, version, gdlVersion string, siteState *sitestate.Tracker, ptrSvc PTRService, pair PairHandlers, saveConfig func(func(*config.Config) error) error) *Handler {
	return &Handler{
		queue:      q,
		runner:     runner,
		mapper:     mapper,
		cfg:        cfg,
		extractors: extractors,
		supported:  supported,
		version:    version,
		gdlVersion: gdlVersion,
		siteState:  siteState,
		ptr:        ptrSvc,
		pair:       pair,
		saveConfig: saveConfig,
	}
}

// Mount registers every API route on mux, straight from the endpoint
// declarations that also build the OpenAPI document, so a route cannot be
// mounted without documenting it. NoAuth endpoints (/health and the self-doc
// pair) skip the bearer gate; the rest go through it.
func (h *Handler) Mount(mux *http.ServeMux) {
	for _, e := range h.endpoints() {
		fn := e.Handler
		if !e.NoAuth {
			scope := scopeForMethod(e.Method)
			if e.ReadScope {
				scope = config.ScopeRead
			}
			fn = h.auth(scope, fn)
		}
		mux.HandleFunc(e.Method+" "+e.Path, fn)
	}

	// CORS preflight for the future browser extension.
	mux.HandleFunc("OPTIONS /api/v1/", func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r)
		w.WriteHeader(http.StatusNoContent)
	})
}

// auth gates a handler behind a bearer token and per-token scope. With no
// tokens configured the API is disabled (503). CORS headers are set on every
// API response so the extension's origin can call from a browser.
func (h *Handler) auth(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r)
		cfg := h.cfg.Current()
		if len(cfg.Auth.Tokens) == 0 {
			apiError(w, http.StatusServiceUnavailable, "api_disabled", "API is disabled: generate an API token in Settings to enable it")
			return
		}
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, prefix) {
			apiError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid authorization header")
			return
		}
		tok := cfg.FindTokenByHash(config.HashToken(got[len(prefix):]))
		if tok == nil {
			apiError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		if !tok.HasScope(scope) {
			apiError(w, http.StatusForbidden, "insufficient_scope", "token lacks the "+scope+" scope")
			return
		}
		next(w, r)
	}
}

// scopeForMethod maps an HTTP method to the privilege a token must hold: writes
// for POST and the DELETE job-cancel, reads for the rest.
func scopeForMethod(method string) string {
	if method == http.MethodPost || method == http.MethodDelete {
		return config.ScopeWrite
	}
	return config.ScopeRead
}

// setCORS reflects the request Origin so the browser extension (a distinct
// origin) can call the API. On a LAN this permissiveness is acceptable; the
// bearer token, when set, is the real gate.
func setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Vary", "Origin")
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxRequestBody caps a JSON request body, like the pairing endpoint's
// 64 KiB but roomy enough for the largest legitimate payload (a bulk
// contribution confirm's tag lists).
const maxRequestBody = 1 << 20

// decodeBody decodes a size-capped JSON request body so an authenticated
// client cannot stream an unbounded body into memory.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(v)
}

// apiPathInt64 parses a numeric path segment.
func apiPathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid "+name)
		return 0, false
	}
	return v, true
}

// parsePage reads page + limit, clamping limit to maxLimit.
func parsePage(r *http.Request, defaultLimit, maxLimit int) (page, limit int) {
	page, limit = 1, defaultLimit
	q := r.URL.Query()
	if p := q.Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = min(n, maxLimit)
		}
	}
	return page, limit
}
