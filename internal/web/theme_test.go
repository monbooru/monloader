package web

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/shared/webui"
	webFS "github.com/monbooru/monloader/web"
)

func TestMainCSSDefinesTheSharedTokens(t *testing.T) {
	b, err := fs.ReadFile(webFS.FS, "static/main.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	root := css[strings.Index(css, ":root {"):]
	root = root[:strings.Index(root, "}")]
	declRe := regexp.MustCompile(`(--[a-z0-9-]+)\s*:`)
	var rootDecls []string
	for _, m := range declRe.FindAllStringSubmatch(root, -1) {
		rootDecls = append(rootDecls, m[1])
	}
	for _, v := range webui.ThemeVars {
		if !slices.Contains(rootDecls, v) {
			t.Errorf("main.css :root leaves the shared token %s undefined", v)
		}
	}
	defined := map[string]bool{}
	for _, m := range declRe.FindAllStringSubmatch(css, -1) {
		defined[m[1]] = true
	}
	for _, v := range webui.ThemeVars {
		defined[v] = true
	}
	for _, m := range regexp.MustCompile(`var\((--[a-z0-9-]+)`).FindAllStringSubmatch(css, -1) {
		if !defined[m[1]] {
			t.Errorf("main.css reads %s, which neither it nor the shared tokens define", m[1])
		}
	}
}

func TestQueueColoursEveryLandedOutcomeAsSuccess(t *testing.T) {
	b, err := fs.ReadFile(webFS.FS, "static/main.css")
	if err != nil {
		t.Fatal(err)
	}
	var success string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, ".st-succeeded,") {
			success = line
		}
	}
	for _, class := range []string{".o-created", ".o-enriched", ".o-matched", ".o-replaced"} {
		if !strings.Contains(success, class+",") && !strings.Contains(success, class+" {") {
			t.Errorf("%s is not in the success rule %q", class, success)
		}
	}
}
