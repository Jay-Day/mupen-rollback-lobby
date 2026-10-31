package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// Admin pages: moderators (admins.json, the same credentials as the lobby's
// /login) review disputed sets, issue password reset codes and disable
// accounts. Sessions live in memory: a lobby restart signs everyone out.
//
// Requests are protected by an HttpOnly SameSite=Strict cookie scoped to the
// admin API, and every change must carry the X-RMGK-Admin header, which a
// cross-site form or image can't send.

const (
	adminCookie     = "rmgk_admin"
	adminSessionTTL = 12 * time.Hour
	adminHeader     = "X-RMGK-Admin"
	maxAdminBody    = 16 << 10
)

// AdminAuthenticator checks a moderator password sent from ip, returning the
// admin's name. limited is true while ip has too many recent failures (the
// budget is shared with the lobby's /login).
type AdminAuthenticator interface {
	AuthenticateAdminFrom(ip, password string) (name string, ok, limited bool)
}

// AdminHooks lets admin actions reach connected players (the lobby hub).
type AdminHooks struct {
	SetResolved     func(set accounts.SetSummary, outcomes []accounts.PlayerOutcome, winnerID int64, reason string)
	AccountDisabled func(accountID int64)
}

type adminSession struct {
	name    string
	expires time.Time
}

type adminState struct {
	auth  AdminAuthenticator
	hooks AdminHooks

	mu       sync.Mutex
	sessions map[string]adminSession
}

// EnableAdmin turns on the admin pages.
func (s *Server) EnableAdmin(auth AdminAuthenticator, hooks AdminHooks) {
	s.admin = &adminState{
		auth: auth, hooks: hooks,
		sessions: map[string]adminSession{},
	}
}

func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /api/admin/logout", s.adminOnly(s.handleAdminLogout))
	mux.HandleFunc("GET /api/admin/session", s.adminOnly(s.handleAdminSession))
	mux.HandleFunc("GET /api/admin/disputes", s.adminOnly(s.handleAdminDisputes))
	mux.HandleFunc("POST /api/admin/sets/{id}/resolve", s.adminOnly(s.handleAdminResolve))
	mux.HandleFunc("GET /api/admin/accounts", s.adminOnly(s.handleAdminAccounts))
	mux.HandleFunc("GET /api/admin/accounts/{name}", s.adminOnly(s.handleAdminAccount))
	mux.HandleFunc("POST /api/admin/accounts/{name}/reset-code", s.adminOnly(s.handleAdminResetCode))
	mux.HandleFunc("POST /api/admin/accounts/{name}/disabled", s.adminOnly(s.handleAdminDisabled))
	mux.HandleFunc("POST /api/admin/accounts/{name}/discord/unlink", s.adminOnly(s.handleAdminUnlinkDiscord))
	mux.HandleFunc("GET /api/admin/log", s.adminOnly(s.handleAdminLog))
}

// SetClientIP sets how client addresses are found (behind a proxy).
func (s *Server) SetClientIP(clientIP func(*http.Request) string) {
	s.clientIP = clientIP
}

func (s *Server) remoteIP(r *http.Request) string {
	if s.clientIP != nil {
		return s.clientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newAdminToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// session returns the signed-in admin's name, "" when none.
func (a *adminState) session(r *http.Request) (string, string) {
	cookie, err := r.Cookie(adminCookie)
	if err != nil || cookie.Value == "" {
		return "", ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for token, session := range a.sessions {
		if now.After(session.expires) {
			delete(a.sessions, token)
		}
	}
	for token, session := range a.sessions {
		if subtle.ConstantTimeCompare([]byte(token), []byte(cookie.Value)) == 1 {
			return session.name, token
		}
	}
	return "", ""
}

func setAdminCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: value, Path: "/api/admin", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
}

type adminHandler func(w http.ResponseWriter, r *http.Request, admin string)

// adminOnly requires a signed-in admin, and the admin header on changes.
func (s *Server) adminOnly(next adminHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.admin == nil {
			writeError(w, r, http.StatusNotFound, "admin pages are off")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(adminHeader) != "1" {
			writeError(w, r, http.StatusForbidden, "missing admin header")
			return
		}
		name, _ := s.admin.session(r)
		if name == "" {
			writeError(w, r, http.StatusUnauthorized, "sign in first")
			return
		}
		next(w, r, name)
	}
}

func decodeBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxAdminBody)
	return json.NewDecoder(r.Body).Decode(v)
}

func (s *Server) audit(admin, action, detail string) {
	if err := s.store.AddAdminLog(admin, action, detail); err != nil {
		slog.Warn("admin log write failed", "err", err)
	}
	slog.Info("admin action", "admin", admin, "action", action, "detail", detail)
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.admin == nil {
		writeError(w, r, http.StatusNotFound, "admin pages are off")
		return
	}
	if r.Header.Get(adminHeader) != "1" {
		writeError(w, r, http.StatusForbidden, "missing admin header")
		return
	}
	ip, now := s.remoteIP(r), time.Now()
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad request")
		return
	}
	name, ok, limited := s.admin.auth.AuthenticateAdminFrom(ip, req.Password)
	if limited {
		writeError(w, r, http.StatusTooManyRequests, "too many failed sign-ins, try again later")
		return
	}
	if !ok {
		slog.Info("admin sign-in failed", "ip", ip)
		writeError(w, r, http.StatusUnauthorized, "wrong password")
		return
	}
	token, err := newAdminToken()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't start a session")
		return
	}
	s.admin.mu.Lock()
	s.admin.sessions[token] = adminSession{name: name, expires: now.Add(adminSessionTTL)}
	s.admin.mu.Unlock()
	setAdminCookie(w, r, token, int(adminSessionTTL/time.Second))
	slog.Info("admin signed in", "admin", name, "ip", ip)
	writeJSON(w, r, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request, admin string) {
	if _, token := s.admin.session(r); token != "" {
		s.admin.mu.Lock()
		delete(s.admin.sessions, token)
		s.admin.mu.Unlock()
	}
	setAdminCookie(w, r, "", -1)
	writeJSON(w, r, http.StatusOK, map[string]string{"name": admin})
}

// handleAdminSession names the signed-in moderator and counts the disputed
// sets, so every moderator page can show it without loading them.
func (s *Server) handleAdminSession(w http.ResponseWriter, r *http.Request, admin string) {
	disputes, err := s.store.DisputedCount()
	if err != nil {
		slog.Warn("disputed set count failed", "err", err)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"name": admin, "disputes": disputes})
}

//
// Disputes
//

type reportDTO struct {
	Reporter   string   `json:"reporter"`
	WinnerSlot int      `json:"winnerSlot"`
	Score      []int    `json:"score"`
	Integrity  string   `json:"integrity"`
	Games      []string `json:"games"`
}

type disputeDTO struct {
	setDTO
	Reports []reportDTO `json:"reports"`
}

// reportsOf reads a disputed set's MATCH_RESULT reports (keyed by username;
// older sets by connection id).
func reportsOf(resultJSON string) []reportDTO {
	var reports map[string]protocol.MatchResultRequest
	if json.Unmarshal([]byte(resultJSON), &reports) != nil {
		return []reportDTO{}
	}
	out := make([]reportDTO, 0, len(reports))
	for reporter, r := range reports {
		dto := reportDTO{Reporter: reporter, WinnerSlot: r.WinnerSlot, Score: r.Score, Integrity: r.Integrity, Games: []string{}}
		for _, g := range r.Games {
			dto.Games = append(dto.Games, fmt.Sprintf("game %d: P%d", g.Game, g.WinnerSlot))
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reporter < out[j].Reporter })
	return out
}

func (s *Server) handleAdminDisputes(w http.ResponseWriter, r *http.Request, admin string) {
	sets, err := s.store.DisputedSets(100)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't list disputed sets")
		return
	}
	out := make([]disputeDTO, 0, len(sets))
	for _, set := range sets {
		out = append(out, disputeDTO{setDTO: s.toSetDTO(set), Reports: reportsOf(set.ResultJSON)})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sets": out})
}

func (s *Server) handleAdminResolve(w http.ResponseWriter, r *http.Request, admin string) {
	id, ok := parseSetID(r)
	if !ok {
		writeError(w, r, http.StatusNotFound, "no such set")
		return
	}
	var req struct {
		Winner string `json:"winner"` // username, "" with void
		Void   bool   `json:"void"`
		Note   string `json:"note"`
	}
	if err := decodeBody(r, &req); err != nil || (req.Void == (req.Winner != "")) || len(req.Note) > 200 {
		writeError(w, r, http.StatusBadRequest, "pick a winner or void the set")
		return
	}
	set, err := s.store.SetByID(id)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "no such set")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't load the set")
		return
	}
	var winnerID int64
	for _, p := range set.Players {
		if !req.Void && strings.EqualFold(p.Username, req.Winner) {
			winnerID = p.AccountID
			req.Winner = p.Username
		}
	}
	if !req.Void && winnerID == 0 {
		writeError(w, r, http.StatusBadRequest, req.Winner+" didn't play this set")
		return
	}
	note := strings.TrimSpace(req.Note)
	reason := "resolved by " + admin
	if note != "" {
		reason += ": " + note
	}
	outcomes, err := s.store.ResolveSet(id, winnerID, reason)
	switch {
	case errors.Is(err, accounts.ErrNotDisputed):
		writeError(w, r, http.StatusConflict, "this set isn't disputed any more")
		return
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "couldn't resolve the set")
		return
	}
	if req.Void {
		s.audit(admin, "resolve", fmt.Sprintf("set #%d voided%s", id, suffix(note)))
	} else {
		s.audit(admin, "resolve", fmt.Sprintf("set #%d rated, %s wins%s", id, req.Winner, suffix(note)))
	}
	if s.admin.hooks.SetResolved != nil {
		s.admin.hooks.SetResolved(set, outcomes, winnerID, reason)
	}
	updated, err := s.store.SetByID(id)
	if err != nil {
		writeJSON(w, r, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, r, http.StatusOK, s.toSetDTO(updated))
}

func suffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

//
// Accounts
//

type adminAccountDTO struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	Created      int64  `json:"created"`
	Disabled     bool   `json:"disabled"`
	Sessions     int    `json:"sessions"`
	ResetPending bool   `json:"resetPending"`
}

func toAdminAccount(a accounts.AccountStatus) adminAccountDTO {
	return adminAccountDTO{ID: a.ID, Username: a.Username, Created: a.CreatedAt.Unix(),
		Disabled: a.Disabled, Sessions: a.Sessions, ResetPending: a.ResetPending}
}

func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request, admin string) {
	list, err := s.store.SearchAccounts(r.URL.Query().Get("q"), 50)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't search accounts")
		return
	}
	out := make([]adminAccountDTO, 0, len(list))
	for _, a := range list {
		out = append(out, toAdminAccount(a))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) adminAccount(w http.ResponseWriter, r *http.Request) (accounts.AccountStatus, bool) {
	account, err := s.store.AccountStatusByName(r.PathValue("name"))
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "no such account")
		return account, false
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't load the account")
		return account, false
	}
	return account, true
}

func (s *Server) handleAdminAccount(w http.ResponseWriter, r *http.Request, admin string) {
	account, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	type ratingDTO struct {
		Ladder      string `json:"ladder"`
		Ruleset     string `json:"ruleset"`
		RulesetName string `json:"rulesetName"`
		Rating      int    `json:"rating"`
		Deviation   int    `json:"deviation"`
		Sets        int    `json:"sets"`
	}
	standings := []ratingDTO{}
	if ratings, err := s.store.Ratings(account.ID); err == nil {
		for _, lr := range ratings {
			name := lr.Ruleset
			if ladder, ok := s.cfg.Ladder(lr.Ladder); ok {
				name = ladder.RulesetName(lr.Ruleset)
			}
			standings = append(standings, ratingDTO{lr.Ladder, lr.Ruleset, name, int(math.Round(lr.Rating.Rating)),
				int(math.Round(lr.Rating.Deviation)), lr.SetsPlayed})
		}
	}
	sets := []setDTO{}
	if list, err := s.store.RecentSets("", "", account.ID, 20); err == nil {
		for _, set := range list {
			sets = append(sets, s.toSetDTO(set))
		}
	}
	// the linked Discord account (moderators only; never shown publicly)
	var discordLink any
	if link, ok, err := s.store.DiscordLinkOf(account.ID); err == nil && ok {
		discordLink = map[string]any{"id": link.DiscordID, "username": link.Username, "name": link.Name,
			"linked": link.LinkedAt.Unix(), "lastLogin": link.LastLoginAt.Unix()}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"account": toAdminAccount(account), "standings": standings, "sets": sets, "discord": discordLink,
	})
}

func (s *Server) handleAdminUnlinkDiscord(w http.ResponseWriter, r *http.Request, admin string) {
	account, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	link, linked, err := s.store.DiscordLinkOf(account.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't load the account")
		return
	}
	if !linked {
		writeError(w, r, http.StatusConflict, "no Discord account is linked")
		return
	}
	if err := s.store.UnlinkDiscord(account.ID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't unlink Discord")
		return
	}
	s.audit(admin, "discord-unlink", account.Username+" unlinked from Discord "+link.Username+" ("+link.DiscordID+")")
	writeJSON(w, r, http.StatusOK, map[string]any{})
}

func (s *Server) handleAdminResetCode(w http.ResponseWriter, r *http.Request, admin string) {
	account, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	if account.Disabled {
		writeError(w, r, http.StatusConflict, "enable the account first")
		return
	}
	code, _, err := s.store.CreatePasswordReset(account.Username, admin)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't create a reset code")
		return
	}
	s.audit(admin, "reset-code", "reset code issued for "+account.Username)
	writeJSON(w, r, http.StatusOK, map[string]any{
		"username": account.Username, "code": code,
		"expires": time.Now().Add(accounts.ResetCodeTTL).Unix(),
	})
}

func (s *Server) handleAdminDisabled(w http.ResponseWriter, r *http.Request, admin string) {
	account, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	var req struct {
		Disabled bool   `json:"disabled"`
		Note     string `json:"note"`
	}
	if err := decodeBody(r, &req); err != nil || len(req.Note) > 200 {
		writeError(w, r, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.store.SetDisabled(account.ID, req.Disabled); err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't update the account")
		return
	}
	note := strings.TrimSpace(req.Note)
	if req.Disabled {
		s.audit(admin, "disable", account.Username+" disabled"+suffix(note))
		if s.admin.hooks.AccountDisabled != nil {
			s.admin.hooks.AccountDisabled(account.ID)
		}
	} else {
		s.audit(admin, "enable", account.Username+" enabled"+suffix(note))
	}
	updated, err := s.store.AccountStatusByName(account.Username)
	if err != nil {
		writeJSON(w, r, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, r, http.StatusOK, toAdminAccount(updated))
}

func (s *Server) handleAdminLog(w http.ResponseWriter, r *http.Request, admin string) {
	entries, err := s.store.AdminLog(200)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "couldn't read the log")
		return
	}
	type entryDTO struct {
		Time   int64  `json:"time"`
		Admin  string `json:"admin"`
		Action string `json:"action"`
		Detail string `json:"detail"`
	}
	out := make([]entryDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryDTO{e.Time.Unix(), e.Admin, e.Action, e.Detail})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"entries": out})
}
