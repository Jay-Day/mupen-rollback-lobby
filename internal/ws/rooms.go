package ws

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	roomNameMaxLen = 48
	minDelay       = 0
	maxDelay       = 9
	minPrediction  = 0
	maxPrediction  = 9
	minPacing      = 0 // 0 = aggressive/symmetric (engine default)
	maxPacing      = 1 // 1 = smooth/asymmetric
	minRoomPlayers = 2
	maxRoomPlayers = 4
)

func (h *Hub) handleRoomCreate(c *Client, env protocol.Envelope) {
	var req protocol.RoomCreateRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		c.sendRoomCreateFail(env.ID, protocol.ReasonInvalidPayload)
		return
	}

	name := strings.TrimSpace(req.Name)
	rom := strings.TrimSpace(req.Rom.Name)
	if name == "" || rom == "" {
		c.sendRoomCreateFail(env.ID, protocol.ReasonInvalidPayload)
		return
	}
	if len(name) > roomNameMaxLen {
		name = name[:roomNameMaxLen]
	}
	// Room names show up in every client's room list — filter them like chat.
	if h.mod != nil {
		if term, hit := h.mod.FilterMessage(name); hit {
			c.sendRoomCreateFail(env.ID, protocol.ReasonInvalidPayload)
			slog.Info("room name blocked by filter", "user", c.user.Username, "ip", c.ip, "term", term)
			return
		}
	}
	if req.MaxPlayers < minRoomPlayers {
		req.MaxPlayers = minRoomPlayers
	} else if req.MaxPlayers > maxRoomPlayers {
		req.MaxPlayers = maxRoomPlayers
	}
	if req.Delay < minDelay {
		req.Delay = minDelay
	} else if req.Delay > maxDelay {
		req.Delay = maxDelay
	}
	if req.Prediction < minPrediction {
		req.Prediction = minPrediction
	} else if req.Prediction > maxPrediction {
		req.Prediction = maxPrediction
	}
	if req.Pacing < minPacing {
		req.Pacing = minPacing
	} else if req.Pacing > maxPacing {
		req.Pacing = maxPacing
	}

	// User cannot create a second room while already in one.
	if c.user.CurrentRoom != nil {
		c.sendRoomCreateFail(env.ID, protocol.ReasonAlreadyInRoom)
		return
	}

	visibility := state.VisibilityPublic
	if req.Visibility == "invite_only" {
		visibility = state.VisibilityInviteOnly
	}

	room := &state.Room{
		ID:         h.state.NextRoomID(),
		Name:       name,
		Host:       c.user,
		RomName:    rom,
		RomMD5:     req.Rom.MD5,
		RomRegion:  req.Rom.Region,
		MaxPlayers: req.MaxPlayers,
		Delay:      req.Delay,
		Prediction: req.Prediction,
		Pacing:     req.Pacing,
		// New rooms open on Auto — the in-room combos default to it and Create
		// Room no longer surfaces these, so this matches what the host sees.
		DelayAuto:      true,
		PredictionAuto: true,
		Visibility:     visibility,
		Password:       req.Password,
		Players:        nil,
		State:          state.RoomStateWaiting,
		CreatedAt:      time.Now(),
	}
	_ = room.AddPlayer(c.user)

	h.state.AddRoom(room)
	c.user.CurrentRoom = room
	c.user.SetState(state.UserStateHosting)

	slog.Info("room created",
		"id", room.ID, "host", c.user.Username, "rom", room.RomName,
		"max", room.MaxPlayers, "delay", room.Delay, "pred", room.Prediction)

	// 1) Tell the host
	c.sendEnvelope(protocol.TypeRoomCreated, env.ID, protocol.RoomCreatedResponse{RoomID: room.ID})
	c.sendEnvelope(protocol.TypeRoomState, "", roomStatePayload(room))

	// 2) Broadcast the new room list and the host's state change
	h.broadcastRoomList()
	h.broadcastPresenceUpdated(c.user)
}

func (h *Hub) handleRoomLeave(c *Client, env protocol.Envelope) {
	_ = env
	room := c.user.CurrentRoom
	if room == nil {
		// Client thinks it's in a room but server already cleared the state
		// (likely because the host disconnected and the room dissolved before
		// the ROOM_LEFT push reached this client). Send ROOM_LEFT anyway so
		// the UI un-sticks.
		slog.Info("ROOM_LEAVE with no current room — sending fallback ROOM_LEFT",
			"user", c.user.Username)
		sendRoomLeft(c.user, "left")
		return
	}
	h.removeUserFromRoom(c.user, room, "left")
}

func (h *Hub) handleRoomJoin(c *Client, env protocol.Envelope) {
	var req protocol.RoomJoinRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		c.sendRoomJoinFail(env.ID, protocol.ReasonInvalidPayload)
		return
	}

	if c.user.CurrentRoom != nil {
		c.sendRoomJoinFail(env.ID, protocol.ReasonAlreadyInRoom)
		return
	}

	room := h.state.GetRoom(req.RoomID)
	if room == nil {
		c.sendRoomJoinFail(env.ID, protocol.ReasonRoomNotFound)
		return
	}

	// A kicked player can't rejoin the same room for a short cooldown.
	const kickRejoinCooldown = 30 * time.Second
	if room.KickCoolingDown(c.user.ID, kickRejoinCooldown) {
		c.sendRoomJoinFail(env.ID, protocol.ReasonKickedRecently)
		return
	}

	if room.State != state.RoomStateWaiting {
		c.sendRoomJoinFail(env.ID, protocol.ReasonAlreadyStarted)
		return
	}
	if room.Password != "" && req.Password != room.Password {
		c.sendRoomJoinFail(env.ID, protocol.ReasonWrongPassword)
		return
	}
	if err := room.AddPlayer(c.user); err != nil {
		// AddPlayer only returns ErrRoomFull right now.
		c.sendRoomJoinFail(env.ID, protocol.ReasonRoomFull)
		return
	}

	c.user.CurrentRoom = room
	c.user.SetState(state.UserStateInRoom)

	slog.Info("room joined",
		"id", room.ID, "user", c.user.Username,
		"slot", len(room.Players), "of", room.MaxPlayers)

	// Tell the joiner first so their UI flips into the room view.
	c.sendEnvelope(protocol.TypeRoomJoinOK, env.ID, roomStatePayload(room))

	// Broadcast updated room state to everyone seated (including the joiner —
	// keeps the joiner's view consistent with what other members see).
	h.broadcastRoomState(room)

	// Update the rooms list seen by everyone else (player count changed).
	h.broadcastRoomList()

	// And the joiner's presence state.
	h.broadcastPresenceUpdated(c.user)
}

func (c *Client) sendRoomJoinFail(id, reason string) {
	c.sendEnvelope(protocol.TypeRoomJoinFail, id, protocol.RoomJoinFailPayload{Reason: reason})
}

// handleRoomStart is invoked by the host to begin the match.
// Validates: caller is host, room is waiting, ≥2 players seated.
func (h *Hub) handleRoomStart(c *Client, env protocol.Envelope) {
	_ = env
	room := c.user.CurrentRoom
	if room == nil {
		slog.Debug("ROOM_START ignored: not in a room", "user", c.user.Username)
		return
	}
	if room.Host == nil || room.Host.ID != c.user.ID {
		slog.Debug("ROOM_START rejected: not host", "user", c.user.Username, "room", room.ID)
		return
	}
	if room.State != state.RoomStateWaiting {
		slog.Debug("ROOM_START rejected: state not waiting", "state", room.State, "room", room.ID)
		return
	}
	if len(room.Players) < 2 {
		slog.Debug("ROOM_START rejected: need at least 2 players", "room", room.ID, "count", len(room.Players))
		return
	}

	h.startMatchInRoom(room, c.user.Username)
}

// handleRoomUpdateSettings lets the host tune the room's rollback delay /
// prediction while it's still waiting. Quick Match relies on this: the host's
// client resolves Auto delay from the measured peer ping during the warmup and
// pushes the concrete value here, so every seated client (and the match itself)
// uses it. Validates: caller is host, room is waiting.
func (h *Hub) handleRoomUpdateSettings(c *Client, env protocol.Envelope) {
	var req protocol.RoomUpdateSettingsRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	room := c.user.CurrentRoom
	if room == nil {
		return
	}
	if room.Host == nil || room.Host.ID != c.user.ID {
		slog.Debug("ROOM_UPDATE_SETTINGS rejected: not host", "user", c.user.Username, "room", room.ID)
		return
	}
	if room.State != state.RoomStateWaiting {
		slog.Debug("ROOM_UPDATE_SETTINGS rejected: not waiting", "room", room.ID, "state", room.State)
		return
	}

	if req.Delay < minDelay {
		req.Delay = minDelay
	} else if req.Delay > maxDelay {
		req.Delay = maxDelay
	}
	if req.Prediction < minPrediction {
		req.Prediction = minPrediction
	} else if req.Prediction > maxPrediction {
		req.Prediction = maxPrediction
	}
	if req.Pacing < minPacing {
		req.Pacing = minPacing
	} else if req.Pacing > maxPacing {
		req.Pacing = maxPacing
	}

	if room.Delay == req.Delay && room.Prediction == req.Prediction &&
		room.Pacing == req.Pacing &&
		room.DelayAuto == req.DelayAuto && room.PredictionAuto == req.PredictionAuto {
		return // no change — skip the rebroadcast
	}
	room.SetRollbackSettings(req.Delay, req.Prediction, req.Pacing, req.DelayAuto, req.PredictionAuto)
	slog.Info("room settings updated", "room", room.ID,
		"delay", req.Delay, "pred", req.Prediction, "pacing", req.Pacing,
		"delayAuto", req.DelayAuto, "predAuto", req.PredictionAuto, "by", c.user.Username)

	h.broadcastRoomState(room)
}

// handleRoomSwapSlots lets the host re-order players by exchanging two seats
// (P1-P4). Host-only and only while the room is waiting. Either slot may be
// empty (the occupied player just moves to it). Rebroadcasts ROOM_STATE.
func (h *Hub) handleRoomSwapSlots(c *Client, env protocol.Envelope) {
	var req protocol.RoomSwapSlotsRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	room := c.user.CurrentRoom
	if room == nil {
		return
	}
	if room.Host == nil || room.Host.ID != c.user.ID {
		slog.Debug("ROOM_SWAP_SLOTS rejected: not host", "user", c.user.Username, "room", room.ID)
		return
	}
	if room.State != state.RoomStateWaiting {
		slog.Debug("ROOM_SWAP_SLOTS rejected: not waiting", "room", room.ID, "state", room.State)
		return
	}
	if req.SlotA < 1 || req.SlotA > room.MaxPlayers ||
		req.SlotB < 1 || req.SlotB > room.MaxPlayers || req.SlotA == req.SlotB {
		slog.Debug("ROOM_SWAP_SLOTS rejected: invalid slots", "room", room.ID, "a", req.SlotA, "b", req.SlotB)
		return
	}
	if !room.SwapSlots(req.SlotA, req.SlotB) {
		return // both slots empty — nothing changed, skip the rebroadcast
	}
	slog.Info("room slots swapped", "room", room.ID, "a", req.SlotA, "b", req.SlotB, "by", c.user.Username)
	h.broadcastRoomState(room)
}

// handleRoomKick lets the host remove another player from the room (and the
// in-progress match, if any). Reuses removeUserFromRoom, so the target gets a
// ROOM_LEFT{reason:"kicked"} and remaining players get MATCH_PEER_LEFT.
func (h *Hub) handleRoomKick(c *Client, env protocol.Envelope) {
	var req protocol.RoomKickRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	room := c.user.CurrentRoom
	if room == nil {
		return
	}
	if room.Host == nil || room.Host.ID != c.user.ID {
		slog.Debug("ROOM_KICK rejected: not host", "user", c.user.Username, "room", room.ID)
		return
	}
	if req.UserID == c.user.ID {
		return // host can't kick themselves
	}
	target := h.state.GetUser(req.UserID)
	if target == nil || target.CurrentRoom == nil || target.CurrentRoom.ID != room.ID {
		slog.Debug("ROOM_KICK rejected: target not in this room",
			"host", c.user.Username, "target", req.UserID, "room", room.ID)
		return
	}
	slog.Info("room kick", "host", c.user.Username, "target", target.Username, "room", room.ID)
	room.MarkKicked(target.ID) // start their rejoin cooldown
	h.removeUserFromRoom(target, room, "kicked")
}

// startMatchInRoom is the shared match-creation path used by both the
// host-driven ROOM_START flow and the auto-started Quick Match flow.
// Callers are responsible for any validation that should happen first.
func (h *Hub) startMatchInRoom(room *state.Room, initiator string) {
	match := &state.Match{
		ID:        h.state.NextMatchID(),
		RoomID:    room.ID,
		StartedAt: time.Now(),
		State:     state.MatchStateConnecting,
	}

	peers := make([]protocol.MatchPeer, 0, len(room.Players))
	for _, seat := range room.Players {
		if seat.User == nil {
			continue
		}
		match.Players = append(match.Players, seat.User)
		seat.User.CurrentMatch = match
		seat.User.SetState(state.UserStatePlaying)

		ip, port := seat.User.GetUDPEndpoint()
		localIP, localPort := seat.User.GetLocalEndpoint()
		peers = append(peers, protocol.MatchPeer{
			UserID:     seat.User.ID,
			Username:   seat.User.Username,
			PublicIP:   ip,
			PublicPort: port,
			LocalIP:    localIP,
			LocalPort:  localPort,
			Slot:       seat.Slot,
		})
	}

	h.state.AddMatch(match)
	room.MarkStarted()
	room.SetState(state.RoomStateInGame)

	// Push the new room state first so clients know we've left "waiting".
	h.broadcastRoomState(room)

	payload := protocol.MatchBeginPayload{
		MatchID: match.ID,
		Peers:   peers,
	}
	h.broadcastToRoom(room, protocol.TypeMatchBegin, payload)

	// Update the rooms list — room is no longer joinable.
	h.broadcastRoomList()

	// Presence updates so other users see "playing" instead of "in_room".
	for _, u := range match.Players {
		h.broadcastPresenceUpdated(u)
	}

	slog.Info("match started",
		"id", match.ID, "room", room.ID, "players", len(peers),
		"initiator", initiator)
}

// removeUserFromRoom handles the bookkeeping for a user leaving a room
// (by ROOM_LEAVE, by disconnect, or by host dissolving). reason is informational
// for log lines and propagated when notifying peers of in-game departures.
func (h *Hub) removeUserFromRoom(u *state.User, room *state.Room, reason string) {
	// If the room is mid-match, also notify peers via MATCH_PEER_LEFT so they
	// can drop emulation. Do this BEFORE we remove the player from the seat list.
	var matchID uint64
	if room.State == state.RoomStateInGame && u.CurrentMatch != nil {
		matchID = u.CurrentMatch.ID
		h.broadcastMatchPeerLeft(room, u, matchID, reason)
	}

	room.RemovePlayer(u.ID)
	u.CurrentRoom = nil
	u.CurrentMatch = nil
	u.SetState(state.UserStateIdle)

	// Dissolve only when the host leaves — the host owns the room. Any
	// non-host drop leaves the room intact regardless of remaining seat count;
	// the surviving players (even just the host alone) keep playing and the
	// host can end the room themselves when they're done. GekkoNet feeds zero
	// input for the dropped actor(s) so the abandoned slots simply go idle.
	dissolve := room.Host != nil && room.Host.ID == u.ID
	if dissolve {
		slog.Info("dissolving room",
			"id", room.ID, "trigger", u.Username, "reason", reason,
			"remaining_seats", len(room.Players))
		for _, seat := range room.Players {
			if seat.User == nil || seat.User.ID == u.ID {
				continue // skip the user who triggered the dissolve
			}
			seat.User.CurrentRoom = nil
			seat.User.CurrentMatch = nil
			seat.User.SetState(state.UserStateIdle)
			h.broadcastPresenceUpdated(seat.User)
			sendRoomLeft(seat.User, "host_left")
		}
		h.state.RemoveRoom(room.ID)
	} else {
		// Otherwise the room still exists; push updated state to remaining members.
		h.broadcastRoomState(room)
	}

	// End any broadcast tied to the match this user just left. A host-dissolve
	// ends it for everyone; otherwise it only ends if this user was the one
	// broadcasting (the stream dies with them).
	if matchID != 0 {
		if dissolve {
			h.broadcasts.endByMatch(matchID)
		} else {
			h.broadcasts.endIfBroadcaster(matchID, u.ID)
		}
	}

	h.broadcastRoomList()
	h.broadcastPresenceUpdated(u)

	// Always tell the leaving user so their client can dismiss the room UI.
	// In the dissolve loop above we explicitly skipped u; in the non-dissolve
	// branch nobody sent to u. Either way, u needs the ROOM_LEFT envelope.
	// For host-disconnect cleanup, u.Send may be on a dying queue — the
	// non-blocking select inside sendRoomLeft makes that harmless.
	sendRoomLeft(u, reason)
}

// sendRoomLeft pushes a ROOM_LEFT envelope to one user. reason lets the client
// distinguish a self-leave from being kicked or the host dissolving the room.
func sendRoomLeft(u *state.User, reason string) {
	if u == nil {
		return
	}
	if u.Send == nil {
		slog.Warn("sendRoomLeft: user has no send channel", "user", u.Username, "id", u.ID)
		return
	}
	env, err := json.Marshal(protocol.Envelope{
		Type: protocol.TypeRoomLeft,
		Data: mustMarshal(protocol.RoomLeftPayload{Reason: reason}),
	})
	if err != nil {
		return
	}
	select {
	case u.Send <- env:
		slog.Info("sendRoomLeft delivered", "user", u.Username, "id", u.ID)
	default:
		slog.Warn("sendRoomLeft dropped (channel full)", "user", u.Username, "id", u.ID)
	}
}

// broadcastMatchPeerLeft notifies every player in the room EXCEPT departing
// that the named user has left their active match.
func (h *Hub) broadcastMatchPeerLeft(room *state.Room, departing *state.User, matchID uint64, reason string) {
	// Look up the departing user's seat before the caller removes them.
	// Slot is informational for clients ("P3 dropped — controller goes idle").
	departingSlot := 0
	for _, seat := range room.Players {
		if seat.User != nil && seat.User.ID == departing.ID {
			departingSlot = seat.Slot
			break
		}
	}
	payload := protocol.MatchPeerLeftPayload{
		MatchID:  matchID,
		UserID:   departing.ID,
		Username: departing.Username,
		Reason:   reason,
		Slot:     departingSlot,
	}
	envBytes, err := json.Marshal(protocol.Envelope{
		Type: protocol.TypeMatchPeerLeft,
		Data: mustMarshal(payload),
	})
	if err != nil {
		return
	}
	for _, seat := range room.Players {
		if seat.User == nil || seat.User.ID == departing.ID || seat.User.Send == nil {
			continue
		}
		select {
		case seat.User.Send <- envBytes:
		default:
		}
	}

	// Also announce the drop as a chat line attributed to the departing player so
	// the whole room (and the in-game overlay) sees who left, in their own voice.
	// Skip kicks — those carry their own host-removal notice, and a kicked player
	// didn't choose to drop. This is the single chokepoint every drop funnels
	// through (Drop Game, Leave mid-match, window-close, disconnect), so it fires
	// once per drop regardless of how the player left.
	if reason != "kicked" {
		h.sendRoomChatAs(room, departing, "I dropped from the game.")
	}
}

// sendRoomChatAs pushes a chat line to a room attributed to a specific user, so
// it renders exactly like a message that user typed.
func (h *Hub) sendRoomChatAs(room *state.Room, u *state.User, message string) {
	h.broadcastToRoom(room, protocol.TypeChatMsg, protocol.ChatMsgPayload{
		Channel:      channelRoom,
		FromUserID:   u.ID,
		FromUsername: u.Username,
		Message:      message,
		ServerTime:   nowMs(),
	})
}

// handleMatchFinished is the client's "drop" report — user stops playing but
// stays seated in the room. Behavior depends on who dropped:
//
//   - Non-host drop: just clear their match association and tell the other
//     seats via MATCH_PEER_LEFT. The match continues for everyone else.
//   - Host drop: end the match for everyone. Room reverts to waiting, every
//     seat's CurrentMatch is cleared. Surviving clients see the ROOM_STATE
//     flip and tear down their own emulation locally. Nobody leaves the room.
//
// Dropping the room itself happens via ROOM_LEAVE, not MATCH_FINISHED.
func (h *Hub) handleMatchFinished(c *Client, env protocol.Envelope) {
	_ = env
	room := c.user.CurrentRoom
	match := c.user.CurrentMatch
	if room == nil || match == nil {
		// Second-call idempotency (notifyEmulationFinished follows the
		// click-side reportMatchFinished and re-fires once emu actually stops).
		return
	}

	isHost := room.Host != nil && room.Host.ID == c.user.ID
	slog.Info("match drop", "match", match.ID, "user", c.user.Username, "room", room.ID, "host", isHost)

	// Tell the other room members the dropper is out. Informational only —
	// the dropper's seat row stays in the player list.
	h.broadcastMatchPeerLeft(room, c.user, match.ID, "dropped")

	// Dropper exits the match but stays seated.
	c.user.CurrentMatch = nil
	c.user.SetState(state.UserStateInRoom)
	h.broadcastPresenceUpdated(c.user)

	// If the dropper was the one broadcasting, the stream dies with them.
	if h.broadcasts.endIfBroadcaster(match.ID, c.user.ID) {
		h.broadcastRoomList()
	}

	if !isHost {
		// Non-host drop: match keeps running for the rest of the seats.
		return
	}

	// Host drop ends the match. Room reverts to waiting; everyone stays
	// seated. Each surviving client's onRoomStateChanged detects the
	// in_game → waiting transition and tears down their own emulation.
	match.SetState(state.MatchStateFinished)
	h.state.RemoveMatch(match.ID)
	h.broadcasts.endByMatch(match.ID) // host ended the match → stop any broadcast
	room.SetState(state.RoomStateWaiting)
	for _, seat := range room.Players {
		if seat.User == nil || seat.User.ID == c.user.ID {
			continue
		}
		seat.User.CurrentMatch = nil
		seat.User.SetState(state.UserStateInRoom)
		h.broadcastPresenceUpdated(seat.User)
	}
	h.broadcastRoomState(room)
	h.broadcastRoomList()
}

// handleMatchConnected transitions the match from "connecting" to "active".
// Idempotent — multiple clients reporting it is fine; only the first wins.
func (h *Hub) handleMatchConnected(c *Client, env protocol.Envelope) {
	_ = env
	match := c.user.CurrentMatch
	if match == nil {
		return
	}
	if match.GetState() == state.MatchStateConnecting {
		match.SetState(state.MatchStateActive)
		slog.Info("match active", "id", match.ID, "by", c.user.Username)
	}
}

// handleMatchPunchFailed lets a client report that its punch attempt to a peer
// timed out. For now we just log it; the user-facing "Close Game" path will
// fire MATCH_FINISHED separately to tear down the room.
func (h *Hub) handleMatchPunchFailed(c *Client, env protocol.Envelope) {
	var req protocol.MatchPeerEventRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	slog.Warn("match punch failed",
		"match", req.MatchID, "peer", req.PeerUserID, "reporter", c.user.Username)
}

func (h *Hub) buildRoomListPayload() protocol.RoomListPayload {
	rooms := h.state.ListRooms()
	payload := protocol.RoomListPayload{
		Rooms: make([]protocol.RoomSummary, 0, len(rooms)),
	}
	for _, r := range rooms {
		id, hostID, _, players, max, st := r.Snapshot()
		hostName := ""
		if r.Host != nil {
			hostName = r.Host.Username
		}
		matchID, broadcasting := h.broadcasts.roomBroadcast(id)
		payload.Rooms = append(payload.Rooms, protocol.RoomSummary{
			ID:       id,
			Name:     r.Name,
			HostID:   hostID,
			HostName: hostName,
			Rom: protocol.RomInfo{
				Name:   r.RomName,
				MD5:    r.RomMD5,
				Region: r.RomRegion,
			},
			Players:      players,
			MaxPlayers:   max,
			State:        string(st),
			HasPasswd:    r.Password != "",
			PlayerNames:  r.PlayerUsernames(),
			StartedAt:    r.StartedAtUnixMs(),
			Broadcasting: broadcasting,
			MatchID:      matchID,
		})
	}
	return payload
}

func (h *Hub) broadcastRoomList() {
	h.broadcastToAll(protocol.TypeRoomList, h.buildRoomListPayload(), 0)
}

func (h *Hub) sendRoomListTo(c *Client) {
	c.sendEnvelope(protocol.TypeRoomList, "", h.buildRoomListPayload())
}

// broadcastRoomState sends the full room state to every user currently in it.
func (h *Hub) broadcastRoomState(room *state.Room) {
	h.broadcastToRoom(room, protocol.TypeRoomState, roomStatePayload(room))
}

// broadcastToRoom sends a message to every seated player in the given room.
// Spectators are not included for now (none yet).
func (h *Hub) broadcastToRoom(room *state.Room, msgType string, data any) {
	envBytes, err := json.Marshal(protocol.Envelope{
		Type: msgType,
		Data: mustMarshal(data),
	})
	if err != nil {
		return
	}
	for _, seat := range room.Players {
		if seat.User == nil || seat.User.Send == nil {
			continue
		}
		select {
		case seat.User.Send <- envBytes:
		default:
		}
	}
}

func roomStatePayload(r *state.Room) protocol.RoomStatePayload {
	out := protocol.RoomStatePayload{
		ID:             r.ID,
		Name:           r.Name,
		MaxPlayers:     r.MaxPlayers,
		Delay:          r.Delay,
		Prediction:     r.Prediction,
		Pacing:         r.Pacing,
		DelayAuto:      r.DelayAuto,
		PredictionAuto: r.PredictionAuto,
		Rom: protocol.RomInfo{
			Name:   r.RomName,
			MD5:    r.RomMD5,
			Region: r.RomRegion,
		},
		State: string(r.State),
	}
	if r.Host != nil {
		out.HostID = r.Host.ID
	}
	for _, seat := range r.Players {
		if seat.User == nil {
			continue
		}
		out.Players = append(out.Players, protocol.RoomSeatPayload{
			UserID:   seat.User.ID,
			Username: seat.User.Username,
			Slot:     seat.Slot,
		})
	}
	return out
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

func (c *Client) sendRoomCreateFail(id, reason string) {
	c.sendEnvelope(protocol.TypeRoomCreateFail, id, protocol.RoomJoinFailPayload{Reason: reason})
}

// broadcastPresenceUpdated emits a PRESENCE_DELTA with the user in "updated".
func (h *Hub) broadcastPresenceUpdated(u *state.User) {
	h.broadcastToAll(protocol.TypePresenceDelta, protocol.PresenceDeltaPayload{
		Updated: []protocol.PresenceUser{h.presenceUserFromState(u)},
	}, 0)
}
