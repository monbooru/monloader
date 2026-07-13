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
	"strings"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/kwdict"
	"github.com/leqwin/monloader/internal/logx"
	"github.com/leqwin/monloader/internal/queue"
)

// Item is one resolved post from a -j pass. Meta is the raw gallery-dl metadata
// kwdict the mapping package interprets.
type Item struct {
	Category    string
	Subcategory string
	ID          string
	Num         int
	Meta        map[string]any
}

// QueueItem is one Message.Queue handoff from a -j pass: a child URL a
// dispatcher (a forum thread, a manga title, an archive board) delegates to,
// with the kwdict the parent stamped on it. A plain -j lists these without
// following them; a deep -J pass recurses into them instead.
type QueueItem struct {
	URL  string
	Meta map[string]any
}

// ResolveResult is a -j pass: the downloadable Url items, any Queue handoffs (a
// dispatcher delegating to child extractors), and the top-level category. In
// practice a URL yields one or the other - a booru search yields Items, a forum
// thread or manga title yields Queue - so the pipeline routes on which is set.
type ResolveResult struct {
	Items    []Item
	Queue    []QueueItem
	Category string
}

// Downloaded is one entry from the download pass, in source order: a written
// file paired with its parsed .json sidecar metadata, or an archive skip
// (Skipped set, no file).
type Downloaded struct {
	Path    string
	Meta    map[string]any
	Skipped bool
}

// Extractor is one entry from --list-extractors.
type Extractor struct {
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	Example     string `json:"example"`
}

// ProbeStatus classifies a per-site connectivity probe.
type ProbeStatus string

const (
	ProbeOK           ProbeStatus = "ok"
	ProbeAuthRequired ProbeStatus = "auth_required"
	ProbeBlocked      ProbeStatus = "blocked"
	ProbeFailed       ProbeStatus = "failed"
)

// ProbeResult is the outcome of a per-site test probe.
type ProbeResult struct {
	Status ProbeStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

// Runner is the gallery-dl surface the rest of the app depends on. The real
// implementation shells out; tests inject a fake.
type Runner interface {
	Resolve(ctx context.Context, url, rng string, deep bool) (ResolveResult, error)
	FetchMeta(ctx context.Context, url string) (map[string]any, error)
	Download(ctx context.Context, url, rng, workDir string, force bool, onFile func(int, Downloaded), deep bool) ([]Downloaded, error)
	ListExtractors(ctx context.Context) ([]Extractor, error)
	Probe(ctx context.Context, exampleURL string) (ProbeResult, error)
	Version(ctx context.Context) string
}

// Tool is the real Runner: it shells out to the gallery-dl binary named in
// config, passing the managed config file written by WriteManagedConfig.
type Tool struct {
	cfg           *config.Config
	flatTagSites  []string
	notesSites    []string
	metadataSites []string
}

// New builds a Tool from config. flatTagSites, metadataSites, and notesSites
// are the categories whose managed config sets `tags: true`, the `metadata`
// include, and `notes: true`; the resolve pass overrides them off (see
// resolveOffArgs).
func New(cfg *config.Config, flatTagSites, metadataSites, notesSites []string) *Tool {
	return &Tool{cfg: cfg, flatTagSites: flatTagSites, notesSites: notesSites, metadataSites: metadataSites}
}

var _ Runner = (*Tool)(nil)

// runResult captures a finished subprocess.
type runResult struct {
	stdout   []byte
	stderr   string
	exitCode int
}

// run executes gallery-dl with args, returning its output and exit code. A
// non-zero exit is reported via runResult.exitCode, not err; err is reserved
// for failures to launch the process at all.
func (t *Tool) run(ctx context.Context, args ...string) (runResult, error) {
	cmd := exec.CommandContext(ctx, t.cfg.GalleryDL.BinaryPath, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	exit, err := exitStatus(cmd.Run())
	res := runResult{stdout: out.Bytes(), stderr: strings.TrimSpace(errBuf.String()), exitCode: exit}
	if err != nil {
		return res, fmt.Errorf("running %s: %w", t.cfg.GalleryDL.BinaryPath, err)
	}
	return res, nil
}

// exitStatus interprets a cmd.Run/Wait error: a non-zero process exit is
// reported as its code (nil error); only a failure to launch the binary
// (missing, not executable) is an error.
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

// configArgs prepends `-c <managed.json>` when the managed config exists, so
// per-site credentials and the tags:true flags take effect.
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

// rangeArgs builds the gallery-dl range flag for a window. A deep (dispatcher)
// pass bounds the number of queued children with --chapter-range; a normal pass
// bounds the files with --range.
func rangeArgs(rng string, deep bool) []string {
	if rng == "" {
		return nil
	}
	if deep {
		return []string{"--chapter-range", rng}
	}
	return []string{"--range", rng}
}

// resolveOffArgs turns the managed config's per-post enrichment options off
// for the resolve pass: `tags: true` for the flat-tag families, `notes: true`
// for the notes sites, and the danbooru/e621 `metadata` include. That pass
// reads only category/id/num and the post URL; each option left on would spend
// extra requests (per post, noted post, or page) for data only the download
// pass needs. The command-line override beats the managed config's
// per-extractor value.
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

// downloadArgs assembles the gallery-dl download argv after the managed-config
// flags: the destination, optional range, and the URL. force adds `-o archive=`,
// an empty (falsy) value that makes gallery-dl open no archive at all, so it
// neither skips already-recorded posts nor writes the archive file.archive.
func downloadArgs(workDir, rng, url string, force, deep bool) []string {
	args := []string{"-D", workDir}
	args = append(args, rangeArgs(rng, deep)...)
	if force {
		args = append(args, "-o", "archive=")
	}
	return append(args, url)
}

// Resolve runs `gallery-dl -j [--range] <url>` and parses the authoritative
// item list. It turns the per-post enrichment options off (see
// resolveOffArgs), whose extra fetches are wasted on this pass. A non-zero exit
// becomes a coded error so the pipeline can attribute the failure. deep runs the
// resolve-json mode (`-J`) instead, which follows Message.Queue handoffs into
// their files; the child window is then bounded by --chapter-range, not --range.
func (t *Tool) Resolve(ctx context.Context, url, rng string, deep bool) (ResolveResult, error) {
	args := t.configArgs()
	args = append(args, resolveOffArgs(t.flatTagSites, t.metadataSites, t.notesSites)...)
	mode := "-j"
	if deep {
		mode = "-J"
	}
	args = append(args, mode)
	args = append(args, rangeArgs(rng, deep)...)
	args = append(args, url)
	res, err := t.run(ctx, args...)
	if err != nil {
		return ResolveResult{}, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	if res.exitCode != 0 {
		cerr := classifyError(res.exitCode, res.stderr)
		// gallery-dl rejects a bare media URL with no file extension as
		// unsupported; resolve it as a directlink instead (see directfetch.go).
		if cerr.Code == queue.ErrCodeUnsupportedURL {
			if items, ok := directlinkResolve(ctx, url); ok {
				return ResolveResult{Items: items}, nil
			}
		}
		return ResolveResult{}, cerr
	}
	return parseResolve(res.stdout)
}

// FetchMeta runs `gallery-dl -j <url>` with the enrichment options left ON
// (unlike Resolve, which turns them off) so the first post's full metadata -
// tags, commentary, notes, md5 - comes back without downloading the file. It
// backs the source-refetch path, which maps that metadata and enriches an image
// monbooru already holds.
func (t *Tool) FetchMeta(ctx context.Context, url string) (map[string]any, error) {
	args := t.configArgs()
	args = append(args, "-j", url)
	res, err := t.run(ctx, args...)
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	if res.exitCode != 0 {
		return nil, classifyError(res.exitCode, res.stderr)
	}
	return firstPostMeta(res.stdout)
}

// firstPostMeta pulls the first resolved post's kwdict out of a -j output
// through the shared parseResolve, so the two readers of the message tuples
// cannot drift; a document with no post is a mapping error rather than a
// silent empty result, and a classified extraction error keeps its code.
func firstPostMeta(data []byte) (map[string]any, error) {
	res, err := parseResolve(data)
	if err != nil {
		var ce *queue.CodedError
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, &queue.CodedError{Code: queue.ErrCodeMappingFailed, Msg: err.Error()}
	}
	if len(res.Items) == 0 {
		return nil, &queue.CodedError{Code: queue.ErrCodeMappingFailed, Msg: "no post metadata in gallery-dl output"}
	}
	return res.Items[0].Meta, nil
}

// Download runs `gallery-dl -D <workDir> [--range] <url>` and returns the files
// it wrote, each paired with its `.json` sidecar metadata. force bypasses the
// download-archive so already-fetched posts are written again. onFile, when set,
// is called for each file as gallery-dl prints it, so the caller can advance
// items live instead of waiting for the whole download. deep bounds a
// dispatcher's queued children with --chapter-range to match a deep resolve; the
// download always follows Message.Queue handoffs regardless.
func (t *Tool) Download(ctx context.Context, url, rng, workDir string, force bool, onFile func(int, Downloaded), deep bool) ([]Downloaded, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	args := append(t.configArgs(), downloadArgs(workDir, rng, url, force, deep)...)
	cmd := exec.CommandContext(ctx, t.cfg.GalleryDL.BinaryPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: fmt.Errorf("running %s: %w", t.cfg.GalleryDL.BinaryPath, err).Error()}
	}
	// Drain stdout to EOF (collecting results as they land) before Wait, per the
	// StdoutPipe contract.
	results := reportDownloads(stdout, onFile)
	exit, runErr := exitStatus(cmd.Wait())
	if runErr != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: runErr.Error()}
	}
	if exit != 0 {
		cerr := classifyError(exit, strings.TrimSpace(errBuf.String()))
		// gallery-dl rejects a bare media URL with no file extension as
		// unsupported; fetch it as a directlink instead (see directfetch.go).
		if cerr.Code == queue.ErrCodeUnsupportedURL {
			if dls, ok, derr := directlinkDownload(ctx, url, workDir, onFile); ok {
				return dls, derr
			}
		}
		// Return what landed before the error so the pipeline can push it and
		// mark the rest failed.
		return results, cerr
	}
	return results, nil
}

// reportDownloads reads gallery-dl's download stdout, returning one entry per
// post in source order: a written file with its sidecar, or an archive skip
// (printed as `# <path>`, no file). onFile is called for each written file at
// its position so the caller can advance that row live. The `.json` sidecar is
// on disk by the time its path prints; a written line whose sidecar can't be
// read is recorded as a skip so positions stay aligned.
func reportDownloads(r io.Reader, onFile func(int, Downloaded)) []Downloaded {
	var out []Downloaded
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			out = append(out, Downloaded{Skipped: true})
			continue
		}
		d, ok := readSidecar(line)
		if !ok {
			out = append(out, Downloaded{Skipped: true})
			continue
		}
		if onFile != nil {
			onFile(len(out), d)
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		logx.Warnf("gdl: reading download output: %v", err)
	}
	return out
}

// Version returns the gallery-dl version string for /health, or "" if the
// binary cannot be run.
func (t *Tool) Version(ctx context.Context) string {
	res, err := t.run(ctx, "--version")
	if err != nil || res.exitCode != 0 {
		return ""
	}
	return strings.TrimSpace(string(res.stdout))
}

// parseResolve decodes gallery-dl's -j output: a JSON array of message
// tuples. Url messages (type 3) carry `[3, file_url, kwdict]` and become items;
// Queue messages (type 6) carry `[6, child_url, kwdict]` and become handoffs a
// dispatcher delegates to; directory messages (type 2) and others are skipped.
// An extraction error is reported as `[-1, {error, message}]` while gallery-dl
// still exits 0, so it is captured and surfaced when nothing resolved.
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
	var gdlErr *queue.CodedError
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
			_ = json.Unmarshal(parts[1], &url)
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
	// A resolve that yielded nothing but reported an error (zero exit) would
	// otherwise look like a clean empty success; fail it instead.
	if len(res.Items) == 0 && len(res.Queue) == 0 && gdlErr != nil {
		return ResolveResult{}, gdlErr
	}
	return res, nil
}

// setCategory records the top-level extractor category from the first message
// that carries one (every message's kwdict is stamped with it).
func (r *ResolveResult) setCategory(meta map[string]any) {
	if r.Category == "" {
		r.Category = kwdict.String(meta, "category")
	}
}

// resolveError classifies a gallery-dl -j error object `{error, message}` into
// the stable code vocabulary, reusing the stderr substring rules.
func resolveError(raw json.RawMessage) *queue.CodedError {
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

// itemFromMeta builds an Item from a kwdict.
func itemFromMeta(meta map[string]any) Item {
	return Item{
		Category:    kwdict.String(meta, "category"),
		Subcategory: kwdict.String(meta, "subcategory"),
		ID:          kwdict.ID(meta),
		Num:         kwdict.Num(meta),
		Meta:        meta,
	}
}

// readSidecar pairs a written file with its `.json` sidecar, reading the
// sidecar for the mapping metadata. False when the sidecar is missing or not
// valid JSON - a file without metadata cannot be mapped.
func readSidecar(path string) (Downloaded, bool) {
	data, err := os.ReadFile(path + ".json")
	if err != nil {
		logx.Warnf("gdl: reading sidecar %s.json: %v", path, err)
		return Downloaded{}, false
	}
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		logx.Warnf("gdl: sidecar %s.json is not valid JSON", path)
		return Downloaded{}, false
	}
	return Downloaded{Path: path, Meta: meta}, true
}
