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
	"github.com/monbooru/monloader/internal/shared/desktop"
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
	Items       []Item
	Queue       []QueueItem
	Category    string
	Subcategory string
	// What ended the listing after these were listed.
	Err error
}

// IsListing is for a manga site's handoffs. mangadex's list and author read
// like listings too, but hand off whole titles rather than books.
func IsListing(subcategory string) bool {
	switch subcategory {
	case "search", "tag", "favorite", "index":
		return true
	}
	return false
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
	ProbeRateLimited  ProbeStatus = "rate_limited"
	ProbeFailed       ProbeStatus = "failed"
)

type ProbeResult struct {
	Status ProbeStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

type Runner interface {
	Resolve(ctx context.Context, url, rng string, deep bool) (ResolveResult, error)
	FetchMeta(ctx context.Context, url string) (map[string]any, error)
	FirstPost(ctx context.Context, url string) (map[string]any, error)
	Download(ctx context.Context, url, rng, workDir string, archive Archive, onFile func(Downloaded), deep bool) ([]Downloaded, error)
	ListExtractors(ctx context.Context) ([]Extractor, error)
	Probe(ctx context.Context, exampleURL string) (ProbeResult, error)
	DebugLog(ctx context.Context, url, workRoot string) (log string, noExtractor bool, err error)
	Version(ctx context.Context) string
	SetSiteLists(lists SiteLists)
	// ExtractorInfo names what would handle a URL, without a network call.
	ExtractorInfo(ctx context.Context, url string) (category, subcategory string)
}

// Tool resolves the binary on every run, so a managed install takes over
// without a restart.
type Tool struct {
	cfg         *config.Provider
	mu          sync.RWMutex
	managedRoot string
	lists       SiteLists
}

func New(cfg *config.Provider, lists SiteLists) *Tool {
	return &Tool{cfg: cfg, lists: lists}
}

func (t *Tool) SetSiteLists(lists SiteLists) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lists = lists
}

func (t *Tool) SetManagedRoot(root string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.managedRoot = root
}

// gallery-dl's HTTP errors quote the request URL, and the gelbooru family
// sends its api_key and user_id in the query string.
func (t *Tool) redactText(text string) string {
	text, _ = config.RedactSecrets(text, t.cfg.Current().Secrets())
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
	return EffectiveBinary(t.cfg.Current(), root)
}

func (t *Tool) siteLists() SiteLists {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lists
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
	killGroupOnCancel(cmd)
	desktop.HideConsole(cmd)
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

// A personal ~/.config/gallery-dl/config.json would merge under the managed
// file, its archive and logins included, so the default files stay unread.
func (t *Tool) configArgs() []string {
	args := []string{"--config-ignore"}
	p := t.cfg.Current().GalleryDL.ConfigPath
	if p == "" {
		return args
	}
	if _, err := os.Stat(p); err != nil {
		return args
	}
	return append(args, "-c", p)
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

// --range bounds files, not handoffs, so a search would walk its whole listing
// before its first gallery. The caller slices its offset from the listing's
// start, and the one handoff past the window is what says more is left.
func handoffArgs(rng string) []string {
	_, end, ok := strings.Cut(rng, "-")
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(end)
	if err != nil {
		return nil
	}
	return []string{"--chapter-range", "1-" + strconv.Itoa(n+1)}
}

// The resolve pass needs only ids and URLs, so each enrichment option left on
// would waste requests. -o beats the managed config's per-extractor value.
func resolveOffArgs(lists SiteLists) []string {
	var args []string
	off := func(sites []string, opt string) {
		for _, s := range sites {
			args = append(args, "-o", "extractor."+s+"."+opt+"=false")
		}
	}
	off(lists.FlatTag, "tags")
	off(lists.Metadata, "metadata")
	off(lists.Notes, "notes")
	return args
}

// PositionRange renders ascending positions as --range wants them:
// 1-4,6,9-20. gallery-dl applies it before the archive check, so a position
// left out is neither fetched nor recorded.
func PositionRange(positions []int) string {
	var parts []string
	for i := 0; i < len(positions); {
		j := i
		for j+1 < len(positions) && positions[j+1] == positions[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(positions[i]))
		} else {
			parts = append(parts, strconv.Itoa(positions[i])+"-"+strconv.Itoa(positions[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

type Archive int

const (
	ArchiveRead Archive = iota
	// An empty -o archive= makes gallery-dl open no archive, so recorded
	// posts are not skipped and nothing new is recorded.
	ArchiveBypass
	// Stops after catchUpSkips recorded posts in a row, where a newest-first
	// listing reaches the previous fetch. Not skip=abort: that replaces the
	// managed skip=enumerate, which keeps a second file of one name.
	ArchiveCatchUp
)

// Slack for a post that gained a searched tag after the previous fetch and
// so sits among older ones.
const catchUpSkips = 20

func downloadArgs(workDir, rng, url string, archive Archive, deep bool) []string {
	args := []string{"-D", workDir}
	args = append(args, rangeArgs(rng, deep)...)
	if archive == ArchiveBypass {
		args = append(args, "-o", "archive=")
	}
	return append(args, "--", url)
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
		return nil, t.runError(res.exitCode, res.stderr)
	}
	return res.stdout, nil
}

func (t *Tool) Resolve(ctx context.Context, url, rng string, deep bool) (ResolveResult, error) {
	res, err := t.resolve(ctx, url, rng, deep)
	res.Err = t.summarized(res.Err)
	return res, t.summarized(err)
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
	if !deep {
		args = append(args, handoffArgs(rng)...)
	}
	args = append(args, "--", url)
	out, err := t.runOut(ctx, args...)
	if err != nil {
		var ce *outcome.CodedError
		if errors.As(err, &ce) && ce.Code == outcome.ErrCodeUnsupportedURL {
			if items, ok := directlinkResolve(ctx, url); ok {
				return ResolveResult{Items: items}, nil
			}
		}
		return ResolveResult{}, err
	}
	return parseResolve(out)
}

// FetchMeta keeps the enrichment options on: the full metadata is what it
// is for.
func (t *Tool) FetchMeta(ctx context.Context, url string) (map[string]any, error) {
	return t.fetchFirst(ctx, "-j", "--", url)
}

// FirstPost is FetchMeta for a URL that may list many posts, which FetchMeta
// would walk to the end.
func (t *Tool) FirstPost(ctx context.Context, url string) (map[string]any, error) {
	meta, err := t.fetchFirst(ctx, "-j", "--range", "1-1", "--chapter-range", "1-1", "--", url)
	var ce *outcome.CodedError
	if errors.As(err, &ce) && ce.Msg == noPostMsg {
		if _, sub := t.ExtractorInfo(ctx, url); sub == "post" {
			return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: NoFileMsg}
		}
	}
	return meta, err
}

func (t *Tool) fetchFirst(ctx context.Context, args ...string) (map[string]any, error) {
	out, err := t.runOut(ctx, append(t.configArgs(), args...)...)
	if err != nil {
		return nil, t.redact(err)
	}
	meta, err := firstPostMeta(out)
	return meta, t.redact(err)
}

const (
	// NoFileMsg names a post gallery-dl resolves to nothing: its extractor skips
	// a post with no file silently.
	NoFileMsg = "the post offers no file - it may be restricted or need an account"
	noPostMsg = "no post metadata in gallery-dl output"
)

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
		if len(res.Queue) > 0 {
			return nil, &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: "this URL lists galleries or chapters, with no post of its own: paste one of them"}
		}
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: noPostMsg}
	}
	return res.Items[0].Meta, nil
}

// The download always follows Message.Queue handoffs; deep only moves the
// window to --chapter-range.
func (t *Tool) Download(ctx context.Context, url, rng, workDir string, archive Archive, onFile func(Downloaded), deep bool) ([]Downloaded, error) {
	dls, err := t.download(ctx, url, rng, workDir, archive, onFile, deep)
	return dls, t.redact(err)
}

func (t *Tool) download(ctx context.Context, url, rng, workDir string, archive Archive, onFile func(Downloaded), deep bool) ([]Downloaded, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	args := append(t.configArgs(), downloadArgs(workDir, rng, url, archive, deep)...)
	bin := t.binary()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.WaitDelay = waitDelay
	killGroupOnCancel(cmd)
	desktop.HideConsole(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: fmt.Errorf("running %s: %w", bin, err).Error()}
	}
	caughtUp := false
	onSkips := func(inARow int) {
		if archive == ArchiveCatchUp && inARow >= catchUpSkips {
			caughtUp = true
			stop()
		}
	}
	// Drained to EOF before Wait, as StdoutPipe requires.
	results, readErr := reportDownloads(stdout, workDir, onFile, onSkips)
	exit, runErr := exitStatus(cmd.Wait())
	// A canceled job kills the subprocess, which must not read as a
	// download failure.
	if ctx.Err() != nil {
		return results, &outcome.CodedError{Code: outcome.ErrCodeCanceled, Msg: ctx.Err().Error()}
	}
	if readErr != nil {
		return results, readErr
	}
	if caughtUp {
		if stderr := strings.TrimSpace(errBuf.String()); strings.Contains(stderr, "[error]") {
			return results, t.runError(exit, stderr)
		}
		return results, nil
	}
	if runErr != nil {
		// Results go back with the error: the pipeline pushes what landed
		// and fails only the rest.
		return results, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: runErr.Error()}
	}
	if exit != 0 {
		cerr := t.runError(exit, strings.TrimSpace(errBuf.String()))
		if cerr.Code == outcome.ErrCodeUnsupportedURL {
			if dls, ok, derr := directlinkDownload(ctx, url, workDir, onFile); ok {
				return dls, derr
			}
		}
		return results, cerr
	}
	return results, nil
}

// CaughtUp says a catch-up run stopped at its last entry, short of any rows past it.
func CaughtUp(downloaded []Downloaded) bool {
	if len(downloaded) < catchUpSkips {
		return false
	}
	for _, d := range downloaded[len(downloaded)-catchUpSkips:] {
		if !d.Skipped {
			return false
		}
	}
	return true
}

// Classified on the whole of stderr, which the summary then cuts to one line.
func (t *Tool) runError(exit int, stderr string) *outcome.CodedError {
	cerr := classifyError(exit, stderr)
	cerr.Msg = t.stderrSummary(cerr.Msg)
	return cerr
}

func (t *Tool) summarized(err error) error {
	var ce *outcome.CodedError
	if !errors.As(err, &ce) {
		return err
	}
	return &outcome.CodedError{Code: ce.Code, Msg: t.stderrSummary(ce.Msg)}
}

const maxErrorMessage = 300

// The message lands in every item the run failed, so it is one line: the
// last [error] one. Redacted first, so a clip cannot split a secret.
func (t *Tool) stderrSummary(stderr string) string {
	lines := strings.Split(stderr, "\n")
	msg := strings.TrimSpace(lines[len(lines)-1])
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "[error]") {
			msg = strings.TrimSpace(lines[i])
			break
		}
	}
	msg = t.redactText(msg)
	if len(msg) <= maxErrorMessage {
		return msg
	}
	return strings.ToValidUTF8(msg[:maxErrorMessage], "") + "..."
}

// gallery-dl prints an archive skip as "# <path>", and a path only once its .json
// sidecar is on disk. A failed download prints nothing; an unusable line takes an
// entry so the pipeline can fail it. onSkips hears the length of the current skip run.
func reportDownloads(r io.Reader, workDir string, onFile func(Downloaded), onSkips func(int)) ([]Downloaded, error) {
	var out []Downloaded
	inARow := 0
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
			inARow++
			if onSkips != nil {
				onSkips(inARow)
			}
			continue
		}
		inARow = 0
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
			onFile(d)
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		logx.Warnf("gdl: reading download output: %v", err)
		// An over-long line stops the scanner; left unread, the pipe fills
		// and gallery-dl blocks writing, which Wait would then wait out.
		_, _ = io.Copy(io.Discard, r)
		// Not a skip: whatever landed past this line is in the archive unpushed.
		return out, &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: "gallery-dl printed a line too long to read"}
	}
	return out, nil
}

func (t *Tool) ExtractorInfo(ctx context.Context, url string) (category, subcategory string) {
	out, err := t.runOut(ctx, append(t.configArgs(), "-E", "--", url)...)
	if err != nil {
		return "", ""
	}
	return parseExtractorInfo(out)
}

// The value line under gallery-dl's -E header: "danbooru" / "post" / "Danbooru"
func parseExtractorInfo(out []byte) (category, subcategory string) {
	_, rest, ok := strings.Cut(string(out), "Category / Subcategory")
	if !ok {
		return "", ""
	}
	_, rest, ok = strings.Cut(rest, "\n")
	if !ok {
		return "", ""
	}
	line, _, _ := strings.Cut(rest, "\n")
	parts := strings.Split(line, " / ")
	if len(parts) < 2 {
		return "", ""
	}
	return strings.Trim(strings.TrimSpace(parts[0]), `"`), strings.Trim(strings.TrimSpace(parts[1]), `"`)
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
				meta, _ = decodeMeta(parts[2])
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
			meta, _ = decodeMeta(parts[2])
		} else {
			meta, _ = decodeMeta(parts[1])
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
	if gdlErr != nil {
		res.Err = gdlErr
	}
	return res, nil
}

func (r *ResolveResult) setCategory(meta map[string]any) {
	if r.Category == "" {
		r.Category = kwdict.String(meta, "category")
		r.Subcategory = kwdict.String(meta, "subcategory")
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
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

func readMeta(path string) (map[string]any, error) {
	data, err := os.ReadFile(path + ".json")
	if err != nil {
		return nil, fmt.Errorf("reading sidecar %s.json: %w", path, err)
	}
	meta, err := decodeMeta(data)
	if err != nil {
		return nil, fmt.Errorf("sidecar %s.json is not valid JSON", path)
	}
	return meta, nil
}

// Numbers stay json.Number: post ids above 2^53 do not survive float64.
func decodeMeta(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var meta map[string]any
	err := dec.Decode(&meta)
	return meta, err
}
