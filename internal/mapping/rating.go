package mapping

import "strings"

const (
	RatingGeneral      = "general"
	RatingSensitive    = "sensitive"
	RatingQuestionable = "questionable"
	RatingExplicit     = "explicit"
)

var ratingRanks = map[string]int{
	RatingGeneral:      0,
	RatingSensitive:    1,
	RatingQuestionable: 2,
	RatingExplicit:     3,
}

func RatingRank(level string) int {
	if r, ok := ratingRanks[level]; ok {
		return r
	}
	return -1
}

func Stricter(a, b string) string {
	if RatingRank(b) > RatingRank(a) {
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
