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

// A managed install only shadows the bundled binary, the fallback a revert
// returns to. It is upstream's one-file build where one exists and a pip venv
// elsewhere, so an architecture upstream skips can still upgrade.

const managedDirName = "gallery-dl"

var defaultGDL = config.Default().GalleryDL

// The version reaches pip and a download URL, so only a plain release
// number passes.
var managedVersionRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

func ValidVersion(v string) bool { return managedVersionRE.MatchString(v) }

type InstallProgress func(step string, percent int)

var pypiJSONURL = "https://pypi.org/pypi/gallery-dl/json"

// The fetches run inside the settings request, and the server sets no
// WriteTimeout, so an unanswered connection would hang that POST.
var releaseMetaClient = &http.Client{Timeout: 30 * time.Second}

func ManagedRoot(stateDir string) string {
	return filepath.Join(stateDir, managedDirName)
}

// A venv from before the one-file builds still counts, unless a python
// upgrade removed the interpreter it links to: then the bundled pin must
// answer.
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

func standaloneName() string {
	if runtime.GOOS == "windows" {
		return "gallery-dl.exe"
	}
	return "gallery-dl"
}

func venvExe(venv, name string) string {
	if runtime.GOOS == "windows" {
		if name == "python3" {
			name = "python"
		}
		return filepath.Join(venv, "Scripts", name+".exe")
	}
	return filepath.Join(venv, "bin", name)
}

// ManagedInstalled counts a broken or outranked install too, so the panel
// can still revert it.
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

func ManagedActive(cfg *config.Config, root string) bool {
	return cfg.GalleryDL.BinaryPath == defaultGDL.BinaryPath && ManagedBinary(root) != ""
}

// The default binary_path is resolved here, never rewritten in the
// config: ManagedActive compares against the default string, so a rewrite
// would reclassify every config.
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

func EffectiveSupportedSitesPath(cfg *config.Config, root string) string {
	if cfg.GalleryDL.SupportedSitesPath == defaultGDL.SupportedSitesPath && ManagedActive(cfg, root) {
		p := filepath.Join(root, "supportedsites.md")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return cfg.GalleryDL.SupportedSitesPath
}

func BinaryVersion(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, path, "--version")
	hideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

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

// An install that leaves no runnable binary is removed: a failure may
// fall back to the bundled binary but never shadow it with a broken one.
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
	// A bundled archive run with -desktop=false keeps its own gallery-dl
	// binary at this path.
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
			ok = false
		default:
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

// Windows installs often lack the python3 alias.
func hostPython() string {
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

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
		// A failed upgrade usually leaves the previous install working;
		// only an unrunnable venv goes.
		if BinaryVersion(ctx, binary) == "" {
			os.RemoveAll(root)
		}
		return "", fmt.Errorf("pip could not install %s: %s", version, err)
	}
	return binary, nil
}

func removeIfUnrunnable(ctx context.Context, root string) {
	if p := ManagedBinary(root); p != "" && BinaryVersion(ctx, p) != "" {
		return
	}
	os.RemoveAll(root)
}

// Wheels only: an sdist would run its build script during the install,
// and gallery-dl and its dependencies all publish wheels.
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

func RevertManaged(stateDir string) error {
	return os.RemoveAll(ManagedRoot(stateDir))
}

// The real file is a few hundred KB; the cap protects the state volume it
// lands on.
const maxSupportedSitesBytes int64 = 4 << 20

// On failure an older copy is removed rather than left describing another
// release.
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
	if err := config.WriteFileBytesAtomic(dst, body); err != nil {
		fail(err.Error())
	}
}

// The last line is pip's actual error; the cut walks back to a rune
// boundary since pip's errors carry paths.
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
