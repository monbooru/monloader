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

func (c *client) SetAccessKey(key string) {
	c.keyMu.Lock()
	c.accessKey = key
	c.keyMu.Unlock()
}

func (c *client) key() string {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	return c.accessKey
}

func (c *client) setAddress(addr string) {
	c.keyMu.Lock()
	c.addr = addr
	c.keyMu.Unlock()
}

func (c *client) address() string {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	return c.addr
}

// Unlike get, doContrib keeps the server's own error text (a ban reason,
// the velocity refusal) in a *ServerError: the operator reads it.
func (c *client) doContrib(ctx context.Context, method, path string, q url.Values, body io.Reader) ([]byte, error) {
	u := c.address() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Hydrus-Key", c.key())
	if method == http.MethodPost {
		// hydrus documents this type for its zlib-compressed JSON bodies.
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// *url.Error carries the full URL, and the account paths put a
		// key in the query string.
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
		serr := parseServerError(resp.StatusCode, raw)
		if serr.Retryable() {
			return nil, &retryable{err: serr}
		}
		return nil, serr
	}
	return raw, nil
}

func (c *client) AccountTypes(ctx context.Context) ([]AccountType, error) {
	raw, err := c.doContrib(ctx, http.MethodGet, "/auto_create_account_types", nil, nil)
	if err != nil {
		return nil, err
	}
	return parseAccountTypes(raw)
}

func (c *client) keyRequest(ctx context.Context, path, param, value, field string) (string, error) {
	raw, err := c.doContrib(ctx, http.MethodGet, path, url.Values{param: {value}}, nil)
	if err != nil {
		return "", err
	}
	return parseKeyResponse(raw, field)
}

// RegistrationKey gets a 400 at the server's account-creation velocity
// limit: not now, rather than malformed.
func (c *client) RegistrationKey(ctx context.Context, accountTypeKey string) (string, error) {
	return c.keyRequest(ctx, "/auto_create_registration_key", "account_type_key", accountTypeKey, "registration_key")
}

// RedeemAccessKey is single-use server-side, so the caller must persist
// the key at once.
func (c *client) RedeemAccessKey(ctx context.Context, registrationKey string) (string, error) {
	return c.keyRequest(ctx, "/access_key", "registration_key", registrationKey, "access_key")
}

func (c *client) Account(ctx context.Context) (*Account, error) {
	raw, err := c.doContrib(ctx, http.MethodGet, "/account", nil, nil)
	if err != nil {
		return nil, err
	}
	return parseAccountResponse(raw)
}

func (c *client) FetchTagFilter(ctx context.Context) (*TagFilter, error) {
	raw, err := c.doContrib(ctx, http.MethodGet, "/tag_filter", nil, nil)
	if err != nil {
		return nil, err
	}
	return parseTagFilter(raw)
}

func (c *client) PostUpdate(ctx context.Context, u *C2SUpdate) error {
	body, err := u.EncodePOSTBody()
	if err != nil {
		return err
	}
	_, err = c.doContrib(ctx, http.MethodPost, "/update", nil, bytes.NewReader(body))
	return err
}
