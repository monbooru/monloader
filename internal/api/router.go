package api

import (
	"context"
	"net/http"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/contrib"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/sitestate"
)

type PTRService interface {
	contrib.Service
	TagGraph(ctx context.Context, names []string) (map[string]ptr.TagInfo, error)
	SearchTags(ctx context.Context, ranges []ptr.TagRange, scanCap int) ([]ptr.TagMatch, bool, error)
	TagsForHash(hashHex string) (tags []string, ok bool, err error)
}

type PairHandlers struct {
	Request  http.HandlerFunc
	Status   http.HandlerFunc
	Teardown http.HandlerFunc
}

type Host struct {
	Pair       PairHandlers
	SimMissing func(service string) (string, bool)
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
		SetCORS(w, r)
		w.WriteHeader(http.StatusNoContent)
	})
}

// CORS goes on before the checks so the extension can read a refusal.
func (h *Handler) auth(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		SetCORS(w, r)
		if _, status, code, msg := h.cfg.Current().Auth.Tokens.Authorize(r.Header.Get("Authorization"), scope); status != 0 {
			contract.WriteError(w, status, code, msg)
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
func SetCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Vary", "Origin")
}

// Sized for the largest legitimate body, a bulk contribution confirm's
// tag lists.
const maxRequestBody = 1 << 20
