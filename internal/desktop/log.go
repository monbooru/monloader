package desktop

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// Rotated only at open: one Stat at startup instead of size accounting on
// every write.
const maxLogBytes = 8 << 20

// OpenLog adds <dir>/<app>.log to the stdlib logger's output.
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
	// File first: MultiWriter stops at the first failing writer, and a
	// GUI launch's stderr fails.
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	return f, nil
}
