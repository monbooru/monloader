package contract

import (
	"strings"
	"unicode"
)

// The tag name rules both apps spell by, so a tag one sends is the tag the
// other holds; testdata/tagnames.json pins them.

// The runes the tag language reads: a quote and the wildcard.
const tagReserved = `"*`

// FoldTagName lowercases a name, drops control, format and private-use
// runes, and folds each run of spaces and reserved runes between words to
// one '_'. keep lists the reserved runes that stay as written. An '_' typed
// at either end stays: trimming is the caller's.
func FoldTagName(name, keep string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(name))
	pendingFold := false
	for _, r := range name {
		switch {
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co):
		case unicode.IsSpace(r) || (strings.ContainsRune(tagReserved, r) && !strings.ContainsRune(keep, r)):
			pendingFold = true
		default:
			if pendingFold && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingFold = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NormalizeTagName is a name as it is stored and sent: every reserved rune
// folded and no '_' at either end. It may return "".
func NormalizeTagName(name string) string { return strings.Trim(FoldTagName(name, ""), "_") }

// CutTagCategory splits a form at its first colon when isCategory knows the
// prefix, lowercased; any other prefix ("nier:automata") stays part of the
// name. isCategory is asked only of a non-empty prefix; nil knows none.
func CutTagCategory(form string, isCategory func(string) bool) (category, name string, ok bool) {
	form = strings.TrimSpace(form)
	prefix, rest, cut := strings.Cut(form, ":")
	if !cut || prefix == "" || isCategory == nil {
		return "", form, false
	}
	if category = strings.ToLower(prefix); !isCategory(category) {
		return "", form, false
	}
	return category, rest, true
}

// TagForm spells a category and a name as the tag language does: the bare
// name for general, "" when either is missing.
func TagForm(category, name string) string {
	switch {
	case category == "" || name == "":
		return ""
	case category == "general":
		return name
	}
	return category + ":" + name
}

// FoldTagForm folds a category:name form: the category split off as
// CutTagCategory reads it, the name folded keeping keep and trimmed, and
// general left bare. A form whose name folds away is "".
func FoldTagForm(form, keep string, isCategory func(string) bool) string {
	category, name, ok := CutTagCategory(form, isCategory)
	name = strings.Trim(FoldTagName(name, keep), "_")
	if !ok {
		return name
	}
	return TagForm(category, name)
}

// NormalizeTagForm is a form as it is stored and sent.
func NormalizeTagForm(form string, isCategory func(string) bool) string {
	return FoldTagForm(form, "", isCategory)
}
