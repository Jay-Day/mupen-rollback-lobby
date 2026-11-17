package modsite

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/ws"
)

// Moderating the live lobby: who's online, and the same kick / mute /
// timeout / ban / unban / unmute / close room as the lobby's MOD_ACTION, plus
// bans and mutes for an IP that isn't online. Every change is audited.
//
//	GET  /api/admin/live
//	POST /api/admin/users/{id}/{kick|mute|timeout|ban}
//	POST /api/admin/rooms/{id}/close
//	GET  /api/admin/sanctions
//	POST /api/admin/sanctions/{ban|mute|unban|unmute}

// limits on what a moderator types
const (
	maxReasonChars = 200
	maxNameChars   = 64
)

func (s *Server) registerLive() {
	s.Handle("GET /api/admin/live", s.handleLive)
	s.Handle("POST /api/admin/users/{id}/{action}", s.handleUserAction)
	s.Handle("POST /api/admin/rooms/{id}/close", s.handleCloseRoom)
	s.Handle("GET /api/admin/sanctions", s.handleSanctions)
	s.Handle("POST /api/admin/sanctions/{action}", s.handleSanctionIP)
}

// requireLobby answers 503 when the site runs without a lobby.
func (s *Server) requireLobby(w http.ResponseWriter) bool {
	if s.lobby == nil {
		WriteError(w, http.StatusServiceUnavailable, "the lobby isn't attached to this site")
		return false
	}
	return true
}

type sanctionDTO struct {
	IP     string `json:"ip"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
	By     string `json:"by"`
	Set    int64  `json:"set"`
	Until  int64  `json:"until"` // 0 = permanent
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func toSanction(s *moderation.Sanction) *sanctionDTO {
	if s == nil {
		return nil
	}
	return &sanctionDTO{IP: s.IP, Name: s.Name, Reason: s.Reason, By: s.By, Set: unixOrZero(s.Set), Until: unixOrZero(s.Until)}
}

func toSanctions(list []moderation.Sanction) []sanctionDTO {
	out := make([]sanctionDTO, 0, len(list))
	for i := range list {
		out = append(out, *toSanction(&list[i]))
	}
	return out
}

type liveUserDTO struct {
	ID        uint64       `json:"id"`
	Username  string       `json:"username"`
	IP        string       `json:"ip"`
	Moderator bool         `json:"moderator"`
	Version   string       `json:"version"`
	Connected int64        `json:"connected"`
	Status    string       `json:"status"`
	RoomID    uint64       `json:"roomId"`
	RoomName  string       `json:"roomName"`
	Country   string       `json:"country"`
	Muted     *sanctionDTO `json:"muted"`
}

type liveRoomDTO struct {
	ID         uint64   `json:"id"`
	Name       string   `json:"name"`
	Game       string   `json:"game"`
	Host       string   `json:"host"`
	Players    []string `json:"players"`
	Spectators int      `json:"spectators"`
	InMatch    bool     `json:"inMatch"`
	MatchID    uint64   `json:"matchId"`
	QuickMatch bool     `json:"quickMatch"`
	Locked     bool     `json:"locked"`
	Created    int64    `json:"created"`
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLobby(w) {
		return
	}
	snap := s.lobby.LiveSnapshot()
	users := make([]liveUserDTO, 0, len(snap.Users))
	for _, u := range snap.Users {
		users = append(users, liveUserDTO{ID: u.ID, Username: u.Username, IP: u.IP, Moderator: u.Moderator,
			Version: u.Version, Connected: unixOrZero(u.Connected), Status: u.Status, RoomID: u.RoomID,
			RoomName: u.RoomName, Country: u.Country, Muted: toSanction(u.Muted)})
	}
	rooms := make([]liveRoomDTO, 0, len(snap.Rooms))
	for _, room := range snap.Rooms {
		players := room.Players
		if players == nil {
			players = []string{}
		}
		rooms = append(rooms, liveRoomDTO{ID: room.ID, Name: room.Name, Game: room.Game, Host: room.Host,
			Players: players, Spectators: room.Spectators, InMatch: room.InMatch, MatchID: room.MatchID,
			QuickMatch: room.QuickMatch, Locked: room.Locked, Created: unixOrZero(room.Created)})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"users": users, "rooms": rooms, "bans": snap.Bans, "mutes": snap.Mutes})
}

// actionRequest is the body of the moderation actions.
type actionRequest struct {
	IP       string `json:"ip"`
	Name     string `json:"name"`
	Duration string `json:"duration"`
	Reason   string `json:"reason"`
	// Confirm: ban even though another signed-in moderator uses the address
	// (the page asks first; see checkBanTarget).
	Confirm bool `json:"confirm"`
}

// read decodes and checks the body: lengths and the duration.
func (a *actionRequest) read(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	if err := decodeBody(r, a); err != nil {
		WriteError(w, http.StatusBadRequest, "bad request")
		return 0, false
	}
	a.Reason = strings.TrimSpace(a.Reason)
	a.Name = strings.TrimSpace(a.Name)
	a.IP = strings.TrimSpace(a.IP)
	if !utf8.ValidString(a.Reason) || utf8.RuneCountInString(a.Reason) > maxReasonChars {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("the reason is too long (%d characters at most)", maxReasonChars))
		return 0, false
	}
	if !utf8.ValidString(a.Name) || utf8.RuneCountInString(a.Name) > maxNameChars {
		WriteError(w, http.StatusBadRequest, "the name is too long")
		return 0, false
	}
	if len(a.IP) > 64 || len(a.Duration) > 16 {
		WriteError(w, http.StatusBadRequest, "bad request")
		return 0, false
	}
	dur, err := ws.ParseModDuration(a.Duration)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "bad duration: use e.g. 10m, 1h or 2d, or leave it empty")
		return 0, false
	}
	return dur, true
}

func (s *Server) handleUserAction(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLobby(w) {
		return
	}
	action := r.PathValue("action")
	switch action {
	case "kick", "mute", "timeout", "ban":
	default:
		WriteError(w, http.StatusNotFound, "no such action")
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		WriteError(w, http.StatusNotFound, "that user isn't connected")
		return
	}
	var req actionRequest
	dur, ok := req.read(w, r)
	if !ok {
		return
	}
	if action == "timeout" && dur <= 0 {
		WriteError(w, http.StatusBadRequest, "a timeout needs a duration, e.g. 10m, 1h or 2d")
		return
	}
	if action == "kick" || action == "ban" {
		dur = 0
	}
	if action == "ban" || action == "timeout" {
		ip, online := s.lobby.UserIP(id)
		if !online {
			WriteError(w, http.StatusNotFound, "that user isn't connected any more")
			return
		}
		if !s.checkBanTarget(w, r, ip, req.Confirm) {
			return
		}
	}
	out, err := s.lobby.ModerateUser(admin, id, action, dur, req.Reason)
	switch {
	case errors.Is(err, ws.ErrNotConnected):
		WriteError(w, http.StatusNotFound, "that user isn't connected any more")
		return
	case errors.Is(err, ws.ErrModerator):
		WriteError(w, http.StatusConflict, out.Target+" is a signed-in moderator")
		return
	case errors.Is(err, ws.ErrNeedsDuration):
		WriteError(w, http.StatusBadRequest, "a timeout needs a duration, e.g. 10m, 1h or 2d")
		return
	case err != nil:
		WriteError(w, http.StatusServiceUnavailable, "moderation isn't configured on this server")
		return
	}
	s.audit(admin, action, out.Detail, RemoteIP(r))
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "target": out.Target, "ip": out.IP,
		"until": unixOrZero(out.Until)})
}

func (s *Server) handleCloseRoom(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLobby(w) {
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		WriteError(w, http.StatusNotFound, "no such room")
		return
	}
	var req actionRequest
	if _, ok := req.read(w, r); !ok {
		return
	}
	name, ok := s.lobby.CloseRoom(admin, id, req.Reason)
	if !ok {
		WriteError(w, http.StatusNotFound, "that room is gone")
		return
	}
	s.audit(admin, "closeroom", fmt.Sprintf("Closed room #%d \"%s\"%s", id, name, suffix(req.Reason)), RemoteIP(r))
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func suffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// checkBanTarget refuses to ban the moderator's own address (it would lock
// them out of this site and the lobby), and asks for confirm before banning
// an address another signed-in moderator uses. It answers and returns false
// when the ban mustn't go ahead.
func (s *Server) checkBanTarget(w http.ResponseWriter, r *http.Request, ip string, confirm bool) bool {
	if ws.NormalizeIP(ip) == RemoteIP(r) {
		WriteError(w, http.StatusBadRequest, "That's your own address ("+ip+"): banning it would lock you out of this site and the lobby.")
		return false
	}
	if confirm {
		return true
	}
	var others []string
	sess, _ := r.Context().Value(sessionKey{}).(*session)
	except := ""
	if sess != nil {
		except = sess.hash
	}
	if n := s.sessions.othersAt(RemoteIPOf(ip), except); n == 1 {
		others = append(others, "a moderator signed in to this site")
	} else if n > 1 {
		others = append(others, fmt.Sprintf("%d moderators signed in to this site", n))
	}
	for _, name := range s.lobby.ModeratorsAt(ip) {
		others = append(others, "moderator "+name+" in the lobby")
	}
	if len(others) == 0 {
		return true
	}
	WriteJSON(w, http.StatusConflict, map[string]any{"needsConfirm": true,
		"error": ip + " is also used by " + strings.Join(others, " and ") + ": the ban locks them out too."})
	return false
}

// RemoteIPOf is ip as RemoteIP writes request addresses.
func RemoteIPOf(ip string) string { return moderation.CanonicalIP(ws.NormalizeIP(ip)) }

// The Sanctions lists, paged like the other lists (newest first; each item
// carries the cursor for the page after it).
//
//	GET /api/admin/sanctions                      the first page of each, with counts
//	GET /api/admin/sanctions?kind=ban|mute&before=&limit=
//	GET /api/admin/sanctions?ip=                  one address's ban and mute
const sanctionsPage = 100

type pagedSanction struct {
	sanctionDTO
	Cursor string `json:"cursor"`
}

// sanctionCursor orders sanctions newest first, then by IP.
func sanctionCursor(sn moderation.Sanction) string {
	var at int64
	if !sn.Set.IsZero() && sn.Set.Year() > 1970 && sn.Set.Year() < 2200 {
		at = sn.Set.UnixNano()
	}
	return fmt.Sprintf("%020d~%s", at, sn.IP)
}

func pageSanctions(list []moderation.Sanction, before string, limit int) ([]pagedSanction, bool) {
	sort.Slice(list, func(i, j int) bool { return sanctionCursor(list[i]) > sanctionCursor(list[j]) })
	out := []pagedSanction{}
	for i := range list {
		c := sanctionCursor(list[i])
		if before != "" && c >= before {
			continue
		}
		if len(out) == limit {
			return out, true
		}
		out = append(out, pagedSanction{sanctionDTO: *toSanction(&list[i]), Cursor: c})
	}
	return out, false
}

func (s *Server) handleSanctions(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLobby(w) {
		return
	}
	q := &query{w: w, r: r}
	kind, before, rawIP := q.text("kind", 8), q.text("before", 128), q.text("ip", maxQueryIP)
	limit := q.limit()
	if !q.ok() {
		return
	}
	if limit == 0 {
		limit = sanctionsPage
	}
	if rawIP != "" {
		ban, mute := s.sanctionsOn(rawIP)
		WriteJSON(w, http.StatusOK, map[string]any{"ban": ban, "mute": mute})
		return
	}
	bans, mutes := s.lobby.Sanctions()
	switch kind {
	case "":
		banPage, moreBans := pageSanctions(bans, "", limit)
		mutePage, moreMutes := pageSanctions(mutes, "", limit)
		WriteJSON(w, http.StatusOK, map[string]any{"bans": banPage, "mutes": mutePage, "moreBans": moreBans,
			"moreMutes": moreMutes, "banCount": len(bans), "muteCount": len(mutes)})
	case "ban", "mute":
		list := bans
		if kind == "mute" {
			list = mutes
		}
		page, more := pageSanctions(list, before, limit)
		WriteJSON(w, http.StatusOK, map[string]any{"items": page, "more": more, "total": len(list)})
	default:
		WriteError(w, http.StatusBadRequest, "bad kind")
	}
}

// parseIP reads an IP address as the lobby writes them, "" when it isn't one.
func parseIP(raw string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || a.Zone() != "" {
		return ""
	}
	return a.Unmap().String()
}

func (s *Server) handleSanctionIP(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLobby(w) {
		return
	}
	action := r.PathValue("action")
	switch action {
	case "ban", "mute", "unban", "unmute":
	default:
		WriteError(w, http.StatusNotFound, "no such action")
		return
	}
	var req actionRequest
	dur, ok := req.read(w, r)
	if !ok {
		return
	}
	if action == "unban" || action == "unmute" {
		if req.IP == "" {
			WriteError(w, http.StatusBadRequest, "give the IP address")
			return
		}
		what, verb := "ban", "Unbanned "
		if action == "unmute" {
			what, verb = "mute", "Unmuted "
		}
		if !s.lobby.LiftSanction(admin, action, req.IP) {
			WriteError(w, http.StatusNotFound, "no "+what+" on "+req.IP)
			return
		}
		s.audit(admin, action, verb+ws.NormalizeIP(req.IP), RemoteIP(r))
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	ip := parseIP(req.IP)
	if ip == "" {
		WriteError(w, http.StatusBadRequest, "that isn't an IP address")
		return
	}
	if moderation.IsLoopback(ip) {
		// the site itself is reached from loopback (an SSH tunnel)
		WriteError(w, http.StatusBadRequest, "a loopback address can't be banned or muted")
		return
	}
	who := ip
	if req.Name != "" {
		who = ip + " (" + req.Name + ")"
	}
	until := ""
	if dur > 0 {
		until = " until " + time.Now().Add(dur).Format("2006-01-02 15:04 MST")
	}
	if action == "mute" {
		if dur <= 0 {
			until = " (permanent)"
		}
		n := s.lobby.MuteIP(admin, ip, req.Name, dur, req.Reason)
		s.audit(admin, "mute", "Muted "+who+until+suffix(req.Reason), RemoteIP(r))
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "told": n})
		return
	}
	if !s.checkBanTarget(w, r, ip, req.Confirm) {
		return
	}
	disconnected := s.lobby.BanIP(admin, ip, req.Name, dur, req.Reason)
	auditAction, detail := "ban", "Banned "+who
	if dur > 0 {
		auditAction, detail = "timeout", "Timed out "+who+until
	}
	detail += suffix(req.Reason)
	if disconnected > 0 {
		detail += fmt.Sprintf(" (%d disconnected)", disconnected)
	}
	s.audit(admin, auditAction, detail, RemoteIP(r))
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "disconnected": disconnected})
}
