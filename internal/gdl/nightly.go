package gdl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Nightly builds come from gdl-org/builds, tagged with their build date.
// gallery-dl numbers its releases 1.x, so a tag never reads as a release.

const NightlyLatest = "nightly"

var nightlyTagRE = regexp.MustCompile(`^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}$`)

func IsNightly(version string) bool { return nightlyTagRE.MatchString(version) }

// ReleaseLabel names a version the way the panel shows it.
func ReleaseLabel(version string) string {
	if IsNightly(version) {
		return "nightly " + version
	}
	return version
}

var (
	nightlyReleasesURL  = "https://api.github.com/repos/gdl-org/builds/releases"
	nightlyDownloadBase = "https://github.com/gdl-org/builds/releases/download/"
)

// No venv fallback: pip has no nightly wheels.
var nightlyAssets = map[string]standaloneAsset{
	"linux/amd64":   {Remote: "gallery-dl_linux", Local: "gallery-dl"},
	"windows/amd64": {Remote: "gallery-dl_windows.exe", Local: "gallery-dl.exe"},
	"windows/386":   {Remote: "gallery-dl_windows_x86.exe", Local: "gallery-dl.exe"},
}

type nightlyRelease struct {
	Tag    string `json:"tag_name"`
	Body   string `json:"body"`
	Assets []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func readNightly(ctx context.Context, path string) (*nightlyRelease, error) {
	body, err := fetch(ctx, nightlyReleasesURL+path, 1<<20)
	if err != nil {
		return nil, err
	}
	var rel nightlyRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("parsing the GitHub answer: %w", err)
	}
	if !IsNightly(rel.Tag) {
		return nil, fmt.Errorf("GitHub reported an unexpected nightly tag %q", rel.Tag)
	}
	return &rel, nil
}

func LatestNightly(ctx context.Context) (string, error) {
	rel, err := readNightly(ctx, "/latest")
	if err != nil {
		return "", err
	}
	return rel.Tag, nil
}

// The release notes link the commit the build came from; the site list
// is read at that commit.
var nightlyCommitRE = regexp.MustCompile(`/commit/([0-9a-f]{40})\b`)

// Refused before anything touches the managed root, so the install in use
// stays.
func nightlyAsset() (standaloneAsset, error) {
	asset, ok := nightlyAssets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return asset, fmt.Errorf("gdl-org publishes no nightly build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return asset, nil
}

// Checked against the digest GitHub publishes for the asset, as the
// release has no checksum file.
func installNightly(ctx context.Context, root, tag string, asset standaloneAsset, report InstallProgress) (path, sitesURL string, err error) {
	rel, err := readNightly(ctx, "/tags/"+tag)
	if errors.Is(err, errNotFound) {
		return "", "", fmt.Errorf("no nightly build is tagged %s", tag)
	}
	if err != nil {
		return "", "", fmt.Errorf("reading the nightly release: %w", err)
	}
	want := ""
	for _, a := range rel.Assets {
		if a.Name == asset.Remote {
			want, _ = strings.CutPrefix(a.Digest, "sha256:")
		}
	}
	if len(want) != 64 {
		return "", "", fmt.Errorf("the %s nightly has no checksummed %s", tag, asset.Remote)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	dst := filepath.Join(root, asset.Local)
	report("downloading gallery-dl "+ReleaseLabel(tag), 20)
	if err := downloadVerified(ctx, nightlyDownloadBase+tag+"/"+asset.Remote, dst, want, report); err != nil {
		return "", "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dst, 0o755); err != nil {
			return "", "", err
		}
	}
	if m := nightlyCommitRE.FindStringSubmatch(rel.Body); m != nil {
		sitesURL = "https://codeberg.org/mikf/gallery-dl/raw/commit/" + m[1] + "/docs/supportedsites.md"
	}
	return dst, sitesURL, nil
}
