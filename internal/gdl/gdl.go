package gdl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/logx"
)

type Item struct {
	Category    string
	Subcategory string
	ID          string
	Num         int
	Meta        map[string]any
}

// QueueItem is a Message.Queue handoff: a plain -j lists it without
// following, a deep -J pass recurses into it.
type QueueItem struct {
	URL  string
	Meta map[string]any
}

// In practice a URL yields Items (a booru search) or Queue (a forum
// thread, a manga title), not both.
type ResolveResult struct {
	Items    []Item
	Queue    []QueueItem
	Category string
}

type Downloaded struct {
	Path    string
	Meta    map[string]any
	Skipped bool
	// A file that cannot be pushed; unlike Skipped, it is not "already in
	// your library".
	Err string
}

type Extractor struct {
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	Example     string `json:"example"`
}

type ProbeStatus string

const (
	ProbeOK           ProbeStatus = "ok"
	ProbeAuthRequired ProbeStatus = "auth_required"
	ProbeBlocked      ProbeStatus = "blocked"
	ProbeFailed       ProbeStatus = "failed"
)

type ProbeResult struct {
	Status ProbeStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

type Runner interface {
	Resolve(ctx context.Context, url, rng string, deep bool) (ResolveResult, error)
	FetchMeta(ctx context.Context, url string) (map[string]any, error)
	Download(ctx context.Context, url, rng, workDir string, archive Archive, onFile func(int, Downloaded), deep bool) ([]Downloaded, error)
	ListExtractors(ctx context.Context) ([]Extractor, error)
	Probe(ctx context.Context, exampleURL string) (ProbeResult, error)
	DebugLog(ctx context.Context, url, workRoot string) (log string, noExtractor bool, err error)
	Version(ctx context.Context) string
}

// Tool resolves the binary on every run, so a managed install takes over
// without a restart.
type Tool struct {
	cfg           *config.Config
	mu            sync.RWMutex
	managedRoot   string
	secrets       func() []config.Secret
	flatTagSites  []string
	notesSites    []string
	metadataSites []string
}

func New(cfg *config.Config, flatTagSites, metadataSites, notesSites []string) *Tool {
	return &Tool{cfg: cfg, flatTagSites: flatTagSites, notesSites: notesSites, metadataSites: metadataSites}
}

func (t *Tool) SetSiteLists(flatTagSites, metadataSites, notesSites []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flatTagSites, t.metadataSites, t.notesSites = flatTagSites, metadataSites, notesSites
}

func (t *Tool) SetManagedRoot(root string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.managedRoot = root
}

// The config Tool was built with predates any credential saved since.
func (t *Tool) SetSecrets(secrets func() []config.Secret) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.secrets = secrets
}

// gallery-dl's HTTP errors quote the request URL, and the gelbooru family
// sends its api_key and user_id in the query string.
func (t *Tool) redactText(text string) string {
	t.mu.RLock()
	secrets := t.secrets
	t.mu.RUnlock()
	list := t.cfg.Secrets()
	if secrets != nil {
		list = secrets()
	}
	text, _ = config.RedactSecrets(text, list)
	return text
}

func (t *Tool) redact(err error) error {
	var ce *outcome.CodedError
	if !errors.As(err, &ce) {
		return err
	}
	return &outcome.CodedError{Code: ce.Code, Msg: t.redactText(ce.Msg)}
}

func (t *Tool) binary() string {
	t.mu.RLock()
	root := t.managedRoot
	t.mu.RUnlock()
	return EffectiveBinary(t.cfg, root)
}

func (t *Tool) siteLists() (flatTagSites, metadataSites, notesSites []string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.flatTagSites, t.metadataSites, t.notesSites
}

var _ Runner = (*Tool)(nil)

type runResult struct {
	stdout   []byte
	stderr   string
	exitCode int
}

// After a cancel kills gallery-dl, a child it left holding the output pipes
// open would otherwise keep Wait, and the job, from returning.
const waitDelay = 5 * time.Second

// A non-zero exit is in exitCode, not err, which is only for a failed launch.
func (t *Tool) run(ctx context.Context, args ...string) (runResult, error) {
	bin := t.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay
	hideConsole(cmd)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	exit, err := exitStatus(cmd.Run())
	res := runResult{stdout: out.Bytes(), stderr: strings.TrimSpace(errBuf.String()), exitCode: exit}
	if err != nil {
		return res, fmt.Errorf("running %s: %w", bin, err)
	}
	return res, nil
}

func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

func (t *Tool) configArgs() []string {
	p := t.cfg.GalleryDL.ConfigPath
	if p == "" {
		return nil
	}
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	return []string{"-c", p}
}

// --chapter-range bounds a dispatcher's queued children, --range the files.
func rangeArgs(rng string, deep bool) []string {
	if rng == "" {
		return nil
	}
	if deep {
		return []string{"--chapter-range", rng}
	}
	return []string{"--range", rng}
}

// The resolve pass needs only ids and URLs, so each enrichment option left on
// would waste requests. -o beats the managed config's per-extractor value.
func resolveOffArgs(flatTagSites, metadataSites, notesSites []string) []string {
	var args []string
	off := func(sites []string, opt string) {
		for _, s := range sites {
			args = append(args, "-o", "extractor."+s+"."+opt+"=false")
		}
	}
	off(flatTagSites, "tags")
	off(metadataSites, "metadata")
	off(notesSites, "notes")
	return args
}

type Archive int

const (
	ArchiveRead Archive = iota
	// An empty -o archive= makes gallery-dl open no archive, so recorded
	// posts are not skipped and nothing new is recorded.
	ArchiveBypass
	// Stops after catchUpSkips recorded posts in a row, which in a
	// newest-first listing is where the previous fetch left off.
	ArchiveCatchUp
)

// Slack for a post that gained a searched tag after the previous fetch and
// so sits among older ones.
const catchUpSkips = 20

func downloadArgs(workDir, rng, url string, archive Archive, deep bool) []string {
	args := []string{"-D", workDir}
	args = append(args, rangeArgs(rng, deep)...)
	switch archive {
	case ArchiveBypass:
		args = append(args, "-o", "archive=")
	case ArchiveCatchUp:
		args = append(args, "-o", "skip=abort:"+strconv.Itoa(catchUpSkips))
	}
	return append(args, url)
}

func (t *Tool) runOut(ctx context.Context, args ...string) ([]byte, error) {
	res, err := t.run(ctx, args...)
	if ctx.Err() != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeCanceled, Msg: ctx.Err().Error()}
	}
	if err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	if res.exitCode != 0 {
		return nil, classifyError(res.exitCode, res.stderr)
	}
	return res.stdout, nil
}

func (t *Tool) Resolve(ctx context.Context, url, rng string, deep bool) (ResolveResult, error) {
	res, err := t.resolve(ctx, url, rng, deep)
	return res, t.redact(err)
}

func (t *Tool) resolve(ctx context.Context, url, rng string, deep bool) (ResolveResult, error) {
	args := t.configArgs()
	args = append(args, resolveOffArgs(t.siteLists())...)
	mode := "-j"
	if deep {
		mode = "-J"
	}
	args = append(args, mode)
	args = append(args, rangeArgs(rng, deep)...)
	args = append(args, url)
	res, err := t.run(ctx, args...)
	if ctx.Err() != nil {
		return ResolveResult{}, &outcome.CodedError{Code: outcome.ErrCodeCanceled, Msg: ctx.Err().Error()}
	}
	if err != nil {
		return ResolveResult{}, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	if res.exitCode != 0 {
		cerr := classifyError(res.exitCode, res.stderr)
		if cerr.Code == outcome.ErrCodeUnsupportedURL {
			if items, ok := directlinkResolve(ctx, url); ok {
				return ResolveResult{Items: items}, nil
			}
		}
		return ResolveResult{}, cerr
	}
	return parseResolve(res.stdout)
}

// FetchMeta keeps the enrichment options on: the full metadata is what it
// is for.
func (t *Tool) FetchMeta(ctx context.Context, url string) (map[string]any, error) {
	out, err := t.runOut(ctx, append(t.configArgs(), "-j", url)...)
	if err != nil {
		return nil, t.redact(err)
	}
	meta, err := firstPostMeta(out)
	return meta, t.redact(err)
}

func firstPostMeta(data []byte) (map[string]any, error) {
	res, err := parseResolve(data)
	if err != nil {
		var ce *outcome.CodedError
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: err.Error()}
	}
	if len(res.Items) == 0 {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: "no post metadata in gallery-dl output"}
	}
	return res.Items[0].Meta, nil
}

// The download always follows Message.Queue handoffs; deep only moves the
// window to --chapter-range.
func (t *Tool) Download(ctx context.Context, url, rng, workDir string, archive Archive, onFile func(int, Downloaded), deep bool) ([]Downloaded, error) {
	dls, err := t.download(ctx, url, rng, workDir, archive, onFile, deep)
	return dls, t.redact(err)
}

func (t *Tool) download(ctx context.Context, url, rng, workDir string, archive Archive, onFile func(int, Downloaded), deep bool) ([]Downloaded, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	args := append(t.configArgs(), downloadArgs(workDir, rng, url, archive, deep)...)
	bin := t.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay
	hideConsole(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: fmt.Errorf("running %s: %w", bin, err).Error()}
	}
	// Drained to EOF before Wait, as StdoutPipe requires.
	results := reportDownloads(stdout, workDir, onFile)
	exit, runErr := exitStatus(cmd.Wait())
	// A canceled job kills the subprocess, which must not read as a
	// download failure.
	if ctx.Err() != nil {
		return results, &outcome.CodedError{Code: outcome.ErrCodeCanceled, Msg: ctx.Err().Error()}
	}
	if runErr != nil {
		// Results go back with the error: the pipeline pushes what landed
		// and fails only the rest.
		return results, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: runErr.Error()}
	}
	if exit != 0 {
		cerr := classifyError(exit, strings.TrimSpace(errBuf.String()))
		if cerr.Code == outcome.ErrCodeUnsupportedURL {
			if dls, ok, derr := directlinkDownload(ctx, url, workDir, onFile); ok {
				return dls, derr
			}
		}
		return results, cerr
	}
	return results, nil
}

// gallery-dl prints an archive skip as "# <path>"; the .json sidecar is on
// disk by the time a path prints, a skip's included. A failed download
// prints nothing; an unusable line takes an entry so the pipeline can fail it.
func reportDownloads(r io.Reader, workDir string, onFile func(int, Downloaded)) []Downloaded {
	var out []Downloaded
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if path, ok := strings.CutPrefix(line, "# "); ok {
			d := Downloaded{Skipped: true}
			if within(workDir, path) {
				d.Meta, _ = readMeta(path)
			}
			out = append(out, d)
			continue
		}
		// The managed config pins the directory and gallery-dl sanitizes
		// names, but a path from the subprocess's stdout is still not
		// trusted.
		if !within(workDir, line) {
			logx.Warnf("gdl: refusing %s, outside the job directory %s", line, workDir)
			out = append(out, Downloaded{Err: "the downloaded file landed outside the job directory"})
			continue
		}
		meta, err := readMeta(line)
		if err != nil {
			logx.Warnf("gdl: %v", err)
			out = append(out, Downloaded{Err: "the downloaded file has no readable metadata sidecar"})
			continue
		}
		d := Downloaded{Path: line, Meta: meta}
		if onFile != nil {
			onFile(len(out), d)
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		logx.Warnf("gdl: reading download output: %v", err)
		// An over-long line stops the scanner; left unread, the pipe fills
		// and gallery-dl blocks writing, which Wait would then wait out.
		_, _ = io.Copy(io.Discard, r)
	}
	return out
}

func (t *Tool) Version(ctx context.Context) string {
	res, err := t.run(ctx, "--version")
	if err != nil || res.exitCode != 0 {
		return ""
	}
	return strings.TrimSpace(string(res.stdout))
}

func parseResolve(data []byte) (ResolveResult, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return ResolveResult{}, nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		return ResolveResult{}, fmt.Errorf("parsing gallery-dl -j output: %w", err)
	}
	var res ResolveResult
	var gdlErr *outcome.CodedError
	for _, raw := range msgs {
		var parts []json.RawMessage
		if err := json.Unmarshal(raw, &parts); err != nil || len(parts) < 2 {
			continue
		}
		var mtype int
		if err := json.Unmarshal(parts[0], &mtype); err != nil {
			continue
		}
		if mtype < 0 { // [-1, {error, message}]: an extraction error
			if gdlErr == nil {
				gdlErr = resolveError(parts[1])
			}
			continue
		}
		if mtype == 6 { // [6, child_url, kwdict]: a dispatcher handoff
			var url string
			var meta map[string]any
			if json.Unmarshal(parts[1], &url) != nil || url == "" {
				continue
			}
			if len(parts) >= 3 {
				_ = json.Unmarshal(parts[2], &meta)
			}
			res.setCategory(meta)
			res.Queue = append(res.Queue, QueueItem{URL: url, Meta: meta})
			continue
		}
		if mtype != 3 { // only Message.Url carries a downloadable file
			continue
		}
		var meta map[string]any
		// The canonical shape is [3, url, kwdict]; tolerate [3, kwdict].
		if len(parts) >= 3 {
			_ = json.Unmarshal(parts[2], &meta)
		} else {
			_ = json.Unmarshal(parts[1], &meta)
		}
		if meta == nil {
			continue
		}
		res.setCategory(meta)
		res.Items = append(res.Items, itemFromMeta(meta))
	}
	if len(res.Items) == 0 && len(res.Queue) == 0 && gdlErr != nil {
		return ResolveResult{}, gdlErr
	}
	return res, nil
}

func (r *ResolveResult) setCategory(meta map[string]any) {
	if r.Category == "" {
		r.Category = kwdict.String(meta, "category")
	}
}

func resolveError(raw json.RawMessage) *outcome.CodedError {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	text := strings.TrimSpace(e.Error + " " + e.Message)
	if text == "" {
		text = "gallery-dl reported an extraction error"
	}
	return classifyError(1, text)
}

func itemFromMeta(meta map[string]any) Item {
	return Item{
		Category:    kwdict.String(meta, "category"),
		Subcategory: kwdict.String(meta, "subcategory"),
		ID:          kwdict.ID(meta),
		Num:         kwdict.Num(meta),
		Meta:        meta,
	}
}

func within(dir, path string) bool {
	if dir == "" {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

func readMeta(path string) (map[string]any, error) {
	data, err := os.ReadFile(path + ".json")
	if err != nil {
		return nil, fmt.Errorf("reading sidecar %s.json: %w", path, err)
	}
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return nil, fmt.Errorf("sidecar %s.json is not valid JSON", path)
	}
	return meta, nil
}
