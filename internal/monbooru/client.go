package monbooru

import (
	"bytes"
	"cmp"
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

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
)

// The JSON tags are the enrich wire names; writeMetaFields spells the
// same names by hand for a push, so a new field goes there too.
type PostFields struct {
	Tags                 []string `json:"tags"`
	Source               string   `json:"source"`
	PostID               string   `json:"post_id,omitempty"`
	URL                  string   `json:"url"`
	Commentary           string   `json:"commentary,omitempty"`
	CommentaryTranslated string   `json:"commentary_translated,omitempty"`
	// The same two bodies in the source's markup, which monbooru converts
	// and prefers over the plain fields.
	CommentaryDText           string    `json:"commentary_dtext,omitempty"`
	CommentaryTranslatedDText string    `json:"commentary_translated_dtext,omitempty"`
	Original                  string    `json:"original,omitempty"`
	ParentURL                 string    `json:"parent_url,omitempty"`
	Notes                     []NoteBox `json:"notes,omitempty"`
	// What the post says about the file it serves, which monbooru checks
	// against the local file.
	PostWidth  int    `json:"post_width,omitempty"`
	PostHeight int    `json:"post_height,omitempty"`
	PostSize   int64  `json:"post_size,omitempty"`
	PostExt    string `json:"post_ext,omitempty"`
}

type PushMeta struct {
	PostFields
	// Outside PostFields, whose JSON names are the enrich's: there it is
	// source_md5.
	MD5             string
	Filename        string
	Collection      string
	CollectionOrder int
	Via             string
	Folder          string
}

type NoteBox struct {
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	Body string `json:"body"`
	// monbooru converts it and falls back to Body, which an older
	// monbooru reads alone.
	BodyHTML string `json:"body_html,omitempty"`
}

type Result struct {
	Outcome     outcome.Outcome
	MonbooruID  int64
	SHA256      string
	TagWarnings []string
	MergeNote   string
}

type Gallery struct {
	Name   string `json:"name"`
	Images int    `json:"images"`
	Tags   int    `json:"tags"`
	Active bool   `json:"active"`
}

// Client reads the API URL and token on every request, so a settings save
// lands without a restart.
type Client struct {
	cfg  *config.Provider
	http *http.Client
}

// The client timeout is only a backstop for a wedged connection; the
// caller's context bounds each request. A redirect is refused: Go would
// re-send a POST as a GET, which monbooru answers 200 for a replace, and
// carry the bearer token to the new host.
func New(cfg *config.Provider) *Client {
	return &Client{cfg: cfg, http: &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
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

const maxResponseBytes = 16 << 20

// Errors come back raw for the caller to classify.
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

// Failing here, not at the decode: a truncated 201 would mark the item
// created with no monbooru id.
func readCappedBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the monbooru response body: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("monbooru response body exceeds %d bytes", maxResponseBytes)
	}
	return body, nil
}

// A canceled job's aborted call must not read as a connectivity failure.
func transportError(ctx context.Context, err error) *outcome.CodedError {
	if ctx.Err() != nil {
		return &outcome.CodedError{Code: outcome.ErrCodeCanceled, Msg: ctx.Err().Error()}
	}
	return &outcome.CodedError{Code: outcome.ErrCodeMonbooruUnreachable, Msg: err.Error()}
}

// do() cannot stand in: a build or oversize-body failure codes as
// monbooru_rejected here, and only a missing response goes through
// transportError.
func (c *Client) postMultipart(ctx context.Context, endpoint, contentType string, body io.Reader) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, nil, &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", contentType)
	c.authHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, transportError(ctx, err)
	}
	defer resp.Body.Close()
	respBody, err := readCappedBody(resp.Body)
	if err != nil {
		return nil, nil, &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: err.Error()}
	}
	return resp, respBody, nil
}

func (c *Client) PushImageFile(ctx context.Context, path string, meta PushMeta, gallery string) (*Result, error) {
	return c.streamUpload(ctx, path, c.imagesEndpoint(gallery), meta, c.sendPush)
}

func (c *Client) ReplaceImageFile(ctx context.Context, imageID int64, path string, meta PushMeta, gallery string) (*Result, error) {
	return c.streamUpload(ctx, path, c.imageEndpoint(imageID, "file", gallery), meta,
		func(ctx context.Context, endpoint, contentType string, body io.Reader) (*Result, error) {
			return c.sendReplace(ctx, endpoint, contentType, body, imageID)
		})
}

// The hash comes from the same read as the upload, so the permalink
// describes the pushed bytes, and the pipe keeps the file out of memory.
func (c *Client) streamUpload(ctx context.Context, path, endpoint string, meta PushMeta,
	send func(ctx context.Context, endpoint, contentType string, body io.Reader) (*Result, error)) (*Result, error) {
	// Opened here, not in the goroutine: a failure there would reach the
	// transport and be coded as a monbooru outage.
	f, err := os.Open(path)
	if err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	contentType := w.FormDataContentType()
	h := sha256.New()
	go func() {
		defer f.Close()
		pw.CloseWithError(streamFileMultipart(w, f, meta, h))
	}()
	res, err := send(ctx, endpoint, contentType, pr)
	if err != nil {
		// send may return without reading the pipe; this unblocks the
		// writer so it closes the file.
		pr.CloseWithError(err)
		return res, err
	}
	// A success means monbooru read the whole body, so the hash has seen
	// every byte.
	res.SHA256 = hex.EncodeToString(h.Sum(nil))
	return res, nil
}

// A 409's refusal passes through as-is: monbooru has already recorded it
// on its fetch status.
func (c *Client) sendReplace(ctx context.Context, endpoint, contentType string, body io.Reader, imageID int64) (*Result, error) {
	resp, respBody, err := c.postMultipart(ctx, endpoint, contentType, body)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		_, warnings := enrichResult(respBody)
		return &Result{Outcome: outcome.Replaced, MonbooruID: imageID, MergeNote: replaceNote(respBody), TagWarnings: warnings}, nil
	case http.StatusConflict:
		msg, code := apiErr(respBody, resp.Status)
		if code != outcome.ErrCodeAlreadyExists && code != outcome.ErrCodeWrongType {
			code = outcome.ErrCodeMonbooruRejected
		}
		return nil, &outcome.CodedError{Code: code, Msg: msg}
	case http.StatusRequestEntityTooLarge:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeFileTooLarge, Msg: apiErrMessage(respBody, resp.Status)}
	default:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: apiErrMessage(respBody, resp.Status)}
	}
}

func replaceNote(data []byte) string {
	var env struct {
		Replaced bool            `json:"replaced"`
		Merge    json.RawMessage `json:"merge"`
	}
	if json.Unmarshal(data, &env) != nil {
		return ""
	}
	note := mergeNote(env.Merge)
	if !env.Replaced {
		if note == "" {
			return "file already matched"
		}
		return "file already matched; " + note
	}
	return note
}

type EnrichPayload struct {
	// Embedded, so encoding/json flattens it into the same wire shape.
	PostFields
	SourceMD5  string  `json:"source_md5,omitempty"`
	Verify     bool    `json:"verify"`
	Similarity float64 `json:"similarity,omitempty"`
}

// A 409 means the post no longer serves the same file, so nothing changed.
func (c *Client) EnrichImage(ctx context.Context, imageID int64, gallery string, payload EnrichPayload) (*Result, error) {
	payload.Source = trimToLen(payload.Source, maxSourceLen)
	payload.PostID = trimToLen(payload.PostID, maxPostIDLen)
	payload.URL = trimToLen(payload.URL, maxURLLen)
	payload.Commentary = trimToLen(payload.Commentary, maxCommentaryLen)
	payload.CommentaryTranslated = trimToLen(payload.CommentaryTranslated, maxCommentaryLen)
	payload.CommentaryDText = trimToLen(payload.CommentaryDText, maxCommentaryDTextLen)
	payload.CommentaryTranslatedDText = trimToLen(payload.CommentaryTranslatedDText, maxCommentaryDTextLen)
	payload.Original = trimToLen(payload.Original, maxOriginalLen)
	payload.ParentURL = trimToLen(payload.ParentURL, maxURLLen)
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMappingFailed, Msg: err.Error()}
	}
	resp, respBody, err := c.do(ctx, http.MethodPost, c.imageEndpoint(imageID, "enrich", gallery),
		"application/json", bytes.NewReader(buf), c.token())
	if err != nil {
		return nil, transportError(ctx, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		note, warnings := enrichResult(respBody)
		return &Result{Outcome: outcome.Enriched, MonbooruID: imageID, MergeNote: note, TagWarnings: warnings}, nil
	case http.StatusConflict:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeHashMismatch, Msg: apiErrMessage(respBody, resp.Status)}
	default:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: apiErrMessage(respBody, resp.Status)}
	}
}

// The warnings are what tells a partly applied lookup from a full one.
func enrichResult(data []byte) (note string, warnings []string) {
	var env struct {
		Merge       json.RawMessage `json:"merge"`
		TagWarnings []string        `json:"tag_warnings"`
	}
	if json.Unmarshal(data, &env) != nil {
		return "", nil
	}
	return mergeNote(env.Merge), env.TagWarnings
}

// For a fetch that failed before EnrichImage ran; without it monbooru's
// detail page polls to its cap. kind and backend name the in-flight lookup
// the report concludes. Best-effort: the queue row is authoritative.
func (c *Client) ReportFetchOutcome(ctx context.Context, imageID int64, gallery, kind, backend, state, message string) error {
	body := map[string]string{"kind": kind, "state": state, "message": message}
	if backend != "" {
		body["backend"] = backend
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, _, err := c.do(ctx, http.MethodPost, c.imageEndpoint(imageID, "fetch-status", gallery),
		"application/json", bytes.NewReader(buf), c.token())
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("monbooru fetch-status returned %s", resp.Status)
	}
	return nil
}

func (c *Client) getOK(ctx context.Context, endpoint string) ([]byte, error) {
	resp, body, err := c.do(ctx, http.MethodGet, endpoint, "", nil, c.token())
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, rejected(body, resp.Status)
	}
	return body, nil
}

// A similarity lookup uploads the thumbnail, so the original never leaves
// the LAN.
func (c *Client) FetchThumbnail(ctx context.Context, imageID int64, gallery string) ([]byte, error) {
	return c.getOK(ctx, c.imageEndpoint(imageID, "thumbnail", gallery))
}

// The /i/<sha256> permalink resolves in whichever gallery holds the
// image; /images/<id> would open the active gallery's image of that id.
func (c *Client) ImageSHA256(ctx context.Context, imageID int64, gallery string) (string, error) {
	body, err := c.getOK(ctx, withGallery(c.base()+"/api/v1/images/"+strconv.FormatInt(imageID, 10), gallery))
	if err != nil {
		return "", err
	}
	var img struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(body, &img); err != nil {
		return "", err
	}
	return img.SHA256, nil
}

func (c *Client) imagesEndpoint(gallery string) string {
	return withGallery(c.base()+"/api/v1/images", gallery)
}

func (c *Client) imageEndpoint(imageID int64, suffix, gallery string) string {
	return withGallery(c.base()+"/api/v1/images/"+strconv.FormatInt(imageID, 10)+"/"+suffix, gallery)
}

func withGallery(endpoint, gallery string) string {
	if gallery == "" {
		return endpoint
	}
	return endpoint + "?gallery=" + url.QueryEscape(gallery)
}

func (c *Client) sendPush(ctx context.Context, endpoint, contentType string, body io.Reader) (*Result, error) {
	resp, respBody, err := c.postMultipart(ctx, endpoint, contentType, body)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusCreated:
		id, warnings, _ := parseImageResult(respBody)
		return &Result{Outcome: outcome.Created, MonbooruID: id, TagWarnings: warnings}, nil
	case http.StatusOK:
		id, warnings, merge := parseImageResult(respBody)
		return &Result{Outcome: outcome.Duplicate, MonbooruID: id, TagWarnings: warnings, MergeNote: merge}, nil
	case http.StatusRequestEntityTooLarge:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeFileTooLarge, Msg: apiErrMessage(respBody, resp.Status)}
	default:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: apiErrMessage(respBody, resp.Status)}
	}
}

func streamFileMultipart(w *multipart.Writer, src io.Reader, meta PushMeta, hash io.Writer) error {
	if err := writeFilePart(w, meta.Filename, io.TeeReader(src, hash)); err != nil {
		return err
	}
	if err := writeMetaFields(w, meta); err != nil {
		return err
	}
	return w.Close()
}

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

// monbooru rejects longer values, so they are trimmed before the push; a
// directlink's URL can pass 2048. The caps count characters, not bytes.
const (
	maxSourceLen          = 200
	maxPostIDLen          = 64
	maxURLLen             = 2048
	maxCommentaryLen      = 10000
	maxCommentaryDTextLen = 4 * maxCommentaryLen
	maxOriginalLen        = 2048
)

func trimToLen(s string, limit int) string {
	for i := range s {
		if limit == 0 {
			return s[:i]
		}
		limit--
	}
	return s
}

// Empty fields are omitted so a bare push touches nothing on monbooru's
// side. The first write error wins: a failing pipe must fail the push,
// not send a body missing fields.
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
	field("commentary_translated", trimToLen(meta.CommentaryTranslated, maxCommentaryLen))
	field("commentary_dtext", trimToLen(meta.CommentaryDText, maxCommentaryDTextLen))
	field("commentary_translated_dtext", trimToLen(meta.CommentaryTranslatedDText, maxCommentaryDTextLen))
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
	if meta.PostWidth > 0 {
		field("post_width", strconv.Itoa(meta.PostWidth))
	}
	if meta.PostHeight > 0 {
		field("post_height", strconv.Itoa(meta.PostHeight))
	}
	if meta.PostSize > 0 {
		field("post_size", strconv.FormatInt(meta.PostSize, 10))
	}
	field("post_ext", meta.PostExt)
	field("md5", meta.MD5)
	return err
}

// The body is a bare image object or an envelope keyed by "image".
func parseImageResult(data []byte) (id int64, warnings []string, merge string) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, nil, ""
	}
	imgRaw, enveloped := env["image"]
	if enveloped {
		// A missing key is a nil RawMessage, which fails to unmarshal and
		// leaves both empty.
		_ = json.Unmarshal(env["tag_warnings"], &warnings)
		merge = mergeNote(env["merge"])
	} else {
		imgRaw = data
	}
	var img struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(imgRaw, &img)
	return img.ID, warnings, merge
}

// tags_removed is what an older monbooru sends for tags it deleted outright.
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

func rejected(body []byte, status string) *outcome.CodedError {
	return &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: apiErrMessage(body, status)}
}

func apiErr(body []byte, status string) (msg, code string) {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &e) != nil {
		return status, ""
	}
	return cmp.Or(e.Error, status), e.Code
}

func apiErrMessage(body []byte, status string) string {
	msg, _ := apiErr(body, status)
	return msg
}

func (c *Client) ListGalleries(ctx context.Context) ([]Gallery, error) {
	body, err := c.getOK(ctx, c.base()+"/api/v1/galleries")
	if err != nil {
		return nil, err
	}
	var galleries []Gallery
	if err := json.Unmarshal(body, &galleries); err != nil {
		return nil, fmt.Errorf("decoding galleries: %w", err)
	}
	return galleries, nil
}

// The version is "" from an older monbooru that omits it.
func (c *Client) TestConnection(ctx context.Context) (string, error) {
	body, err := c.getOK(ctx, c.base()+"/api/v1/")
	if err != nil {
		return "", err
	}
	var info struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &info)
	return info.Version, nil
}

// APIPair reads monbooru's /health, which needs no token; zeroes when it
// predates the fields.
func (c *Client) APIPair(ctx context.Context) (api, minPeer int) {
	resp, body, err := c.do(ctx, http.MethodGet, c.base()+"/health", "", nil, "")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0, 0
	}
	var h struct {
		API        int `json:"api"`
		MinPeerAPI int `json:"min_peer_api"`
	}
	_ = json.Unmarshal(body, &h)
	return h.API, h.MinPeerAPI
}

// Unauthenticated: the operator approves the request in monbooru.
// peerToken is what monbooru will call back with.
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
	// A 200 from anything but monbooru's pairing endpoint carries no id.
	if out.RequestID == "" {
		return "", fmt.Errorf("monbooru answered the pairing request with no request id")
	}
	return out.RequestID, nil
}

// The token arrives once, on approval. A 404 means monbooru dropped the
// request (a restart or its TTL), so it is status "not_found", not an error.
func (c *Client) PairStatus(ctx context.Context, requestID string) (status, token string, err error) {
	resp, body, err := c.do(ctx, http.MethodGet, c.base()+"/api/v1/pair/status?id="+url.QueryEscape(requestID), "", nil, "")
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode == http.StatusNotFound {
		return "not_found", "", nil
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

// The caller passes the issued token, captured before it clears the stored one.
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
