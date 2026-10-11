package gdl

import (
	"path/filepath"
	"strings"
)

// The types monbooru accepts; anything else is skipped rather than pushed
// and rejected.
var ingestableExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".avif": true,
	".jxl": true, ".gif": true, ".mp4": true, ".webm": true, ".cbz": true,
}

func Ingestable(path string) bool {
	return ingestableExts[strings.ToLower(filepath.Ext(path))]
}

func ingestableMedia(contentType string) (string, bool) {
	ext, ok := mediaExtension(contentType)
	if !ok || !ingestableExts["."+ext] {
		return "", false
	}
	return ext, true
}
