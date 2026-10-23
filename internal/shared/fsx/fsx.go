package fsx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteAtomic is atomic, not durable: a caller that needs the content to
// survive a crash calls Sync at the end of write and SyncDir after.
func WriteAtomic(path, pattern string, write func(*os.File) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// SyncDir is best effort: a refused open or fsync still leaves the renamed
// file.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// WriteDurable also syncs the file before the rename and the directory after it.
func WriteDurable(path string, write func(*os.File) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	if err := WriteAtomic(path, "."+filepath.Base(path)+".*", func(f *os.File) error {
		if err := write(f); err != nil {
			return err
		}
		return f.Sync()
	}); err != nil {
		return err
	}
	SyncDir(dir)
	return nil
}

func WriteFile(path string, data []byte) error {
	return WriteDurable(path, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

func AbsIn(path, base string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func RelIn(path, base string) string {
	if path == "" || base == "" {
		return path
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(rel)
}

// SanitizeFilename folds only what a filesystem refuses, so a name in any
// script keeps its letters. It uses Windows' set, as the file may land on an
// SMB share, and trims the trailing dots and spaces Windows drops.
func SanitizeFilename(s string) string {
	return strings.Trim(FoldFilename(s), " .")
}

func FoldFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
