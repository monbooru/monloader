package api

import (
	"github.com/monbooru/monloader/internal/outcome"

	"github.com/monbooru/monloader/internal/shared/contract"
)

// The error payload's code values: clients branch on them and the OpenAPI
// text quotes them, so never respell one.
const (
	errInvalidRequest = contract.CodeInvalidRequest
	errNotFound       = "not_found"
	errConflict       = "conflict"
	errNamespaceReq   = contract.CodeNamespaceRequired

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

// ErrSetupRequired is answered by the web first-run gate, in front of every
// route here.
const ErrSetupRequired = contract.CodeSetupRequired
