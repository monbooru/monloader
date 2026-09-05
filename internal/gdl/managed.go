package gdl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/fsx"
	"github.com/monbooru/monloader/internal/logx"
)

// A managed install lets the operator run a newer or older gallery-dl than
// what ships with the app: a file under the config directory, so it survives
// an update. It only ever shadows the bundled binary, which stays the
// fallback a revert returns to.
//
// What it fetches depends on the platform. Where upstream publishes a
// one-file build, that is what lands - no Python anywhere, which is the
// whole point of the desktop bundle. Elsewhere it is still a pip venv, so an
// architecture upstream does not build for is not left without a way to
// upgrade; an existing venv keeps working either way.

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

// ManagedRoot is the managed install's directory under the state dir.
func ManagedRoot(stateDir string) string {
	return filepath.Join(stateDir, managedDirName)
}

// ManagedBinary returns the managed install's gallery-dl path, or "" when no
// managed install exists or it cannot run. The standalone comes first and the
// venv is the legacy rung: an install made before this app shipped one-file
// builds keeps working instead of silently reverting to the bundled version.
//
// A venv is bound to the interpreter that built it - bin/python3 is a symlink
// to it - so a base image moving to a new python minor leaves the console
// script in place but unstartable; the bundled pin has to answer then, rather
// than every download failing until someone notices.
func ManagedBinary(root string) string {
	if root == "" {
		return ""
	}
	if p := filepath.Join(root, standaloneName()); fsx.IsFile(p) {
		return p
	}
	venv := filepath.Join(root, "venv")
	if _, err := os.Stat(venvExe(venv, "python3")); err != nil {
		return ""
	}
	p := venvExe(venv, "gallery-dl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// standaloneName is what a downloaded one-file build is stored as here.
func standaloneName() string {
	if runtime.GOOS == "windows" {
		return "gallery-dl.exe"
	}
	return "gallery-dl"
}

// venvExe locates one of a venv's executables. Windows puts them in
// Scripts/ with an .exe suffix and names the interpreter python; every
// other platform uses bin/ and the bare name.
func venvExe(venv, name string) string {
	if runtime.GOOS == "windows" {
		if name == "python3" {
			name = "python"
		}
		return filepath.Join(venv, "Scripts", name+".exe")
	}
	return filepath.Join(venv, "bin", name)
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
	if fsx.IsFile(filepath.Join(root, standaloneName())) {
		return true
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
//
// The bare default resolves at exec time rather than being rewritten in the
// config: ManagedActive and EffectiveSupportedSitesPath both decide whether
// the operator has taken over by comparing binary_path against the default
// string, so changing that string would quietly reclassify every existing
// config. The config keeps saying "gallery-dl"; a copy shipped beside the
// executable is simply where that name is found first.
func EffectiveBinary(cfg *config.Config, root string) string {
	if cfg.GalleryDL.BinaryPath != defaultGDL.BinaryPath {
		return cfg.GalleryDL.BinaryPath
	}
	if p := ManagedBinary(root); p != "" {
		return p
	}
	if p := bundledBinary(); p != "" {
		return p
	}
	return cfg.GalleryDL.BinaryPath
}

// bundledBinary is the copy a download ships beside the executable, which is
// not on PATH and would otherwise never be found. Empty when there is none.
func bundledBinary() string {
	dir := fsx.ExeDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, standaloneName())
	if !fsx.IsFile(p) {
		return ""
	}
	if runtime.GOOS != "windows" {
		// A file unpacked without its mode bits would shadow a working copy
		// on PATH with one that cannot run.
		fi, err := os.Stat(p)
		if err != nil || fi.Mode()&0o111 == 0 {
			return ""
		}
	}
	return p
}

// EffectiveSupportedSitesPath resolves the supportedsites.md the app parses:
// the copy fetched beside the managed install while that install is active
// (so site names and auth kinds match the running binary), else the operator's
// own path. An empty result means the embedded copy, which is the default and
// what every install without a managed one reads.
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
	cmd := exec.CommandContext(ctx, path, "--version")
	hideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LatestVersion asks PyPI for the newest gallery-dl release.
func LatestVersion(ctx context.Context) (string, error) {
	body, err := fetch(ctx, pypiJSONURL, 1<<20)
	if err != nil {
		return "", err
	}
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parsing the pypi answer: %w", err)
	}
	if !managedVersionRE.MatchString(doc.Info.Version) {
		return "", fmt.Errorf("pypi reported an unexpected version %q", doc.Info.Version)
	}
	return doc.Info.Version, nil
}

// InstallManaged puts gallery-dl==version under the managed root, taking the
// published one-file build where upstream has one and a pip venv where it
// does not. An install that leaves no runnable binary is removed entirely: a
// failure may fall back to the bundled binary but never shadow it with a
// broken one.
func InstallManaged(ctx context.Context, stateDir, version string, progress InstallProgress) error {
	if !ValidVersion(version) {
		return fmt.Errorf("%q is not a release number like 1.32.9", version)
	}
	report := func(step string, percent int) {
		if progress != nil {
			progress(step, percent)
		}
	}
	root := ManagedRoot(stateDir)
	// A bundled archive ships its gallery-dl as <install>/gallery-dl, which is
	// this exact path when the state dir is the install dir - the shape a
	// bundled archive run with -desktop=false takes.
	if fi, err := os.Stat(root); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s is the bundled gallery-dl, not a directory; set gallerydl.binary_path to install elsewhere", root)
	}
	report("resolving the release", 5)

	installed := ""
	asset, ok := standaloneForHost()
	if ok {
		path, err := installStandalone(ctx, root, version, asset, report)
		switch {
		case err == nil:
			installed = path
		case errors.Is(err, errNoAsset):
			// This release published nothing for us - an old version, or a
			// platform upstream dropped - so the venv answers instead.
			ok = false
		default:
			// Nothing partial survives: the temp file is gone and an
			// unverifiable download never reached its name.
			removeIfUnrunnable(ctx, root)
			return fmt.Errorf("could not install gallery-dl %s: %s", version, err)
		}
	}
	if !ok {
		var err error
		if installed, err = installVenv(ctx, root, version, report); err != nil {
			return err
		}
	}

	report("checking the new binary", 80)
	if got := BinaryVersion(ctx, installed); got == "" {
		os.RemoveAll(root)
		return fmt.Errorf("the installed gallery-dl does not run; removed it, the bundled binary is active again")
	}
	report("fetching the site list", 88)
	fetchManagedSupportedSites(ctx, root, version)
	return nil
}

// hostPython is the interpreter that can build a venv. Windows installs
// commonly ship python without the python3 alias, so both names are tried
// before giving up. Empty when neither is on PATH.
func hostPython() string {
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// installVenv is the fallback for a platform upstream publishes no one-file
// build for. Without a python interpreter it refuses by name rather than
// handing back a pip error the operator has to interpret.
func installVenv(ctx context.Context, root, version string, report InstallProgress) (string, error) {
	python := hostPython()
	if python == "" {
		return "", fmt.Errorf("gallery-dl publishes no build for %s/%s, and installing one needs python3, which is not on PATH",
			runtime.GOOS, runtime.GOARCH)
	}
	venv := filepath.Join(root, "venv")
	binary := venvExe(venv, "gallery-dl")
	if _, err := os.Stat(venvExe(venv, "pip")); err != nil {
		report("creating the python environment", 5)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, python, "-m", "venv", venv)
		hideConsole(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(root)
			return "", fmt.Errorf("creating the venv: %v: %s", err, outputTail(out))
		}
	}
	if err := runPip(ctx, venv, version, report); err != nil {
		// A failed upgrade usually leaves the previous install intact; only a
		// venv with no runnable gallery-dl is torn down.
		if BinaryVersion(ctx, binary) == "" {
			os.RemoveAll(root)
		}
		return "", fmt.Errorf("pip could not install %s: %s", version, err)
	}
	return binary, nil
}

// removeIfUnrunnable drops the whole managed root when nothing in it runs,
// so a failed install can never shadow the bundled binary.
func removeIfUnrunnable(ctx context.Context, root string) {
	if p := ManagedBinary(root); p != "" && BinaryVersion(ctx, p) != "" {
		return
	}
	os.RemoveAll(root)
}

// runPip installs the release wheels-only - an sdist would run its build
// script during the install, and gallery-dl and its dependencies all publish
// wheels - reporting each wheel as it lands. The error is pip's own last
// line, which is what names the reason.
func runPip(ctx context.Context, venv, version string, report InstallProgress) error {
	cmd := exec.CommandContext(ctx, venvExe(venv, "pip"),
		"install", "--no-cache-dir", "--only-binary", ":all:", "gallery-dl=="+version)
	hideConsole(cmd)
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
func RevertManaged(stateDir string) error {
	return os.RemoveAll(ManagedRoot(stateDir))
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
	body, err := fetch(ctx, url, maxSupportedSitesBytes+1)
	if err != nil {
		fail(err.Error())
		return
	}
	if int64(len(body)) > maxSupportedSitesBytes {
		fail("the site list is larger than expected")
		return
	}
	err = config.WriteFileAtomic(dst, func(f *os.File) error {
		_, err := f.Write(body)
		return err
	})
	if err != nil {
		fail(err.Error())
	}
}

// outputTail reduces a subprocess's combined output to its last non-empty
// line - with pip that is the actual error - bounded for a flash message.
// The cut walks back to a rune boundary: pip's errors carry paths.
func outputTail(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 300 {
				end := 300
				for end > 0 && !utf8.RuneStart(l[end]) {
					end--
				}
				l = l[:end] + "..."
			}
			return l
		}
	}
	return "no output"
}
