package webui

import (
	"html/template"
	"regexp"
	"strings"
)

type SuggestItem struct {
	Name        string
	Alias       string
	Color       string
	Description string
	UsageCount  int
	ShowCount   bool
}

// Attr and OnClick go out unescaped, so only ever set them from constants.
type SuggestList struct {
	Attr    template.HTMLAttr
	OnClick template.HTMLAttr
	Items   []SuggestItem
}

// TagSuggestList is the answer to a "webui/tags_input": a click puts the tag
// in place of the word at the caret. That template takes Name, InputID,
// SuggestID, Route (the suggest endpoint), ValsKey (the parameter the word
// goes out as), and optionally Value, Placeholder, Class and Required; its
// parent places the list, so needs position: relative.
func TagSuggestList(items []SuggestItem) SuggestList {
	return SuggestList{Attr: "data-tag-name", OnClick: `onclick="applyTagSuggest(this)"`, Items: items}
}

var categoryColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// Keyed on the colour, not the category, so a theme can remap the shipped
// palette while an operator's own colour falls through. template.CSS to
// pass the style-attribute sanitizer: only a validated colour gets in.
func CategoryColor(color string) template.CSS {
	if !categoryColorRe.MatchString(color) {
		return "#888888"
	}
	return template.CSS("var(--cat-" + strings.ToLower(strings.TrimPrefix(color, "#")) + "," + color + ")")
}
