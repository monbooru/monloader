// Package outcome holds the item vocabulary: what happened to a post, and
// why it failed when it did. It is a leaf on purpose - the gallery-dl
// wrapper, the monbooru client and the similarity client all classify their
// failures into it, and none of them touches a queue, so the words live apart
// from the engine that carries them.
package outcome

// Outcome is the terminal result the UI and API branch on.
type Outcome string

const (
	Created   Outcome = "created"
	Duplicate Outcome = "duplicate"
	Enriched  Outcome = "enriched"
	// Matched is a batch hash lookup answering with an image's tags.
	// Distinct from enriched: nothing was written, the caller applies them.
	Matched            Outcome = "matched"
	Replaced           Outcome = "replaced"
	SkippedArchive     Outcome = "skipped_archive"
	SkippedUnsupported Outcome = "skipped_unsupported"
	Failed             Outcome = "failed"
)

// Error-code vocabulary. Stable strings the future browser
// extension can branch on; the gallery-dl wrapper, the monbooru client, and
// the pipeline all classify failures into these.
const (
	ErrCodeDownloadFailed      = "download_failed"
	ErrCodeUnsupportedURL      = "unsupported_url"
	ErrCodeAuthRequired        = "auth_required"
	ErrCodeBlocked             = "blocked"
	ErrCodeRateLimited         = "rate_limited"
	ErrCodeNetworkUnreachable  = "network_unreachable"
	ErrCodeFileTooLarge        = "file_too_large"
	ErrCodeMonbooruUnreachable = "monbooru_unreachable"
	ErrCodeMonbooruRejected    = "monbooru_rejected"
	ErrCodeHashMismatch        = "hash_mismatch"
	// Replace refusals monbooru answers with a 409: the downloaded original
	// already exists as another image, or the target row is an archive/video.
	ErrCodeAlreadyExists      = "already_exists"
	ErrCodeWrongType          = "wrong_type"
	ErrCodeMappingFailed      = "mapping_failed"
	ErrCodeCanceled           = "canceled"
	ErrCodeHashNotFound       = "hash_not_found"
	ErrCodePTRUnavailable     = "ptr_unavailable"
	ErrCodePTRAccountRequired = "ptr_account_required"
	ErrCodePTRBanned          = "ptr_banned"
	ErrCodePTRSyncing         = "ptr_syncing"
)

// CodedError pairs a failure message with a stable code from the vocabulary
// above. The gallery-dl wrapper and the monbooru client both return it; the
// pipeline pulls the code with errors.As to set the item/job outcome.
type CodedError struct {
	Code string
	Msg  string
}

func (e *CodedError) Error() string { return e.Code + ": " + e.Msg }
