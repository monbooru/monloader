package mapping

import (
	"strings"

	"github.com/monbooru/monloader/internal/shared/contract"
)

const (
	RatingGeneral      = contract.RatingGeneral
	RatingSensitive    = contract.RatingSensitive
	RatingQuestionable = contract.RatingQuestionable
	RatingExplicit     = contract.RatingExplicit
)

func Stricter(a, b string) string {
	if contract.RatingRank(b) > contract.RatingRank(a) {
		return b
	}
	return a
}

// `s` is family dependent, so it stays out of this table.
var ratingWords = map[string]string{
	"general":      RatingGeneral,
	"sensitive":    RatingSensitive,
	"questionable": RatingQuestionable,
	"explicit":     RatingExplicit,
	"safe":         RatingGeneral,
	"suggestive":   RatingSensitive, // philomena and mangadex term for the sensitive tier
	"g":            RatingGeneral,
	"q":            RatingQuestionable,
	"e":            RatingExplicit,
}

// An unrecognized value returns "": an unset rating is visible under
// every monbooru ceiling.
func mapRating(family, raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "s" {
		if family == FamilyDanbooru {
			return RatingSensitive
		}
		return RatingGeneral // e621, moebooru, gelbooru_v02, generic: s = safe
	}
	return ratingWords[v]
}

func philomenaRatingTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "safe", "suggestive", "questionable", "explicit":
		return true
	}
	return false
}

// Returns the raw tag, so user and profile overrides still apply.
func philomenaRating(meta map[string]any) string {
	for _, t := range parseTagField(meta["tags"]) {
		if philomenaRatingTag(t) {
			return strings.ToLower(t)
		}
	}
	return ""
}
