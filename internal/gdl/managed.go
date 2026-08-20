package gdl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

// A managed install lets the operator run a newer or older gallery-dl than
// the image's pin: a pip venv on the config volume, so it survives image
// updates. It only ever shadows the bundled binary, which stays the fallback
// a revert returns to.

const managedDirName = "gallery-dl"

// The defaults matter to resolution: a value the operator changed away from
// the default always wins over the managed install.
var defaultGDL = config.Default().GalleryDL

// managedVersionRE admits only a plain release number. The string is handed to
// pip and spliced into a download URL, so anything looser is rejected before
// it reaches either.
var managedVersionRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

// ValidVersion reports whether v is a release number the managed install
// accepts, so a caller can refuse one before starting anything.
func ValidVersion(v string) bool { return managedVersionRE.MatchString(v) }

// InstallProgress reports an install's current phase and how far through it
// is, for a caller drawing a progress bar. Every call marks a step that
// actually completed; nil reports nothing.
type InstallProgress func(step string, percent int)

// pypiJSONURL answers the latest-release query; swapped in tests.
var pypiJSONURL = "https://pypi.org/pypi/gallery-dl/json"

// releaseMetaClient bounds the two release-metadata fetches. Both run inside
// the settings install request, and the server sets no WriteTimeout (a wait=N
// enqueue and a large push both need it), so an unanswered connection would
// hang that POST until the operator gave up on the tab.
var releaseMetaClient = &http.Client{Timeout: 30 * time.Second}

// ManagedRoot is the managed install's directory under the config dir.
func ManagedRoot(configDir string) string {
	return filepath.Join(configDir, managedDirName)
}

// ManagedBinary returns the managed install's gallery-dl path, or "" when no
// managed install exists or it cannot run. A venv is bound to the interpreter
// that built it - bin/python3 is a symlink to it - so a base image moving to a
// new python minor leaves the console script in place but unstartable; the
// bundled pin has to answer then, rather than every download failing until
// someone notices.
func ManagedBinary(root string) string {
	if root == "" {
		return ""
	}
	venv := filepath.Join(root, "venv")
	if _, err := os.Stat(filepath.Join(venv, "bin", "python3")); err != nil {
		return ""
	}
	p := filepath.Join(venv, "bin", "gallery-dl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// ManagedInstalled reports whether a managed install exists on disk at all,
// runnable or in use or not. The settings panel gates its revert on this
// rather than on ManagedActive, so an install an explicit binary_path outranks
// - or one a python upgrade broke - can still be removed from the panel that
// wrote it.
func ManagedInstalled(root string) bool {
	if root == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(root, "venv"))
	return err == nil
}

// ManagedActive reports whether runs use the managed install: the operator
// left binary_path at its default and a managed binary exists.
func ManagedActive(cfg *config.Config, root string) bool {
	return cfg.GalleryDL.BinaryPath == defaultGDL.BinaryPath && ManagedBinary(root) != ""
}

// EffectiveBinary resolves the binary a run uses. An explicit non-default
// binary_path always wins - the operator asked for that exact binary - and
// otherwise a managed install, when present, shadows the bundled default.
func EffectiveBinary(cfg *config.Config, root string) string {
	if cfg.GalleryDL.BinaryPath != defaultGDL.BinaryPath {
		return cfg.GalleryDL.BinaryPath
	}
	if p := ManagedBinary(root); p != "" {
		return p
	}
	return cfg.GalleryDL.BinaryPath
}

// EffectiveSupportedSitesPath resolves the supportedsites.md the app parses:
// the copy fetched beside the managed install while that install is active
// (so site names and auth kinds match the running binary), else the
// configured (bundled) path.
func EffectiveSupportedSitesPath(cfg *config.Config, root string) string {
	if cfg.GalleryDL.SupportedSitesPath == defaultGDL.SupportedSitesPath && ManagedActive(cfg, root) {
		p := filepath.Join(root, "supportedsites.md")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return cfg.GalleryDL.SupportedSitesPath
}

// BinaryVersion runs `<path> --version`, returning "" when the binary cannot
// be run. Unlike Tool.Version it names an explicit binary, so the bundled
// version stays readable while a managed install shadows it.
func BinaryVersion(ctx context.Context, path string) string {
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LatestVersion asks PyPI for the newest gallery-dl release.
func LatestVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pypiJSONURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := releaseMetaClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pypi answered %s", resp.Status)
	}
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("parsing the pypi answer: %w", err)
	}
	if !managedVersionRE.MatchString(doc.Info.Version) {
		return "", fmt.Errorf("pypi reported an unexpected version %q", doc.Info.Version)
	}
	return doc.Info.Version, nil
}

// InstallManaged puts gallery-dl==version into the managed venv, creating it
// on first use. An install that leaves no runnable binary is removed
// entirely: a failure may fall back to the bundled binary but never shadow
// it with a broken one.
func InstallManaged(ctx context.Context, configDir, version string, progress InstallProgress) error {
	if !ValidVersion(version) {
		return fmt.Errorf("%q is not a release number like 1.32.9", version)
	}
	report := func(step string, percent int) {
		if progress != nil {
			progress(step, percent)
		}
	}
	root := ManagedRoot(configDir)
	venv := filepath.Join(root, "venv")
	if _, err := os.Stat(filepath.Join(venv, "bin", "pip")); err != nil {
		report("creating the python environment", 5)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		if out, err := exec.CommandContext(ctx, "python3", "-m", "venv", venv).CombinedOutput(); err != nil {
			os.RemoveAll(root)
			return fmt.Errorf("creating the venv: %v: %s", err, outputTail(out))
		}
	}
	if err := runPip(ctx, venv, version, report); err != nil {
		// A failed upgrade usually leaves the previous install intact; only a
		// venv with no runnable gallery-dl is torn down.
		if BinaryVersion(ctx, filepath.Join(venv, "bin", "gallery-dl")) == "" {
			os.RemoveAll(root)
		}
		return fmt.Errorf("pip could not install %s: %s", version, err)
	}
	report("checking the new binary", 80)
	if got := BinaryVersion(ctx, filepath.Join(venv, "bin", "gallery-dl")); got == "" {
		os.RemoveAll(root)
		return fmt.Errorf("the installed gallery-dl does not run; removed it, the bundled binary is active again")
	}
	report("fetching the site list", 88)
	fetchManagedSupportedSites(ctx, root, version)
	return nil
}

// runPip installs the release wheels-only - an sdist would run its build
// script during the install, and gallery-dl and its dependencies all publish
// wheels - reporting each wheel as it lands. The error is pip's own last
// line, which is what names the reason.
func runPip(ctx context.Context, venv, version string, report InstallProgress) error {
	cmd := exec.CommandContext(ctx, filepath.Join(venv, "bin", "pip"),
		"install", "--no-cache-dir", "--only-binary", ":all:", "gallery-dl=="+version)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	report("downloading gallery-dl "+version, 20)
	var lastLine string
	wheels := 0
	scan := bufio.NewScanner(stdout)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "Downloading "):
			wheels++
			report("downloading gallery-dl "+version, min(60, 20+8*wheels))
		case strings.HasPrefix(line, "Installing collected packages"):
			report("installing", 70)
		}
		if line != "" {
			lastLine = line
		}
	}
	if err := cmd.Wait(); err != nil {
		if stderr.Len() == 0 {
			stderr.WriteString(lastLine)
		}
		return errors.New(outputTail(stderr.Bytes()))
	}
	return nil
}

// RevertManaged removes the managed install; runs fall back to the bundled
// binary on their next invocation.
func RevertManaged(configDir string) error {
	return os.RemoveAll(ManagedRoot(configDir))
}

// maxSupportedSitesBytes caps what the site-list fetch will write into
// /config, which is the small volume also holding the config file, the cookies
// and the download archive. The real file is a few hundred KB.
const maxSupportedSitesBytes int64 = 4 << 20

// fetchManagedSupportedSites fetches the supportedsites.md matching the
// installed release. Best-effort: on failure a previous managed copy is
// removed rather than left describing another version, and parsing falls
// back to the bundled copy.
func fetchManagedSupportedSites(ctx context.Context, root, version string) {
	dst := filepath.Join(root, "supportedsites.md")
	url := "https://raw.githubusercontent.com/mikf/gallery-dl/v" + version + "/docs/supportedsites.md"
	fail := func(reason string) {
		os.Remove(dst)
		logx.Warnf("gdl: supportedsites for %s unavailable (%s); site names seed from the bundled copy", version, reason)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fail(err.Error())
		return
	}
	resp, err := releaseMetaClient.Do(req)
	if err != nil {
		fail(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(resp.Status)
		return
	}
	tmp, err := os.CreateTemp(root, ".supportedsites-*")
	if err != nil {
		fail(err.Error())
		return
	}
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxSupportedSitesBytes+1))
	if err == nil && n > maxSupportedSitesBytes {
		err = errors.New("the site list is larger than expected")
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		fail(err.Error())
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		fail(err.Error())
	}
}

// outputTail reduces a subprocess's combined output to its last non-empty
// line - with pip that is the actual error - bounded for a flash message.
func outputTail(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 300 {
				l = l[:300] + "..."
			}
			return l
		}
	}
	return "no output"
}
