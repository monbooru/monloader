// Package outcome holds the item outcomes and error codes; API clients
// branch on these strings.
package outcome

import "github.com/monbooru/monloader/internal/shared/contract"

type Outcome string

const (
	Created   Outcome = contract.OutcomeCreated
	Duplicate Outcome = contract.OutcomeDuplicate
	Enriched  Outcome = contract.OutcomeEnriched
	// Matched is a hash lookup that found tags; unlike Enriched nothing
	// was written, the caller applies them.
	Matched            Outcome = contract.OutcomeMatched
	Replaced           Outcome = contract.OutcomeReplaced
	SkippedArchive     Outcome = contract.OutcomeSkippedArchive
	SkippedUnsupported Outcome = contract.OutcomeSkippedUnsupported
	Failed             Outcome = contract.OutcomeFailed
)

const (
	ErrCodeDownloadFailed      = contract.CodeDownloadFailed
	ErrCodeUnsupportedURL      = contract.CodeUnsupportedURL
	ErrCodeAuthRequired        = contract.CodeAuthRequired
	ErrCodeBlocked             = contract.CodeBlocked
	ErrCodeRateLimited         = contract.CodeRateLimited
	ErrCodeNetworkUnreachable  = contract.CodeNetworkUnreachable
	ErrCodeFileTooLarge        = contract.CodeFileTooLarge
	ErrCodeMonbooruUnreachable = contract.CodeMonbooruUnreachable
	ErrCodeMonbooruRejected    = contract.CodeMonbooruRejected
	ErrCodeHashMismatch        = contract.CodeHashMismatch
	// ErrCodeAlreadyExists and ErrCodeWrongType are monbooru's 409 replace
	// refusals: the original already exists as another image, or the
	// target or the new file is not an image.
	ErrCodeAlreadyExists      = contract.CodeAlreadyExists
	ErrCodeWrongType          = contract.CodeWrongType
	ErrCodeMappingFailed      = contract.CodeMappingFailed
	ErrCodeCanceled           = contract.CodeCanceled
	ErrCodeHashNotFound       = contract.CodeHashNotFound
	ErrCodePTRUnavailable     = contract.CodePTRUnavailable
	ErrCodePTRAccountRequired = contract.CodePTRAccountRequired
	ErrCodePTRBanned          = contract.CodePTRBanned
	ErrCodePTRSyncing         = contract.CodePTRSyncing
)

type CodedError struct {
	Code string
	Msg  string
}

func (e *CodedError) Error() string { return e.Code + ": " + e.Msg }
