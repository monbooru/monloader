package api

import "github.com/monbooru/monloader/internal/outcome"

// The error payload's code values: clients branch on them and the OpenAPI
// text quotes them, so never respell one.
const (
	errInvalidRequest    = "invalid_request"
	errNotFound          = "not_found"
	errConflict          = "conflict"
	errNamespaceReq      = "namespace_required"
	errUnauthorized      = "unauthorized"
	errInsufficientScope = "insufficient_scope"
	errAPIDisabled       = "api_disabled"

	errMonbooruUnconfigured = "monbooru_unconfigured"
	errMonbooruPaused       = "monbooru_paused"
	errBudgetExhausted      = "budget_exhausted"
	errAlreadyRunning       = "already_running"
	errAccountExists        = "account_exists"
	errPTRVelocity          = "ptr_velocity"
	errPTRError             = "ptr_error"
	errNotRescindable       = "not_rescindable"

	// Shared with the item codes: a lookup or a contribution send refused
	// up front and one that fails mid-job report the same string.
	errPTRUnavailable     = outcome.ErrCodePTRUnavailable
	errPTRSyncing         = outcome.ErrCodePTRSyncing
	errPTRAccountRequired = outcome.ErrCodePTRAccountRequired
	errPTRBanned          = outcome.ErrCodePTRBanned

	errInternal   = "internal_error"
	errSaveFailed = "save_failed"
)
