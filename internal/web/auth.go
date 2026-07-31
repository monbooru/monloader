package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"golang.org/x/crypto/bcrypt"
)

type contextKey int

const sessionContextKey contextKey = 1

// Session is one logged-in browser session.
type Session struct {
	ID        string
	ExpiresAt time.Time
}

// SessionStore is an in-memory session set (copied from monbooru).
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

func NewSessionStore() *SessionStore { return &SessionStore{sessions: map[string]Session{}} }

func (s *SessionStore) New(lifetimeDays int) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	s.sessions[id] = Session{ID: id, ExpiresAt: time.Now().Add(time.Duration(lifetimeDays) * 24 * time.Hour)}
	s.mu.Unlock()
	return id, nil
}

func (s *SessionStore) Get(id string) (Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return Session{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		// Drop it here: nothing else sweeps the map, so a closed tab or an
		// expired week-old cookie would otherwise leave a permanent entry.
		s.Delete(id)
		return Session{}, false
	}
	return sess, true
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// Clear drops every session, used after the password is removed so nobody is
// left locked out of the now-open instance.
func (s *SessionStore) Clear() {
	s.mu.Lock()
	s.sessions = map[string]Session{}
	s.mu.Unlock()
}

func sessionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionContextKey).(string)
	return v
}

func isHTMXRequest(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// SessionMiddleware injects a session id for CSRF and, when a UI password is
// configured, redirects unauthenticated requests to /login. The API and
// static assets bypass it.
func (s *Server) SessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bypassAuth(r.URL.Path) || !s.cfg.Current().Auth.EnablePassword {
			ctx := context.WithValue(r.Context(), sessionContextKey, "anon")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		sessID := sessionCookie(r)
		if _, ok := s.sessions.Get(sessID); !ok {
			if isHTMXRequest(r) {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), sessionContextKey, sessID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bypassAuth lists the paths that never see the login redirect: the API (its
// own bearer gate), the health probe, the static assets, and the login page
// itself, which would otherwise redirect to itself.
func bypassAuth(p string) bool {
	return strings.HasPrefix(p, "/api/v1/") || p == "/health" ||
		strings.HasPrefix(p, "/static/") || p == "/login" ||
		p == "/custom.css" || p == "/custom.logo"
}

func sessionCookie(r *http.Request) string {
	c, err := r.Cookie("monloader_session")
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Current().Auth.EnablePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", s.loginData(""))
}

// loginData is the login page's template data; errMsg is set only on a failed
// attempt.
func (s *Server) loginData(errMsg string) map[string]any {
	data := map[string]any{
		"Title":        "Login - " + s.titleName(),
		"CSRFToken":    s.csrfToken("anon"),
		"Conn":         "checking",
		"BooruName":    s.booruName(),
		"BooruFavicon": s.booruFaviconURL(),
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	return data
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Current().Auth.EnablePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	password := r.FormValue("password")
	if bcrypt.CompareHashAndPassword([]byte(s.cfg.Current().Auth.PasswordHash), []byte(password)) != nil {
		// The content-type must land before the status: headers set after
		// WriteHeader are dropped.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", s.loginData("incorrect password"))
		return
	}
	id, err := s.sessions.New(s.cfg.Current().Auth.SessionLifetimeDays)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "monloader_session",
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   s.cfg.Current().Auth.SessionLifetimeDays * 24 * 3600,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	s.sessions.Delete(sessionCookie(r))
	http.SetCookie(w, &http.Cookie{Name: "monloader_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// settingsPasswordPost sets or changes the optional UI password from the
// authentication settings section. Setting a password also turns auth on. An
// existing password must be confirmed before it changes.
func (s *Server) settingsPasswordPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "bad form")
		return
	}
	newPass := r.FormValue("new_password")
	if newPass == "" {
		flashFragment(w, "err", "new password required")
		return
	}
	if s.cfg.Current().Auth.EnablePassword && s.cfg.Current().Auth.PasswordHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(s.cfg.Current().Auth.PasswordHash), []byte(r.FormValue("current_password"))) != nil {
			flashFragment(w, "err", "current password is incorrect")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		flashFragment(w, "err", "error hashing password")
		return
	}
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.PasswordHash = string(hash)
		c.Auth.EnablePassword = true
		return nil
	}); err != nil {
		flashFragment(w, "err", "could not save: "+err.Error())
		return
	}
	flashFragment(w, "ok", "password updated")
	s.renderAuthPasswordOOB(w, r)
}

// settingsRemovePasswordPost disables the UI password. The current password is
// required whenever a hash is set, even if the TOML flag was flipped off, so
// the disable path cannot be bypassed.
func (s *Server) settingsRemovePasswordPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "bad form")
		return
	}
	if s.cfg.Current().Auth.PasswordHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(s.cfg.Current().Auth.PasswordHash), []byte(r.FormValue("current_password"))) != nil {
			flashFragment(w, "err", "current password is incorrect")
			return
		}
	}
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.EnablePassword = false
		c.Auth.PasswordHash = ""
		return nil
	}); err != nil {
		flashFragment(w, "err", "could not save: "+err.Error())
		return
	}
	s.sessions.Clear()
	flashFragment(w, "ok", "password removed; authentication is now disabled")
	s.renderAuthPasswordOOB(w, r)
}

// settingsTokenCreate mints a named API bearer token with all scopes. The
// secret is generated before updateConfig (whose closure runs twice) so both
// the runtime and file layers store the same value; it is shown once in the
// response and never echoed by the settings page again.
func (s *Server) settingsTokenCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "bad form data")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := config.ValidateTokenName(name); err != nil {
		flashFragment(w, "err", err.Error())
		return
	}
	tok, secret := config.GenerateToken(name, config.AllScopes)
	if err := s.updateConfig(func(c *config.Config) error {
		if c.TokenNameExists(name) {
			return fmt.Errorf("a token named %q already exists", name)
		}
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		flashFragment(w, "err", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("HX-Trigger", "token-created")
	s.render(w, "token_flash", map[string]any{"Token": secret})
	s.renderAuthTokensOOB(w, r)
}

// hasPairingTeardown reports whether a token's peer has its own remove-pairing
// control. Only these two do, so a token minted under any other app name would
// have no way out at all if the list refused it as well.
func hasPairingTeardown(peer string) bool {
	return peer == "monbooru" || peer == "monsender"
}

// settingsTokenRevoke drops a named API token by id.
func (s *Server) settingsTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok := s.cfg.Current().FindToken(id)
	if tok == nil {
		flashFragment(w, "err", "token not found")
		return
	}
	if hasPairingTeardown(tok.Paired) {
		flashFragment(w, "err", "this token is managed by a pairing; remove the pairing instead")
		return
	}
	if err := s.updateConfig(func(c *config.Config) error {
		c.RemoveToken(id)
		return nil
	}); err != nil {
		flashFragment(w, "err", "could not save: "+err.Error())
		return
	}
	flashFragment(w, "ok", "token revoked")
	s.renderAuthTokensOOB(w, r)
}

// renderAuthTokensOOB re-renders the token list out of band so it reflects the
// latest set after a create or revoke without a full page reload.
func (s *Server) renderAuthTokensOOB(w http.ResponseWriter, r *http.Request) {
	s.render(w, "auth_tokens", map[string]any{
		"Tokens":    slices.Clone(s.cfg.Current().Auth.Tokens),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"OOB":       true,
	})
}

type tokenScopeRow struct {
	Name    string
	Desc    string
	Checked bool
}

func (s *Server) settingsTokenPrivilegesGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok := s.cfg.Current().FindToken(id)
	if tok == nil {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	scopes := slices.Clone(tok.Scopes)
	descs := map[string]string{
		config.ScopeRead:  "read - queue and sites",
		config.ScopeWrite: "write - enqueue and manage jobs",
	}
	rows := make([]tokenScopeRow, 0, len(config.AllScopes))
	for _, sc := range config.AllScopes {
		rows = append(rows, tokenScopeRow{Name: sc, Desc: descs[sc], Checked: slices.Contains(scopes, sc)})
	}
	s.render(w, "token_privileges_dialog", map[string]any{
		"ID":        id,
		"Scopes":    rows,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"Paired":    tok.Paired != "",
	})
}

func (s *Server) settingsTokenPrivilegesPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "bad form data")
		return
	}
	id := r.PathValue("id")
	tok := s.cfg.Current().FindToken(id)
	if tok == nil {
		flashFragment(w, "err", "token not found")
		return
	}
	if tok.Paired != "" {
		flashFragment(w, "err", "this token is managed by a pairing; its privileges can't be changed")
		return
	}
	scopes := filterScopes(r.Form["scope"])
	if err := s.updateConfig(func(c *config.Config) error {
		c.SetTokenScopes(id, scopes)
		return nil
	}); err != nil {
		flashFragment(w, "err", "could not save: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", fmt.Sprintf(`{"token-saved":{"dialog":"token-cfg-%s"}}`, id))
	fmt.Fprintf(w,
		`<span id="token-scopes-%s" hx-swap-oob="true">%s</span>`+
			`<div id="flash-auth" hx-swap-oob="true"><div class="flash flash-ok">token privileges saved.</div></div>`,
		id, strings.Join(scopes, " "))
}

// filterScopes keeps only recognized scopes, in canonical order, dropping
// anything a tampered form might submit.
func filterScopes(in []string) []string {
	var out []string
	for _, sc := range config.AllScopes {
		if slices.Contains(in, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// renderAuthPasswordOOB re-renders the password sub-section out of band so its
// enabled/disabled line updates after a change without a full page reload.
func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.render(w, "auth_password_section", map[string]any{
		"AuthEnabled": s.cfg.Current().Auth.EnablePassword,
		"CSRFToken":   s.csrfToken(sessionFromContext(r.Context())),
		"OOB":         true,
	})
}
