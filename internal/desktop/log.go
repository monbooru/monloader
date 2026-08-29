package desktop

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// maxLogBytes is the size at which the log rotates. Rotating at open
// rather than during the run keeps one Stat on the startup path and no
// size accounting on every write; one generation carries the previous
// session plus the current one, which is what a bug report needs.
const maxLogBytes = 8 << 20

// OpenLog adds <dir>/<app>.log to the stdlib logger's output, so a
// GUI-launched process still leaves a record where stderr goes nowhere.
// The caller closes the returned file at shutdown.
func OpenLog(dir, app string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, app+".log")
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	// The file goes first: a GUI launch has a dead stderr and MultiWriter
	// stops at the first failed writer, which would cost the file every line.
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	return f, nil
}
