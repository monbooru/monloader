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

// PTRService is nil when the PTR is not built into the run.
type PTRService interface {
	Status() ptr.Status
	TagGraph(ctx context.Context, names []string) (map[string]ptr.TagInfo, error)
	SearchTags(ctx context.Context, ranges []ptr.TagRange, scanCap int) ([]ptr.TagMatch, bool, error)
	TagsForHash(hashHex string) (tags []string, ok bool, err error)
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

type PairHandlers struct {
	Request  http.HandlerFunc
	Status   http.HandlerFunc
	Teardown http.HandlerFunc
}

type Host struct {
	Pair PairHandlers
	// May be nil; a handler that saves refuses rather than calling it.
	SaveConfig func(func(*config.Config) error) error
	// Whether a lookup walk would query the source rather than skip it for
	// a missing credential; nil counts every source.
	Queryable func(mapping.LookupSource) bool
}

type Handler struct {
	queue     *queue.Queue
	runner    gdl.Runner
	mapper    *mapping.Mapper
	cfg       *config.Provider
	catalog   *gdl.Catalog
	version   string
	siteState *sitestate.Tracker
	ptr       PTRService
	host      Host
}

func New(q *queue.Queue, runner gdl.Runner, mapper *mapping.Mapper, cfg *config.Provider, catalog *gdl.Catalog, version string, siteState *sitestate.Tracker, ptrSvc PTRService, host Host) *Handler {
	return &Handler{
		queue:     q,
		runner:    runner,
		mapper:    mapper,
		cfg:       cfg,
		catalog:   catalog,
		version:   version,
		siteState: siteState,
		ptr:       ptrSvc,
		host:      host,
	}
}

// Mounting from endpoints(), which also builds the OpenAPI document,
// keeps every route documented.
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

	mux.HandleFunc("OPTIONS /api/v1/", func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r)
		w.WriteHeader(http.StatusNoContent)
	})
}

// CORS goes on before the checks so the extension can read a refusal.
func (h *Handler) auth(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r)
		cfg := h.cfg.Current()
		if len(cfg.Auth.Tokens) == 0 {
			apiError(w, http.StatusServiceUnavailable, errAPIDisabled, "API is disabled: generate an API token in Settings to enable it")
			return
		}
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, prefix) {
			apiError(w, http.StatusUnauthorized, errUnauthorized, "missing or invalid authorization header")
			return
		}
		tok := cfg.FindTokenByHash(config.HashToken(got[len(prefix):]))
		if tok == nil {
			apiError(w, http.StatusUnauthorized, errUnauthorized, "invalid bearer token")
			return
		}
		if !tok.HasScope(scope) {
			apiError(w, http.StatusForbidden, errInsufficientScope, "token lacks the "+scope+" scope")
			return
		}
		next(w, r)
	}
}

func scopeForMethod(method string) string {
	if method == http.MethodPost || method == http.MethodDelete {
		return config.ScopeWrite
	}
	return config.ScopeRead
}

// Any Origin is reflected: the bearer token, not CORS, is the gate.
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

// Sized for the largest legitimate body, a bulk contribution confirm's
// tag lists.
const maxRequestBody = 1 << 20

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(v)
}

func apiPathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "invalid "+name)
		return 0, false
	}
	return v, true
}

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
