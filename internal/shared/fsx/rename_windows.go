package fsx

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	renameRetryFor  = time.Second
	renameRetryStep = 50 * time.Millisecond
)

// An antivirus scanner or an indexer holding the target for a moment fails the rename.
func rename(oldpath, newpath string) error {
	deadline := time.Now().Add(renameRetryFor)
	for {
		err := os.Rename(oldpath, newpath)
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(renameRetryStep)
	}
}
