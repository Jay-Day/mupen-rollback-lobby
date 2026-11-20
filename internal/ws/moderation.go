package ws

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/usernames"
)

// maxAuthFails is how many bad ADMIN_AUTH passwords one connection gets
// before it is disconnected. The login guard (SetLoginGuard) also counts them
// per IP, with the moderator website's, and bans the IP at its limit.
const maxAuthFails = 5

// guardBanNotice is what a connection hears when the login guard bans its IP.
const guardBanNotice = "This address was banned after too many failed moderator logins."

// handleAdminAuth validates a password against the configured admins and, on
// success, elevates this connection to moderator for its lifetime.
func (h *Hub) handleAdminAuth(c *Client, env protocol.Envelope) {
	if h.mod == nil {
		c.sendEnvelope(protocol.TypeAdminAuthFail, env.ID, protocol.ModNoticePayload{
			Text: "Moderation is not configured on this server.", Severity: "error",
		})
		return
	}
	var req protocol.AdminAuthRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	if c.fromWeb {
		// A web page opened this connection (the desktop client sends no
		// Origin). Any site can do that from a visitor's browser, so its
		// passwords are never checked and never counted: it can't guess one,
		// and it can't get the visitor's address banned by failing. It gets
		// the same answer as a wrong password.
		if !c.webAuthSeen {
			c.webAuthSeen = true
			slog.Info("admin auth from a web page refused (not checked or counted)", "user", c.user.Username, "ip", c.ip)
		}
		c.sendEnvelope(protocol.TypeAdminAuthFail, env.ID, protocol.ModNoticePayload{
			Text: "Incorrect password.", Severity: "error",
		})
		return
	}
	var name, key string
	result := h.guard.Try(c.ip, "lobby", func() bool {
		var ok bool
		name, key, ok = h.mod.AuthenticateAdminKey(req.Password)
		return ok
	})
	switch result {
	case moderation.LoginRefused:
		// a loopback address the guard locked out doesn't get to try
		slog.Info("admin auth refused: locked out", "user", c.user.Username, "ip", c.ip)
		c.sendEnvelope(protocol.TypeAdminAuthFail, env.ID, protocol.ModNoticePayload{
			Text: "Too many failed login attempts. Try again later.", Severity: "error",
		})
		return
	case moderation.LoginBanned:
		c.authFails++
		slog.Info("admin auth failed", "user", c.user.Username, "ip", c.ip, "fails", c.authFails)
		return // the ban closed every connection from the IP, this one too
	}
	if result != moderation.LoginOK {
		c.authFails++
		slog.Info("admin auth failed", "user", c.user.Username, "ip", c.ip, "fails", c.authFails)
		if c.authFails >= maxAuthFails {
			// Cut off brute-forcing; reconnecting costs them the connect gate.
			c.disconnect("Too many failed login attempts.")
			return
		}
		c.sendEnvelope(protocol.TypeAdminAuthFail, env.ID, protocol.ModNoticePayload{
			Text: "Incorrect password.", Severity: "error",
		})
		return
	}
	c.setModerator(true, name, key)
	slog.Info("admin authenticated", "admin", name, "user", c.user.Username, "ip", c.ip)
	c.sendEnvelope(protocol.TypeAdminAuthOK, env.ID, protocol.AdminAuthOKResponse{Name: name})
	h.audit(name, "sign-in", "Signed in to the lobby as "+c.user.Username, modlog.SourceLobby, c.ip)
	h.broadcastModPeers()
}

// handleAdminLogout gives the moderator role back for the rest of the session.
func (h *Hub) handleAdminLogout(c *Client, env protocol.Envelope) {
	on, name := c.moderatorState()
	if !on {
		return
	}
	slog.Info("admin logged out", "admin", name, "user", c.user.Username)
	c.setModerator(false, "", "")
	c.sendEnvelope(protocol.TypeAdminLoggedOut, env.ID, struct{}{})
	h.broadcastModPeers()
}

// broadcastModPeers tells every moderator who else is one, so their menus can
// grey out what the server would refuse. Everyone on a moderator's connection
// counts: findClientByUsername reaches the connection through any of them.
func (h *Hub) broadcastModPeers() {
	var mods []*Client
	for _, cl := range h.clientList() {
		if on, _ := cl.moderatorState(); on {
			mods = append(mods, cl)
		}
	}
	payload := protocol.ModPeersPayload{UserIDs: []uint64{}}
	for _, cl := range mods {
		for _, u := range cl.user.Group() {
			payload.UserIDs = append(payload.UserIDs, u.ID)
		}
	}
	for _, cl := range mods {
		cl.sendEnvelope(protocol.TypeModPeers, "", payload)
	}
}

// loginGuardBanned runs after the login guard bans an IP (from the lobby or
// the moderator website): every lobby connection from it is closed, and the
// ban goes into the moderators' log and audit trail.
func (h *Hub) loginGuardBanned(ban moderation.GuardBan) {
	action, detail := "ban", "Banned "+ban.IP+": "+ban.Reason+" ("+ban.Where+")"
	if ban.Duration > 0 {
		action = "timeout"
		detail = "Timed out " + ban.IP + untilSuffix(timeFromDur(ban.Duration)) + ": " + ban.Reason + " (" + ban.Where + ")"
	}
	h.modlog.Event(modlog.Event{Type: modlog.EventModeration, IP: ban.IP, Text: moderation.AutoBanBy + ": " + detail})
	h.audit(moderation.AutoBanBy, action, detail, modlog.SourceAuto, ban.IP)
	for _, tc := range h.clientList() {
		if moderation.CanonicalIP(tc.ip) == ban.IP {
			go tc.disconnect(guardBanNotice)
		}
	}
}

// activeModerator reports whether c is a moderator whose admins.json entry
// still exists unchanged; one whose entry was removed or whose password
// changed is signed out of the lobby here (and told so).
func (h *Hub) activeModerator(c *Client) bool {
	active, demoted := c.activeModerator(h.mod)
	if demoted {
		slog.Info("moderator signed out: their admins.json entry is gone or changed", "user", c.user.Username, "ip", c.ip)
		c.notice("warn", "Your moderator sign-in is no longer valid.")
		h.broadcastModPeers()
	}
	return active
}

// audit records a moderator action in the audit trail (and the server log).
func (h *Hub) audit(admin, action, detail, source, ip string) {
	h.modlog.Audit(modlog.AuditEntry{Admin: admin, Action: action, Detail: detail, Source: source, IP: ip})
}

// handleModAction executes a moderator command. Requires an authenticated
// moderator connection. Every change is audited (source "lobby").
func (h *Hub) handleModAction(c *Client, env protocol.Envelope) {
	// Checked against admins.json on every action: a moderator removed from
	// it (or given a new password) loses the lobby's powers here.
	if h.mod == nil || !h.activeModerator(c) {
		c.notice("error", "You are not authorized to do that.")
		return
	}
	var req protocol.ModActionRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	target := strings.TrimSpace(req.Target)
	dur, durErr := parseModDuration(req.Duration)
	if durErr != nil {
		c.notice("error", "Bad duration (use e.g. 10m, 1h, 2d, or leave blank for permanent).")
		return
	}

	switch action {
	case "list":
		bans, mutes := h.mod.ListSanctions()
		c.sendEnvelope(protocol.TypeModList, env.ID, protocol.ModListReply{
			Bans:  toModSanctions(bans),
			Mutes: toModSanctions(mutes),
		})
		return

	case "unban":
		if target == "" {
			c.notice("error", "Usage: /unban <ip>")
			return
		}
		if h.LiftSanction(c.adminName, "unban", target) {
			h.audit(c.adminName, "unban", "Unbanned "+NormalizeIP(target), modlog.SourceLobby, c.ip)
			c.notice("info", "Unbanned "+target+".")
		} else {
			c.notice("warn", "No ban found for "+target+".")
		}
		return

	case "unmute":
		if target == "" {
			c.notice("error", "Usage: /unmute <ip>")
			return
		}
		if h.LiftSanction(c.adminName, "unmute", target) {
			h.audit(c.adminName, "unmute", "Unmuted "+NormalizeIP(target), modlog.SourceLobby, c.ip)
			c.notice("info", "Unmuted "+target+".")
		} else {
			c.notice("warn", "No mute found for "+target+".")
		}
		return

	case "closeroom":
		// Cleanly dissolve a room (and end its match, if one is running) by id.
		// Reuses the host-leave teardown: peers get MATCH_PEER_LEFT so their
		// emulation drops, everyone gets ROOM_LEFT, and broadcast/match records
		// are reaped.
		roomID, err := strconv.ParseUint(target, 10, 64)
		if err != nil || roomID == 0 {
			c.notice("error", "Usage: closeroom <roomId>")
			return
		}
		reason := strings.TrimSpace(req.Reason)
		name, ok := h.closeRoom(roomID, c.adminName, reason)
		if !ok {
			c.notice("warn", "Room not found (already gone?).")
			return
		}
		h.audit(c.adminName, "closeroom", closeRoomDetail(roomID, name, reason), modlog.SourceLobby, c.ip)
		h.modConfirm(c, "Closed room \""+name+"\".")
		return
	}

	// Remaining actions target a connected user by username.
	if target == "" {
		c.notice("error", "Usage: /"+action+" <username>")
		return
	}
	tc := h.findClientByUsername(target)
	if tc == nil {
		c.notice("warn", "User \""+target+"\" is not connected.")
		return
	}
	// Name them as the moderator did: this may be a local player, and whatever
	// happens next reaches the whole connection.
	tname := tc.user.Username
	for _, u := range tc.user.Group() {
		if usernames.Same(u.Username, target) {
			tname = u.Username
		}
	}
	if action == "inspect" {
		var others []string
		for _, u := range tc.user.Group() {
			if u.Username != tname {
				others = append(others, u.Username)
			}
		}
		c.sendEnvelope(protocol.TypeModTargetInfo, env.ID, protocol.ModTargetInfoPayload{
			Target: tname, Others: others, Moderator: h.activeModerator(tc),
		})
		return
	}
	if tc.user.ID == c.user.ID {
		c.notice("warn", "You can't moderate yourself.")
		return
	}
	if h.activeModerator(tc) {
		c.notice("warn", target+" is a moderator.")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	out, err := h.sanction(tc, tname, c.adminName, action, reason, dur)
	switch {
	case errors.Is(err, ErrNeedsDuration):
		c.notice("error", "Timeout needs a duration, e.g. /timeout "+tname+" 10m.")
		return
	case err != nil:
		c.notice("error", "Unknown action: "+action)
		return
	}
	h.audit(c.adminName, action, out.Detail, modlog.SourceLobby, c.ip)
	h.modConfirm(c, out.Confirm)
	h.sendModResult(c, action, out)
}

// Errors from the moderation actions the website calls.
var (
	ErrNotConnected  = errors.New("that user isn't connected")
	ErrModerator     = errors.New("that user is a moderator")
	ErrNeedsDuration = errors.New("a timeout needs a duration")
	ErrUnknownAction = errors.New("unknown action")
)

// ModOutcome is what a moderator action did.
type ModOutcome struct {
	Target  string    // the name the moderator acted on
	IP      string    // the connection's IP, which unban and unmute take
	Until   time.Time // zero = permanent (a kick has none)
	Confirm string    // the confirmation the lobby shows the moderator
	Detail  string    // the audit trail's line
}

// sanction kicks, mutes, times out or bans the connection tc; admin is who
// did it, and tname the name they acted on (a local player's, maybe: the
// whole connection is sanctioned either way). The player hears the same
// messages whether it came from the lobby or the website.
func (h *Hub) sanction(tc *Client, tname, admin, action, reason string, dur time.Duration) (ModOutcome, error) {
	tip := tc.ip
	out := ModOutcome{Target: tname, IP: tip}
	switch action {
	case "kick":
		out.Confirm = "Kicked " + tname + "."
		out.Detail = "Kicked " + tname + " (" + tip + ")" + reasonSuffix(reason)
	case "mute":
		out.Until = timeFromDur(dur)
		out.Confirm = "Muted " + tname + " (" + tip + ")" + untilSuffix(out.Until) + "."
		out.Detail = "Muted " + tname + " (" + tip + ")" + untilSuffix(out.Until) + reasonSuffix(reason)
	case "timeout":
		if dur <= 0 {
			return out, ErrNeedsDuration
		}
		out.Until = timeFromDur(dur)
		out.Confirm = "Timed out " + tname + " (" + tip + ")" + untilSuffix(out.Until) + "."
		out.Detail = "Timed out " + tname + " (" + tip + ")" + untilSuffix(out.Until) + reasonSuffix(reason)
	case "ban":
		out.Confirm = "Banned " + tname + " (" + tip + ")."
		out.Detail = "Banned " + tname + " (" + tip + ")" + reasonSuffix(reason)
	default:
		return out, ErrUnknownAction
	}
	if h.mod == nil {
		return out, ErrUnknownAction
	}
	// the timeline line goes in before the disconnect's own lines
	h.logSanction(tc, admin, out.Detail)
	// A disconnect waits for the client to answer the close, so it runs on
	// its own: a stalled client mustn't hold up the moderator. The ban is in
	// place before it starts, so the player can't come straight back.
	switch action {
	case "kick":
		go tc.disconnect("You were kicked by a moderator" + reasonSuffix(reason) + ".")
	case "mute":
		h.mod.MuteNamed(tip, tname, admin, reason, dur)
		tc.notice("warn", "You were muted by a moderator"+untilSuffix(out.Until)+reasonSuffix(reason)+".")
	case "timeout":
		h.mod.BanNamed(tip, tname, admin, reason, dur)
		go tc.disconnect("You were timed out by a moderator" + untilSuffix(out.Until) + reasonSuffix(reason) + ".")
	case "ban":
		h.mod.BanNamed(tip, tname, admin, reason, 0) // permanent
		go tc.disconnect("You were banned by a moderator" + reasonSuffix(reason) + ".")
	}
	return out, nil
}

// logSanction puts a moderator action on the target's room and game timeline.
func (h *Hub) logSanction(tc *Client, admin, detail string) {
	if h.modlog == nil || tc.user == nil {
		return
	}
	e := modlog.Event{Type: modlog.EventModeration, Name: tc.user.Username, IP: tc.ip, Text: admin + ": " + detail}
	if room := tc.user.CurrentRoom; room != nil {
		e.RoomID = room.ID
		if m := tc.user.CurrentMatch; m != nil {
			e.MatchID = m.ID
		}
	}
	h.modlog.Event(e)
}

// closeRoom dissolves a room (ending its match, if one runs) the way a host
// leaving does: peers get MATCH_PEER_LEFT, everyone ROOM_LEFT with reason
// admin_closed. Reports the room's name, or false when it's gone.
func (h *Hub) closeRoom(roomID uint64, admin, reason string) (string, bool) {
	room := h.state.GetRoom(roomID)
	if room == nil {
		return "", false
	}
	name := room.Name
	h.modlog.Event(modlog.Event{Type: modlog.EventModeration, RoomID: roomID,
		Text: admin + ": " + closeRoomDetail(roomID, name, reason)})
	if room.Host == nil {
		// Hostless rooms shouldn't exist; reap the record directly.
		h.state.RemoveRoom(roomID)
		h.modlog.RoomClosed(roomID, name, modlog.EndModerator, "")
		h.broadcastRoomList()
	} else {
		h.removeUserFromRoom(room.Host, room, "admin_closed")
	}
	return name, true
}

func closeRoomDetail(roomID uint64, name, reason string) string {
	return fmt.Sprintf("Closed room #%d \"%s\"%s", roomID, name, reasonSuffix(reason))
}

// modConfirm sends the acting moderator a confirmation and logs the action.
func (h *Hub) modConfirm(c *Client, text string) {
	c.notice("info", text)
	slog.Info("mod action", "admin", c.adminName, "detail", text)
}

// sendModResult tells the moderator's client what just took effect, so it can
// offer Undo. The IP is what unban and unmute take; a kick has nothing to undo.
func (h *Hub) sendModResult(c *Client, action string, out ModOutcome) {
	ip := out.IP
	if action == "kick" {
		ip = ""
	}
	var until int64
	if !out.Until.IsZero() {
		until = out.Until.UnixMilli()
	}
	c.sendEnvelope(protocol.TypeModResult, "", protocol.ModResultPayload{
		Action: action, Target: out.Target, IP: ip, Until: until,
	})
}

// findClientByUsername returns the connected client with a matching username
// (case-insensitive), or nil. Usernames are unique among live connections. A
// local player's name finds the connection it shares: sanctions apply to the
// whole PC.
func (h *Hub) findClientByUsername(name string) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cl := range h.clients {
		if cl.user == nil {
			continue
		}
		for _, u := range cl.user.Group() {
			if usernames.Same(u.Username, name) {
				return cl
			}
		}
	}
	return nil
}

func toModSanctions(in []moderation.Sanction) []protocol.ModSanction {
	out := make([]protocol.ModSanction, 0, len(in))
	for _, s := range in {
		var until int64
		if !s.Until.IsZero() {
			until = s.Until.UnixMilli()
		}
		out = append(out, protocol.ModSanction{
			IP: s.IP, Name: s.Name, Reason: s.Reason, By: s.By, Until: until,
		})
	}
	return out
}

// parseModDuration accepts "", "0" (=> permanent, 0), standard Go durations
// ("10m", "1h30m", "45s"), and a bare "<n>d" for days.
func parseModDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad day duration: %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad duration: %q", s)
	}
	return d, nil
}

// timeFromDur returns the absolute expiry for a duration (zero => permanent).
func timeFromDur(d time.Duration) time.Time {
	if d <= 0 {
		return time.Time{}
	}
	return time.Now().Add(d)
}

// untilSuffix renders " until <local time>" or " (permanent)".
func untilSuffix(until time.Time) string {
	if until.IsZero() {
		return " (permanent)"
	}
	return " until " + until.Format("2006-01-02 15:04 MST")
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}
