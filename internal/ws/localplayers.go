package ws

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// Local players: several people on one PC. Each is an ordinary lobby user with
// its own name, presence row and seat, except that it has no connection: its
// Owner's connection speaks for it (see state.User). Other players aren't told;
// the only trace is each seat's PeerID, which clients need to route by.
//
// Whatever happens to one player on a PC happens to all of them. They enter a
// room together or not at all (seatUser), and leave, get kicked, drop and join a
// running match together (removeUserFromRoom, handleRoomKick, handleMatchFinished,
// handleMatchAdmit). The roster only changes in the lobby, outside any room.

const (
	maxPlayersPerPC = 4

	// A roster change reaches every connected client through presence, so it
	// gets a flood limit like chat.
	rosterBurst  = 8
	rosterWindow = 10 * time.Second
)

// allowRosterChange records a roster change and reports whether it's within the
// flood limit. Called only from this client's read loop.
func (c *Client) allowRosterChange(now time.Time) bool {
	cutoff := now.Add(-rosterWindow)
	kept := c.rosterTimes[:0]
	for _, t := range c.rosterTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.rosterTimes = kept
	if len(c.rosterTimes) >= rosterBurst {
		return false
	}
	c.rosterTimes = append(c.rosterTimes, now)
	return true
}

// rosterLocked returns why c's roster can't change right now, or "".
func (h *Hub) rosterLocked(c *Client) string {
	if c.user.CurrentRoom != nil {
		return protocol.ReasonInRoom
	}
	if h.matchmaker.contains(c.user.ID) {
		return protocol.ReasonSearching
	}
	return ""
}

func (c *Client) sendLocalPlayerFail(id, name, reason string) {
	c.sendEnvelope(protocol.TypeLocalPlayerFail, id, protocol.LocalPlayerFailPayload{Name: name, Reason: reason})
}

func (h *Hub) handleLocalPlayerAdd(c *Client, env protocol.Envelope) {
	var req protocol.LocalPlayerAddRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		c.sendLocalPlayerFail(env.ID, "", protocol.ReasonInvalidPayload)
		return
	}
	name := strings.TrimSpace(req.Name)
	if reason := h.rosterLocked(c); reason != "" {
		c.sendLocalPlayerFail(env.ID, name, reason)
		return
	}
	if !isValidUsername(name) {
		c.sendLocalPlayerFail(env.ID, name, protocol.ReasonInvalidName)
		return
	}
	// Usernames are broadcast to the whole lobby — same filter as HELLO.
	if h.mod != nil {
		if term, hit := h.mod.FilterMessage(name); hit {
			c.sendLocalPlayerFail(env.ID, name, protocol.ReasonInvalidName)
			slog.Info("local player name blocked by filter", "user", c.user.Username, "ip", c.ip, "term", term)
			return
		}
	}
	if !c.user.LocalPlayers || len(c.user.Group()) >= maxPlayersPerPC {
		c.sendLocalPlayerFail(env.ID, name, protocol.ReasonTooManyPlayers)
		return
	}
	if !c.allowRosterChange(time.Now()) {
		c.sendLocalPlayerFail(env.ID, name, protocol.ReasonTooFast)
		return
	}

	owner := c.user
	local := &state.User{
		ID:            h.state.NextUserID(),
		Username:      name,
		State:         owner.GetState(),
		PublicIP:      owner.PublicIP,
		LocalIP:       owner.LocalIP,
		Region:        owner.Region,
		Country:       owner.Country,
		Anonymous:     owner.Anonymous,
		ClientVersion: owner.ClientVersion,
		Connection:    owner.Connection,
		ICESupported:  owner.ICESupported,
		LateJoin:      owner.LateJoin,
		LocalPlayers:  owner.LocalPlayers,
		Social:        owner.Social,
		PingToServer:  owner.PingToServer,
		JoinedAt:      time.Now(),
		LastSeen:      time.Now(),
	}
	if err := h.state.AddUser(local); err != nil {
		c.sendLocalPlayerFail(env.ID, name, protocol.ReasonNameTaken)
		return
	}
	owner.AddLocal(local)
	slog.Info("local player added", "owner", owner.Username, "name", name, "id", local.ID)

	// The reply first, so the owner's client knows this user is on its PC when
	// the presence update arrives.
	c.sendEnvelope(protocol.TypeLocalPlayerAdded, env.ID, protocol.LocalPlayerAddedPayload{UserID: local.ID, Name: name})
	h.broadcastPresenceAdded(local)
	if c.isModerator {
		h.broadcastModPeers()
	}
}

func (h *Hub) handleLocalPlayerRemove(c *Client, env protocol.Envelope) {
	var req protocol.LocalPlayerRemoveRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		c.sendLocalPlayerFail(env.ID, "", protocol.ReasonInvalidPayload)
		return
	}
	if reason := h.rosterLocked(c); reason != "" {
		c.sendLocalPlayerFail(env.ID, "", reason)
		return
	}
	if !c.allowRosterChange(time.Now()) {
		c.sendLocalPlayerFail(env.ID, "", protocol.ReasonTooFast)
		return
	}
	local := c.user.RemoveLocal(req.UserID)
	if local == nil {
		return
	}
	h.state.RemoveUser(local.ID)
	h.endChallengesTo(local, protocol.ChallengeUnavailable)
	slog.Info("local player removed", "owner", c.user.Username, "name", local.Username, "id", local.ID)
	c.sendEnvelope(protocol.TypeLocalPlayerRemoved, env.ID, protocol.LocalPlayerRemovedPayload{UserID: local.ID})
	h.broadcastPresenceRemoved(local.ID)
	if c.isModerator {
		h.broadcastModPeers()
	}
}

// localPlayersRefusal returns why u's PC can't sit in room, or "". A client that
// doesn't read PeerID would try to reach a local player directly, so such a
// client never shares a room with one.
func localPlayersRefusal(u *state.User, room *state.Room) string {
	group := u.Group()
	for _, seat := range room.Players {
		if seat.User == nil {
			continue
		}
		if seat.User.Owner != nil && !u.LocalPlayers {
			return protocol.ReasonNeedsUpdate
		}
		if len(group) > 1 && !seat.User.LocalPlayers {
			return protocol.ReasonPeerNeedsUpdate
		}
	}
	return ""
}

// setGroupDelay applies a PC's input delay to all of its seats: the local
// players share one rollback session, and with it one delay. It reports whether
// any seat changed.
func setGroupDelay(room *state.Room, u *state.User, delay int, delayAuto bool) bool {
	changed := false
	for _, member := range u.Group() {
		if room.SetPlayerDelay(member.ID, delay, delayAuto) {
			changed = true
		}
	}
	return changed
}
