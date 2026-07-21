package monbooru

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/queue"
)

// PushMeta is the mapped metadata sent alongside a file.
type PushMeta struct {
	Filename        string
	Tags            []string
	Source          string
	PostID          string
	URL             string
	Collection      string
	CollectionOrder int
	Via             string
	Folder          string
	Commentary      string
	Original        string
	ParentURL       string
	Notes           []NoteBox
}

// NoteBox is one positional note pushed alongside a file; the field names match
// monbooru's create/enrich `notes` payload.
type NoteBox struct {
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	Body string `json:"body"`
}

// Result is a successful push outcome.
type Result struct {
	Outcome     queue.Outcome
	MonbooruID  int64
	SHA256      string
	TagWarnings []string
	// MergeNote summarises what a duplicate merge folded into the existing
	// image (e.g. "+7 tags"); empty for a create or a no-op duplicate.
	MergeNote string
}

// Gallery is one entry from GET /api/v1/galleries.
type Gallery struct {
	Name   string `json:"name"`
	Images int    `json:"images"`
	Tags   int    `json:"tags"`
	Active bool   `json:"active"`
}

// Client talks to a monbooru instance over its REST API. It reads the API URL
// and token from the config provider on each request, so a settings save takes
// effect without a restart and without racing the worker goroutine.
type Client struct {
	cfg  *config.Provider
	http *http.Client
}

// New builds a client from the config provider. Request lifetime is bounded by
// the caller's context; a generous backstop timeout guards against a wedged
// connection.
func New(cfg *config.Provider) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 10 * time.Minute}}
}

func (c *Client) base() string {
	return strings.TrimRight(c.cfg.Current().Monbooru.APIURL, "/")
}

func (c *Client) token() string {
	return c.cfg.Current().Monbooru.APIToken
}

func (c *Client) authHeader(req *http.Request) {
	if tok := c.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// maxResponseBytes bounds how much of a monbooru response is read into
// memory. Roomy enough for any real list body; a body past it errors
// loudly instead of being silently truncated into a decode failure.
const maxResponseBytes = 16 << 20

// do sends one API request and returns the response with its body read
// (bounded) and closed. bearer is the token to authenticate with, empty for
// the unauthenticated pairing endpoints. A build or transport failure returns
// the raw error; the caller decides how to classify it.
func (c *Client) do(ctx context.Context, method, endpoint, contentType string, body io.Reader, bearer string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := readCappedBody(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, respBody, nil
}

// readCappedBody reads a response body up to maxResponseBytes, failing past the
// cap instead of handing back a truncated body that only fails later, at the
// decode.
func readCappedBody(r io.Reader) ([]byte, error) {
	body, _ := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("monbooru response body exceeds %d bytes", maxResponseBytes)
	}
	return body, nil
}

// transportError classifies a failed request: a canceled job aborts the
// in-flight call, which must not read to the future extension like a real
// connectivity failure; anything else got no HTTP response (connection
// refused, DNS failure, timeout) and is unreachable.
func transportError(ctx context.Context, err error) *queue.CodedError {
	if ctx.Err() != nil {
		return &queue.CodedError{Code: queue.ErrCodeCanceled, Msg: ctx.Err().Error()}
	}
	return &queue.CodedError{Code: queue.ErrCodeMonbooruUnreachable, Msg: err.Error()}
}

// PushImageFile streams a file plus mapped metadata to
// POST /api/v1/images?gallery=<name>, hashing it in a streaming pass and writing
// the multipart body through an io.Pipe so the file is never buffered whole in
// memory. The digest is taken from the same read as the upload, so the
// permalink hash always describes the pushed bytes.
func (c *Client) PushImageFile(ctx context.Context, path string, meta PushMeta, gallery string) (*Result, error) {
	// Opened here rather than inside the body goroutine: a scratch file that is
	// gone or unreadable is a local fault, and a failure in the goroutine would
	// reach the transport as a request error and be coded as a monbooru outage.
	f, err := os.Open(path)
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	contentType := w.FormDataContentType()
	h := sha256.New()
	go func() {
		defer f.Close()
		pw.CloseWithError(streamFileMultipart(w, f, meta, h))
	}()
	res, err := c.sendPush(ctx, c.imagesEndpoint(gallery), contentType, pr)
	if err != nil {
		// sendPush can return before consuming the body (a request-build error
		// never reads the pipe); unblock the writer goroutine so it closes the file.
		pr.CloseWithError(err)
		return res, err
	}
	// A success response means monbooru consumed the whole body, so the hash
	// has seen every pushed byte.
	res.SHA256 = hex.EncodeToString(h.Sum(nil))
	return res, nil
}

// EnrichPayload is the metadata-only body POSTed to the enrich endpoint - a
// push with no file, used to fold a refetched source's tags, commentary, and
// notes into an image monbooru already holds.
type EnrichPayload struct {
	Tags       []string  `json:"tags"`
	Source     string    `json:"source"`
	PostID     string    `json:"post_id,omitempty"`
	URL        string    `json:"url"`
	SourceMD5  string    `json:"source_md5,omitempty"`
	Verify     bool      `json:"verify"`
	Similarity float64   `json:"similarity,omitempty"`
	Commentary string    `json:"commentary,omitempty"`
	Original   string    `json:"original,omitempty"`
	ParentURL  string    `json:"parent_url,omitempty"`
	Notes      []NoteBox `json:"notes,omitempty"`
}

// EnrichImage applies fetched metadata to an existing monbooru image via
// POST /api/v1/images/{id}/enrich. 200 -> enriched (with any merge note); 409 ->
// hash_mismatch (the post no longer serves the same file, so nothing changed);
// any other non-2xx -> rejected.
func (c *Client) EnrichImage(ctx context.Context, imageID int64, gallery string, payload EnrichPayload) (*Result, error) {
	endpoint := c.base() + "/api/v1/images/" + strconv.FormatInt(imageID, 10) + "/enrich"
	if gallery != "" {
		endpoint += "?gallery=" + url.QueryEscape(gallery)
	}
	payload.Source = trimToLen(payload.Source, maxSourceLen)
	payload.PostID = trimToLen(payload.PostID, maxPostIDLen)
	payload.URL = trimToLen(payload.URL, maxURLLen)
	payload.Commentary = trimToLen(payload.Commentary, maxCommentaryLen)
	payload.Original = trimToLen(payload.Original, maxOriginalLen)
	payload.ParentURL = trimToLen(payload.ParentURL, maxURLLen)
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeMappingFailed, Msg: err.Error()}
	}
	resp, respBody, err := c.do(ctx, http.MethodPost, endpoint, "application/json", bytes.NewReader(buf), c.token())
	if err != nil {
		return nil, transportError(ctx, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return &Result{Outcome: queue.OutcomeEnriched, MonbooruID: imageID, MergeNote: enrichMergeNote(respBody)}, nil
	case http.StatusConflict:
		return nil, &queue.CodedError{Code: queue.ErrCodeHashMismatch, Msg: apiErrMessage(respBody, resp.Status)}
	default:
		return nil, &queue.CodedError{Code: queue.ErrCodeMonbooruRejected, Msg: apiErrMessage(respBody, resp.Status)}
	}
}

// enrichMergeNote reads the optional {merge:{...}} block from an enrich 200.
func enrichMergeNote(data []byte) string {
	var env struct {
		Merge json.RawMessage `json:"merge"`
	}
	if json.Unmarshal(data, &env) != nil || len(env.Merge) == 0 {
		return ""
	}
	return mergeNote(env.Merge)
}

// ReportFetchOutcome tells monbooru how a metadata fetch ended when it failed
// before EnrichImage could run, so the detail page's poll surfaces the outcome
// instead of spinning to its cap. Best-effort and advisory: the queue row holds
// the authoritative result, so a report monbooru can't accept is not fatal.
func (c *Client) ReportFetchOutcome(ctx context.Context, imageID int64, gallery, state, message string) error {
	endpoint := c.base() + "/api/v1/images/" + strconv.FormatInt(imageID, 10) + "/fetch-status"
	if gallery != "" {
		endpoint += "?gallery=" + url.QueryEscape(gallery)
	}
	buf, err := json.Marshal(map[string]string{"state": state, "message": message})
	if err != nil {
		return err
	}
	resp, _, err := c.do(ctx, http.MethodPost, endpoint, "application/json", bytes.NewReader(buf), c.token())
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("monbooru fetch-status returned %s", resp.Status)
	}
	return nil
}

// FetchThumbnail downloads an image's static thumbnail - the downscaled query
// bytes a similarity lookup uploads, so the original never leaves the LAN. A
// thumbnail is a few KB, comfortably inside do()'s bounded body read.
func (c *Client) FetchThumbnail(ctx context.Context, imageID int64, gallery string) ([]byte, error) {
	endpoint := c.base() + "/api/v1/images/" + strconv.FormatInt(imageID, 10) + "/thumbnail"
	if gallery != "" {
		endpoint += "?gallery=" + url.QueryEscape(gallery)
	}
	resp, body, err := c.do(ctx, http.MethodGet, endpoint, "", nil, c.token())
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &queue.CodedError{Code: queue.ErrCodeMonbooruRejected, Msg: apiErrMessage(body, resp.Status)}
	}
	return body, nil
}

// imagesEndpoint is the push URL with the optional gallery selector.
func (c *Client) imagesEndpoint(gallery string) string {
	endpoint := c.base() + "/api/v1/images"
	if gallery != "" {
		endpoint += "?gallery=" + url.QueryEscape(gallery)
	}
	return endpoint
}

// sendPush posts a prepared multipart body and classifies the response: 201 ->
// created, 200 (alias) -> duplicate, 413 -> file_too_large, a missing response
// -> monbooru_unreachable (canceled if the job was aborted), any other 4xx/5xx
// -> monbooru_rejected. The success body is polymorphic (a bare image or an
// envelope); the id and any tag warnings are read from whichever arrives.
func (c *Client) sendPush(ctx context.Context, endpoint, contentType string, body io.Reader) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeMonbooruRejected, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", contentType)
	c.authHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer resp.Body.Close()
	respBody, err := readCappedBody(resp.Body)
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeMonbooruRejected, Msg: err.Error()}
	}

	switch resp.StatusCode {
	case http.StatusCreated:
		id, warnings, _ := parseImageResult(respBody)
		return &Result{Outcome: queue.OutcomeCreated, MonbooruID: id, TagWarnings: warnings}, nil
	case http.StatusOK:
		id, warnings, merge := parseImageResult(respBody)
		return &Result{Outcome: queue.OutcomeDuplicate, MonbooruID: id, TagWarnings: warnings, MergeNote: merge}, nil
	case http.StatusRequestEntityTooLarge:
		return nil, &queue.CodedError{Code: queue.ErrCodeFileTooLarge, Msg: apiErrMessage(respBody, resp.Status)}
	default:
		return nil, &queue.CodedError{Code: queue.ErrCodeMonbooruRejected, Msg: apiErrMessage(respBody, resp.Status)}
	}
}

// streamFileMultipart writes the multipart body for a file push into w (backed
// by an io.Pipe): the file part streamed from src, then the metadata fields.
// The file bytes are teed into hash on the way through.
func streamFileMultipart(w *multipart.Writer, src io.Reader, meta PushMeta, hash io.Writer) error {
	if err := writeFilePart(w, meta.Filename, io.TeeReader(src, hash)); err != nil {
		return err
	}
	if err := writeMetaFields(w, meta); err != nil {
		return err
	}
	return w.Close()
}

// writeFilePart writes the "file" part, streaming from src.
func writeFilePart(w *multipart.Writer, filename string, src io.Reader) error {
	if filename == "" {
		filename = "upload"
	}
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, src)
	return err
}

// monbooru caps source at 200 and url at 2048 and rejects anything longer, so
// trim before the push - a directlink's submitted URL can blow past 2048.
const (
	maxSourceLen     = 200
	maxPostIDLen     = 64
	maxURLLen        = 2048
	maxCommentaryLen = 10000
	maxOriginalLen   = 2048
)

// trimToLen caps s to max bytes without splitting a multibyte rune.
func trimToLen(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// writeMetaFields writes the non-file form fields, omitting empty optionals so
// a bare push touches nothing on monbooru's side. The first field-write error
// wins: a failing pipe must fail the push, not send a body missing fields.
func writeMetaFields(w *multipart.Writer, meta PushMeta) error {
	var err error
	field := func(name, value string) {
		if err == nil && value != "" {
			err = w.WriteField(name, value)
		}
	}
	if len(meta.Tags) > 0 {
		tagsJSON, jerr := json.Marshal(meta.Tags)
		if jerr != nil {
			return jerr
		}
		field("tags", string(tagsJSON))
	}
	field("via", meta.Via)
	field("folder", meta.Folder)
	field("source", trimToLen(meta.Source, maxSourceLen))
	field("post_id", trimToLen(meta.PostID, maxPostIDLen))
	field("url", trimToLen(meta.URL, maxURLLen))
	field("commentary", trimToLen(meta.Commentary, maxCommentaryLen))
	field("original", trimToLen(meta.Original, maxOriginalLen))
	field("parent_url", trimToLen(meta.ParentURL, maxURLLen))
	if len(meta.Notes) > 0 {
		if notesJSON, jerr := json.Marshal(meta.Notes); jerr == nil {
			field("notes", string(notesJSON))
		}
	}
	field("collection", meta.Collection)
	if meta.CollectionOrder > 0 {
		field("collection_order", strconv.Itoa(meta.CollectionOrder))
	}
	return err
}

// parseImageResult reads the monbooru id, tag warnings, and duplicate-merge
// note from the polymorphic
// create/duplicate body: a bare image object, or an envelope keyed by "image".
func parseImageResult(data []byte) (id int64, warnings []string, merge string) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, nil, ""
	}
	imgRaw, enveloped := env["image"]
	if enveloped {
		if w, ok := env["tag_warnings"]; ok {
			_ = json.Unmarshal(w, &warnings)
		}
		if m, ok := env["merge"]; ok {
			merge = mergeNote(m)
		}
	} else {
		imgRaw = data
	}
	var img struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(imgRaw, &img)
	return img.ID, warnings, merge
}

// mergeNote renders monbooru's duplicate-merge summary as a short human
// string ("+7 tags", "+7 tags, 2 no longer listed, rating"); empty when
// nothing changed. tags_removed is the pre-retire spelling an older
// monbooru still sends for tags it deleted outright.
func mergeNote(raw json.RawMessage) string {
	var m struct {
		TagsAdded    int  `json:"tags_added"`
		TagsRemoved  int  `json:"tags_removed"`
		TagsRetired  int  `json:"tags_retired"`
		RatingFilled bool `json:"rating_filled"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var parts []string
	if m.TagsAdded > 0 {
		parts = append(parts, fmt.Sprintf("+%d tags", m.TagsAdded))
	}
	if m.TagsRemoved > 0 {
		parts = append(parts, fmt.Sprintf("-%d tags", m.TagsRemoved))
	}
	if m.TagsRetired > 0 {
		parts = append(parts, fmt.Sprintf("%d no longer listed", m.TagsRetired))
	}
	if m.RatingFilled {
		parts = append(parts, "rating")
	}
	return strings.Join(parts, ", ")
}

// rejected classifies a non-OK response as monbooru_rejected.
func rejected(body []byte, status string) *queue.CodedError {
	return &queue.CodedError{Code: queue.ErrCodeMonbooruRejected, Msg: apiErrMessage(body, status)}
}

// apiErrMessage pulls monbooru's {error,code} message out of an error body,
// falling back to the HTTP status line.
func apiErrMessage(body []byte, status string) string {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return status
}

// ListGalleries reads GET /api/v1/galleries for the settings dropdown.
func (c *Client) ListGalleries(ctx context.Context) ([]Gallery, error) {
	resp, body, err := c.do(ctx, http.MethodGet, c.base()+"/api/v1/galleries", "", nil, c.token())
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeMonbooruUnreachable, Msg: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, rejected(body, resp.Status)
	}
	var galleries []Gallery
	if err := json.Unmarshal(body, &galleries); err != nil {
		return nil, fmt.Errorf("decoding galleries: %w", err)
	}
	return galleries, nil
}

// TestConnection hits GET /api/v1/ to verify the configured URL and token,
// backing the settings "test connection" button. On success it returns the
// monbooru version from the root response, or "" when the response omits it
// (an older monbooru).
func (c *Client) TestConnection(ctx context.Context) (string, error) {
	resp, body, err := c.do(ctx, http.MethodGet, c.base()+"/api/v1/", "", nil, c.token())
	if err != nil {
		return "", &queue.CodedError{Code: queue.ErrCodeMonbooruUnreachable, Msg: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return "", rejected(body, resp.Status)
	}
	var info struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &info)
	return info.Version, nil
}

// PairRequest sends a pairing offer to monbooru and returns the request id. The
// pairing endpoints are unauthenticated (the operator approves in monbooru), so
// no bearer token is sent. selfURL is how monbooru should reach this monloader;
// peerToken is the token monbooru will use to call back.
func (c *Client) PairRequest(ctx context.Context, peerToken, selfURL string, scopes []string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"app":              "monloader",
		"url":              selfURL,
		"requested_scopes": scopes,
		"peer_token":       peerToken,
	})
	resp, body, err := c.do(ctx, http.MethodPost, c.base()+"/api/v1/pair/request", "application/json", bytes.NewReader(payload), "")
	if err != nil {
		return "", err
	}
	var out struct {
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return "", fmt.Errorf("%s", out.Error)
		}
		return "", fmt.Errorf("monbooru pairing request failed: %s", resp.Status)
	}
	return out.RequestID, nil
}

// PairStatus polls a pairing request. On approval it returns the issued token
// once; otherwise the token is empty and status carries the state.
func (c *Client) PairStatus(ctx context.Context, requestID string) (status, token string, err error) {
	resp, body, err := c.do(ctx, http.MethodGet, c.base()+"/api/v1/pair/status?id="+url.QueryEscape(requestID), "", nil, "")
	if err != nil {
		return "", "", err
	}
	var out struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("monbooru pairing status failed: %s", resp.Status)
	}
	return out.Status, out.Token, nil
}

// PairTeardown asks monbooru to drop its side of the pairing, authenticating
// with the token monbooru issued (captured by the caller before it clears the
// stored credential). Best-effort: the caller proceeds whatever this returns.
func (c *Client) PairTeardown(ctx context.Context, token string) error {
	resp, _, err := c.do(ctx, http.MethodPost, c.base()+"/api/v1/pair/remove", "", nil, token)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("monbooru pairing remove failed: %s", resp.Status)
	}
	return nil
}
