// Package similarity queries perceptual image-similarity services - danbooru's
// iqdb instance and SauceNAO - for the booru posts visually matching an image.
// The services return pointers (post URLs and scores), never metadata: each
// candidate is then fetched through gallery-dl like an operator-pasted URL, so
// gallery-dl stays the only metadata extractor.
package similarity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/queue"
)

// Candidate is one post a service offered for the queried image.
type Candidate struct {
	Site       string  // gallery-dl category ("danbooru", "gelbooru", ...)
	URL        string  // canonical post URL
	Similarity float64 // 0-100
}

// Result is one service's answer. Extras are hits outside the curated sites
// (SauceNAO indexes anime databases, pixiv, twitter, ...): nothing there can
// be fetched through gallery-dl, so they never join the candidate walk, but a
// full miss lists them on the trail so the operator still sees the best fit
// the service found. DailyRemaining is the service-reported query quota left
// (-1 when the service reports none), surfaced by the settings test button.
type Result struct {
	Candidates     []Candidate
	Extras         []Candidate
	DailyRemaining int
}

// Cooldowns after a rate-limited answer, so queued lookups skip a limited
// service instead of burning requests against a closed window. SauceNAO
// meters a 30 s window and a daily quota; iqdb rides danbooru's general
// throttle.
const (
	saucenaoShortCooldown = 35 * time.Second
	saucenaoDailyCooldown = time.Hour
	iqdbCooldown          = time.Minute
)

// Client queries the two services with credentials read from the config on
// each call. canonical and postURL come from the mapper: the first recognizes
// a service's result URL and rebuilds it canonically, the second builds a
// danbooru post URL from the bare id iqdb answers with.
type Client struct {
	cfg       *config.Provider
	http      *http.Client
	canonical func(rawURL string) (site, canonical string, ok bool)
	postURL   func(site, id string) string

	// Service endpoints; fixed except in tests.
	iqdbURL     string
	saucenaoURL string

	mu           sync.Mutex
	limitedUntil map[string]time.Time
}

// New builds a Client against the services' real endpoints.
func New(cfg *config.Provider, canonical func(string) (string, string, bool), postURL func(site, id string) string) *Client {
	return &Client{
		cfg:          cfg,
		http:         &http.Client{Timeout: 30 * time.Second},
		canonical:    canonical,
		postURL:      postURL,
		iqdbURL:      "https://danbooru.donmai.us/iqdb_queries.json",
		saucenaoURL:  "https://saucenao.com/search.php",
		limitedUntil: map[string]time.Time{},
	}
}

// Missing reports whether a service lacks the credential it needs, with the
// label for the walk trail. iqdb's file upload is CSRF-refused without
// danbooru API authentication; saucenao's API refuses anonymous queries.
func (c *Client) Missing(service string) (string, bool) {
	switch service {
	case "iqdb":
		site := c.cfg.Current().FindSite("danbooru")
		if site == nil || site.Username == "" || site.APIKey == "" {
			return "danbooru api key", true
		}
	case "saucenao":
		if c.cfg.Current().Lookup.Saucenao.APIKey == "" {
			return "api key", true
		}
	}
	return "", false
}

// LimitedUntil returns when a service's rate-limit cooldown ends (zero when
// it is not cooling down).
func (c *Client) LimitedUntil(service string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitedUntil[service]
}

func (c *Client) limit(service string, d time.Duration) {
	c.mu.Lock()
	if until := time.Now().Add(d); until.After(c.limitedUntil[service]) {
		c.limitedUntil[service] = until
	}
	c.mu.Unlock()
}

// Search asks one service for the posts matching the image bytes. Failures
// come back as coded errors from the standard vocabulary; a service inside
// its rate-limit cooldown is refused locally without a request.
func (c *Client) Search(ctx context.Context, service string, img []byte) (Result, error) {
	if until := c.LimitedUntil(service); time.Now().Before(until) {
		// The cooldown is read off a wall clock, so name it in the process
		// timezone; a bare UTC clock time would not match the one on screen.
		return Result{}, &queue.CodedError{Code: queue.ErrCodeRateLimited, Msg: "cooling down until " + until.In(time.Local).Format("15:04:05")}
	}
	switch service {
	case "iqdb":
		return c.searchIQDB(ctx, img)
	case "saucenao":
		return c.searchSauceNAO(ctx, img)
	}
	return Result{}, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: "unknown similarity service " + service}
}

// searchIQDB queries danbooru's iqdb instance by multipart file upload. The
// answer is a plain array of scored posts, best first; every candidate is a
// danbooru post rebuilt from its bare id. The credentials ride the query
// string: danbooru's CSRF exemption checks them before the multipart body is
// parsed, so form fields would be invisible and the upload refused.
func (c *Client) searchIQDB(ctx context.Context, img []byte) (Result, error) {
	params := url.Values{"limit": {"8"}}
	// Both or neither, matching the Missing gate, so half a credential is
	// never sent.
	if site := c.cfg.Current().FindSite("danbooru"); site != nil && site.Username != "" && site.APIKey != "" {
		params.Set("login", site.Username)
		params.Set("api_key", site.APIKey)
	}
	status, body, err := c.post(ctx, "iqdb", c.iqdbURL+"?"+params.Encode(), img, "search[file]")
	if err != nil {
		return Result{}, err
	}
	if cerr := c.classify("iqdb", status, body, iqdbCooldown); cerr != nil {
		// With credentials attached, a 403 is danbooru refusing the privilege,
		// not the key (a bad key answers 401): the iqdb upload is a
		// create-class action, denied to restricted (unverified-email)
		// accounts and to api keys scoped by endpoint or IP even when reads -
		// the site test - work fine.
		if cerr.Code == queue.ErrCodeAuthRequired && status == http.StatusForbidden && params.Has("login") {
			cerr.Msg += " - the key was accepted but the query refused: the danbooru account must be email-verified and the api key unrestricted"
		}
		return Result{}, cerr
	}
	var entries []struct {
		PostID int64   `json:"post_id"`
		Score  float64 `json:"score"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return Result{}, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: "unexpected iqdb response: " + err.Error()}
	}
	res := Result{DailyRemaining: -1}
	for _, e := range entries {
		if e.PostID <= 0 {
			continue
		}
		res.Candidates = append(res.Candidates, Candidate{
			Site:       "danbooru",
			URL:        c.postURL("danbooru", strconv.FormatInt(e.PostID, 10)),
			Similarity: e.Score,
		})
	}
	return res, nil
}

// searchSauceNAO queries SauceNAO's JSON API across every index. Each
// result's ext_urls are matched against the curated profiles: a match is a
// fetchable candidate, the rest come back as extras for the miss trail. The
// result count is padded so booru entries survive a popular image whose top
// slots fill with pixiv / twitter / anime-database hits.
func (c *Client) searchSauceNAO(ctx context.Context, img []byte) (Result, error) {
	params := url.Values{
		"output_type": {"2"},
		"numres":      {"16"},
		"api_key":     {c.cfg.Current().Lookup.Saucenao.APIKey},
	}
	status, body, err := c.post(ctx, "saucenao", c.saucenaoURL+"?"+params.Encode(), img, "file")
	if err != nil {
		return Result{}, err
	}
	var answer struct {
		Header struct {
			Status         int    `json:"status"`
			Message        string `json:"message"`
			ShortRemaining *int   `json:"short_remaining"`
			LongRemaining  *int   `json:"long_remaining"`
		} `json:"header"`
		Results []struct {
			Header struct {
				Similarity string `json:"similarity"`
			} `json:"header"`
			Data struct {
				ExtURLs []string `json:"ext_urls"`
			} `json:"data"`
		} `json:"results"`
	}
	parsed := json.Unmarshal(body, &answer) == nil
	// A zero long_remaining means the daily quota is spent - possibly by this
	// very answer, whose results are still good. Arm the long cooldown either
	// way so queued lookups stop knocking.
	dailyOut := parsed && answer.Header.LongRemaining != nil && *answer.Header.LongRemaining <= 0
	if dailyOut {
		c.limit("saucenao", saucenaoDailyCooldown)
	} else if parsed && answer.Header.ShortRemaining != nil && *answer.Header.ShortRemaining <= 0 {
		c.limit("saucenao", saucenaoShortCooldown)
	}
	// A negative status is saucenao's client-error class - an invalid or
	// anonymous api key in practice - so surface the server's message instead
	// of reading it as a closed rate window.
	if parsed && answer.Header.Status < 0 {
		msg := answer.Header.Message
		if msg == "" {
			msg = fmt.Sprintf("saucenao refused the query (status %d)", answer.Header.Status)
		}
		return Result{}, &queue.CodedError{Code: queue.ErrCodeAuthRequired, Msg: msg}
	}
	// A positive status with results is a partial answer (an index was down);
	// use what came back. With none, the window is closed.
	if status == http.StatusTooManyRequests || (parsed && answer.Header.Status > 0 && len(answer.Results) == 0) {
		msg, d := "rate limited", saucenaoShortCooldown
		if dailyOut {
			msg, d = "daily query quota exhausted", saucenaoDailyCooldown
		}
		c.limit("saucenao", d)
		return Result{}, &queue.CodedError{Code: queue.ErrCodeRateLimited, Msg: msg}
	}
	if cerr := c.classify("saucenao", status, body, saucenaoShortCooldown); cerr != nil {
		return Result{}, cerr
	}
	if !parsed {
		return Result{}, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: "unexpected saucenao response"}
	}
	res := Result{DailyRemaining: -1}
	if answer.Header.LongRemaining != nil {
		res.DailyRemaining = *answer.Header.LongRemaining
	}
	for _, r := range answer.Results {
		score, err := strconv.ParseFloat(r.Header.Similarity, 64)
		if err != nil {
			continue
		}
		matched := false
		for _, raw := range r.Data.ExtURLs {
			if site, canonical, ok := c.canonical(raw); ok {
				res.Candidates = append(res.Candidates, Candidate{Site: site, URL: canonical, Similarity: score})
				matched = true
				break
			}
		}
		if !matched && len(r.Data.ExtURLs) > 0 {
			res.Extras = append(res.Extras, Candidate{Site: hostLabel(r.Data.ExtURLs[0]), URL: r.Data.ExtURLs[0], Similarity: score})
		}
	}
	return res, nil
}

// hostLabel names an extra by its URL's host, the closest thing to a site
// label for a result outside the curated profiles.
func hostLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.TrimPrefix(u.Host, "www.")
}

// post sends one multipart query: the image bytes under fileField, and
// returns the bounded response body. The endpoint carries sensitive query
// parameters (api keys), so failures never echo the URL.
func (c *Client) post(ctx context.Context, service, endpoint string, img []byte, fileField string) (int, []byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(fileField, "query.png")
	if err == nil {
		_, err = fw.Write(img)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return 0, nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return 0, nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, &queue.CodedError{Code: queue.ErrCodeCanceled, Msg: ctx.Err().Error()}
		}
		return 0, nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: service + " unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// classify maps a non-2xx service answer to the standard error codes, arming
// the rate-limit cooldown on a 429. Any 2xx is success: danbooru's iqdb
// endpoint is a Rails create action that answers 201 with the same JSON body
// a 200 carries. A challenge page reads as blocked; 401 and 403 read as
// auth_required (danbooru refuses an unauthenticated upload with 403,
// saucenao an invalid key the same way). The refusal carries the service's
// own message when it sent one - "Invalid API key" and "Access Denied" need
// different operator fixes.
func (c *Client) classify(service string, status int, body []byte, cooldown time.Duration) *queue.CodedError {
	if status >= 200 && status < 300 {
		return nil
	}
	low := strings.ToLower(string(body))
	switch {
	case strings.Contains(low, "cloudflare") || strings.Contains(low, "just a moment") || strings.Contains(low, "captcha"):
		return &queue.CodedError{Code: queue.ErrCodeBlocked, Msg: fmt.Sprintf("%s answered a challenge page (HTTP %d)", service, status)}
	case status == http.StatusTooManyRequests:
		c.limit(service, cooldown)
		return &queue.CodedError{Code: queue.ErrCodeRateLimited, Msg: fmt.Sprintf("%s rate limited (HTTP %d)", service, status)}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		msg := fmt.Sprintf("%s refused the credentials", service)
		if m := serviceMessage(body); m != "" {
			msg = m
		}
		return &queue.CodedError{Code: queue.ErrCodeAuthRequired, Msg: fmt.Sprintf("%s (HTTP %d)", msg, status)}
	default:
		msg := ""
		if m := serviceMessage(body); m != "" {
			msg = ": " + m
		}
		return &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: fmt.Sprintf("%s answered HTTP %d%s", service, status, msg)}
	}
}

// serviceMessage pulls the error message out of a refusal's JSON body:
// danbooru answers {"message": ...}, saucenao {"header": {"message": ...}}.
func serviceMessage(body []byte) string {
	var env struct {
		Message string `json:"message"`
		Header  struct {
			Message string `json:"message"`
		} `json:"header"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	if env.Message != "" {
		return env.Message
	}
	return env.Header.Message
}

// ProbeImage is a small deterministic PNG for the settings test button and
// the live smokes: a generated gradient, so nothing copyrighted ships and a
// "no match" answer is itself a successful round trip.
func ProbeImage() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: uint8((x + y) * 2), A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
