// Package contract spells the words monbooru and monloader exchange over
// /api/v1 once, for both. Respelling one is a contract break: it moves
// both apps' APIVersion.
package contract

// Item outcomes of a monloader job.
const (
	OutcomeCreated            = "created"
	OutcomeDuplicate          = "duplicate"
	OutcomeEnriched           = "enriched"
	OutcomeMatched            = "matched"
	OutcomeReplaced           = "replaced"
	OutcomeSkippedArchive     = "skipped_archive"
	OutcomeSkippedUnsupported = "skipped_unsupported"
	OutcomeFailed             = "failed"
)

// Error codes a failed item, or a fetch report to monbooru, carries.
const (
	CodeDownloadFailed      = "download_failed"
	CodeUnsupportedURL      = "unsupported_url"
	CodeAuthRequired        = "auth_required"
	CodeBlocked             = "blocked"
	CodeRateLimited         = "rate_limited"
	CodeNetworkUnreachable  = "network_unreachable"
	CodeFileTooLarge        = "file_too_large"
	CodeMonbooruUnreachable = "monbooru_unreachable"
	CodeMonbooruRejected    = "monbooru_rejected"
	CodeHashMismatch        = "hash_mismatch"
	CodeAlreadyExists       = "already_exists"
	CodeWrongType           = "wrong_type"
	CodeMappingFailed       = "mapping_failed"
	CodeCanceled            = "canceled"
	CodeHashNotFound        = "hash_not_found"
	CodePTRUnavailable      = "ptr_unavailable"
	CodePTRAccountRequired  = "ptr_account_required"
	CodePTRBanned           = "ptr_banned"
	CodePTRSyncing          = "ptr_syncing"
)

// Job statuses.
const (
	JobQueued      = "queued"
	JobRunning     = "running"
	JobSucceeded   = "succeeded"
	JobPartial     = "partial"
	JobFailed      = "failed"
	JobCanceled    = "canceled"
	JobInterrupted = "interrupted"
)

// KindLookup is the job kind a lookup's fetch report names.
const KindLookup = "lookup"

// A lookup trail names each backend it tried; monbooru splits the trail on
// these.
const (
	TrailPTR      = "Public Tag Repository"
	TrailIQDB     = "iqdb"
	TrailSauceNAO = "saucenao"
)

// Via is the origin monloader stamps on a pushed image and the tagger name
// on its tags; SourcePTR is the source label of tags the PTR supplied.
const (
	Via       = "monloader"
	SourcePTR = "ptr"
)
