package web

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/apitoken"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/session"
	"github.com/monbooru/monloader/internal/shared/webui"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "monloader_session"

func isOpenPath(p string) bool {
	return p == "/health" ||
		strings.HasPrefix(p, "/static/") || p == "/login" ||
		p == "/theme.css" || p == "/theme.logo" || p == "/theme.favicon"
}

func (s *Server) SweepSessions(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.sessions.SweepExpired()
			s.loginRL.Sweep()
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Current().Auth.EnablePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "webui/login_page", s.loginData("", loginNext(r.URL.Query().Get("next"))))
}

func loginNext(p string) string {
	if session.LocalPath(p) {
		return p
	}
	return ""
}

func (s *Server) loginData(errMsg, next string) map[string]any {
	theme := s.activeTheme()
	data := map[string]any{
		"App":          "monloader",
		"Title":        "Login - " + s.titleName(),
		"CSRFToken":    s.sessions.CSRFToken(session.Anon),
		"BooruName":    s.booruName(),
		"BooruFavicon": themeFaviconURL(theme),
		"Theme":        theme.Path != "",
		"Next":         next,
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
		http.Error(w, "Bad form data.", http.StatusBadRequest)
		return
	}
	next := loginNext(r.FormValue("next"))
	ip := session.ClientIP(r)
	if wait := s.loginRL.Wait(ip); wait > 0 {
		logx.Warnf("login rate-limited from %s", ip)
		secs := int(math.Ceil(wait.Seconds()))
		unit := "seconds"
		if secs == 1 {
			unit = "second"
		}
		s.renderLoginRefusal(w, http.StatusTooManyRequests, fmt.Sprintf("Wait %d %s before trying again.", secs, unit), next)
		return
	}
	password := r.FormValue("password")
	if bcrypt.CompareHashAndPassword([]byte(s.cfg.Current().Auth.PasswordHash), []byte(password)) != nil {
		s.loginRL.Fail(ip)
		logx.Warnf("login failed from %s", ip)
		s.renderLoginRefusal(w, http.StatusUnauthorized, "Invalid password", next)
		return
	}
	s.loginRL.Succeed(ip)
	logx.Infof("login success from %s", ip)
	days := s.cfg.Current().Auth.SessionLifetimeDays
	session.SetCookie(w, r, sessionCookie, s.sessions.Create(days), days*24*3600)
	http.Redirect(w, r, cmp.Or(next, "/"), http.StatusSeeOther)
}

func (s *Server) renderLoginRefusal(w http.ResponseWriter, status int, msg, next string) {
	// render sets this too, but after WriteHeader, which drops it.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	s.render(w, "webui/login_page", s.loginData(msg, next))
}

func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	s.sessions.Delete(session.FromCookie(r, sessionCookie))
	session.SetCookie(w, r, sessionCookie, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) settingsPasswordPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "Bad form data.")
		return
	}
	newPass := r.FormValue("new_password")
	if newPass == "" {
		flashFragment(w, "err", "New password required.")
		return
	}
	if s.cfg.Current().Auth.PasswordHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(s.cfg.Current().Auth.PasswordHash), []byte(r.FormValue("current_password"))) != nil {
			flashFragment(w, "err", "Current password is incorrect.")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		flashFragment(w, "err", "Error hashing password.")
		return
	}
	wasOn := s.cfg.Current().Auth.EnablePassword
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.PasswordHash = string(hash)
		c.Auth.EnablePassword = true
		return nil
	}); err != nil {
		flashFragment(w, "err", "Could not save: "+err.Error())
		return
	}
	// A password is often changed because the old one leaked: every other
	// browser signs in again. With auth off the setter holds only the anon
	// session, so it gets a real one rather than the login page.
	logx.Infof("settings: password updated from %s", session.ClientIP(r))
	keep := session.FromContext(r.Context())
	if !wasOn {
		days := s.cfg.Current().Auth.SessionLifetimeDays
		keep = s.sessions.Create(days)
		session.SetCookie(w, r, sessionCookie, keep, days*24*3600)
	}
	s.sessions.ClearExcept(keep)
	flashFragment(w, "ok", "Password updated.")
	s.renderAuthPasswordOOB(w, r)
}

// Gated on the hash, not EnablePassword: a TOML flag flipped off must not
// skip the check.
func (s *Server) settingsRemovePasswordPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "Bad form data.")
		return
	}
	if s.cfg.Current().Auth.PasswordHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(s.cfg.Current().Auth.PasswordHash), []byte(r.FormValue("current_password"))) != nil {
			flashFragment(w, "err", "Current password is incorrect.")
			return
		}
	}
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.EnablePassword = false
		c.Auth.PasswordHash = ""
		return nil
	}); err != nil {
		flashFragment(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: password removed from %s", session.ClientIP(r))
	s.sessions.Clear()
	flashFragment(w, "ok", "Password removed. Authentication is now disabled.")
	s.renderAuthPasswordOOB(w, r)
}

// The secret is minted outside updateConfig, whose closure runs twice, so
// both layers store the same one.
func (s *Server) settingsTokenCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "Bad form data.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := apitoken.ValidateName(name); err != nil {
		flashFragment(w, "err", err.Error())
		return
	}
	tok, secret := apitoken.New(name, config.AllScopes)
	if err := s.updateConfig(func(c *config.Config) error {
		return c.Auth.Tokens.Add(tok)
	}); err != nil {
		flashFragment(w, "err", err.Error())
		return
	}
	logx.Infof("settings: API token %q created from %s", name, session.ClientIP(r))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("HX-Trigger", "token-created")
	s.render(w, "webui/token_created", map[string]any{"Token": secret})
	s.renderAuthTokensOOB(w, r)
}

// Only these two peers have a remove-pairing control, so a token paired
// under any other name stays revocable from the list.
func hasPairingTeardown(peer string) bool {
	return peer == contract.AppMonbooru || peer == "monsender"
}

func (s *Server) settingsTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok := s.cfg.Current().Auth.Tokens.ByID(id)
	if tok == nil {
		flashFragment(w, "err", "Token not found.")
		return
	}
	if hasPairingTeardown(tok.Paired) {
		flashFragment(w, "err", "This token is managed by a pairing; remove the pairing instead.")
		return
	}
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens.Remove(id)
		return nil
	}); err != nil {
		flashFragment(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: API token %s revoked from %s", id, session.ClientIP(r))
	flashFragment(w, "ok", "Token revoked.")
	s.renderAuthTokensOOB(w, r)
}

func (s *Server) renderAuthTokensOOB(w http.ResponseWriter, r *http.Request) {
	s.render(w, "webui/auth_tokens", map[string]any{
		"Tokens":    slices.Clone(s.cfg.Current().Auth.Tokens),
		"CSRFToken": s.sessions.CSRFToken(session.FromContext(r.Context())),
		"OOB":       true,
	})
}

func (s *Server) settingsTokenPrivilegesGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok := s.cfg.Current().Auth.Tokens.ByID(id)
	if tok == nil {
		http.Error(w, "Token not found.", http.StatusNotFound)
		return
	}
	scopes := slices.Clone(tok.Scopes)
	descs := map[string]string{
		config.ScopeRead:  "read - queue and sites",
		config.ScopeWrite: "write - enqueue and manage jobs",
	}
	s.render(w, "webui/token_privileges_dialog", map[string]any{
		"ID":        id,
		"Scopes":    webui.ScopeRows(config.AllScopes, scopes, descs),
		"CSRFToken": s.sessions.CSRFToken(session.FromContext(r.Context())),
		"Paired":    tok.Paired != "",
	})
}

func (s *Server) settingsTokenPrivilegesPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		flashFragment(w, "err", "Bad form data.")
		return
	}
	id := r.PathValue("id")
	tok := s.cfg.Current().Auth.Tokens.ByID(id)
	if tok == nil {
		flashFragment(w, "err", "Token not found.")
		return
	}
	if tok.Paired != "" {
		flashFragment(w, "err", "This token is managed by a pairing; its privileges can't be changed.")
		return
	}
	scopes := apitoken.Filter(r.Form["scope"], config.AllScopes)
	if err := s.updateConfig(func(c *config.Config) error {
		c.Auth.Tokens.SetScopes(id, scopes)
		return nil
	}); err != nil {
		flashFragment(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: API token %s privileges updated from %s", id, session.ClientIP(r))
	w.Header().Set("HX-Trigger", fmt.Sprintf(`{"token-saved":{"dialog":"token-cfg-%s"}}`, id))
	s.render(w, "webui/token_scopes_saved", map[string]string{"ID": id, "Scopes": strings.Join(scopes, " ")})
}

func grantedScopes(requested []string) []string {
	if sc := apitoken.Filter(requested, config.AllScopes); len(sc) > 0 {
		return sc
	}
	return []string{config.ScopeRead}
}

func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.render(w, "webui/auth_password_section", map[string]any{
		"AuthEnabled": s.cfg.Current().Auth.EnablePassword,
		"HasPassword": s.cfg.Current().Auth.PasswordHash != "",
		"CSRFToken":   s.sessions.CSRFToken(session.FromContext(r.Context())),
		"OOB":         true,
	})
}
