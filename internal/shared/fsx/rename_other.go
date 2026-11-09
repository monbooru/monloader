//go:build !windows

package fsx

import "os"

func rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
