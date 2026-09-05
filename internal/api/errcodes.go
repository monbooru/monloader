package api

import "github.com/monbooru/monloader/internal/outcome"

// The request-error vocabulary, the second of the two the app publishes. This
// one answers "why was this call refused"; internal/outcome's answers "what
// happened to this item", and the two meet only where a request is refused
// for a reason an item can also carry - the two PTR codes below, which are the
// item vocabulary's constants rather than new spellings of them.
//
// Both are contract: they ride in the error payload's `code`, the OpenAPI
// response descriptions quote them, and the browser extension branches on
// them. A new one belongs here, not at the call site.
const (
	// The request itself: shape, field, or value.
	errInvalidRequest    = "invalid_request"
	errNotFound          = "not_found"
	errConflict          = "conflict"
	errNamespaceReq      = "namespace_required"
	errUnauthorized      = "unauthorized"
	errInsufficientScope = "insufficient_scope"
	errAPIDisabled       = "api_disabled"

	// What the install cannot do right now.
	errMonbooruUnconfigured = "monbooru_unconfigured"
	errMonbooruPaused       = "monbooru_paused"
	errBudgetExhausted      = "budget_exhausted"
	errAlreadyRunning       = "already_running"
	errAccountExists        = "account_exists"
	errPTRVelocity          = "ptr_velocity"
	errPTRError             = "ptr_error"

	// The PTR backend's two states, shared with the item vocabulary: a lookup
	// refused at enqueue and one that failed mid-job report the same string.
	errPTRUnavailable = outcome.ErrCodePTRUnavailable
	errPTRSyncing     = outcome.ErrCodePTRSyncing

	// Something on this side went wrong.
	errInternal   = "internal_error"
	errSaveFailed = "save_failed"
)
