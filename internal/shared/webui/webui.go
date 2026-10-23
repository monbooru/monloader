// static/htmx.min.js is htmx 2.0.11, byte-identical to
// https://unpkg.com/htmx.org@2.0.11/dist/htmx.min.js
// (sha256 d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717).
// No manifest records it, so the version lives here or nowhere.
package webui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/url"
	"slices"
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
}

func Funcs() template.FuncMap {
	return template.FuncMap{
		"browseURL":  BrowseURL,
		"humanBytes": HumanBytes,
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
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

// hx-get is not a URL attribute to html/template, which would only HTML-escape it.
func BrowseURL(route, into, path string) string {
	return route + "?" + url.Values{"into": {into}, "path": {path}}.Encode()
}

var ThemeVars = []string{
	"--bg", "--bg-surface", "--bg-input", "--border", "--fg", "--fg-dim",
	"--accent", "--link", "--success", "--warning", "--error",
	"--scrim", "--font-mono", "--danger", "--on-danger", "--peer",
}

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
