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

type client struct {
	// keyMu guards addr and accessKey: SetAccessKey swaps the key under a
	// running sync, and the address moves while the sync is off.
	keyMu     sync.RWMutex
	addr      string
	accessKey string
	hc        *http.Client
	// Paces update fetches to stay polite to the donated repository server.
	sleep time.Duration
}

type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

func IsRetryable(err error) bool {
	var r *retryable
	if errors.As(err, &r) {
		return true
	}
	var s *ServerError
	return errors.As(err, &s) && s.Retryable()
}

func newClient(cfg config.PTRConfig) *client {
	return &client{
		addr:      cfg.Address,
		accessKey: cfg.EffectiveAccessKey(),
		sleep:     time.Duration(cfg.FetchSleep * float64(time.Second)),
		hc: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				// The PTR's certificate is self-signed, as hydrus accepts it. The
				// blob sha256 catches corruption, not an on-path attacker, who
				// could serve a matching manifest and read the Hydrus-Key.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed PTR; on-path risk accepted, as hydrus does
			},
		},
	}
}

func (c *client) Metadata(ctx context.Context, since uint64) (*metadataSlice, error) {
	q := url.Values{"since": {strconv.FormatUint(since, 10)}}
	raw, err := c.get(ctx, "/metadata", q)
	if err != nil {
		return nil, err
	}
	return parseMetadata(raw)
}

// updateRaw skips the parse: the sync holds an entry's blobs compressed
// and inflates one at a time.
func (c *client) updateRaw(ctx context.Context, hashHex string) ([]byte, error) {
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

func (c *client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := c.address() + path
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
	// hydrus answers 509 over its bandwidth limit; other 4xx (bad key,
	// unknown hash) are permanent.
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
