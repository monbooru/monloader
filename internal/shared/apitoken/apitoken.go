// Package apitoken holds the named bearer tokens that open /api/v1.
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

type Token struct {
	ID        string   `toml:"id"`
	Name      string   `toml:"name"`
	TokenHash string   `toml:"token_hash"`
	Scopes    []string `toml:"scopes"`
	CreatedAt string   `toml:"created_at"`
	Paired    string   `toml:"paired,omitempty"`
	PeerURL   string   `toml:"peer_url,omitempty"`
}

func (t Token) HasScope(scope string) bool { return slices.Contains(t.Scopes, scope) }

func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func NewSecret() string { return randomHex(16) }

// crypto/rand.Read never returns an error; it crashes the process instead.
func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// New belongs outside a replayed config mutation, so the id, secret and
// timestamp stay stable.
func New(name string, scopes []string) (Token, string) {
	secret := NewSecret()
	return FromSecret(name, secret, scopes), secret
}

func FromSecret(name, secret string, scopes []string) Token {
	return Token{
		ID:        randomHex(8),
		Name:      name,
		TokenHash: Hash(secret),
		Scopes:    scopes,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// An operator's token must not pass for one the pairing flow minted.
func ValidateName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return fmt.Errorf("token name must not be empty")
	}
	if strings.HasSuffix(strings.ToLower(n), "(paired)") {
		return fmt.Errorf("token names ending in \"(paired)\" are reserved")
	}
	return nil
}

func Filter(in, all []string) []string {
	var out []string
	for _, sc := range all {
		if slices.Contains(in, sc) {
			out = append(out, sc)
		}
	}
	return out
}

type List []Token

func (l List) NameExists(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return slices.ContainsFunc(l, func(t Token) bool { return strings.ToLower(t.Name) == n })
}

func (l List) ByHash(hash string) *Token {
	for i := range l {
		if subtle.ConstantTimeCompare([]byte(l[i].TokenHash), []byte(hash)) == 1 {
			return &l[i]
		}
	}
	return nil
}

// Authorize finds the token an Authorization header names, or returns the
// status, error code and message that refuse the request.
func (l List) Authorize(header, scope string) (*Token, int, string, string) {
	if len(l) == 0 {
		return nil, http.StatusServiceUnavailable, "api_disabled",
			"API is disabled: generate an API token in Settings to enable it"
	}
	secret, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return nil, http.StatusUnauthorized, "unauthorized", "missing or invalid authorization header"
	}
	tok := l.ByHash(Hash(secret))
	if tok == nil {
		return nil, http.StatusUnauthorized, "unauthorized", "invalid bearer token"
	}
	if !tok.HasScope(scope) {
		return nil, http.StatusForbidden, "insufficient_scope", "token lacks the " + scope + " scope"
	}
	return tok, 0, "", ""
}

func (l List) ByID(id string) *Token {
	return l.find(func(t Token) bool { return t.ID == id })
}

func (l List) Paired(app string) *Token {
	return l.find(func(t Token) bool { return t.Paired == app })
}

func (l List) find(match func(Token) bool) *Token {
	i := slices.IndexFunc(l, match)
	if i < 0 {
		return nil
	}
	return &l[i]
}

func (l *List) Add(t Token) error {
	if l.NameExists(t.Name) {
		return fmt.Errorf("a token named %q already exists", t.Name)
	}
	*l = append(*l, t)
	return nil
}

func (l *List) Remove(id string) bool {
	i := slices.IndexFunc(*l, func(t Token) bool { return t.ID == id })
	if i < 0 {
		return false
	}
	*l = slices.Delete(*l, i, i+1)
	return true
}

func (l List) SetScopes(id string, scopes []string) bool {
	tok := l.ByID(id)
	if tok == nil {
		return false
	}
	tok.Scopes = scopes
	return true
}
