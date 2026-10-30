package ptr

import (
	"encoding/hex"
	"net/url"
	"strings"
)

func ContribItemLabel(kind, tag, tag2 string, hash []byte) string {
	switch kind {
	case ContribMappingAdd, ContribMappingPetition:
		short := ""
		if len(hash) >= 4 {
			short = " @ " + hex.EncodeToString(hash[:4]) + ".."
		}
		return tag + short
	case ContribSibling, ContribSiblingPetition:
		return tag + " -> " + tag2
	default:
		return tag + " => " + tag2
	}
}

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
		return "implies " + tag + " => " + tag2
	case ContribParentPetition:
		return "remove implies " + tag + " => " + tag2
	}
	return tag
}

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
