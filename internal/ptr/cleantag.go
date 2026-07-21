package ptr

import (
	"regexp"
	"strings"
)

// CleanTag replicates the hydrus server's tag normalization
// (HydrusTags.CleanTag, v678). The server runs it on every ingested tag
// and its output is what gets stored, so the contribution path refuses
// any tag that does not survive it byte-for-byte identical: what we
// send is then exactly what the server stores, and the local diff never
// drifts against a silent server-side rewrite.
//
// The steps, in hydrus's order: cap at 1024 code points; lowercase;
// double a single leading colon (":D" -> "::D", a forced unnamespaced
// tag); then strip "gumpf" from the whole tag and, when a colon is
// present, from the namespace and subtag separately before recombining.
var (
	// Undesired control characters are removed before whitespace
	// handling, so a tab or newline vanishes rather than collapsing to
	// a space. The private-use planes and BOM are in the set.
	reTagControl = regexp.MustCompile(`[\x{0000}-\x{001F}\x{007F}-\x{009F}\x{200B}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{E000}-\x{F8FF}\x{F0000}-\x{FFFFD}\x{100000}-\x{10FFFD}]`)
	// Python's \s is unicode-aware; Go's is ASCII-only, so the class
	// spells out the unicode spaces (the ASCII controls in Python's \s
	// are already gone via reTagControl).
	reTagWhitespace  = regexp.MustCompile(`[\s\x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]+`)
	reLeadingGarbage = regexp.MustCompile(`^(-|system:)+`)
	// Matches ":D" but not ":weird:stuff" (Python uses a lookahead;
	// anchoring the whole string is equivalent).
	reLeadingSingleColon = regexp.MustCompile(`^:[^:]+$`)
	reLooksLikeHangul    = regexp.MustCompile(`[\x{1100}-\x{11FF}\x{AC00}-\x{D7AF}]`)
	reAllLatinAndZW      = regexp.MustCompile(`^[\x{0020}-\x{007E}\x{00A0}-\x{024F}\x{200C}\x{200D}]+$`)
	reZeroWidthJoiners   = regexp.MustCompile(`[\x{200C}\x{200D}]`)
	reAllZeroWidth       = regexp.MustCompile(`^[\x{200C}\x{200D}]+$`)
)

const hangulFiller = "ㅤ"

// stripTagGumpf is hydrus's StripTagTextOfGumpf.
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

// CleanTag returns the form the hydrus server would store for tag.
func CleanTag(tag string) string {
	if r := []rune(tag); len(r) > 1024 {
		tag = string(r[:1024])
	}
	t := strings.ToLower(tag)
	if reLeadingSingleColon.MatchString(t) {
		t = ":" + t
	}
	if strings.Contains(t, ":") {
		t = stripTagGumpf(t) // repeated to catch 'system:' before the split
		var ns, sub string
		if strings.Contains(t, ":") {
			ns, sub, _ = strings.Cut(t, ":")
		} else {
			sub = t // the strip ate the only colon (e.g. "system:x")
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

// TagOK reports whether tag is already in the server's stored form with
// a non-empty subtag - the validity gate the mapper and stage path use.
// A tag that fails is refused, never silently adjusted.
func TagOK(tag string) bool {
	if tag == "" || CleanTag(tag) != tag {
		return false
	}
	_, sub, has := strings.Cut(tag, ":")
	return !has || sub != ""
}
