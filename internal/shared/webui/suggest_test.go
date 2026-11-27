package webui

import (
	"html/template"
	"strings"
	"testing"
)

func TestSuggestTemplates(t *testing.T) {
	tmpl := template.Must(template.New("").Funcs(Funcs("")).ParseFS(Templates, "suggest_list.html", "tags_input.html"))
	render := func(name string, data any) string {
		t.Helper()
		var b strings.Builder
		if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	input := map[string]any{"Route": "/r/suggest", "Name": "tags", "InputID": "t-in", "SuggestID": "t-dd", "ValsKey": "q"}
	filled := map[string]any{"Route": "/r/suggest", "Name": "tags", "InputID": "t-in", "SuggestID": "t-dd", "ValsKey": "q", "Value": `user_a "wip"`}
	cases := []struct {
		name          string
		tmpl          string
		data          any
		want, wantNot []string
	}{
		{"a tag row picks into the field", "webui/suggest_list",
			TagSuggestList([]SuggestItem{{Name: "artist:user_a", Color: "#AA0000", UsageCount: 12, ShowCount: true}}),
			[]string{`data-tag-name="artist:user_a"`, `onclick="applyTagSuggest(this)"`, `style="color:var(--cat-aa0000,#AA0000)"`, `<span class="suggest-count">12</span>`},
			[]string{`suggest-alias`}},
		{"a row found through an alias names it", "webui/suggest_list",
			TagSuggestList([]SuggestItem{{Name: "wip", Alias: "work_in_progress"}}),
			[]string{`<span class="suggest-alias">work_in_progress →</span> wip`}, []string{`suggest-count`, `style=`}},
		{"no rows render nothing", "webui/suggest_list", TagSuggestList(nil), nil, []string{`suggest-list`}},
		{"the field asks the app's route for the word at the caret", "webui/tags_input", input,
			[]string{`hx-get="/r/suggest"`, `data-multi-tags="1"`, `hx-target="#t-dd"`, `q: wordAtCaret(document.getElementById("t-in")).word`, `<div id="t-dd" class="suggest-dropdown tag-suggest-dropdown">`},
			[]string{`value=`, `required`}},
		{"it can open holding tags", "webui/tags_input", filled, []string{`value="user_a &#34;wip&#34;"`}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := render(c.tmpl, c.data)
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("unexpected %q in:\n%s", w, out)
				}
			}
		})
	}
}

func TestCategoryColor(t *testing.T) {
	cases := []struct {
		in   string
		want template.CSS
	}{
		{"#3D90E3", "var(--cat-3d90e3,#3D90E3)"},
		{"#abc", "var(--cat-abc,#abc)"},
		{"", "#888888"},
		{"red", "#888888"},
		{"#12345", "#888888"},
		{"#000000;background:url(x)", "#888888"},
	}
	for _, c := range cases {
		if got := CategoryColor(c.in); got != c.want {
			t.Errorf("CategoryColor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
