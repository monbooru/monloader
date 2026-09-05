package mapping

import "slices"

// Extras is the metadata a client attached to a submission. It rides the job,
// so every item the job pushes carries it.
type Extras struct {
	Tags       []string
	Commentary string
	Source     string
	Via        string
}

// Empty reports whether there is nothing to merge.
func (e Extras) Empty() bool {
	return len(e.Tags) == 0 && e.Commentary == "" && e.Source == "" && e.Via == ""
}

// MergeExtras folds a client's metadata into what the mapper made of the post.
// What the post said about itself wins: its own commentary stands, and a source
// label survives whenever something chose it (sourceClaimed), so one booru
// cannot land under two names. A bare host nothing claims is the case the
// override exists for. Via has no post-side value to protect - the mapper
// stamps a constant - so a client naming itself always wins.
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

// appendExtraTags adds the client tags the mapped set does not already carry,
// normalized the way every other tag is. A profile's tag_rules are deliberately
// skipped: they correct the spellings one site emits, not a sender's.
func appendExtraTags(tags, extra []string) []string {
	if len(extra) == 0 {
		return tags
	}
	seen := make(map[string]bool, len(tags)+len(extra))
	for _, t := range tags {
		seen[t] = true
	}
	for _, raw := range extra {
		t := normalizeTag(raw)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	slices.Sort(tags)
	return tags
}
