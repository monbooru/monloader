package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func isBuiltinCategory(c string) bool { return slices.Contains(BuiltinCategories, c) }

func TestTagNameRulesMeetTheVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "tagnames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Names, Forms []struct{ In, Out string } }
	if err := json.Unmarshal(b, &v); err != nil || len(v.Names) == 0 || len(v.Forms) == 0 {
		t.Fatalf("vectors = %+v, %v", v, err)
	}
	for _, c := range v.Names {
		if got := NormalizeTagName(c.In); got != c.Out {
			t.Errorf("NormalizeTagName(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range append(v.Names, v.Forms...) {
		if got := NormalizeTagForm(c.In, isBuiltinCategory); got != c.Out {
			t.Errorf("NormalizeTagForm(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestFoldTagNameKeepsWhatItIsTold(t *testing.T) {
	cases := []struct{ in, keep, want string }{
		{`a "b" *c`, `"*`, `a_"b"_*c`},
		{`a "b" *c`, `*`, `a_b_*c`},
		{`a "b" *c`, ``, `a_b_c`},
		{"_lead ", `"*`, "_lead"},
	}
	for _, c := range cases {
		if got := FoldTagName(c.in, c.keep); got != c.want {
			t.Errorf("FoldTagName(%q, %q) = %q, want %q", c.in, c.keep, got, c.want)
		}
	}
	if got := FoldTagForm("Artist: * (VA)", "*", isBuiltinCategory); got != "artist:*_(va)" {
		t.Errorf("FoldTagForm keeping * = %q", got)
	}
}

func TestTagForm(t *testing.T) {
	cases := []struct{ category, name, want string }{
		{"general", "solo", "solo"},
		{"artist", "user_a", "artist:user_a"},
		{"", "solo", ""},
		{"artist", "", ""},
	}
	for _, c := range cases {
		if got := TagForm(c.category, c.name); got != c.want {
			t.Errorf("TagForm(%q, %q) = %q, want %q", c.category, c.name, got, c.want)
		}
	}
}

func TestCutTagCategory(t *testing.T) {
	var asked []string
	known := func(c string) bool { asked = append(asked, c); return c == "artist" }
	cases := []struct {
		in, category, name string
		ok                 bool
	}{
		{" Artist:Foo ", "artist", "Foo", true},
		{"nier:automata", "", "nier:automata", false},
		{":3", "", ":3", false},
		{"plain", "", "plain", false},
	}
	for _, c := range cases {
		category, name, ok := CutTagCategory(c.in, known)
		if category != c.category || name != c.name || ok != c.ok {
			t.Errorf("CutTagCategory(%q) = %q, %q, %v; want %q, %q, %v", c.in, category, name, ok, c.category, c.name, c.ok)
		}
	}
	if !slices.Equal(asked, []string{"artist", "nier"}) {
		t.Errorf("asked about %q, want only the non-empty prefixes", asked)
	}
	if _, name, ok := CutTagCategory("artist:foo", nil); ok || name != "artist:foo" {
		t.Errorf("a nil isCategory split %q", name)
	}
}
