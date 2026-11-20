package ws

import (
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// What the moderator website (internal/modsite) needs from the running lobby:
// who's online, and the same moderation actions as MOD_ACTION. These run on
// the web server's goroutines, so they take h.mu only to copy the client
// list and never hold it while sending or disconnecting. Nothing here sends
// a client anything a MOD_ACTION wouldn't.

// LiveUser is one connection.
type LiveUser struct {
	ID        uint64
	Username  string
	IP        string
	Moderator bool
	Version   string
	Connected time.Time
	Status    string // lobby, room, playing, spectating
	RoomID    uint64
	RoomName  string
	Country   string
	Muted     *moderation.Sanction
}

// LiveRoom is one room.
type LiveRoom struct {
	ID         uint64
	Name       string
	Game       string
	Host       string
	Players    []string
	Spectators int
	InMatch    bool
	MatchID    uint64
	QuickMatch bool
	Locked     bool
	Created    time.Time
}

// LiveSnapshot is the lobby right now.
type LiveSnapshot struct {
	Users []LiveUser
	Rooms []LiveRoom
	Bans  int
	Mutes int
}

// LiveRef names a connection.
type LiveRef struct {
	ID       uint64
	Username string
}

func (h *Hub) clientList() []*Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Client, 0, len(h.clients))
	for _, c := range h.clients {
		if c.user != nil {
			out = append(out, c)
		}
	}
	return out
}

func liveStatus(u *state.User) string {
	switch {
	case u.CurrentMatch != nil:
		return "playing"
	case u.CurrentRoom != nil:
		return "room"
	case u.GetState() == state.UserStateSpectating:
		return "spectating"
	}
	return "lobby"
}

// LiveSnapshot lists every connection and room.
func (h *Hub) LiveSnapshot() LiveSnapshot {
	var snap LiveSnapshot
	for _, c := range h.clientList() {
		u := c.user
		isMod := h.activeModerator(c)
		lu := LiveUser{
			ID: u.ID, Username: u.Username, IP: c.ip, Moderator: isMod, Version: u.ClientVersion,
			Connected: u.JoinedAt, Status: liveStatus(u), Country: knownCountry(u.Country),
		}
		if room := u.CurrentRoom; room != nil {
			lu.RoomID, lu.RoomName = room.ID, room.Name
		}
		if h.mod != nil {
			if s, muted := h.mod.IsMuted(c.ip); muted {
				lu.Muted = &s
			}
		}
		snap.Users = append(snap.Users, lu)
	}
	sort.Slice(snap.Users, func(i, j int) bool {
		a, b := strings.ToLower(snap.Users[i].Username), strings.ToLower(snap.Users[j].Username)
		return a < b || (a == b && snap.Users[i].ID < snap.Users[j].ID)
	})
	for _, r := range h.state.ListRooms() {
		id, _, name, _, _, st := r.Snapshot()
		lr := LiveRoom{ID: id, Name: name, Game: r.RomName, Players: r.PlayerUsernames(),
			InMatch: st == state.RoomStateInGame, QuickMatch: r.QuickMatch, Locked: r.Password != "",
			Created: r.CreatedAt}
		if host := r.Host; host != nil {
			lr.Host = host.Username
		}
		if m := roomMatch(r); m != nil {
			lr.MatchID = m.ID
		}
		if h.broadcasts != nil {
			_, _, lr.Spectators = h.broadcasts.roomBroadcast(id)
		}
		snap.Rooms = append(snap.Rooms, lr)
	}
	sort.Slice(snap.Rooms, func(i, j int) bool { return snap.Rooms[i].ID < snap.Rooms[j].ID })
	if h.mod != nil {
		bans, mutes := h.mod.ListSanctions()
		snap.Bans, snap.Mutes = len(bans), len(mutes)
	}
	return snap
}

// OnlineCount is how many connections are open.
func (h *Hub) OnlineCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// IsOnline reports whether someone has this name now.
func (h *Hub) IsOnline(name string) bool {
	return h.findClientByUsername(name) != nil
}

// OnlineAt lists the connections from an IP.
func (h *Hub) OnlineAt(ip string) []LiveRef {
	want := NormalizeIP(ip)
	out := []LiveRef{}
	for _, c := range h.clientList() {
		if NormalizeIP(c.ip) == want {
			out = append(out, LiveRef{ID: c.user.ID, Username: c.user.Username})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Sanctions lists the bans and mutes in effect.
func (h *Hub) Sanctions() (bans, mutes []moderation.Sanction) {
	if h.mod == nil {
		return nil, nil
	}
	return h.mod.ListSanctions()
}

// SanctionsOn returns the ban and mute on an IP, nil for none.
func (h *Hub) SanctionsOn(ip string) (ban, mute *moderation.Sanction) {
	if h.mod == nil {
		return nil, nil
	}
	for _, key := range ipKeys(ip) {
		if s, ok := h.mod.IsBanned(key); ok && ban == nil {
			ban = &s
		}
		if s, ok := h.mod.IsMuted(key); ok && mute == nil {
			mute = &s
		}
	}
	return ban, mute
}

// knownCountry is a country code for the moderators, "" when the IP data
// doesn't know (it says ZZ).
func knownCountry(cc string) string {
	if len(cc) != 2 || cc == "ZZ" {
		return ""
	}
	return cc
}

// NormalizeIP is ip as the lobby writes it (IPv4-mapped IPv6 as IPv4, IPv6
// in its short lowercase form), or ip trimmed when it doesn't parse.
func NormalizeIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if parsed := net.ParseIP(ip); parsed != nil {
		return parsed.String()
	}
	return ip
}

// ipKeys is ip as given and as the lobby writes it: a sanction typed in
// another spelling (2001:DB8::1) is still found.
func ipKeys(ip string) []string {
	ip = strings.TrimSpace(ip)
	if n := NormalizeIP(ip); n != ip {
		return []string{n, ip}
	}
	return []string{ip}
}

// ParseModDuration reads a moderator's duration: "" or "0" (none), Go
// durations like "10m" or "1h30m", or "<n>d" for days.
func ParseModDuration(s string) (time.Duration, error) { return parseModDuration(s) }

// clientOfUser finds the connection of a live user id.
func (h *Hub) clientOfUser(userID uint64) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.clients[userID]; c != nil && c.user != nil {
		return c
	}
	return nil
}

// ModerateUser kicks, mutes, times out or bans the connection of a live user
// id, as MOD_ACTION does. ErrNotConnected when they're gone, ErrModerator
// when the connection is a signed-in moderator's, ErrNeedsDuration for a
// timeout without one. The caller audits the outcome.
func (h *Hub) ModerateUser(admin string, userID uint64, action string, dur time.Duration, reason string) (ModOutcome, error) {
	if h.mod == nil {
		return ModOutcome{}, ErrUnknownAction
	}
	tc := h.clientOfUser(userID)
	if tc == nil {
		return ModOutcome{}, ErrNotConnected
	}
	if h.activeModerator(tc) {
		return ModOutcome{Target: tc.user.Username, IP: tc.ip}, ErrModerator
	}
	return h.sanction(tc, tc.user.Username, admin, action, strings.TrimSpace(reason), dur)
}

// CloseRoom dissolves a room, as MOD_ACTION closeroom does; ok is false when
// the room is gone. The caller audits it.
func (h *Hub) CloseRoom(admin string, roomID uint64, reason string) (string, bool) {
	// Take h.mu first, as the other entry points here do (clientList), so
	// this goroutine sees what read loops wrote before their last broadcast.
	h.mu.Lock()
	h.mu.Unlock() //nolint:staticcheck // a barrier, not a critical section
	return h.closeRoom(roomID, admin, strings.TrimSpace(reason))
}

// BanIP bans an IP, online or not (dur 0 = permanent, otherwise a timeout),
// and disconnects every connection from it with the usual message, except a
// signed-in moderator's. Returns how many were disconnected. The caller
// audits it.
func (h *Hub) BanIP(admin, ip, name string, dur time.Duration, reason string) int {
	if h.mod == nil {
		return 0
	}
	ip, reason = NormalizeIP(ip), strings.TrimSpace(reason)
	targets := h.sanctionTargets(ip)
	if name == "" && len(targets) > 0 {
		name = targets[0].user.Username
	}
	h.mod.BanNamed(ip, name, admin, reason, dur)
	until := timeFromDur(dur)
	message := "You were banned by a moderator" + reasonSuffix(reason) + "."
	detail := "Banned " + ip
	if dur > 0 {
		message = "You were timed out by a moderator" + untilSuffix(until) + reasonSuffix(reason) + "."
		detail = "Timed out " + ip + untilSuffix(until)
	}
	h.modlog.Event(modlog.Event{Type: modlog.EventModeration, Name: name, IP: ip, Text: admin + ": " + detail + reasonSuffix(reason)})
	// each close waits for its client's answer, so none holds up the others
	for _, tc := range targets {
		h.logSanction(tc, admin, detail+reasonSuffix(reason))
		go tc.disconnect(message)
	}
	return len(targets)
}

// MuteIP mutes an IP, online or not (dur 0 = until unmuted), and tells every
// connection from it. Returns how many were told. The caller audits it.
func (h *Hub) MuteIP(admin, ip, name string, dur time.Duration, reason string) int {
	if h.mod == nil {
		return 0
	}
	ip, reason = NormalizeIP(ip), strings.TrimSpace(reason)
	targets := h.sanctionTargets(ip)
	if name == "" && len(targets) > 0 {
		name = targets[0].user.Username
	}
	h.mod.MuteNamed(ip, name, admin, reason, dur)
	until := timeFromDur(dur)
	detail := "Muted " + ip + untilSuffix(until)
	h.modlog.Event(modlog.Event{Type: modlog.EventModeration, Name: name, IP: ip, Text: admin + ": " + detail + reasonSuffix(reason)})
	for _, tc := range targets {
		tc.notice("warn", "You were muted by a moderator"+untilSuffix(until)+reasonSuffix(reason)+".")
	}
	return len(targets)
}

// ModeratorsAt names the signed-in lobby moderators (whose admins.json entry
// is still valid) connected from ip, so the site can ask before banning it.
func (h *Hub) ModeratorsAt(ip string) []string {
	want := NormalizeIP(ip)
	var out []string
	for _, c := range h.clientList() {
		if NormalizeIP(c.ip) != want {
			continue
		}
		if active, _ := c.activeModerator(h.mod); active {
			out = append(out, c.user.Username)
		}
	}
	sort.Strings(out)
	return out
}

// UserIP is the address a live user id connects from; ok is false when the
// user isn't connected.
func (h *Hub) UserIP(userID uint64) (string, bool) {
	if c := h.clientOfUser(userID); c != nil {
		return NormalizeIP(c.ip), true
	}
	return "", false
}

// sanctionTargets are the connections from ip a sanction reaches (not a
// signed-in moderator's).
func (h *Hub) sanctionTargets(ip string) []*Client {
	var out []*Client
	for _, c := range h.clientList() {
		if NormalizeIP(c.ip) != ip {
			continue
		}
		if h.activeModerator(c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// LiftSanction removes an IP's ban ("unban") or mute ("unmute") and reports
// whether there was one. The caller audits it.
func (h *Hub) LiftSanction(admin, action, ip string) bool {
	if h.mod == nil {
		return false
	}
	for _, key := range ipKeys(ip) {
		var ok bool
		verb := "Unbanned "
		if action == "unban" {
			ok = h.mod.Unban(key, admin)
		} else {
			ok = h.mod.Unmute(key, admin)
			verb = "Unmuted "
		}
		if ok {
			h.modlog.Event(modlog.Event{Type: modlog.EventModeration, IP: key, Text: admin + ": " + verb + key})
			if action == "unban" && h.actions != nil {
				// an early unban: the next run of refusals may ban again
				h.actions.ClearBan(moderation.CanonicalIP(key))
			}
			return true
		}
	}
	return false
}
