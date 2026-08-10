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
	NoAuth bool
	// ReadScope marks a POST that only reads (the contribution preview),
	// so a read-scoped token suffices despite the method.
	ReadScope bool
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
						{Name: "page_url", Type: "string", Description: "The page a direct-file send came from; a directlink job records it as the item's source link instead of the bare file URL"},
					}},
				},
			},
			Responses: []response{
				{Status: "200", Description: "Resolved job (only when wait elapsed in time)", Ref: "Job"},
				{Status: "202", Description: "Job accepted; poll GET /api/v1/queue/{id}", Ref: "EnqueueResponse"},
				{Status: "400", Description: "Missing or non-http(s) url, negative max_items, or non-http(s) page_url", Ref: "Error"},
				{Status: "409", Description: "No monbooru is configured (monbooru_unconfigured), or its link is paused from the footer light (monbooru_paused)", Ref: "Error"},
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
				{Status: "409", Description: "Job is not in a retryable state, no monbooru is configured (monbooru_unconfigured), or its link is paused (monbooru_paused)", Ref: "Error"},
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
				{Status: "409", Description: "Job was not capped so there is no next window, no monbooru is configured (monbooru_unconfigured), or its link is paused (monbooru_paused)", Ref: "Error"},
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
				{Status: "409", Description: "Job was not capped so there is no next window, no monbooru is configured (monbooru_unconfigured), or its link is paused (monbooru_paused)", Ref: "Error"},
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
			Method: "POST", Path: "/api/v1/replace",
			Summary: "Download a post's file and replace a monbooru image's bytes", OperationID: "replaceFile",
			Description: "Download the file the post URL serves and push it into the monbooru image " +
				"it names, replacing its bytes in place while the image's tags, sources, and relations " +
				"survive. The queued job reports the replaced outcome, or failed with hash_mismatch " +
				"(the download did not match the md5 the source claims), already_exists (monbooru holds " +
				"the original as another image), or wrong_type (the target is an archive or video row).",
			Request: &reqBody{
				Required: []string{"image_id", "url"},
				Props: []prop{
					{Name: "image_id", Type: "integer", Description: "monbooru image whose file is replaced"},
					{Name: "gallery", Type: "string", Description: "Gallery holding the image; empty uses monbooru's active gallery"},
					{Name: "url", Type: "string", Description: "Source post URL to download"},
				},
			},
			Responses: []response{
				{Status: "202", Description: "Replace queued; poll GET /api/v1/queue/{id}", Ref: "EnqueueResponse"},
				{Status: "400", Description: "Missing image_id, or a non-http(s) url", Ref: "Error"},
			},
			Handler: h.replace,
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
					{Name: "background", Type: "boolean", Description: "Queue behind anything a person is waiting on, for bulk or unattended work (default false)"},
					{Name: "budgeted", Type: "boolean", Description: "Count the job against the daily scheduled-lookup budget and refuse it when that is spent (default false)"},
				},
			},
			Responses: []response{
				{Status: "202", Description: "Lookup queued; poll GET /api/v1/queue/{id}", Ref: "EnqueueResponse"},
				{Status: "400", Description: "Missing image_id, unknown backend, or a malformed hash", Ref: "Error"},
				{Status: "409", Description: "The ptr backend is disabled or not fully synced", Ref: "Error"},
				{Status: "429", Description: "Budgeted and today's scheduled-lookup budget is spent", Ref: "Error"},
			},
			Handler: h.lookup,
		},
		{
			Method: "GET", Path: "/api/v1/lookup/status",
			Summary: "Scheduled-lookup budget and chain readout", OperationID: "lookupStatus",
			Description: "Report how much of today's scheduled-lookup budget is left, when it rolls " +
				"over, and which sources the walk would query. " +
				"A readout only: the 429 on an enqueue stays authoritative, so a caller must " +
				"not keep its own count.",
			Responses: []response{
				{Status: "200", Description: "Budget and chain state", Ref: "LookupStatus"},
			},
			Handler: h.lookupStatus,
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
				"to it, its implications, and the tags that imply it (direct children, at most 200), " +
				"mapped back to monbooru form. Batched so a caller can sweep its tag list page by page.",
			Request: &reqBody{
				Required: []string{"tags"},
				Props: []prop{
					{Name: "tags", Type: "array", Items: &prop{Type: "string"}, Description: "monbooru-form tag names, at most 500"},
				},
			},
			Responses: []response{
				{Status: "200", Description: "Per-tag graph answers", Ref: "PTRTagResults"},
				{Status: "400", Description: "Empty or over-long tag list", Ref: "Error"},
				{Status: "409", Description: "The PTR index is not available or not fully synced", Ref: "Error"},
			},
			Handler: h.ptrTags,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/lookup",
			Summary: "Look a batch of file hashes up in the PTR index", OperationID: "ptrLookup",
			Description: "Return the tags the local index holds for each file sha256, in monbooru form. " +
				"Answers in process, so a whole-library sweep costs one call per batch instead of a " +
				"queued job per image; a hash the index does not hold is absent from the results.",
			Request: &reqBody{
				Required: []string{"images"},
				Props: []prop{
					{Name: "images", Type: "array", Items: &prop{Type: "object"}, Description: "At most 100 `{ image_id, sha256 }` pairs; the id names the image on the queue row the answer files"},
					{Name: "gallery", Type: "string", Description: "Gallery the images come from; recorded on that row"},
					{Name: "scheduled", Type: "boolean", Description: "Set by monbooru's nightly run, so the row reads as scheduled rather than bulk"},
				},
			},
			Responses: []response{
				{Status: "200", Description: "Tags per matched hash, plus the index cursor", Ref: "PTRLookupResults"},
				{Status: "400", Description: "Empty or over-long image list, or a malformed hash", Ref: "Error"},
				{Status: "409", Description: "The PTR index is not available or not fully synced", Ref: "Error"},
			},
			Handler: h.ptrLookup,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/account",
			Summary: "Create the personal PTR contribution account", OperationID: "ptrAccountCreate",
			Description: "Run the repository's open auto-creation once and store the resulting access " +
				"key as the instance's personal account, shared by sync and uploads. Refused while a " +
				"personal key is already set; replacing an account is a deliberate manual step " +
				"(clear the key in settings first).",
			Responses: []response{
				{Status: "200", Description: "Account created and key stored"},
				{Status: "409", Description: "PTR disabled, or a personal key already exists", Ref: "Error"},
				{Status: "503", Description: "The repository is not taking new accounts right now", Ref: "Error"},
			},
			Handler: h.ptrAccountCreate,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/contrib/preview", ReadScope: true,
			Summary: "Preview a file's contribution diff", OperationID: "ptrContribPreview",
			Description: "For one file's sha256 and its monbooru-form tags, answer the two-way diff " +
				"against the synced PTR copy: which tags a send would add (with the exact PTR spelling " +
				"and a per-tag status of new, known, ineligible, filtered, or unsent) and which " +
				"PTR-current tags the submitted list lacks (the removal-petition candidates; both " +
				"sides compare on the display view, so alias spellings and implied tags never read " +
				"as diffs). provisional is true while the index is still syncing, so the diff may " +
				"overstate what is new.",
			Request: &reqBody{
				Required: []string{"sha256", "tags"},
				Props: []prop{
					{Name: "sha256", Type: "string", Description: "the file's sha256, 64 lowercase hex characters"},
					{Name: "tags", Type: "array", Items: &prop{Type: "string"}, Description: "the file's storage tags in monbooru form (non-implied, non-alias, rating excluded)"},
					{Name: "implied", Type: "array", Items: &prop{Type: "string"}, Description: "the file's implied tags, context only: never offered as adds, but they suppress removal petitions like submitted tags do"},
				},
			},
			Responses: []response{
				{Status: "200", Description: "The two-way diff", Ref: "PTRContribPreview"},
				{Status: "400", Description: "Malformed hash or body", Ref: "Error"},
				{Status: "409", Description: "The PTR index is not available", Ref: "Error"},
			},
			Handler: h.ptrContribPreview,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/contrib/pair-preview", ReadScope: true,
			Summary: "Resolve a relation pair's contribution direction", OperationID: "ptrContribPairPreview",
			Description: "For a sibling (a=bad, b=good) or parent (a=child, b=parent) pair in monbooru " +
				"form, report the exact PTR spellings and which direction a contribution takes: suggest " +
				"(absent from the PTR), petition (current on the PTR), pending (already in the ledger " +
				"awaiting janitors), conflict (a sibling holds the reverse), covered (both spellings " +
				"already resolve to one ideal), or ineligible.",
			Request: &reqBody{
				Required: []string{"kind", "a", "b"},
				Props: []prop{
					{Name: "kind", Type: "string", Description: "sibling or parent"},
					{Name: "a", Type: "string", Description: "the alias (sibling) or carrying tag (parent), monbooru form"},
					{Name: "b", Type: "string", Description: "the canonical (sibling) or implied tag (parent), monbooru form"},
				},
			},
			Responses: []response{
				{Status: "200", Description: "The pair's direction and PTR spellings"},
				{Status: "400", Description: "Malformed body or kind", Ref: "Error"},
				{Status: "409", Description: "The PTR index is not available", Ref: "Error"},
			},
			Handler: h.ptrContribPairPreview,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/contrib",
			Summary: "Stage contributions and send them", OperationID: "ptrContribStage",
			Description: "Stage items (tags in monbooru form; monloader maps and validates) and, by " +
				"default, commit them in the same call as one queue send job. Per-item results report " +
				"staged, duplicate, already_known, already_suggested, not_on_ptr, conflict, ineligible, " +
				"or invalid_reason; one refused item never sinks the rest. A reason is required on every " +
				"kind except mapping_add. With commit true the account is validated synchronously first.",
			Request: &reqBody{
				Required: []string{"items"},
				Props: []prop{
					{Name: "commit", Type: "boolean", Description: "send the accepted items now as one queue job (default true)"},
					{Name: "origin", Type: "string", Description: "display provenance carried on the staged row while it is unsent, e.g. \"image 42\"; the committed ledger entry identifies the item by hash instead"},
					{Name: "items", Type: "array", Items: &prop{Type: "object", Props: []prop{
						{Name: "kind", Type: "string", Description: "mapping_add, mapping_petition, sibling, parent, sibling_petition, or parent_petition"},
						{Name: "sha256", Type: "string", Description: "the file hash, for the mapping kinds"},
						{Name: "tag", Type: "string", Description: "the tag in monbooru form, for the mapping kinds"},
						{Name: "bad", Type: "string", Description: "the alias name, for the sibling kinds"},
						{Name: "good", Type: "string", Description: "the canonical name, for the sibling kinds"},
						{Name: "child", Type: "string", Description: "the carrying tag, for the parent kinds"},
						{Name: "parent", Type: "string", Description: "the implied tag, for the parent kinds"},
						{Name: "reason", Type: "string", Description: "the janitor-facing reason; required on every kind except mapping_add"},
					}}},
				},
			},
			Responses: []response{
				{Status: "200", Description: "Per-item results; nothing was accepted for sending"},
				{Status: "202", Description: "Per-item results plus the send job id"},
				{Status: "400", Description: "Malformed body", Ref: "Error"},
				{Status: "409", Description: "ptr_unavailable, ptr_account_required, ptr_banned, or ptr_syncing", Ref: "Error"},
			},
			Handler: h.ptrContribStage,
		},
		{
			Method: "GET", Path: "/api/v1/ptr/contrib",
			Summary: "Contribution ledger", OperationID: "ptrContribLedger",
			Description: "The unsent backlog (staged and failed items with errors), counts by kind and " +
				"status, and the newest slice of the committed history with each row's janitor outcome.",
			Responses: []response{
				{Status: "200", Description: "Unsent items, counts, and history"},
				{Status: "409", Description: "The PTR index is not available", Ref: "Error"},
			},
			Handler: h.ptrContribLedger,
		},
		{
			Method: "DELETE", Path: "/api/v1/ptr/contrib/{id}",
			Summary: "Rescind one unsent item", OperationID: "ptrContribRescind",
			Description: "Delete one staged or failed item exactly - nothing ever left the machine.",
			Params:      []param{{Name: "id", In: "path", Required: true, Description: "Unsent item id"}},
			Responses: []response{
				{Status: "204", Description: "Rescinded"},
				{Status: "404", Description: "No unsent item with that id", Ref: "Error"},
				{Status: "409", Description: "The PTR index is not available", Ref: "Error"},
			},
			Handler: h.ptrContribRescind,
		},
		{
			Method: "DELETE", Path: "/api/v1/ptr/contrib",
			Summary: "Rescind every unsent item", OperationID: "ptrContribRescindAll",
			Responses: []response{
				{Status: "200", Description: "Count of rescinded items"},
				{Status: "409", Description: "The PTR index is not available", Ref: "Error"},
			},
			Handler: h.ptrContribRescindAll,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/contrib/commit",
			Summary: "Send the unsent backlog", OperationID: "ptrContribCommit",
			Description: "Queue one send job over every staged and failed item - the retry path for " +
				"failed leftovers. Refused while another send is queued or running.",
			Responses: []response{
				{Status: "202", Description: "The send job id"},
				{Status: "409", Description: "ptr_unavailable, ptr_account_required, ptr_banned, ptr_syncing, or already_running", Ref: "Error"},
			},
			Handler: h.ptrContribCommit,
		},
		{
			Method: "POST", Path: "/api/v1/ptr/contrib/log/{id}/rescind",
			Summary: "Rescind a committed mapping add", OperationID: "ptrContribLogRescind",
			Description: "Stage and send a removal petition for the same tag and hash with a fixed " +
				"reason, and mark the ledger row rescinded. Committed suggestions and petitions cannot " +
				"be withdrawn - the protocol reserves their resolution for janitors.",
			Params: []param{{Name: "id", In: "path", Required: true, Description: "Ledger row id"}},
			Responses: []response{
				{Status: "202", Description: "The send job id"},
				{Status: "404", Description: "No ledger row with that id", Ref: "Error"},
				{Status: "409", Description: "not_rescindable, or an account refusal", Ref: "Error"},
			},
			Handler: h.ptrContribLogRescind,
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
				{Status: "400", Description: "The supplied url is not http(s)", Ref: "Error"},
				{Status: "404", Description: "Unknown site and no url supplied", Ref: "Error"},
			},
			Handler: h.testSite,
		},
		{
			Method: "POST", Path: "/api/v1/pair/request",
			Summary: "Offer a pairing", OperationID: "pairRequest", NoAuth: true,
			Description: "Step one of the handshake a client runs before it can call anything else. " +
				"Nothing is issued here: the operator approves the request in monloader's settings, " +
				"after which the client claims its token from GET /api/v1/pair/status. Unauthenticated " +
				"by design - it is how a client obtains a token.",
			Request: &reqBody{
				Required: []string{"app"},
				Props: []prop{
					{Name: "app", Type: "string", Description: "The client's name, shown to the operator; \"monbooru\" is reserved for the dedicated pairing flow"},
					{Name: "requested_scopes", Type: "array", Items: &prop{Type: "string"}, Description: "Scopes to ask for (read, write); an empty list grants read only"},
				},
			},
			Responses: []response{
				{Status: "200", Description: "Request registered, awaiting approval", Props: []prop{
					{Name: "request_id", Type: "string", Description: "Poll GET /api/v1/pair/status with this id"},
					{Name: "status", Type: "string", Description: "Always \"pending\""},
				}},
				{Status: "400", Description: "Missing app name, or the reserved name \"monbooru\"", Ref: "Error"},
				{Status: "409", Description: "Already paired with that app; remove the existing pairing first", Ref: "Error"},
				{Status: "429", Description: "Too many pending pairing requests", Ref: "Error"},
			},
			Handler: h.pair.Request,
		},
		{
			Method: "GET", Path: "/api/v1/pair/status",
			Summary: "Poll a pairing request", OperationID: "pairStatus", NoAuth: true,
			Description: "Reports pending / denied / approved. The first poll after approval mints the " +
				"client's token and returns the secret once; later polls answer approved with no token.",
			Params: []param{
				{Name: "id", In: "query", Required: true, Description: "The request_id from POST /api/v1/pair/request"},
			},
			Responses: []response{
				{Status: "200", Description: "Request state, with the token on the claiming poll", Props: []prop{
					{Name: "status", Type: "string", Description: "pending, denied, or approved"},
					{Name: "token", Type: "string", Description: "The bearer secret, present once, on the first poll after approval"},
				}},
				{Status: "404", Description: "Unknown pairing request", Ref: "Error"},
			},
			Handler: h.pair.Status,
		},
		{
			Method: "POST", Path: "/api/v1/pair/remove",
			Summary: "Drop a pairing from the client side", OperationID: "pairRemove", NoAuth: true,
			Description: "Authenticates with the paired token itself rather than the bearer gate, so one " +
				"\"remove pairing\" tears down both ends. monloader removes only locally and never calls back.",
			Responses: []response{
				{Status: "200", Description: "Pairing removed", Props: []prop{
					{Name: "status", Type: "string", Description: "Always \"removed\""},
				}},
				{Status: "401", Description: "The Authorization header is missing or is not a pairing token", Ref: "Error"},
			},
			Handler: h.pair.Teardown,
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
		{Name: "replaced", Type: "integer", Description: "In-place file replacements of an image monbooru already holds"},
		{Name: "skipped", Type: "integer", Description: "Posts the gallery-dl archive already had, or files monbooru cannot ingest"},
		{Name: "failed", Type: "integer"},
		{Name: "matched", Type: "integer", Description: "Hashes a batch PTR lookup answered with tags; true past the bound on the items the row keeps"},
		{Name: "canceled", Type: "integer", Description: "Items aborted by a job cancel, kept out of failed"},
		{Name: "total", Type: "integer"},
	}},
	{Name: "Item", Props: []prop{
		{Name: "post_id", Type: "string"},
		{Name: "num", Type: "integer", Description: "1-based pool page order"},
		{Name: "url", Type: "string", Description: "canonical source post page"},
		{Name: "status", Type: "string", Description: "pending, downloaded, uploaded, done, skipped, failed"},
		{Name: "outcome", Type: "string", Description: "created, duplicate, enriched, matched, replaced, skipped_archive, skipped_unsupported, failed"},
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
		{Name: "status", Type: "string", Description: "queued, running, succeeded, partial, failed, canceled, interrupted (was running when the process died; requeue to re-run)"},
		{Name: "kind", Type: "string", Description: "download (the default, omitted), metadata (a source refetch that enriches an existing image), lookup (a hash lookup that enriches an existing image), hash_import (an md5 resolved to a post and imported), replace (a post's file downloaded and pushed over an existing image's bytes), contrib (a PTR contribution send), or ptr_lookup (a batch PTR hash lookup answered in process)"},
		{Name: "image_id", Type: "integer", Description: "monbooru image a metadata, lookup, or replace job targets"},
		{Name: "backend", Type: "string", Description: "Lookup source (booru, ptr, or all) for a lookup job"},
		{Name: "md5", Type: "string", Description: "File md5 a lookup or hash import is keyed on"},
		{Name: "sha256", Type: "string", Description: "File sha256 a ptr lookup is keyed on"},
		{Name: "page_url", Type: "string", Description: "Page a direct-file send came from; a directlink item records it as its source link"},
		{Name: "site", Type: "string", Description: "gallery-dl category, set after resolve"},
		{Name: "gallery", Type: "string", Description: "target monbooru gallery"},
		{Name: "folder", Type: "string", Description: "destination subfolder under the gallery"},
		{Name: "max_items", Type: "integer", Description: "per-job item cap supplied at enqueue"},
		{Name: "force", Type: "boolean", Description: "Last/next run bypasses the gallery-dl archive (set by a forced retry)"},
		{Name: "summary", Ref: "Summary"},
		{Name: "note", Type: "string", Description: "The job's result line (a contribution send's commit summary); empty for other kinds"},
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
		{Name: "name", Type: "string", Description: "Display name from gallery-dl's supported-sites data"},
		{Name: "example", Type: "string"},
		{Name: "hosts", Type: "array", Items: &prop{Type: "string"}, Description: "The curated profile's host aliases (file CDNs, mirror instances); a registrable domain covers its subdomains"},
		{Name: "kind", Type: "string", Description: "booru, manga, other"},
		{Name: "curated", Type: "boolean", Description: "Has a mapping profile (shipped or user)"},
		{Name: "configured", Type: "boolean", Description: "Carries user-set site data or a custom profile (the settings tables' visibility rule)"},
		{Name: "custom_profile", Type: "boolean", Description: "A user profile file overrides the shipped mapping"},
		{Name: "auth", Type: "string", Description: "none, api_optional, api_required, username_password, cookies, oauth"},
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
			{Name: "blobs_done", Type: "integer", Description: "update blobs applied; over blobs_total this is the volume-weighted sync fraction (0 until the blob census is known)"},
			{Name: "blobs_total", Type: "integer", Description: "update blobs the server publishes"},
			{Name: "downloaded_bytes", Type: "integer", Description: "compressed bytes fetched this session"},
			{Name: "download_rate", Type: "integer", Description: "average fetch rate in bytes per second over the pass's network time, while syncing"},
			{Name: "process_rate", Type: "integer", Description: "rows replayed into the index per second of replay time, while syncing"},
			{Name: "last_applied_at", Type: "integer", Description: "unix time the last update this process committed was applied (absent until one lands)"},
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
		{Name: "contrib", Type: "object", Description: "contribution gate; absent while the PTR is off", Props: []prop{
			{Name: "account", Type: "boolean", Description: "a personal (non-public) access key is set; every contribution surface gates on this"},
			{Name: "banned", Type: "boolean", Description: "the account is banned; contribution surfaces hide, sync stays allowed"},
			{Name: "unsent", Type: "integer", Description: "staged or failed items awaiting a send"},
			{Name: "failed", Type: "integer", Description: "items whose last send failed, kept for retry"},
		}},
	}},
	{Name: "PTRTagResults", Props: []prop{
		{Name: "results", Type: "object", Description: "map of the queried tag to its PTR ideal, aliases, implications, and implied_by (monbooru form); known=false when the PTR does not have the tag"},
	}},
	{Name: "LookupStatus", Props: []prop{
		{Name: "daily_budget", Type: "integer", Description: "images per day a scheduled lookup may cover; 0 refuses them all"},
		{Name: "left_today", Type: "integer", Description: "budgeted lookups still acceptable before the next rollover"},
		{Name: "resets_at", Type: "integer", Description: "unix time of the next local-midnight rollover"},
		{Name: "chain", Type: "array", Items: &prop{Type: "string"}, Description: "the sources the booru walk would query, in order; empty means nothing is configured"},
	}},
	{Name: "PTRLookupResults", Props: []prop{
		{Name: "index", Type: "integer", Description: "the index's applied-update cursor at answer time"},
		{Name: "results", Type: "object", Description: "map of a matched sha256 to its monbooru-form tags; a hash the index does not hold is absent"},
	}},
	{Name: "PTRContribPreview", Props: []prop{
		{Name: "provisional", Type: "boolean", Description: "the index is still syncing; the diff may overstate what is new"},
		{Name: "to_add", Type: "array", Items: &prop{Type: "object", Props: []prop{
			{Name: "tag", Type: "string", Description: "the submitted monbooru-form tag"},
			{Name: "ptr", Type: "string", Description: "the exact PTR spelling a send would use; empty when ineligible"},
			{Name: "status", Type: "string", Description: "new, known, ineligible, filtered, or unsent"},
			{Name: "note", Type: "string", Description: "why the tag is ineligible or filtered"},
		}}},
		{Name: "ptr_only", Type: "array", Items: &prop{Type: "object", Props: []prop{
			{Name: "tag", Type: "string", Description: "monbooru form of a PTR-current tag the submitted list lacks"},
			{Name: "ptr", Type: "string", Description: "the raw PTR spelling a removal petition would target"},
			{Name: "petitionable", Type: "boolean", Description: "false when a removal for this tag and hash is already staged or awaiting janitor review"},
		}}},
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
					"description":  "Required on every endpoint except /health, /api/v1/openapi.json, /api/v1/docs, and the unauthenticated /api/v1/pair/* pairing bootstrap (not listed in this document). Use a named, scoped token from Settings -> Authentication (read: queue/sites; write: enqueue/manage); the API is disabled until at least one token exists.",
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
 h3 { font-size:13px; color:#8f8faa; margin:12px 0 4px; font-weight:normal; text-transform:uppercase; letter-spacing:0.5px; }
 a { color:#8592e4; text-decoration:none; }
 a:hover { text-decoration:underline; }
 code { font-family:inherit; }
 table { border-collapse:collapse; width:100%; margin:6px 0 10px; font-size:13px; }
 th, td { border:1px solid #2a2a36; padding:4px 8px; text-align:left; vertical-align:top; }
 th { color:#8f8faa; font-weight:normal; background:#16161c; }
 .muted { color:#8f8faa; font-size:12px; }
 .method { display:inline-block; padding:1px 6px; border:1px solid; font-weight:bold; margin-right:8px; font-size:12px; min-width:52px; text-align:center; }
 .method-get    { color:#22aa44; border-color:#22aa44; }
 .method-post   { color:#8592e4; border-color:#8592e4; }
 .method-delete { color:#e75c5c; border-color:#e75c5c; }
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
 <p style="color:#22aa44;border:1px solid #22aa44;padding:4px 8px;">An API token is configured: send <code>Authorization: Bearer &lt;token&gt;</code> (with a scope covering the method) on every endpoint except <code>/health</code>, <code>/api/v1/openapi.json</code>, <code>/api/v1/docs</code>, and the <code>/api/v1/pair/*</code> handshake below, which is unauthenticated by design - it is how a client obtains a token.</p>
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
