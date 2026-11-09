package mapping

import (
	"slices"
	"strings"
)

// Extras is a client's metadata for a submission; every item the job
// pushes carries it.
type Extras struct {
	Tags       []string
	Commentary string
	Source     string
	Via        string
}

func (e Extras) Empty() bool {
	return len(e.Tags) == 0 && e.Commentary == "" && e.Source == "" && e.Via == ""
}

// The post's own commentary and any claimed source label win, so one
// booru cannot land under two names; Via is only the mapper's constant,
// so a client's always wins.
func MergeExtras(pf PushFields, e Extras, sourceClaimed bool) PushFields {
	if e.Via != "" {
		pf.Via = e.Via
	}
	if e.Commentary != "" && pf.Commentary == "" {
		pf.Commentary = e.Commentary
	}
	if e.Source != "" && !sourceClaimed {
		pf.Source = e.Source
	}
	pf.ClientTags = clientTags(e.Tags, pf.Rating != "")
	return pf
}

// tag_rules are skipped on purpose: they correct one site's spellings, not a
// sender's. What a post says of its own rating wins, since monbooru's rating
// ceiling hangs on it.
func clientTags(raw []string, rated bool) []string {
	var tags []string
	for _, r := range raw {
		t := normalizeTag(r)
		if t == "" || (rated && strings.HasPrefix(t, "rating:")) {
			continue
		}
		tags = append(tags, t)
	}
	slices.Sort(tags)
	return slices.Compact(tags)
}
