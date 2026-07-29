package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// maxAuthFails is how many bad ADMIN_AUTH passwords one connection gets
// before it is disconnected.
const maxAuthFails = 5

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
	name, ok := h.mod.AuthenticateAdmin(req.Password)
	if !ok {
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
	c.isModerator = true
	c.adminName = name
	slog.Info("admin authenticated", "admin", name, "user", c.user.Username, "ip", c.ip)
	c.sendEnvelope(protocol.TypeAdminAuthOK, env.ID, protocol.AdminAuthOKResponse{Name: name})
}

// handleModAction executes a moderator command. Requires an authenticated
// moderator connection.
func (h *Hub) handleModAction(c *Client, env protocol.Envelope) {
	if !c.isModerator || h.mod == nil {
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
		if h.mod.Unban(target, c.adminName) {
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
		if h.mod.Unmute(target, c.adminName) {
			c.notice("info", "Unmuted "+target+".")
		} else {
			c.notice("warn", "No mute found for "+target+".")
		}
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
	if tc.user.ID == c.user.ID {
		c.notice("warn", "You can't moderate yourself.")
		return
	}
	if tc.isModerator {
		c.notice("warn", target+" is a moderator.")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	tname, tip := tc.user.Username, tc.ip

	switch action {
	case "kick":
		tc.disconnect("You were kicked by a moderator" + reasonSuffix(reason) + ".")
		h.modConfirm(c, "Kicked "+tname+".")

	case "mute":
		h.mod.Mute(tip, c.adminName, reason, dur)
		tc.notice("warn", "You were muted by a moderator"+untilSuffix(timeFromDur(dur))+reasonSuffix(reason)+".")
		h.modConfirm(c, "Muted "+tname+" ("+tip+")"+untilSuffix(timeFromDur(dur))+".")

	case "timeout":
		if dur <= 0 {
			c.notice("error", "Timeout needs a duration, e.g. /timeout "+tname+" 10m.")
			return
		}
		h.mod.Ban(tip, c.adminName, reason, dur)
		tc.disconnect("You were timed out by a moderator" + untilSuffix(timeFromDur(dur)) + reasonSuffix(reason) + ".")
		h.modConfirm(c, "Timed out "+tname+" ("+tip+")"+untilSuffix(timeFromDur(dur))+".")

	case "ban":
		h.mod.Ban(tip, c.adminName, reason, 0) // permanent
		tc.disconnect("You were banned by a moderator" + reasonSuffix(reason) + ".")
		h.modConfirm(c, "Banned "+tname+" ("+tip+").")

	default:
		c.notice("error", "Unknown action: "+action)
	}
}

// modConfirm sends the acting moderator a confirmation and logs the action.
func (h *Hub) modConfirm(c *Client, text string) {
	c.notice("info", text)
	slog.Info("mod action", "admin", c.adminName, "detail", text)
}

// findClientByUsername returns the connected client with a matching username
// (case-insensitive), or nil. Usernames are unique among live connections.
func (h *Hub) findClientByUsername(name string) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cl := range h.clients {
		if cl.user != nil && strings.EqualFold(cl.user.Username, name) {
			return cl
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
			IP: s.IP, Reason: s.Reason, By: s.By, Until: until,
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
