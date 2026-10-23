package gdl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/outcome"
)

// The head names the extractor gallery-dl chose and the tail holds the
// traceback, so both ends survive the cut.
const maxDebugLog = 256 << 10

// The download pass, not a resolve: most failures happen fetching the
// file. noExtractor means gallery-dl matched no extractor, so monloader
// fetched that file itself and the log's refusal is not what happened.
func (t *Tool) DebugLog(ctx context.Context, url, workRoot string) (log string, noExtractor bool, err error) {
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return "", false, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	dir, err := os.MkdirTemp(workRoot, "debug-")
	if err != nil {
		return "", false, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "debug.log")

	// archive= so an already-recorded post is fetched, not skipped with
	// nothing to show.
	args := append(t.configArgs(), "-o", logfileOption(path), "-o", "archive=",
		"-D", dir, "--range", "1-1", "--", url)
	res, runErr := t.run(ctx, args...)
	if ctx.Err() != nil {
		return "", false, &outcome.CodedError{Code: outcome.ErrCodeCanceled, Msg: ctx.Err().Error()}
	}
	if runErr != nil {
		return "", false, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: runErr.Error()}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	noExtractor = res.exitCode != 0 &&
		classifyError(res.exitCode, res.stderr).Code == outcome.ErrCodeUnsupportedURL
	// An error can reach stderr alone with a zero exit; it usually lands
	// in the file too, so only a missing one is appended.
	out := string(data)
	if res.stderr != "" && !strings.Contains(out, res.stderr) {
		out += "\n" + res.stderr
	}
	return boundLog(out), noExtractor, nil
}

// Debug on the file handler, not the console, so stderr stays at the
// level classifyError reads.
func logfileOption(path string) string {
	v, _ := json.Marshal(map[string]string{"path": path, "level": "debug", "mode": "w"})
	return "output.logfile=" + string(v)
}

// Both cuts walk to a rune boundary: half a rune in a logged URL or path
// pastes as a replacement character.
func boundLog(s string) string {
	if len(s) <= maxDebugLog {
		return s
	}
	const marker = "\n\n... trimmed ...\n\n"
	budget := maxDebugLog - len(marker)
	head := budget / 4
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (budget - head)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + marker + s[tail:]
}
