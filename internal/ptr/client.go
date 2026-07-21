package ptr

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

// Client talks the Hydrus repository protocol over HTTP. It reads two content
// endpoints - GET /metadata (the manifest) and GET /update (an update blob) -
// authenticating with the access key header. The PTR serves a self-signed
// certificate, so chain verification is skipped for this dedicated client; the
// compensating control is that every update blob is verified against its own
// sha256 from the manifest before it is parsed.
type Client struct {
	addr string
	// keyMu guards accessKey: the account-creation flow re-points a live
	// client at the new personal key with no restart.
	keyMu     sync.RWMutex
	accessKey string
	hc        *http.Client
	// sleep paces consecutive update fetches, keeping the initial stream polite
	// to the donated repository server.
	sleep time.Duration
}

// retryable marks an HTTP status or transport failure the caller may retry.
type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

// IsRetryable reports whether err is a transient fetch failure (a 429, a 5xx,
// a bandwidth refusal, or a transport error) rather than a permanent one.
func IsRetryable(err error) bool {
	var r *retryable
	if errors.As(err, &r) {
		return true
	}
	var s *ServerError
	return errors.As(err, &s) && s.Retryable()
}

// NewClient builds a repository client from the PTR config.
func NewClient(cfg config.PTRConfig) *Client {
	return &Client{
		addr:      cfg.Address,
		accessKey: cfg.EffectiveAccessKey(),
		sleep:     time.Duration(cfg.FetchSleep * float64(time.Second)),
		hc: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				// The PTR uses a self-signed certificate; blob sha256 verification
				// (see Update) is the integrity control instead of the chain.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // documented in the threat model
			},
		},
	}
}

// Metadata fetches the manifest of updates from index `since` onward.
func (c *Client) Metadata(ctx context.Context, since uint64) (*MetadataSlice, error) {
	q := url.Values{"since": {strconv.FormatUint(since, 10)}}
	raw, err := c.get(ctx, "/metadata", q)
	if err != nil {
		return nil, err
	}
	return ParseMetadata(raw)
}

// Update fetches one update blob by its sha256 (hex), verifies the response
// bytes hash to that value, and parses it. A mismatch is a hard error: the
// content-addressed blob was corrupted in transit.
func (c *Client) Update(ctx context.Context, hashHex string) (*Update, error) {
	raw, err := c.updateRaw(ctx, hashHex)
	if err != nil {
		return nil, err
	}
	return ParseUpdate(raw)
}

// updateRaw is Update without the parse: the sync buffers an entry's blobs
// compressed (the replay inflates one at a time), so a many-blob entry costs
// megabytes, not its parsed rows.
func (c *Client) updateRaw(ctx context.Context, hashHex string) ([]byte, error) {
	if c.sleep > 0 {
		select {
		case <-time.After(c.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	q := url.Values{"update_hash": {hashHex}}
	raw, err := c.get(ctx, "/update", q)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, hashHex) {
		return nil, fmt.Errorf("update %s failed integrity check (got %s)", hashHex, got)
	}
	return raw, nil
}

// get issues one authenticated GET and returns the body, classifying a
// transient failure as retryable.
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := c.addr + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Hydrus-Key", c.key())
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &retryable{err: fmt.Errorf("reaching the repository: %w", err)}
	}
	defer resp.Body.Close()
	// A 429 or a 5xx (hydrus's 509 bandwidth limit among them) is transient;
	// the caller backs off and retries. Other 4xx are permanent (bad key,
	// unknown hash).
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, &retryable{err: fmt.Errorf("repository returned %s", resp.Status)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("repository returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateBytes+1))
	if err != nil {
		return nil, &retryable{err: fmt.Errorf("reading the response: %w", err)}
	}
	return body, nil
}
