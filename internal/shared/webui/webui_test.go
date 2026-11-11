package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestVendoredFilesAreThePinnedBuilds(t *testing.T) {
	for name, want := range map[string]string{
		"htmx.min.js":    "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717",
		"wordmark.woff2": "3a06b9b5bf71110c6b102bebf4b28ccc92e57f7ed30d150fae5ac11540b2335f",
	} {
		b, err := fs.ReadFile(Static, name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s sha256 = %s", name, got)
		}
	}
}

func TestWordmarkSetsApartOnlyWhatFollowsMon(t *testing.T) {
	for name, want := range map[string]template.HTML{
		"monloader":    `mon<span class="wordmark-accent">loader</span>`,
		"Monbooru":     `Mon<span class="wordmark-accent">booru</span>`,
		"mon":          "mon",
		"My <gallery>": "My &lt;gallery&gt;",
		"mon<x>":       `mon<span class="wordmark-accent">&lt;x&gt;</span>`,
	} {
		if got := Wordmark(name); got != want {
			t.Errorf("Wordmark(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSharedFilesNameNoApp(t *testing.T) {
	err := fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(embedded, path)
		if err != nil {
			return err
		}
		for _, word := range []string{"monbooru", "monloader", "/internal/"} {
			if strings.Contains(strings.ToLower(string(b)), word) {
				t.Errorf("%s contains %q", path, word)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBaseCSSReadsOnlySharedTokens(t *testing.T) {
	b, err := fs.ReadFile(Static, "base.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(--[a-z0-9-]+)\s*:`).FindAllStringSubmatch(css, -1) {
		declared[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`var\((--[a-z0-9-]+)\s*(,?)`).FindAllStringSubmatch(css, -1) {
		if !slices.Contains(ThemeVars, m[1]) && !declared[m[1]] && m[2] == "" {
			t.Errorf("base.css reads %s, which is no shared token, is not declared there and has no fallback", m[1])
		}
	}
}

func TestBaseFilesAreServed(t *testing.T) {
	for _, name := range []string{"base.css", "base.js", "htmx.min.js", "wordmark.woff2"} {
		if _, err := fs.Stat(Static, name); err != nil {
			t.Error(err)
		}
	}
}

func TestTheSharedTemplatesParseWithTheirFuncs(t *testing.T) {
	tmpl, err := template.New("").Funcs(Funcs("")).ParseFS(Templates, "*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"webui/head", "webui/login_page", "webui/peer_light", "webui/message_page", "webui/dir_picker", "webui/confirm_dialog", "webui/auth_tokens", "webui/token_privileges_dialog", "webui/auth_password_section", "webui/theme_cluster", "webui/token_created", "webui/settings_section_header", "webui/dir_picker_dialog", "webui/token_scopes_saved", "webui/pair_notice", "webui/dialog_x"} {
		if tmpl.Lookup(name) == nil {
			t.Errorf("%s is not defined", name)
		}
	}
	if got := BrowseURL("/browse", "f", "/a b&c"); got != "/browse?into=f&path=%2Fa+b%26c" {
		t.Errorf("BrowseURL = %q", got)
	}
}

func TestScopeRowsFollowTheFullList(t *testing.T) {
	got := ScopeRows([]string{"read", "write"}, []string{"write", "gone"}, map[string]string{"read": "r"})
	want := []ScopeRow{{Name: "read", Desc: "r"}, {Name: "write", Checked: true}}
	if !slices.Equal(got, want) {
		t.Errorf("ScopeRows = %+v", got)
	}
}

func TestHumanBytesStepsByKibibytes(t *testing.T) {
	for b, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536 << 10: "1.5 MiB", 3 << 40: "3.0 TiB"} {
		if got := HumanBytes(b); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", b, got, want)
		}
	}
}

func TestTheStylesheetAndScriptTypesAreRegistered(t *testing.T) {
	for ext, want := range map[string]string{".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8"} {
		if got := mime.TypeByExtension(ext); got != want {
			t.Errorf("TypeByExtension(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestAStaticURLIsKeptOnlyUnderTheCurrentVersion(t *testing.T) {
	files := fstest.MapFS{"main.css": {Data: []byte("body{}")}}
	version := AssetVersion(files)
	h := StaticHandler(files, version)
	for target, want := range map[string]string{
		"/main.css?v=" + version: "max-age=31536000, immutable",
		"/main.css?v=0ld":        "no-cache",
		"/main.css":              "no-cache",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != want {
			t.Errorf("%s: %d %q, want 200 %q", target, w.Code, w.Header().Get("Cache-Control"), want)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/main.css", nil)
	r.Header.Set("If-None-Match", `"`+version+`"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Errorf("revalidation: %d, want 304", w.Code)
	}
	if AssetVersion(fstest.MapFS{"main.css": {Data: []byte("body{ }")}}) == version {
		t.Error("the version did not move with the bytes")
	}
}
