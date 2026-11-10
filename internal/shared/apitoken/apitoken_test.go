package apitoken

import (
	"slices"
	"testing"
)

func TestNewHashesTheSecret(t *testing.T) {
	tok, secret := New("ci", []string{"read"})
	if len(secret) != 32 || tok.TokenHash != Hash(secret) || tok.TokenHash == secret {
		t.Fatal("New must store the hash of a 32-hex-character secret, not the secret")
	}
	if tok.ID == "" || tok.CreatedAt == "" {
		t.Errorf("token = %+v, want an id and a creation time", tok)
	}
}

func TestListLookups(t *testing.T) {
	tok, secret := New("ci", []string{"read"})
	paired, _ := New("app (paired)", []string{"read", "write"})
	paired.Paired = "app"
	l := List{tok, paired}
	if !l.NameExists("CI") || l.NameExists("other") {
		t.Error("NameExists must match names case-insensitively")
	}
	if got := l.ByHash(Hash(secret)); got == nil || got.ID != tok.ID || !got.HasScope("read") || got.HasScope("write") {
		t.Errorf("ByHash = %+v", got)
	}
	if l.ByHash(Hash("wrong")) != nil || l.ByID("nope") != nil || l.Paired("nope") != nil {
		t.Error("a lookup that matches nothing returned a token")
	}
	if got := l.Paired("app"); got == nil || got.ID != paired.ID {
		t.Errorf("Paired = %+v", got)
	}
	if !l.SetScopes(tok.ID, []string{"read", "write"}) || !l.ByID(tok.ID).HasScope("write") {
		t.Error("SetScopes did not apply")
	}
	if err := l.Add(Token{ID: "x", Name: "Ci"}); err == nil || len(l) != 2 {
		t.Errorf("Add took a second token named ci: %v", err)
	}
	if !l.Remove(tok.ID) || len(l) != 1 || l.Remove(tok.ID) {
		t.Errorf("Remove left %d tokens", len(l))
	}
	if err := l.Add(tok); err != nil || l.ByID(tok.ID) == nil {
		t.Errorf("Add = %v", err)
	}
}

func TestValidateNameReservesThePairedSuffix(t *testing.T) {
	if err := ValidateName("app"); err != nil {
		t.Errorf("plain name rejected: %v", err)
	}
	for _, bad := range []string{"", "  ", "app (paired)", "X (PAIRED)"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", bad)
		}
	}
}

func TestFilterKeepsKnownScopesInOrder(t *testing.T) {
	if got := Filter([]string{"delete", "bogus", "read"}, []string{"read", "write", "delete"}); !slices.Equal(got, []string{"read", "delete"}) {
		t.Errorf("Filter = %v", got)
	}
}

func TestAuthorizeRefusesInOrder(t *testing.T) {
	tok, secret := New("ci", []string{"read"})
	l := List{tok}
	for _, tc := range []struct {
		list          List
		header, scope string
		status        int
		code          string
	}{
		{nil, "Bearer " + secret, "read", 503, "api_disabled"},
		{l, secret, "read", 401, "unauthorized"},
		{l, "Bearer wrong", "read", 401, "unauthorized"},
		{l, "Bearer " + secret, "write", 403, "insufficient_scope"},
		{l, "Bearer " + secret, "read", 0, ""},
	} {
		got, status, code, msg := tc.list.Authorize(tc.header, tc.scope)
		if status != tc.status || code != tc.code || (status == 0) != (got != nil) || (status == 0) != (msg == "") {
			t.Errorf("Authorize(%q, %q) = %v, %d, %q, %q", tc.header, tc.scope, got, status, code, msg)
		}
	}
}
