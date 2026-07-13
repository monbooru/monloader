package api

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
)

// The API surface is declared once as typed values: the endpoint table feeds
// the mux registration (Mount), the OpenAPI document (/api/v1/openapi.json),
// and the docs page (/api/v1/docs), so a mounted route cannot go undocumented.

// endpoint is one API operation.
type endpoint struct {
	Method      string // upper-case HTTP verb, as the mux pattern wants it
	Path        string // OpenAPI-style path; {id} segments match the mux syntax
	Summary     string
	OperationID string
	Description string
	// NoAuth marks the endpoints outside the bearer gate (/health and the two
	// self-doc routes), which set their own CORS.
	NoAuth    bool
	Params    []param
	Request   *reqBody
	Responses []response
	Handler   http.HandlerFunc
}

// MethodLower backs the docs page's method CSS class and anchors.
func (e endpoint) MethodLower() string { return strings.ToLower(e.Method) }

// Anchor is the endpoint's stable fragment id on the docs page.
func (e endpoint) Anchor() string { return e.MethodLower() + "-" + anchorize(e.Path) }

type param struct {
	Name, In, Description string
	Required              bool
}

// reqBody is a JSON request body: an object with required fields and
// properties.
type reqBody struct {
	Required []string
	Props    []prop
}

// response is one declared status. Ref names a component schema; Props declare
// an inline object schema instead; neither means no body.
type response struct {
	Status, Description string
	Ref                 string
	Props               []prop
}

// RefAnchor links the docs response row to its schema section.
func (r response) RefAnchor() string { return anchorize(r.Ref) }

// prop is one schema property. Ref points at a component schema; Items types
// an array's elements; Props nest an object's children.
type prop struct {
	Name, Type, Description string
	Format                  string
	Minimum                 int
	Ref                     string
	Items                   *prop
	Props                   []prop
	Required                []string
}

// apiSchema is one named component schema.
type apiSchema struct {
	Name  string
	Props []prop
}

// Anchor is the schema's fragment id on the docs page.
func (s apiSchema) Anchor() string { return anchorize(s.Name) }

// endpoints declares every operation the API serves, in the order the docs
// page lists them.
func (h *Handler) endpoints() []endpoint {
	return []endpoint{
		{
			Method: "GET", Path: "/health",
			Summary: "Liveness and versions", OperationID: "health", NoAuth: true,
			Responses: []response{
				{Status: "200", Description: "Service status and versions", Ref: "Health"},
			},
			Handler: h.health,
		},
		{
			Method: "POST", Path: "/api/v1/queue",
			Summary: "Enqueue a URL", OperationID: "enqueue",
			Params: []param{
				{Name: "wait", In: "query", Description: "Block up to N seconds (capped at 60) for the job to resolve and return it inline; otherwise 202 with a job id"},
			},
			Request: &reqBody{
				Required: []string{"url"},
				Props: []prop{
					{Name: "url", Type: "string", Description: "A booru post, pool, tag-search, or artist URL gallery-dl supports; or \"md5:<32 hex>\" to find and import the post carrying that file hash via the lookup walk"},
					{Name: "options", Type: "object", Props: []prop{
						{Name: "gallery", Type: "string", Description: "Target monbooru gallery; overrides the per-source default"},
						{Name: "folder", Type: "string", Description: "Destination subfolder under the gallery"},
						{Name: "max_items", Type: "integer", Minimum: 1, Description: "Cap on posts fetched this run (--range 1-N); only lowers the cap, never above the server's max_items_per_job. A pool is exempt and always fetched whole"},
					}},
				},
			},
			Responses: []response{
				{Status: "200", Description: "Resolved job (only when wait elapsed in time)", Ref: "Job"},
				{Status: "202", Description: "Job accepted; poll GET /api/v1/queue/{id}", Ref: "EnqueueResponse"},
				{Status: "400", Description: "Missing or non-http(s) url, or negative max_items", Ref: "Error"},
			},
			Handler: h.enqueue,
		},
		{
			Method: "GET", Path: "/api/v1/queue",
			Summary: "List jobs", OperationID: "listJobs",
			Params: []param{
				{Name: "status", In: "query", Description: "Filter by job status"},
				{Name: "page", In: "query", Description: "Page number (1-based)"},
				{Name: "limit", In: "query", Description: "Results per page (max 200)"},
			},
			Responses: []response{
				{Status: "200", Description: "Paginated job list", Ref: "PaginatedJobs"},
				{Status: "400", Description: "Unknown status filter", Ref: "Error"},
			},
			Handler: h.listJobs,
		},
		{
			Method: "POST", Path: "/api/v1/queue/pause",
			Summary: "Pause the queue", OperationID: "pauseQueue",
			Description: "Hold new downloads globally. A job already running finishes; " +
				"pending and newly submitted jobs wait until resume.",
			Responses: []response{
				{Status: "200", Description: "Queue paused", Ref: "PauseState"},
			},
			Handler: h.pauseQueue,
		},
		{
			Method: "POST", Path: "/api/v1/queue/resume",
			Summary: "Resume the queue", OperationID: "resumeQueue",
			Description: "Lift a pause so held downloads run.",
			Responses: []response{
				{Status: "200", Description: "Queue resumed", Ref: "PauseState"},
			},
			Handler: h.resumeQueue,
		},
		{
			Method: "GET", Path: "/api/v1/queue/{id}",
			Summary: "Get a job", OperationID: "getJob",
			Params: []param{{Name: "id", In: "path", Required: true, Description: "Job id"}},
			Responses: []response{
				{Status: "200", Description: "The job with items and outcomes", Ref: "Job"},
				{Status: "404", Description: "Job not found", Ref: "Error"},
			},
			Handler: h.getJob,
		},
		{
			Method: "DELETE", Path: "/api/v1/queue/{id}",
			Summary: "Cancel or remove a job", OperationID: "deleteJob",
			Params: []param{{Name: "id", In: "path", Required: true, Description: "Job id"}},
			Responses: []response{
				{Status: "204", Description: "Canceled (if running) or removed"},
				{Status: "404", Description: "Job not found", Ref: "Error"},
			},
			Handler: h.deleteJob,
		},
		{
			Method: "POST", Path: "/api/v1/queue/{id}/retry",
			Summary: "Retry a finished job", OperationID: "retryJob",
			Params: []param{
				{Name: "id", In: "path", Required: true, Description: "Job id"},
				{Name: "force", In: "query", Description: "Set to 1 to re-run with the gallery-dl archive bypassed, re-downloading already-fetched posts"},
			},
			Responses: []response{
				{Status: "202", Description: "Re-queued", Ref: "EnqueueResponse"},
				{Status: "404", Description: "Job not found", Ref: "Error"},
				{Status: "409", Description: "Job is not in a retryable state", Ref: "Error"},
			},
			Handler: h.retryJob,
		},
		{
			Method: "POST", Path: "/api/v1/queue/{id}/continue",
			Summary: "Fetch the next window of a capped job", OperationID: "continueJob",
			Params: []param{{Name: "id", In: "path", Required: true, Description: "Job id"}},
			Responses: []response{
				{Status: "202", Description: "Follow-up job queued for the next window", Ref: "EnqueueResponse"},
				{Status: "404", Description: "Job not found", Ref: "Error"},
				{Status: "409", Description: "Job was not capped, so there is no next window", Ref: "Error"},
			},
			Handler: h.continueJob,
		},
		{
			Method: "POST", Path: "/api/v1/queue/{id}/continue-all",
			Summary: "Fetch every remaining window of a capped job", OperationID: "continueAllJob",
			Params: []param{{Name: "id", In: "path", Required: true, Description: "Job id"}},
			Responses: []response{
				{Status: "202", Description: "First follow-up queued; the queue continues until the search runs short", Ref: "EnqueueResponse"},
				{Status: "404", Description: "Job not found", Ref: "Error"},
				{Status: "409", Description: "Job was not capped, so there is no next window", Ref: "Error"},
			},
			Handler: h.continueAllJob,
		},
		{
			Method: "POST", Path: "/api/v1/metadata",
			Summary: "Refetch a post's metadata and enrich a monbooru image", OperationID: "refetchMetadata",
			Description: "Re-read a post URL for its tags, commentary, and notes (no file download) " +
				"and fold them into the monbooru image it names. The queued job reports the enriched " +
				"outcome, or failed with hash_mismatch when the source no longer serves the stored file.",
			Request: &reqBody{
				Required: []string{"image_id", "url"},
				Props: []prop{
					{Name: "image_id", Type: "integer", Description: "monbooru image to enrich"},
					{Name: "gallery", Type: "string", Description: "Gallery holding the image; empty uses monbooru's active gallery"},
					{Name: "url", Type: "string", Description: "Source post URL to re-read"},
				},
			},
			Responses: []response{
				{Status: "202", Description: "Refetch queued; poll GET /api/v1/queue/{id}", Ref: "EnqueueResponse"},
				{Status: "400", Description: "Missing image_id, or a non-http(s) url", Ref: "Error"},
			},
			Handler: h.metadata,
		},
		{
			Method: "POST", Path: "/api/v1/lookup",
			Summary: "Find tags for a file hash and enrich a monbooru image", OperationID: "lookupHash",
			Description: "Look a file hash up - the booru backend walks the sites given a lookup order " +
				"in settings using their md5 search, the ptr backend queries the local PTR index by " +
				"sha256, the all backend runs both (skipping the PTR when it is disabled) - and fold " +
				"the tags found into the monbooru image it names. The queued job reports the enriched " +
				"outcome, or failed with hash_not_found when no source knows the hash.",
			Request: &reqBody{
				Required: []string{"image_id", "backend"},
				Props: []prop{
					{Name: "image_id", Type: "integer", Description: "monbooru image to enrich"},
					{Name: "backend", Type: "string", Description: "\"booru\", \"ptr\", or \"all\""},
					{Name: "md5", Type: "string", Description: "32-hex file md5, required by the booru and all backends"},
					{Name: "sha256", Type: "string", Description: "64-hex file sha256, required by the ptr backend, and by the all backend while the ptr sync is enabled"},
					{Name: "gallery", Type: "string", Description: "Gallery holding the image; empty uses monbooru's active gallery"},
				},
			},
			Responses: []response{
				{Status: "202", Description: "Lookup queued; poll GET /api/v1/queue/{id}", Ref: "EnqueueResponse"},
				{Status: "400", Description: "Missing image_id, unknown backend, or a malformed hash", Ref: "Error"},
				{Status: "409", Description: "The ptr backend is disabled", Ref: "Error"},
			},
			Handler: h.lookup,
		},
		{
			Method: "GET", Path: "/api/v1/ptr/status",
			Summary: "PTR sync status and capability", OperationID: "ptrStatus",
			Description: "Report whether the Hydrus PTR lookup backend is enabled and, when it is, its " +
				"sync state, progress, and index counts. Always answers, so a caller's capability " +
				"check is one unconditional GET; a run without the PTR built in reports disabled.",
			Responses: []response{
				{Status: "200", Description: "PTR status", Ref: "PTRStatus"},
			},
			Handler: h.ptrStatus,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/tags",
			Summary: "Query the PTR alias / implication graph", OperationID: "ptrTags",
			Description: "For each monbooru-form tag, return the PTR's ideal spelling, the tags that alias " +
				"to it, and its implications, mapped back to monbooru form. Batched so a caller can sweep " +
				"its tag list page by page.",
			Request: &reqBody{
				Required: []string{"tags"},
				Props: []prop{
					{Name: "tags", Type: "array", Items: &prop{Type: "string"}, Description: "monbooru-form tag names, at most 500"},
				},
			},
			Responses: []response{
				{Status: "200", Description: "Per-tag graph answers", Ref: "PTRTagResults"},
				{Status: "400", Description: "Empty or over-long tag list", Ref: "Error"},
				{Status: "409", Description: "The PTR index is not available", Ref: "Error"},
			},
			Handler: h.ptrTags,
		},
		{
			Method: "GET", Path: "/api/v1/sites",
			Summary: "List supported sites", OperationID: "listSites",
			Params: []param{{Name: "q", In: "query", Description: "Substring filter on category/subcategory"}},
			Responses: []response{
				{Status: "200", Description: "Supported sites (curated first)", Props: []prop{
					{Name: "total", Type: "integer"},
					{Name: "sites", Type: "array", Items: &prop{Ref: "Site"}},
				}},
			},
			Handler: h.listSites,
		},
		{
			Method: "POST", Path: "/api/v1/sites/{name}/test",
			Summary: "Probe a site", OperationID: "testSite",
			Params: []param{
				{Name: "name", In: "path", Required: true, Description: "gallery-dl category"},
				{Name: "url", In: "query", Description: "URL to probe; defaults to the site's example URL"},
			},
			Responses: []response{
				{Status: "200", Description: "Probe result", Ref: "ProbeResult"},
				{Status: "404", Description: "Unknown site and no url supplied", Ref: "Error"},
			},
			Handler: h.testSite,
		},
		{
			Method: "GET", Path: "/api/v1/openapi.json",
			Summary: "This OpenAPI document", OperationID: "openapi", NoAuth: true,
			Responses: []response{
				{Status: "200", Description: "OpenAPI 3 spec"},
			},
			Handler: h.openAPIJSON,
		},
		{
			Method: "GET", Path: "/api/v1/docs",
			Summary: "HTML API reference", OperationID: "docs", NoAuth: true,
			Responses: []response{
				{Status: "200", Description: "HTML reference"},
			},
			Handler: h.openAPIDocs,
		},
	}
}

// apiSchemas declares the component schemas, in the order the docs page lists
// them.
var apiSchemas = []apiSchema{
	{Name: "Error", Props: []prop{
		{Name: "error", Type: "string"},
		{Name: "code", Type: "string"},
	}},
	{Name: "Summary", Props: []prop{
		{Name: "created", Type: "integer"},
		{Name: "duplicate", Type: "integer"},
		{Name: "enriched", Type: "integer", Description: "Source refetches and hash lookups that merged tags into an image monbooru already holds"},
		{Name: "skipped", Type: "integer", Description: "Posts the gallery-dl archive already had, or files monbooru cannot ingest"},
		{Name: "failed", Type: "integer"},
		{Name: "canceled", Type: "integer", Description: "Items aborted by a job cancel, kept out of failed"},
		{Name: "total", Type: "integer"},
	}},
	{Name: "Item", Props: []prop{
		{Name: "post_id", Type: "string"},
		{Name: "num", Type: "integer", Description: "1-based pool page order"},
		{Name: "url", Type: "string", Description: "canonical source post page"},
		{Name: "status", Type: "string", Description: "pending, downloaded, uploaded, done, skipped, failed"},
		{Name: "outcome", Type: "string", Description: "created, duplicate, enriched, skipped_archive, skipped_unsupported, failed"},
		{Name: "monbooru_id", Type: "integer"},
		{Name: "sha256", Type: "string"},
		{Name: "tag_warnings", Type: "array", Items: &prop{Type: "string"}, Description: "Tags monbooru rejected on the push; recorded, not fatal"},
		{Name: "merge_note", Type: "string", Description: "What a duplicate or enrich merge folded into the existing image (e.g. \"+7 tags\")"},
		{Name: "error_code", Type: "string"},
		{Name: "error", Type: "string"},
	}},
	{Name: "Job", Props: []prop{
		{Name: "id", Type: "integer"},
		{Name: "url", Type: "string"},
		{Name: "status", Type: "string", Description: "queued, running, succeeded, partial, failed, canceled"},
		{Name: "kind", Type: "string", Description: "download (the default, omitted), metadata (a source refetch that enriches an existing image), lookup (a hash lookup that enriches an existing image), or hash_import (an md5 resolved to a post and imported)"},
		{Name: "image_id", Type: "integer", Description: "monbooru image a metadata or lookup job enriches"},
		{Name: "backend", Type: "string", Description: "Lookup source (booru or ptr) for a lookup job"},
		{Name: "md5", Type: "string", Description: "File md5 a lookup or hash import is keyed on"},
		{Name: "sha256", Type: "string", Description: "File sha256 a ptr lookup is keyed on"},
		{Name: "site", Type: "string", Description: "gallery-dl category, set after resolve"},
		{Name: "gallery", Type: "string", Description: "target monbooru gallery"},
		{Name: "folder", Type: "string", Description: "destination subfolder under the gallery"},
		{Name: "max_items", Type: "integer", Description: "per-job item cap supplied at enqueue"},
		{Name: "force", Type: "boolean", Description: "Last/next run bypasses the gallery-dl archive (set by a forced retry)"},
		{Name: "summary", Ref: "Summary"},
		{Name: "capped", Type: "boolean", Description: "The resolve hit the per-job item cap, so more posts may remain"},
		{Name: "cap", Type: "integer", Description: "The applied item cap when capped is true"},
		{Name: "root", Type: "integer", Description: "Originating job of a continue-series; a capped search and its continuations share it. Self for a standalone job"},
		{Name: "error_code", Type: "string"},
		{Name: "error", Type: "string"},
		{Name: "items", Type: "array", Items: &prop{Ref: "Item"}},
		{Name: "created_at", Type: "string", Format: "date-time"},
		{Name: "started_at", Type: "string", Format: "date-time"},
		{Name: "finished_at", Type: "string", Format: "date-time"},
	}},
	{Name: "PaginatedJobs", Props: []prop{
		{Name: "page", Type: "integer"},
		{Name: "limit", Type: "integer"},
		{Name: "total", Type: "integer"},
		{Name: "paused", Type: "boolean", Description: "Whether the queue is holding new downloads (see the pause/resume endpoints)"},
		{Name: "jobs", Type: "array", Items: &prop{Ref: "Job"}},
	}},
	{Name: "PauseState", Props: []prop{
		{Name: "paused", Type: "boolean"},
	}},
	{Name: "Site", Props: []prop{
		{Name: "category", Type: "string"},
		{Name: "subcategory", Type: "string"},
		{Name: "example", Type: "string"},
		{Name: "curated", Type: "boolean", Description: "Has a built-in mapping profile"},
		{Name: "auth", Type: "string", Description: "none, api_optional, api_required, cookies"},
	}},
	{Name: "ProbeResult", Props: []prop{
		{Name: "status", Type: "string", Description: "ok, auth_required, blocked, failed"},
		{Name: "detail", Type: "string"},
	}},
	{Name: "Health", Props: []prop{
		{Name: "status", Type: "string"},
		{Name: "version", Type: "string"},
		{Name: "gallerydl_version", Type: "string"},
	}},
	{Name: "EnqueueResponse", Props: []prop{
		{Name: "job_id", Type: "integer"},
	}},
	{Name: "PTRStatus", Props: []prop{
		{Name: "enabled", Type: "boolean"},
		{Name: "state", Type: "string", Description: "disabled, syncing, ready, paused, error"},
		{Name: "progress", Type: "object", Props: []prop{
			{Name: "update_index", Type: "integer", Description: "the update index being applied"},
			{Name: "update_count", Type: "integer", Description: "total updates the server publishes"},
			{Name: "downloaded_bytes", Type: "integer", Description: "compressed bytes fetched this session"},
			{Name: "download_rate", Type: "integer", Description: "current download rate in bytes per second while syncing"},
		}},
		{Name: "counts", Type: "object", Props: []prop{
			{Name: "hashes", Type: "integer"},
			{Name: "tags", Type: "integer"},
			{Name: "mappings", Type: "integer"},
			{Name: "siblings", Type: "integer"},
			{Name: "parents", Type: "integer"},
		}},
		{Name: "disk_bytes", Type: "integer"},
		{Name: "next_update_due", Type: "integer", Description: "unix time the next update is expected"},
		{Name: "covered_through", Type: "integer", Description: "unix time the synced data reaches; absent until an update has been applied"},
		{Name: "error", Type: "string"},
	}},
	{Name: "PTRTagResults", Props: []prop{
		{Name: "results", Type: "object", Description: "map of the queried tag to its PTR ideal, aliases, and implications (monbooru form); known=false when the PTR does not have the tag"},
	}},
}

// buildSpec folds the endpoint and schema declarations into the OpenAPI 3
// document shape. The server is the root base URL so /health (root) and the
// /api/v1/* endpoints share one document.
func buildSpec(baseURL, version string, eps []endpoint) map[string]any {
	paths := map[string]any{}
	for _, e := range eps {
		ops, _ := paths[e.Path].(map[string]any)
		if ops == nil {
			ops = map[string]any{}
			paths[e.Path] = ops
		}
		ops[e.MethodLower()] = operationJSON(e)
	}
	schemas := map[string]any{}
	for _, s := range apiSchemas {
		schemas[s.Name] = objectJSON(nil, s.Props)
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "monloader API",
			"description": "Queue booru URLs for download into monbooru.",
			"version":     version,
		},
		"servers": []map[string]any{
			{"url": baseURL, "description": "This server"},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "token",
					"description":  "Required on every endpoint except /health, /api/v1/openapi.json, and /api/v1/docs. Use a named, scoped token from Settings -> Authentication (read: queue/sites; write: enqueue/manage); the API is disabled until at least one token exists.",
				},
			},
			"schemas": schemas,
		},
		"security": []map[string]any{
			{"bearerAuth": []string{}},
		},
		"paths": paths,
	}
}

func operationJSON(e endpoint) map[string]any {
	op := map[string]any{
		"summary":     e.Summary,
		"operationId": e.OperationID,
	}
	if e.Description != "" {
		op["description"] = e.Description
	}
	if e.NoAuth {
		op["security"] = []map[string]any{}
	}
	if len(e.Params) > 0 {
		params := make([]map[string]any, len(e.Params))
		for i, p := range e.Params {
			params[i] = map[string]any{
				"name": p.Name, "in": p.In, "required": p.Required,
				"description": p.Description, "schema": map[string]any{"type": "string"},
			}
		}
		op["parameters"] = params
	}
	if e.Request != nil {
		op["requestBody"] = map[string]any{
			"required": true,
			"content":  map[string]any{"application/json": map[string]any{"schema": objectJSON(e.Request.Required, e.Request.Props)}},
		}
	}
	responses := map[string]any{}
	for _, r := range e.Responses {
		resp := map[string]any{"description": r.Description}
		switch {
		case r.Ref != "":
			resp["content"] = map[string]any{"application/json": map[string]any{"schema": refJSON(r.Ref)}}
		case len(r.Props) > 0:
			resp["content"] = map[string]any{"application/json": map[string]any{"schema": objectJSON(nil, r.Props)}}
		}
		responses[r.Status] = resp
	}
	op["responses"] = responses
	return op
}

func objectJSON(required []string, props []prop) map[string]any {
	properties := map[string]any{}
	for _, p := range props {
		properties[p.Name] = propJSON(p)
	}
	obj := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		obj["required"] = required
	}
	return obj
}

func propJSON(p prop) map[string]any {
	if p.Ref != "" {
		return refJSON(p.Ref)
	}
	m := map[string]any{"type": p.Type}
	if p.Description != "" {
		m["description"] = p.Description
	}
	if p.Format != "" {
		m["format"] = p.Format
	}
	if p.Minimum > 0 {
		m["minimum"] = p.Minimum
	}
	if p.Items != nil {
		m["items"] = propJSON(*p.Items)
	}
	if len(p.Props) > 0 {
		obj := objectJSON(p.Required, p.Props)
		m["properties"] = obj["properties"]
		if req, ok := obj["required"]; ok {
			m["required"] = req
		}
	}
	return m
}

func refJSON(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func anchorize(s string) string {
	r := strings.ToLower(s)
	r = strings.ReplaceAll(r, "/", "-")
	r = strings.ReplaceAll(r, "{", "")
	r = strings.ReplaceAll(r, "}", "")
	r = strings.ReplaceAll(r, ".", "-")
	r = strings.Trim(r, "-")
	if r == "" {
		r = "root"
	}
	return r
}

// openAPIJSON serves the raw spec (no auth, so it sets CORS itself for the
// browser extension that fetches it cross-origin).
func (h *Handler) openAPIJSON(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildSpec(h.cfg.Current().Server.BaseURL, h.version, h.endpoints()))
}

// docsView is the docs template's context: the endpoint and schema
// declarations rendered as-is.
type docsView struct {
	Title        string
	Version      string
	BaseURL      string
	APIProtected bool
	Endpoints    []endpoint
	Schemas      []apiSchema
}

// openAPIDocs renders the declarations as a self-contained HTML reference (no
// auth, no external assets, so it sets CORS itself like openAPIJSON).
func (h *Handler) openAPIDocs(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	view := docsView{
		Title:        "monloader API",
		Version:      h.version,
		BaseURL:      h.cfg.Current().Server.BaseURL,
		APIProtected: len(h.cfg.Current().Auth.Tokens) > 0,
		Endpoints:    h.endpoints(),
		Schemas:      apiSchemas,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := docsTemplate.Execute(w, view); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// docsTemplate renders the API reference with inline CSS in the indigo
// downloader palette.
var docsTemplate = template.Must(template.New("api-docs").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - Docs</title>
<style>
 body { background:#0d0d0d; color:#c8c8c8; font-family:"JetBrains Mono","Fira Mono","Courier New",monospace; font-size:14px; line-height:1.5; padding:24px; max-width:1000px; margin:0 auto; }
 h1 { font-size:20px; font-weight:bold; margin-bottom:4px; }
 h2 { font-size:16px; color:#c8c8c8; border-bottom:1px solid #2a2a36; padding-bottom:4px; margin:24px 0 8px; }
 h3 { font-size:13px; color:#6a6a82; margin:12px 0 4px; font-weight:normal; text-transform:uppercase; letter-spacing:0.5px; }
 a { color:#5c6bc0; text-decoration:none; }
 a:hover { text-decoration:underline; }
 code { font-family:inherit; }
 table { border-collapse:collapse; width:100%; margin:6px 0 10px; font-size:13px; }
 th, td { border:1px solid #2a2a36; padding:4px 8px; text-align:left; vertical-align:top; }
 th { color:#6a6a82; font-weight:normal; background:#16161c; }
 .muted { color:#6a6a82; font-size:12px; }
 .method { display:inline-block; padding:1px 6px; border:1px solid; font-weight:bold; margin-right:8px; font-size:12px; min-width:52px; text-align:center; }
 .method-get    { color:#22aa44; border-color:#22aa44; }
 .method-post   { color:#5c6bc0; border-color:#5c6bc0; }
 .method-delete { color:#cc3333; border-color:#cc3333; }
 .path { color:#c8c8c8; }
 ul.toc { list-style:none; padding:0; margin:8px 0 20px; }
 ul.toc li { padding:2px 0; }
</style>
</head>
<body>
 <p class="muted"><a href="/">&larr; Back</a></p>
 <h1>{{.Title}}</h1>
 <p class="muted">Version {{.Version}} &middot; base URL <code>{{.BaseURL}}</code></p>
 {{if .APIProtected}}
 <p style="color:#22aa44;border:1px solid #22aa44;padding:4px 8px;">An API token is configured: send <code>Authorization: Bearer &lt;token&gt;</code> (with a scope covering the method) on every endpoint except <code>/health</code>, <code>/api/v1/openapi.json</code>, and <code>/api/v1/docs</code>.</p>
 {{else}}
 <p style="color:#ffaa00;border:1px solid #ffaa00;padding:4px 8px;">No API token is configured, so the API is disabled (every authenticated endpoint returns <code>503 api_disabled</code>). Create one in Settings -&gt; Authentication.</p>
 {{end}}
 <p class="muted">Raw spec: <a href="/api/v1/openapi.json">openapi.json</a></p>

 <h2>Endpoints</h2>
 <ul class="toc">
 {{range .Endpoints}}
  <li><a href="#{{.Anchor}}"><span class="method method-{{.MethodLower}}">{{.Method}}</span><span class="path">{{.Path}}</span></a>{{if .Summary}} <span class="muted">- {{.Summary}}</span>{{end}}</li>
 {{end}}
 </ul>

 {{range .Endpoints}}
 <div class="endpoint">
  <h2 id="{{.Anchor}}"><span class="method method-{{.MethodLower}}">{{.Method}}</span><span class="path">{{.Path}}</span></h2>
  {{if .Summary}}<p>{{.Summary}}</p>{{end}}
  {{if .Params}}
  <h3>Parameters</h3>
  <table><thead><tr><th>Name</th><th>In</th><th>Required</th><th>Description</th></tr></thead><tbody>
  {{range .Params}}<tr><td><code>{{.Name}}</code></td><td>{{.In}}</td><td>{{if .Required}}yes{{else}}no{{end}}</td><td>{{.Description}}</td></tr>{{end}}
  </tbody></table>
  {{end}}
  {{if .Request}}
  <h3>Request body</h3>
   <p class="muted">Content-Type: <code>application/json</code></p>
   {{if .Request.Required}}<p class="muted">Required: {{range .Request.Required}}<code>{{.}}</code> {{end}}</p>{{end}}
   {{if .Request.Props}}<table><thead><tr><th>Field</th><th>Type</th><th>Description</th></tr></thead><tbody>
   {{range .Request.Props}}<tr><td><code>{{.Name}}</code></td><td>{{.Type}}</td><td>{{.Description}}</td></tr>{{end}}
   </tbody></table>{{end}}
  {{end}}
  {{if .Responses}}
  <h3>Responses</h3>
  <table><thead><tr><th>Status</th><th>Description</th><th>Schema</th></tr></thead><tbody>
  {{range .Responses}}<tr><td><code>{{.Status}}</code></td><td>{{.Description}}</td><td>{{if .Ref}}<a href="#schema-{{.RefAnchor}}"><code>{{.Ref}}</code></a>{{end}}</td></tr>{{end}}
  </tbody></table>
  {{end}}
 </div>
 {{end}}

 <h2>Schemas</h2>
 {{range .Schemas}}
  <h3 id="schema-{{.Anchor}}" style="color:#c8c8c8;font-size:14px;text-transform:none;letter-spacing:0;margin-top:14px">{{.Name}}</h3>
  {{if .Props}}<table><thead><tr><th>Field</th><th>Type</th><th>Description</th></tr></thead><tbody>
  {{range .Props}}<tr><td><code>{{.Name}}</code></td><td>{{.Type}}</td><td>{{.Description}}</td></tr>{{end}}
  </tbody></table>{{else}}<p class="muted">(no fields)</p>{{end}}
 {{end}}
</body>
</html>`))
