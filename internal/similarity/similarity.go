// Package similarity asks iqdb and SauceNAO for posts matching an image.
// It takes only post URLs from them: gallery-dl stays the only metadata
// extractor.
package similarity

import (
	"bytes"
	"cmp"
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
	"github.com/monbooru/monloader/internal/outcome"
)

type Candidate struct {
	Site       string // gallery-dl category; an extra's host
	URL        string
	Similarity float64 // 0-100
}

// Result's Extras are hits outside the curated profiles and never join the
// candidate walk. DailyRemaining is -1 when the service reports no quota.
type Result struct {
	Candidates     []Candidate
	Extras         []Candidate
	DailyRemaining int
}

// SauceNAO meters a 30 s window and a daily quota; iqdb rides danbooru's
// general throttle.
const (
	saucenaoShortCooldown = 35 * time.Second
	saucenaoDailyCooldown = time.Hour
	iqdbCooldown          = time.Minute
)

type Client struct {
	cfg       *config.Provider
	http      *http.Client
	canonical func(rawURL string) (site, canonical string, ok bool)
	postURL   func(site, id string) string

	// The public endpoints, under any [lookup.<service>].url.
	iqdbURL     string
	saucenaoURL string

	mu           sync.Mutex
	limitedUntil map[string]time.Time
}

func New(cfg *config.Provider, canonical func(string) (string, string, bool), postURL func(site, id string) string) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: 30 * time.Second,
			// The credentials ride the query string, which Go re-sends to
			// a redirect target; neither endpoint redirects legitimately.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		canonical:    canonical,
		postURL:      postURL,
		iqdbURL:      "https://danbooru.donmai.us/iqdb_queries.json",
		saucenaoURL:  "https://saucenao.com/search.php",
		limitedUntil: map[string]time.Time{},
	}
}

// CredentialSite names the [[sites]] block a service signs in through.
func CredentialSite(service string) string {
	if service == "iqdb" {
		return "danbooru"
	}
	return ""
}

// Missing reflects the services' refusals: iqdb's upload needs a danbooru
// login, and saucenao refuses anonymous queries.
func (c *Client) Missing(service string) (string, bool) {
	switch service {
	case "iqdb":
		// Name the missing half: the sites table already shows the other
		// as set.
		site := c.cfg.Current().FindSite(CredentialSite(service))
		switch {
		case site == nil || (site.Username == "" && site.APIKey == ""):
			return "danbooru login and api key", true
		case site.Username == "":
			return "danbooru username", true
		case site.APIKey == "":
			return "danbooru api key", true
		}
	case "saucenao":
		if c.cfg.Current().Lookup.Saucenao.APIKey == "" {
			return "api key", true
		}
	}
	return "", false
}

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

func (c *Client) Search(ctx context.Context, service string, img []byte) (Result, error) {
	if until := c.LimitedUntil(service); time.Now().Before(until) {
		return Result{}, &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: "cooling down until " + until.In(time.Local).Format("15:04:05")}
	}
	switch service {
	case "iqdb":
		return c.searchIQDB(ctx, img)
	case "saucenao":
		return c.searchSauceNAO(ctx, img)
	}
	return Result{}, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "unknown similarity service " + service}
}

// The credentials ride the query string: danbooru's CSRF check runs
// before the multipart body is parsed, so form fields would go unseen.
func (c *Client) searchIQDB(ctx context.Context, img []byte) (Result, error) {
	params := url.Values{"limit": {"8"}}
	if site := c.cfg.Current().FindSite(CredentialSite("iqdb")); site != nil && site.Username != "" && site.APIKey != "" {
		params.Set("login", site.Username)
		params.Set("api_key", site.APIKey)
	}
	endpoint := cmp.Or(c.cfg.Current().Lookup.Iqdb.URL, c.iqdbURL)
	status, body, err := c.post(ctx, "iqdb", endpoint+"?"+params.Encode(), img, "search[file]")
	if err != nil {
		return Result{}, err
	}
	if cerr := c.classify("iqdb", status, body, iqdbCooldown); cerr != nil {
		// With credentials, a 403 refuses the privilege, not the key (a
		// bad key is a 401): the upload is a create action, denied to
		// unverified accounts and restricted keys.
		if cerr.Code == outcome.ErrCodeAuthRequired && status == http.StatusForbidden && params.Has("login") {
			cerr.Msg += " - the key was accepted but the query refused: the danbooru account must be email-verified and the api key unrestricted"
		}
		return Result{}, cerr
	}
	var entries []struct {
		PostID int64   `json:"post_id"`
		Score  float64 `json:"score"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return Result{}, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "unexpected iqdb response: " + err.Error()}
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

// numres is padded so booru hits survive a popular image whose top slots
// are pixiv, twitter or anime databases.
func (c *Client) searchSauceNAO(ctx context.Context, img []byte) (Result, error) {
	params := url.Values{
		"output_type": {"2"},
		"numres":      {"16"},
		"api_key":     {c.cfg.Current().Lookup.Saucenao.APIKey},
	}
	endpoint := cmp.Or(c.cfg.Current().Lookup.Saucenao.URL, c.saucenaoURL)
	status, body, err := c.post(ctx, "saucenao", endpoint+"?"+params.Encode(), img, "file")
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
	// A zero long_remaining means the daily quota is spent, possibly by
	// this answer, whose results still count.
	dailyOut := parsed && answer.Header.LongRemaining != nil && *answer.Header.LongRemaining <= 0
	if dailyOut {
		c.limit("saucenao", saucenaoDailyCooldown)
	} else if parsed && answer.Header.ShortRemaining != nil && *answer.Header.ShortRemaining <= 0 {
		c.limit("saucenao", saucenaoShortCooldown)
	}
	// A negative status is saucenao's client-error class, in practice a
	// bad or missing key.
	if parsed && answer.Header.Status < 0 {
		msg := cmp.Or(answer.Header.Message, fmt.Sprintf("saucenao refused the query (status %d)", answer.Header.Status))
		return Result{}, &outcome.CodedError{Code: outcome.ErrCodeAuthRequired, Msg: msg}
	}
	// A positive status with results is a partial answer (an index was
	// down); without results the window is closed.
	if status == http.StatusTooManyRequests || (parsed && answer.Header.Status > 0 && len(answer.Results) == 0) {
		msg, d := "rate limited", saucenaoShortCooldown
		if dailyOut {
			msg, d = "daily query quota exhausted", saucenaoDailyCooldown
		}
		c.limit("saucenao", d)
		return Result{}, &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: msg}
	}
	if cerr := c.classify("saucenao", status, body, saucenaoShortCooldown); cerr != nil {
		return Result{}, cerr
	}
	if !parsed {
		return Result{}, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "unexpected saucenao response"}
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

func hostLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.TrimPrefix(u.Host, "www.")
}

// Errors never echo endpoint: its query string carries the api keys.
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
		return 0, nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return 0, nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, &outcome.CodedError{Code: outcome.ErrCodeCanceled, Msg: ctx.Err().Error()}
		}
		return 0, nil, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: service + " unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// Any 2xx is success: iqdb is a Rails create action answering 201. The
// service's own refusal text is kept, as "Invalid API key" and "Access
// Denied" need different fixes.
func (c *Client) classify(service string, status int, body []byte, cooldown time.Duration) *outcome.CodedError {
	if status >= 200 && status < 300 {
		return nil
	}
	low := strings.ToLower(string(body))
	switch {
	case strings.Contains(low, "cloudflare") || strings.Contains(low, "just a moment") || strings.Contains(low, "captcha"):
		return &outcome.CodedError{Code: outcome.ErrCodeBlocked, Msg: fmt.Sprintf("%s answered a challenge page (HTTP %d)", service, status)}
	case status == http.StatusTooManyRequests:
		c.limit(service, cooldown)
		return &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: fmt.Sprintf("%s rate limited (HTTP %d)", service, status)}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		msg := cmp.Or(serviceMessage(body), fmt.Sprintf("%s refused the credentials", service))
		return &outcome.CodedError{Code: outcome.ErrCodeAuthRequired, Msg: fmt.Sprintf("%s (HTTP %d)", msg, status)}
	default:
		msg := ""
		if m := serviceMessage(body); m != "" {
			msg = ": " + m
		}
		return &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: fmt.Sprintf("%s answered HTTP %d%s", service, status, msg)}
	}
}

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
	return cmp.Or(env.Message, env.Header.Message)
}

// ProbeImage is generated, so nothing copyrighted ships and a no-match
// answer is still a successful round trip.
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
