package webui

import (
	"html/template"
	"regexp"
	"strings"
)

// SuggestItem is one row of a "webui/suggest_list"; Color is a category's
// hex colour, Alias the alias the row was found through.
type SuggestItem struct {
	Name        string
	Alias       string
	Color       string
	Description string
	UsageCount  int
	ShowCount   bool
}

// SuggestList is what "webui/suggest_list" renders. Attr names the data
// attribute that carries each row's name and OnClick what a click runs;
// both go out unescaped, so only ever set them from constants.
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
