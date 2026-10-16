package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"golang.org/x/crypto/bcrypt"
)

type contextKey int

const sessionContextKey contextKey = 1

func sessionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionContextKey).(string)
	return v
}

func isHTMXRequest(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (s *Server) SessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bypassAuth(r.URL.Path) || !s.cfg.Current().Auth.EnablePassword {
			ctx := context.WithValue(r.Context(), sessionContextKey, "anon")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		sessID := sessionCookie(r)
		if !s.sessions.Valid(sessID) {
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

// The API has its own bearer-token gate.
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

func (s *Server) loginData(errMsg string) map[string]any {
	data := map[string]any{
		"Title":        "Login - " + s.titleName(),
		"CSRFToken":    s.sessions.CSRFToken("anon"),
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
		// render sets this too, but after WriteHeader, which drops it.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", s.loginData("incorrect password"))
		return
	}
	id := s.sessions.Create(s.cfg.Current().Auth.SessionLifetimeDays)
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
	if s.cfg.Current().Auth.PasswordHash != "" {
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
	// A password is often changed because the old one leaked: every other
	// browser signs in again.
	s.sessions.ClearExcept(sessionFromContext(r.Context()))
	flashFragment(w, "ok", "password updated")
	s.renderAuthPasswordOOB(w, r)
}

// Gated on the hash, not EnablePassword: a TOML flag flipped off must not
// skip the check.
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

// The secret is minted outside updateConfig, whose closure runs twice, so
// both layers store the same one.
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

// Only these two peers have a remove-pairing control, so a token paired
// under any other name stays revocable from the list.
func hasPairingTeardown(peer string) bool {
	return peer == "monbooru" || peer == "monsender"
}

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

func (s *Server) renderAuthTokensOOB(w http.ResponseWriter, r *http.Request) {
	s.render(w, "auth_tokens", map[string]any{
		"Tokens":    slices.Clone(s.cfg.Current().Auth.Tokens),
		"CSRFToken": s.sessions.CSRFToken(sessionFromContext(r.Context())),
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
		"CSRFToken": s.sessions.CSRFToken(sessionFromContext(r.Context())),
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

func filterScopes(in []string) []string {
	var out []string
	for _, sc := range config.AllScopes {
		if slices.Contains(in, sc) {
			out = append(out, sc)
		}
	}
	return out
}

func grantedScopes(requested []string) []string {
	if sc := filterScopes(requested); len(sc) > 0 {
		return sc
	}
	return []string{config.ScopeRead}
}

func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.render(w, "auth_password_section", map[string]any{
		"AuthEnabled": s.cfg.Current().Auth.EnablePassword,
		"HasPassword": s.cfg.Current().Auth.PasswordHash != "",
		"CSRFToken":   s.sessions.CSRFToken(sessionFromContext(r.Context())),
		"OOB":         true,
	})
}
