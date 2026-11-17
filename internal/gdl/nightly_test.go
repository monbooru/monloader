package gdl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const stubNightlyCommit = "8c1cb63f6d49e01d75268364f42e3ecf2d906c17"

// stubNightly serves one nightly release the way GitHub's API and download
// host answer for it.
func stubNightly(t *testing.T, tag string, corrupt bool) {
	t.Helper()
	payload := []byte("#!/bin/sh\necho 1.33.0-dev\n")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if corrupt {
		digest = "sha256:" + strings.Repeat("0", 64)
	}
	release, err := json.Marshal(map[string]any{
		"tag_name": tag,
		"body":     "[`8c1cb63f6d` [docs] update](https://codeberg.example/gallery-dl/commit/" + stubNightlyCommit + ")",
		"assets": []map[string]string{
			{"name": "gallery-dl_linux", "digest": digest},
			{"name": "gallery-dl_windows.exe", "digest": "sha256:" + strings.Repeat("f", 64)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest", "/releases/tags/" + tag:
			_, _ = w.Write(release)
		case "/download/" + tag + "/gallery-dl_linux":
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldDL := nightlyReleasesURL, nightlyDownloadBase
	nightlyReleasesURL, nightlyDownloadBase = srv.URL+"/releases", srv.URL+"/download/"
	t.Cleanup(func() { nightlyReleasesURL, nightlyDownloadBase = oldAPI, oldDL })
}

func TestIsNightly(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"2026.11.12", true},
		{"2026.01.01", true},
		{"1.32.9", false},
		{"1.30.10", false},
		{"nightly", false},
		{"26.11.12", false},
		{"2026.11.12.1", false},
		{"2026.1.12", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsNightly(c.in); got != c.want {
			t.Errorf("IsNightly(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReleaseLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.32.9", "1.32.9"},
		{"2026.11.12", "nightly 2026.11.12"},
	}
	for _, c := range cases {
		if got := ReleaseLabel(c.in); got != c.want {
			t.Errorf("ReleaseLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLatestNightly(t *testing.T) {
	stubNightly(t, "2026.11.12", false)
	got, err := LatestNightly(context.Background())
	if err != nil || got != "2026.11.12" {
		t.Errorf("LatestNightly = %q, %v; want 2026.11.12", got, err)
	}
}

func TestLatestNightlyRejectsAnOddTag(t *testing.T) {
	stubNightly(t, "v1.2.3; rm -rf /", false)
	if got, err := LatestNightly(context.Background()); err == nil {
		t.Errorf("LatestNightly = %q, want an error for a tag that is no date", got)
	}
}

func TestInstallNightlyVerifiesAndLands(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the nightly build this exercises is linux/amd64")
	}
	const tag = "2026.11.12"
	stubNightly(t, tag, false)

	root := ManagedRoot(t.TempDir())
	bin, sitesURL, err := installNightly(context.Background(), root, tag, nightlyAssets["linux/amd64"], func(string, int) {})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if bin != filepath.Join(root, "gallery-dl") || ManagedBinary(root) != bin {
		t.Fatalf("binary = %q, managed = %q", bin, ManagedBinary(root))
	}
	if got := BinaryVersion(context.Background(), bin); got != "1.33.0-dev" {
		t.Errorf("BinaryVersion = %q, want the nightly's", got)
	}
	if !strings.Contains(sitesURL, "/commit/"+stubNightlyCommit+"/docs/supportedsites.md") {
		t.Errorf("site list url = %q, want it read at the build's commit", sitesURL)
	}
	if _, err := os.Stat(filepath.Join(root, "venv")); err == nil {
		t.Error("a nightly install created a venv")
	}
}

func TestInstallNightlyRefuses(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the nightly build this exercises is linux/amd64")
	}
	cases := []struct {
		name    string
		tag     string
		corrupt bool
		want    string
	}{
		{"a wrong checksum", "2026.11.12", true, "checksum"},
		{"an unknown tag", "2020.01.01", false, "no nightly build is tagged 2020.01.01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubNightly(t, "2026.11.12", c.corrupt)
			dir := t.TempDir()
			err := InstallManaged(context.Background(), dir, c.tag, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("install error = %v, want one naming %q", err, c.want)
			}
			if ManagedBinary(ManagedRoot(dir)) != "" {
				t.Error("a refused nightly left a managed binary behind")
			}
		})
	}
}

func TestInstallNightlyWithoutABuildKeepsTheInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	old := nightlyAssets
	nightlyAssets = map[string]standaloneAsset{}
	t.Cleanup(func() { nightlyAssets = old })

	dir := t.TempDir()
	bin := fakeManagedBinary(t, ManagedRoot(dir))
	err := InstallManaged(context.Background(), dir, "2026.11.12", nil)
	if err == nil || !strings.Contains(err.Error(), "publishes no nightly build for "+runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("install error = %v, want the platform refusal", err)
	}
	if ManagedBinary(ManagedRoot(dir)) != bin {
		t.Error("the refusal touched the install in use")
	}
}
