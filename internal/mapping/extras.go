package mapping

import "slices"

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
	pf.Tags = appendExtraTags(pf.Tags, e.Tags)
	return pf
}

// tag_rules are skipped on purpose: they correct one site's spellings,
// not a sender's.
func appendExtraTags(tags, extra []string) []string {
	if len(extra) == 0 {
		return tags
	}
	for _, raw := range extra {
		if t := normalizeTag(raw); t != "" {
			tags = append(tags, t)
		}
	}
	slices.Sort(tags)
	return slices.Compact(tags)
}
