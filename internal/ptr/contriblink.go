package ptr

import (
	"encoding/hex"
	"net/url"
	"strings"
)

// ContribItemLabel renders a contribution's identity for a row: the mapping
// tag with a short hash, or the pair.
func ContribItemLabel(kind, tag, tag2 string, hash []byte) string {
	switch kind {
	case ContribMappingAdd, ContribMappingPetition:
		short := ""
		if len(hash) >= 4 {
			short = " @ " + hex.EncodeToString(hash[:4]) + ".."
		}
		return tag + short
	default:
		return tag + " => " + tag2
	}
}

// ContribQueueLabel phrases a contribution for a queue item row, leading with a
// word that names what it is so the subject reads without a separate kind
// column. The image the mapping hangs off is the row's link, not its text.
func ContribQueueLabel(kind, tag, tag2 string) string {
	switch kind {
	case ContribMappingAdd:
		return "tag " + tag
	case ContribMappingPetition:
		return "remove tag " + tag
	case ContribSibling:
		return "alias " + tag + " -> " + tag2
	case ContribSiblingPetition:
		return "remove alias " + tag + " -> " + tag2
	case ContribParent:
		return "implies " + tag + " -> " + tag2
	case ContribParentPetition:
		return "remove implies " + tag + " -> " + tag2
	}
	return tag
}

// ContribItemLink resolves a contribution's monbooru detail URL: the image by
// hash for mapping kinds, the tag the relation hangs off for pair kinds (the
// alias or the child, via the tags-page name filter). Empty when nothing
// resolves.
func ContribItemLink(base, kind, tag string, hash []byte) string {
	if base == "" {
		return ""
	}
	switch kind {
	case ContribMappingAdd, ContribMappingPetition:
		if len(hash) == 0 {
			return ""
		}
		return base + "/i/" + hex.EncodeToString(hash)
	default:
		if _, sub, hasNS := strings.Cut(tag, ":"); hasNS {
			tag = sub
		}
		name := strings.ReplaceAll(tag, " ", "_")
		if name == "" {
			return ""
		}
		return base + "/tags?q=" + url.QueryEscape(name)
	}
}
