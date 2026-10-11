// Package outcome holds the item outcomes and error codes; API clients
// branch on these strings.
package outcome

type Outcome string

const (
	Created   Outcome = "created"
	Duplicate Outcome = "duplicate"
	Enriched  Outcome = "enriched"
	// Matched is a hash lookup that found tags; unlike Enriched nothing
	// was written, the caller applies them.
	Matched            Outcome = "matched"
	Replaced           Outcome = "replaced"
	SkippedArchive     Outcome = "skipped_archive"
	SkippedUnsupported Outcome = "skipped_unsupported"
	Failed             Outcome = "failed"
)

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
	// ErrCodeAlreadyExists and ErrCodeWrongType are monbooru's 409 replace
	// refusals: the original already exists as another image, or the
	// target or the new file is not an image.
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

type CodedError struct {
	Code string
	Msg  string
}

func (e *CodedError) Error() string { return e.Code + ": " + e.Msg }
