package contract

import (
	"strings"
	"unicode"
)

// The tag name rules both apps spell by, so a tag one sends is the tag the
// other holds; testdata/tagnames.json pins them.

const tagReserved = `"*`

// keep lists the reserved runes that stay as written. An '_' typed at
// either end stays: trimming is the caller's.
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

// A name as it is stored and sent. It may return "".
func NormalizeTagName(name string) string { return strings.Trim(FoldTagName(name, ""), "_") }

// A prefix isCategory does not know ("nier:automata") stays part of the
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

func TagForm(category, name string) string {
	switch {
	case category == "" || name == "":
		return ""
	case category == "general":
		return name
	}
	return category + ":" + name
}

func FoldTagForm(form, keep string, isCategory func(string) bool) string {
	category, name, ok := CutTagCategory(form, isCategory)
	name = strings.Trim(FoldTagName(name, keep), "_")
	if !ok {
		return name
	}
	return TagForm(category, name)
}

// A form as it is stored and sent.
func NormalizeTagForm(form string, isCategory func(string) bool) string {
	return FoldTagForm(form, "", isCategory)
}
