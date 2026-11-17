package modsite

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
)

// loginFailed is the one answer every failed sign-in gets, whatever the
// reason (wrong password, locked out, a malformed request), so the page
// tells an attacker nothing.
const loginFailed = "Sign-in failed."

// handleLogin signs a moderator in with the admins.json password. The login
// guard counts failures per IP across the site and the lobby.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request, ip string) {
	fail := func() { WriteError(w, http.StatusUnauthorized, loginFailed) }
	if checkAPIRequest(r) != "" {
		fail()
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &req); err != nil || req.Password == "" || len(req.Password) > 1024 {
		fail()
		return
	}
	var name, key string
	result := s.guard.Try(ip, "moderator site", func() bool {
		var ok bool
		name, key, ok = s.mod.AuthenticateAdminKey(req.Password)
		return ok
	})
	switch result {
	case moderation.LoginOK:
	case moderation.LoginRefused:
		slog.Warn("moderator site sign-in refused: locked out", "ip", ip)
		fail()
		return
	default:
		// LoginBanned and LoginLocked are logged (Warn) by the guard
		slog.Info("moderator site sign-in failed", "ip", ip)
		fail()
		return
	}
	token, err := s.sessions.create(name, key, ip)
	if err != nil {
		slog.Error("moderator session: no random token", "err", err)
		WriteError(w, http.StatusInternalServerError, "couldn't start a session")
		return
	}
	setCookie(w, token)
	slog.Info("moderator signed in to the site", "admin", name, "ip", ip)
	s.audit(name, "sign-in", "Signed in to the moderator site", ip)
	WriteJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, admin string) {
	s.sessions.end(r)
	setCookie(w, "")
	s.audit(admin, "sign-out", "Signed out of the moderator site", RemoteIP(r))
	WriteJSON(w, http.StatusOK, map[string]string{"name": admin})
}

// handleSession names the signed-in moderator, counts who's online, and
// says whether the server keeps the moderators' log.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request, admin string) {
	online := 0
	if s.lobby != nil {
		online = s.lobby.OnlineCount()
	}
	WriteJSON(w, http.StatusOK, map[string]any{"name": admin, "online": online, "logging": s.log != nil,
		"idleMinutes": int(sessionIdle / time.Minute)})
}

// servePage answers GET /: the moderator pages to a signed-in moderator, the
// sign-in page to anyone else. The sign-in page names nothing about the
// server.
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, ip string) {
	p := s.pages.login
	signedIn := s.sessions.get(r, ip, s.mod.AdminKeyValid, true) != nil
	if signedIn {
		p = s.pages.app
	}
	h := w.Header()
	setSecurityHeaders(h, p.csp)
	if !signedIn {
		h.Set("Connection", "close") // signed out: nothing stays open
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(p.html)
	}
}
