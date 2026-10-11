package ptr

import (
	"regexp"
	"strings"
)

var (
	reTagControl = regexp.MustCompile(`[\x{0000}-\x{001F}\x{007F}-\x{009F}\x{200B}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{E000}-\x{F8FF}\x{F0000}-\x{FFFFD}\x{100000}-\x{10FFFD}]`)
	// Python's \s is unicode-aware and Go's ASCII-only, hence the
	// spelled-out spaces; the ASCII controls in Python's \s are stripped
	// before this runs.
	reTagWhitespace      = regexp.MustCompile(`[\s\x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]+`)
	reLeadingGarbage     = regexp.MustCompile(`^(-|system:)+`)
	reLeadingSingleColon = regexp.MustCompile(`^:[^:]+$`)
	reLooksLikeHangul    = regexp.MustCompile(`[\x{1100}-\x{11FF}\x{AC00}-\x{D7AF}]`)
	reAllLatinAndZW      = regexp.MustCompile(`^[\x{0020}-\x{007E}\x{00A0}-\x{024F}\x{200C}\x{200D}]+$`)
	reZeroWidthJoiners   = regexp.MustCompile(`[\x{200C}\x{200D}]`)
	reAllZeroWidth       = regexp.MustCompile(`^[\x{200C}\x{200D}]+$`)
)

const hangulFiller = "ㅤ"

func stripTagGumpf(t string) string {
	t = reTagControl.ReplaceAllString(t, "")
	t = reTagWhitespace.ReplaceAllString(t, " ")
	t = strings.TrimSpace(t)
	t = reLeadingGarbage.ReplaceAllString(t, "")
	t = strings.TrimSpace(t)
	if !reLooksLikeHangul.MatchString(t) {
		t = strings.ReplaceAll(t, hangulFiller, "")
	}
	if reAllLatinAndZW.MatchString(t) {
		t = reZeroWidthJoiners.ReplaceAllString(t, "")
	}
	t = reAllZeroWidth.ReplaceAllString(t, "")
	t = reTagWhitespace.ReplaceAllString(t, " ")
	return strings.TrimSpace(t)
}

// cleanTag is hydrus's HydrusTags.CleanTag (v678), whose output the
// server stores.
func cleanTag(tag string) string {
	if r := []rune(tag); len(r) > 1024 {
		tag = string(r[:1024])
	}
	t := strings.ToLower(tag)
	if reLeadingSingleColon.MatchString(t) {
		t = ":" + t
	}
	if strings.Contains(t, ":") {
		t = stripTagGumpf(t) // repeated to catch 'system:' before the split
		ns, sub, found := strings.Cut(t, ":")
		if !found {
			ns, sub = "", t // the strip ate the only colon (e.g. "system:x")
		}
		ns = stripTagGumpf(ns)
		sub = stripTagGumpf(sub)
		switch {
		case ns == "" && strings.Contains(sub, ":"):
			t = ":" + sub
		case ns == "":
			t = sub
		default:
			t = ns + ":" + sub
		}
	} else {
		t = stripTagGumpf(t)
	}
	return t
}

// TagOK passes only a tag already in stored form: a failure is refused,
// never cleaned, so what is sent is what the server stores. The server
// raises on an empty subtag.
func TagOK(tag string) bool {
	if tag == "" || cleanTag(tag) != tag {
		return false
	}
	_, sub, has := strings.Cut(tag, ":")
	return !has || sub != ""
}
