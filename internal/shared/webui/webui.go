// static/htmx.min.js is htmx 2.0.11, byte-identical to
// https://unpkg.com/htmx.org@2.0.11/dist/htmx.min.js
// (sha256 d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717).
// No manifest records it, so the version lives here or nowhere.
//
// static/wordmark.woff2 is Dela Gothic One (OFL, static/wordmark-OFL.txt)
// from https://github.com/google/fonts/raw/main/ofl/delagothicone/DelaGothicOne-Regular.ttf
// (sha256 4ff87a0965f1b0505e5a2c58424bc6ad3cff27e56a82f21c2fc9d6b0e3857ee2), cut with
// fonttools 4.67.0 to U+0020-007E,U+00A0-00FF: pyftsubset --layout-features=kern,liga
// --flavor=woff2 --no-hinting --desubroutinize.
//
// static/jetbrains-mono.woff2 and static/jetbrains-mono-bold.woff2 are JetBrains
// Mono 2.304 (OFL, static/jetbrains-mono-OFL.txt), fonts/ttf/JetBrainsMono-Regular.ttf
// (sha256 a0bf60ef0f83c5ed4d7a75d45838548b1f6873372dfac88f71804491898d138f) and
// fonts/ttf/JetBrainsMono-Bold.ttf
// (sha256 5590990c82e097397517f275f430af4546e1c45cff408bde4255dad142479dcb) from
// https://github.com/JetBrains/JetBrainsMono/releases/download/v2.304/JetBrainsMono-2.304.zip,
// cut with fonttools 4.67.0 to Latin-1 plus the UI's arrows and marks the font draws:
// pyftsubset --unicodes=U+0020-007E,U+00A0-00FF,U+03C9,U+2013-2014,U+2022,U+2026,
// U+2039-203A,U+2190-2194,U+2197,U+2261,U+2514,U+25B2,U+25B6,U+25B8,U+25BC,U+25BE,
// U+26A0,U+2713,U+2717 --layout-features=kern,liga,calt --flavor=woff2 --no-hinting
// --desubroutinize.
package webui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/url"
	"slices"
	"strings"
)

//go:embed static templates
var embedded embed.FS

var Static = mustSub(embedded, "static")

var Templates = mustSub(embedded, "templates")

// On Windows the registry's MIME types override Go's built-ins, and a .css
// registered as text/plain breaks the stylesheet. AddExtensionType loads the
// OS table first, so these win.
func init() {
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

func Funcs() template.FuncMap {
	return template.FuncMap{
		"browseURL":   BrowseURL,
		"humanBytes":  HumanBytes,
		"minutesLeft": MinutesLeft,
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
		"wordmark":    Wordmark,
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				k, _ := pairs[i].(string)
				m[k] = pairs[i+1]
			}
			return m
		},
	}
}

// ScopeRow is one checkbox of webui/token_privileges_dialog.
type ScopeRow struct {
	Name    string
	Desc    string
	Checked bool
}

func ScopeRows(all, granted []string, desc map[string]string) []ScopeRow {
	rows := make([]ScopeRow, 0, len(all))
	for _, sc := range all {
		rows = append(rows, ScopeRow{Name: sc, Desc: desc[sc], Checked: slices.Contains(granted, sc)})
	}
	return rows
}

func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func Wordmark(name string) template.HTML {
	if len(name) > 3 && strings.EqualFold(name[:3], "mon") {
		return template.HTML(template.HTMLEscapeString(name[:3]) + `<span class="wordmark-accent">` + template.HTMLEscapeString(name[3:]) + `</span>`)
	}
	return template.HTML(template.HTMLEscapeString(name))
}

// hx-get is not a URL attribute to html/template, which would only HTML-escape it.
func BrowseURL(route, into, path string) string {
	return route + "?" + url.Values{"into": {into}, "path": {path}}.Encode()
}

var ThemeVars = []string{
	"--bg", "--bg-surface", "--bg-input", "--border", "--fg", "--fg-dim",
	"--accent", "--link", "--success", "--warning", "--error",
	"--scrim", "--backdrop", "--shadow-block", "--font-mono", "--danger", "--on-danger", "--on-accent", "--peer",
	"--wordmark-font", "--wordmark-case", "--wordmark-tracking", "--wordmark-accent",
}

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
