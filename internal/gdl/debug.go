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

// maxDebugLog bounds what one run hands back. The head carries the extractor
// gallery-dl chose and the tail carries the traceback, so a log past the bound
// keeps both ends and drops the middle.
const maxDebugLog = 256 << 10

// DebugLog re-runs one post with gallery-dl's debug logging on and returns what
// it wrote, for pasting into a bug report. It runs the download pass rather
// than a metadata resolve: most of what fails happens while fetching the file,
// and a bare media URL makes no request at all until then. The bytes land in a
// scratch directory under workRoot and are deleted with it; nothing is pushed.
//
// noExtractor reports that gallery-dl matched no extractor for the URL, which
// is the one case where the log contradicts the item beside it: monloader
// fetched that file itself, so the refusal below is not what happened.
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

	// The archive is bypassed so the post is fetched rather than skipped: a run
	// that already succeeded is recorded there, and skipping it is exactly the
	// case with nothing to show.
	args := append(t.configArgs(), "-o", logfileOption(path), "-o", "archive=",
		"-D", dir, "--range", "1-1", url)
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
	// An error can reach stderr alone while the process still exits 0, so the
	// file would end on a clean note without it. At this level it usually lands
	// in both; append only what is missing.
	out := string(data)
	if res.stderr != "" && !strings.Contains(out, res.stderr) {
		out += "\n" + res.stderr
	}
	return boundLog(out), noExtractor, nil
}

// logfileOption asks gallery-dl for a debug log file. The level rides the file
// handler, not the console, so stderr keeps the level classifyError reads and
// turning this on cannot change how a failure is coded. gallery-dl's startup
// banner predates the handler and never reaches the file.
func logfileOption(path string) string {
	v, _ := json.Marshal(map[string]string{"path": path, "level": "debug", "mode": "w"})
	return "output.logfile=" + string(v)
}

// boundLog trims the middle out of an oversized log, keeping both ends. Both
// cuts walk to a rune boundary: gallery-dl logs urls and file paths, and half
// a rune renders as a replacement character in whatever gets pasted into a
// bug report.
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
