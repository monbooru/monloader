package monbooru

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/monbooru/monloader/internal/outcome"
)

// ResolvedTag is what the gallery does with a name on arrival.
type ResolvedTag struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"` // exists, alias, rejected, new, refused
	ResolvesTo string   `json:"resolves_to"`
	Implies    []string `json:"implies"`
	Reason     string   `json:"reason"`
}

// ResolveTags asks the gallery about names without creating anything.
func (c *Client) ResolveTags(ctx context.Context, gallery string, names []string) ([]ResolvedTag, error) {
	buf, _ := json.Marshal(map[string]any{"names": names})
	resp, body, err := c.do(ctx, http.MethodPost, withGallery(c.base()+"/api/v1/tags/resolve", gallery), "application/json", bytes.NewReader(buf), c.token())
	if err != nil {
		return nil, transportError(ctx, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: "monbooru v1.24.0 or newer fills the gallery column"}
	default:
		return nil, &outcome.CodedError{Code: outcome.ErrCodeMonbooruRejected, Msg: apiErrMessage(body, resp.Status)}
	}
	var out struct {
		Results []ResolvedTag `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, notJSON(resp.Status)
	}
	return out.Results, nil
}
