package worker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client calls one worker. HTTP nil means http.DefaultClient.
type Client struct {
	BaseURL string
	Token   string
	// Gateway, when set, is the key of a gateway in front of the worker:
	// it goes as the bearer and Token moves to HeaderToken.
	Gateway string
	// Format is the frame format of a tag call; empty is FormatRGB8.
	Format string
	HTTP   *http.Client
}

// StatusError is a worker's answer other than 2xx: it was reached, so a
// retry elsewhere would not change it.
type StatusError struct {
	Status int
	Msg    string
	Code   string
}

func (e *StatusError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("worker answered %d: %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("worker answered %d", e.Status)
}

// IsTransport reports whether err left the worker's answer unknown: the
// call may go to another worker.
func IsTransport(err error) bool {
	var se *StatusError
	return err != nil && !errors.As(err, &se) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	_, err := c.getJSON(ctx, PathInfo, "", &out)
	return out, err
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out []Model
	_, err := c.getJSON(ctx, PathModels, "", &out)
	return out, err
}

func (c *Client) Catalog(ctx context.Context) ([]CatalogEntry, error) {
	var out []CatalogEntry
	_, err := c.getJSON(ctx, PathCatalog, "", &out)
	return out, err
}

// Labels returns changed false, and no labels, when etag still matches.
func (c *Client) Labels(ctx context.Context, model, etag string) (out Labels, changed bool, err error) {
	status, err := c.getJSON(ctx, PathLabels(url.PathEscape(model)), etag, &out)
	return out, status != http.StatusNotModified, err
}

// Tag runs frames, each Size x Size RGB8, through models that share that
// input.
func (c *Client) Tag(ctx context.Context, req TagRequest, frames [][]byte) (TagResponse, error) {
	var out TagResponse
	req.Frames = FrameSpec{Format: cmp.Or(c.Format, FormatRGB8), Size: req.Frames.Size, Count: len(frames)}
	var body bytes.Buffer
	ctype, err := WriteTag(&body, req, frames)
	if err != nil {
		return out, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(PathTag), &body)
	if err != nil {
		return out, err
	}
	hreq.Header.Set("Content-Type", ctype)
	if _, err := c.do(hreq, &out); err != nil {
		return out, err
	}
	if len(out.Results) != len(req.Models) {
		return out, fmt.Errorf("worker answered %d models, want %d", len(out.Results), len(req.Models))
	}
	for _, r := range out.Results {
		if len(r.Frames) != len(frames) {
			return out, fmt.Errorf("worker answered %d frames, want %d", len(r.Frames), len(frames))
		}
	}
	return out, nil
}

func (c *Client) url(path string) string { return strings.TrimRight(c.BaseURL, "/") + path }

func (c *Client) getJSON(ctx context.Context, path, etag string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return 0, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) (int, error) {
	switch {
	case c.Gateway != "":
		req.Header.Set("Authorization", "Bearer "+c.Gateway)
		req.Header.Set(HeaderToken, c.Token)
	case c.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		return resp.StatusCode, nil
	}
	if resp.StatusCode/100 != 2 {
		var e Error
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		return resp.StatusCode, &StatusError{Status: resp.StatusCode, Msg: e.Error, Code: e.Code}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", req.URL.Path, err)
	}
	return resp.StatusCode, nil
}
