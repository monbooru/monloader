package pairing

import (
	"encoding/json"
	"net/http"
	"regexp"
)

// A Refusal is a pairing route's error reply; each app writes it in its own
// error body.
type Refusal struct {
	Status    int
	Code, Msg string
}

// The codes the pairing routes refuse with.
const (
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
	CodeUnauthorized   = "unauthorized"
	CodeTooMany        = "too_many_requests"
	CodeMintFailed     = "mint_failed"
	CodeRemoveFailed   = "remove_failed"
)

const maxOfferBody = 1 << 16

var appNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ValidApp is the name rule a pairing request's app must meet, since the
// name becomes a token's and a plugin's.
func ValidApp(name string) bool { return appNameRe.MatchString(name) }

type offerBody interface{ body() *RequestBody }

func (b *RequestBody) body() *RequestBody { return b }

// DecodeOffer reads a pairing request into dst, a *RequestBody or a pointer
// to a struct embedding one.
func DecodeOffer(w http.ResponseWriter, r *http.Request, dst offerBody) *Refusal {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOfferBody)).Decode(dst); err != nil || dst.body().App == "" {
		return &Refusal{http.StatusBadRequest, CodeInvalidRequest, "app and a JSON body are required"}
	}
	return nil
}

// Offer files a request, or refuses it while MaxPending wait.
func (s *Store[T, P]) Offer(req T) (Accepted, *Refusal) {
	id, ok := s.Create(req)
	if !ok {
		return Accepted{}, &Refusal{http.StatusTooManyRequests, CodeTooMany, "too many pending pairing requests"}
	}
	return Accepted{RequestID: id, Status: Pending}, nil
}

// Poll answers a peer's status poll. Only the poll whose claim wins carries
// the token, minted then; a failed mint leaves the request for the next
// poll. A body with a token must not be cached.
func (s *Store[T, P]) Poll(id string, mint func(T) (string, error)) (StatusBody, *Refusal) {
	req, ok := s.Get(id)
	if !ok {
		return StatusBody{}, &Refusal{http.StatusNotFound, CodeNotFound, "unknown pairing request"}
	}
	if st := P(&req).request().State; st != Approved {
		return StatusBody{Status: st}, nil
	}
	claimed, won := s.Claim(id)
	if !won {
		return StatusBody{Status: Approved}, nil
	}
	secret, err := mint(claimed)
	if err != nil {
		s.Unclaim(id)
		return StatusBody{}, &Refusal{http.StatusInternalServerError, CodeMintFailed, err.Error()}
	}
	s.Remove(id)
	return StatusBody{Status: Approved, Token: secret}, nil
}

// NoPairingToken refuses a teardown whose bearer is no paired token.
func NoPairingToken() *Refusal {
	return &Refusal{http.StatusUnauthorized, CodeUnauthorized, "pairing token required"}
}
