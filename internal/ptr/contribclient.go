package ptr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The account-management and write requests the contribution feature
// adds to the client. Unlike the read side's get, these surface the
// server's JSON error verbatim (a janitor's ban text, the velocity
// refusal) instead of the bare status line, because the operator is
// meant to read them.

// SetAccessKey re-points the client at a new key with no restart; the
// next request (sync included) uses it.
func (c *Client) SetAccessKey(key string) {
	c.keyMu.Lock()
	c.accessKey = key
	c.keyMu.Unlock()
}

func (c *Client) key() string {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	return c.accessKey
}

// doContrib issues one request and returns the body; a non-200 answer
// parses into a *ServerError (wrapped retryable when transient) so the
// caller can branch on the server's own status and message.
func (c *Client) doContrib(ctx context.Context, method, path string, q url.Values, body io.Reader) ([]byte, error) {
	u := c.addr + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Hydrus-Key", c.key())
	if method == http.MethodPost {
		// hydrus's documented type for its POST bodies, which are
		// zlib-compressed JSON; the server inflates regardless.
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Go's *url.Error carries the full URL, and the account paths put a
		// single-use key in the query string; this message is rendered to the
		// operator, so report the path and the cause only.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, &retryable{err: fmt.Errorf("reaching the repository at %s: %w", path, err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateBytes+1))
	if err != nil {
		return nil, &retryable{err: fmt.Errorf("reading the response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		serr := ParseServerError(resp.StatusCode, raw)
		if serr.Retryable() {
			return nil, &retryable{err: serr}
		}
		return nil, serr
	}
	return raw, nil
}

// AccountTypes fetches the repository's auto-creatable account types.
func (c *Client) AccountTypes(ctx context.Context) ([]AccountType, error) {
	raw, err := c.doContrib(ctx, http.MethodGet, "/auto_create_account_types", nil, nil)
	if err != nil {
		return nil, err
	}
	return ParseAccountTypes(raw)
}

// RegistrationKey requests a single-use registration key for the given
// account type. The velocity refusal comes back as a *ServerError with
// status 400: unavailable-now, not malformed - the request is built
// from a type the server just served.
func (c *Client) RegistrationKey(ctx context.Context, accountTypeKey string) (string, error) {
	q := url.Values{"account_type_key": {accountTypeKey}}
	raw, err := c.doContrib(ctx, http.MethodGet, "/auto_create_registration_key", q, nil)
	if err != nil {
		return "", err
	}
	return ParseKeyResponse(raw, "registration_key")
}

// RedeemAccessKey trades a registration key for the account's access
// key. Redemption is single-use server-side; the caller must persist
// the returned key immediately.
func (c *Client) RedeemAccessKey(ctx context.Context, registrationKey string) (string, error) {
	q := url.Values{"registration_key": {registrationKey}}
	raw, err := c.doContrib(ctx, http.MethodGet, "/access_key", q, nil)
	if err != nil {
		return "", err
	}
	return ParseKeyResponse(raw, "access_key")
}

// Account fetches the current account's state: type, permissions,
// creation time, ban and janitor message.
func (c *Client) Account(ctx context.Context) (*Account, error) {
	raw, err := c.doContrib(ctx, http.MethodGet, "/account", nil, nil)
	if err != nil {
		return nil, err
	}
	return ParseAccountResponse(raw)
}

// FetchTagFilter fetches the repository's live tag filter.
func (c *Client) FetchTagFilter(ctx context.Context) (*TagFilter, error) {
	raw, err := c.doContrib(ctx, http.MethodGet, "/tag_filter", nil, nil)
	if err != nil {
		return nil, err
	}
	return ParseTagFilter(raw)
}

// PostUpdate uploads one encoded ClientToServerUpdate.
func (c *Client) PostUpdate(ctx context.Context, u *C2SUpdate) error {
	body, err := u.EncodePOSTBody()
	if err != nil {
		return err
	}
	_, err = c.doContrib(ctx, http.MethodPost, "/update", nil, bytes.NewReader(body))
	return err
}
