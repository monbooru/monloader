package gdl

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/config"
)

func TestLogfileOptionAsksForADebugFile(t *testing.T) {
	opt, ok := strings.CutPrefix(logfileOption("/tmp/x.log"), "output.logfile=")
	if !ok {
		t.Fatalf("logfileOption = %q, want the output.logfile key", opt)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(opt), &got); err != nil {
		t.Fatalf("value is not JSON: %v", err)
	}
	if got["path"] != "/tmp/x.log" || got["level"] != "debug" || got["mode"] != "w" {
		t.Errorf("logfile option = %v, want the path at debug level", got)
	}
}

func debugStub(t *testing.T, logBody, stderrBody string) *Tool {
	t.Helper()
	dir := t.TempDir()
	_, cfg := stubGDL(t, dir, `#!/bin/sh
for a in "$@"; do
  case "$a" in
    output.logfile=*) printf '%s' "${a#output.logfile=}" | sed 's/.*"path": *"\([^"]*\)".*/\1/' > "$0.path" ;;
  esac
done
printf '%s' '`+logBody+`' > "$(cat "$0.path")"
printf '%s' '`+stderrBody+`' >&2
exit 1
`)
	cfg.GalleryDL.ConfigPath = ""
	return New(config.NewProvider(cfg), SiteLists{})
}

func TestDebugLogAppendsAStderrOnlyError(t *testing.T) {
	tool := debugStub(t, "[extractor][debug] resolving\n", "[extractor][error] KeyError")
	got, _, err := tool.DebugLog(context.Background(), "https://site.example/post/1", t.TempDir())
	if err != nil {
		t.Fatalf("DebugLog: %v", err)
	}
	if !strings.Contains(got, "[extractor][debug] resolving") {
		t.Errorf("log = %q, want the debug lines gallery-dl wrote", got)
	}
	if !strings.Contains(got, "[extractor][error] KeyError") {
		t.Errorf("log = %q, want the stderr error appended", got)
	}
}

func TestDebugLogDoesNotRepeatAnErrorTheFileHas(t *testing.T) {
	const errLine = "[gallery-dl][error] Unsupported URL"
	tool := debugStub(t, "[gallery-dl][debug] starting\n"+errLine+"\n", errLine)
	got, _, err := tool.DebugLog(context.Background(), "https://site.example/post/1", t.TempDir())
	if err != nil {
		t.Fatalf("DebugLog: %v", err)
	}
	if n := strings.Count(got, errLine); n != 1 {
		t.Errorf("error appears %d times in %q, want once", n, got)
	}
}

func TestBoundLogKeepsBothEnds(t *testing.T) {
	long := strings.Repeat("a", 10) + strings.Repeat("b", maxDebugLog) + strings.Repeat("z", 10)
	got := boundLog(long)
	if len(got) > maxDebugLog {
		t.Errorf("bounded length = %d, want at most %d", len(got), maxDebugLog)
	}
	if !strings.HasPrefix(got, "aaaaaaaaaa") || !strings.HasSuffix(got, "zzzzzzzzzz") {
		t.Errorf("bounded log dropped an end: %.20q ... %.20q", got, got[len(got)-20:])
	}
	if !strings.Contains(got, "trimmed") {
		t.Errorf("bounded log = %.40q..., want the trim marker", got)
	}
	if short := "small"; boundLog(short) != short {
		t.Errorf("boundLog(%q) = %q, want it untouched", short, boundLog(short))
	}
}

func TestBoundLogCutsOnRuneBoundaries(t *testing.T) {
	long := strings.Repeat("e\u0301", maxDebugLog)
	got := boundLog(long)
	if len(got) > maxDebugLog {
		t.Errorf("bounded length = %d, want at most %d", len(got), maxDebugLog)
	}
	if !utf8.ValidString(got) {
		t.Errorf("boundLog split a rune: %q...%q", got[:20], got[len(got)-20:])
	}
}
