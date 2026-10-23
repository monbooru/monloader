// Package contract spells the words monbooru and monloader exchange over
// /api/v1 once, for both. Respelling one is a contract break: it moves
// both apps' APIVersion.
package contract

import (
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

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

const CodeSetupRequired = "setup_required"

const (
	AppMonbooru  = "monbooru"
	AppMonloader = "monloader"
)

// Each app's default address for its peer assumes the other's default port.
const (
	PortMonbooru  = "8455"
	PortMonloader = "8456"
)

const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

const (
	LookupBooru = "booru"
	LookupPTR   = "ptr"
	LookupAll   = "all"
)

// Trail entries are "<backend>: <reason>", joined by TrailSeparator.
const (
	TrailSeparator = "; "
	TrailMatch     = ": match"
	TrailSkipped   = "skipped"
)

const PTRReady = "ready"

const (
	MaxPTRHashes      = 100
	MaxPTRTags        = 500
	PTRSearchLimit    = 20
	MaxPTRSearchLimit = 50
)

const SearchContains = "contains"

const (
	ContribMappingAdd      = "mapping_add"
	ContribMappingPetition = "mapping_petition"
	ContribSibling         = "sibling"
	ContribParent          = "parent"
	ContribSiblingPetition = "sibling_petition"
	ContribParentPetition  = "parent_petition"
)

const ContribStaged = "staged"

const (
	PairSuggest  = "suggest"
	PairPetition = "petition"
	PairPending  = "pending"
	PairConflict = "conflict"
	PairCovered  = "covered"
)

const (
	PreviewNew    = "new"
	PreviewKnown  = "known"
	PreviewUnsent = "unsent"
)

// ContribSendBudget bounds monbooru's wait for a contribution send.
const ContribSendBudget = 10 * time.Second

// Field caps in runes: monbooru refuses a longer value.
const (
	MaxCollection  = 200
	MaxCommentary  = 10000
	MaxDText       = 40000
	MaxOriginal    = 2048
	MaxSourceLabel = 200
	MaxVia         = 200
	MaxPostExt     = 16
	MaxMD5         = 64
	MaxPostID      = 64
	MaxURL         = 2048
)

// monbooru selects the tag-source group with [data-source="<via>"], so
// whitespace or a quote there breaks the selector.
func ValidateVia(via string) error {
	if utf8.RuneCountInString(via) > MaxVia {
		return fmt.Errorf("via must be %d characters or less", MaxVia)
	}
	for _, r := range via {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '<', '>', '[', ']', '\\':
			return fmt.Errorf("via must not contain whitespace or any of: \" ' < > [ ] \\")
		}
	}
	return nil
}

type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Every field is optional: an older peer may send none of them.
type Health struct {
	App              string `json:"app,omitempty"`
	Status           string `json:"status"`
	Version          string `json:"version"`
	GalleryDLVersion string `json:"gallerydl_version,omitempty"`
	API              int    `json:"api,omitempty"`
	MinPeerAPI       int    `json:"min_peer_api,omitempty"`
}

// APIMismatch is empty when self and the peer can work together, or when
// the peer predates the fields.
func (h Health) APIMismatch(self string, api, minPeer int) string {
	if h.API == 0 || (h.API >= minPeer && h.MinPeerAPI <= api) {
		return ""
	}
	return fmt.Sprintf("api mismatch: it speaks api %d and drives %d or newer, this %s speaks %d and drives %d or newer",
		h.API, h.MinPeerAPI, self, api, minPeer)
}

const (
	RatingGeneral      = "general"
	RatingSensitive    = "sensitive"
	RatingQuestionable = "questionable"
	RatingExplicit     = "explicit"
)

// RatingLevels runs low to high; highest-wins resolution and the rating
// ceiling rely on the order.
var RatingLevels = []string{RatingGeneral, RatingSensitive, RatingQuestionable, RatingExplicit}

// RatingRank is -1 for a word that is not a rating level.
func RatingRank(level string) int { return slices.Index(RatingLevels, level) }

var BuiltinCategories = []string{
	"general", "character", "artist", "copyright", "meta",
	"rating", "medium", "person", "year", "species",
}
