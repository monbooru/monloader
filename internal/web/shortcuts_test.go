package web

import (
	"html"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	webFS "github.com/monbooru/monloader/web"
)

func TestShortcutSheetMatchesTheBoundKeys(t *testing.T) {
	b, err := fs.ReadFile(webFS.FS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	layout := string(b)
	sheet := layout[strings.Index(layout, `<dialog id="shortcuts-help">`):]
	sheet = sheet[:strings.Index(sheet, "</dialog>")]
	js, err := fs.ReadFile(webFS.FS, "static/main.js")
	if err != nil {
		t.Fatal(err)
	}

	var links []string
	for _, m := range regexp.MustCompile(`data-chord="([^"]+)"`).FindAllStringSubmatch(layout, -1) {
		links = append(links, m[1])
	}
	var rows []string
	for _, row := range regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`).FindAllStringSubmatch(sheet, -1) {
		var keys []string
		for _, k := range regexp.MustCompile(`<kbd>([^<]+)</kbd>`).FindAllStringSubmatch(row[1], -1) {
			keys = append(keys, html.UnescapeString(k[1]))
		}
		if len(keys) == 2 && keys[0] == "g" {
			rows = append(rows, strings.Join(keys, " "))
			continue
		}
		for _, k := range keys {
			if k == "Esc" {
				k = "Escape"
			}
			if !strings.Contains(string(js), `e.key === "`+k+`"`) {
				t.Errorf("sheet lists %q, which main.js does not bind", k)
			}
		}
	}
	slices.Sort(links)
	slices.Sort(rows)
	if len(links) == 0 || !slices.Equal(links, rows) {
		t.Errorf("data-chord links %q, sheet chord rows %q", links, rows)
	}
}

func TestLandingCarriesTheSheetsChords(t *testing.T) {
	b, err := fs.ReadFile(webFS.FS, "templates/add.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, chord := range []string{"g q", "g w", "g p", "g s"} {
		if !strings.Contains(string(b), `data-chord="`+chord+`"`) {
			t.Errorf("the landing has no %q link, though its sheet lists the chord", chord)
		}
	}
}
