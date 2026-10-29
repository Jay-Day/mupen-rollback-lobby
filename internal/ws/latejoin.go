package ws

import (
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// Late join: a room member who isn't in the running match (they joined the
// room mid-match, or dropped out of it) can be added by the host. Every client
// already emulates one controller port per seat (MatchBeginPayload.Seats), so
// the joiner takes over an idle controller instead of changing the machine.
//
// The hand-off itself is peer to peer; the server only checks and routes it:
//
//	host   MATCH_ADMIT                → server: MATCH_ADMIT_BEGIN → players, joiner
//	host   manifest (chunked)         → joiner   (pre-match settings)
//	joiner ready                      → host     (booted, connected to every player)
//	host   cut {frame}                → players  (stop at that frame)
//	player cut_ready {ok}             → host
//	host   state (chunked)            → joiner   (the game at the cut)
//	host   go                         → players, joiner; the server commits the join
//	anyone abort                      → everyone else involved
//
// Only one hand-off runs per match. A stale one (the host's client vanished
// mid-way without the room dissolving) is replaced after admitStaleAfter.

const (
	admitStaleAfter    = 90 * time.Second
	admitMaxChunkB64   = 700 * 1024 // stays under the 1 MiB WebSocket read limit
	admitMaxChunkCount = 256
)

var admitIDs atomic.Uint64

// canLateJoin reports whether u's client can take part in a hand-off.
func canLateJoin(u *state.User) bool {
	return u != nil && u.LateJoin && u.ICESupported
}

// roomMatch returns the room's running match. The host is always in it: a
// host drop ends the match and a host leave dissolves the room.
func roomMatch(room *state.Room) *state.Match {
	if room == nil || room.Host == nil {
		return nil
	}
	m := room.Host.CurrentMatch
	if m == nil || m.RoomID != room.ID {
		return nil
	}
	return m
}

// roomAcceptsLateJoin reports whether the room's running match can take more
// players.
func roomAcceptsLateJoin(room *state.Room) bool {
	if room == nil || room.QuickMatch || room.State != state.RoomStateInGame {
		return false
	}
	m := roomMatch(room)
	return m != nil && m.Seats > 0
}

// matchSeatsFor sizes a new match: every port the room can seat when all of
// its players can take part in a late join, otherwise 0 (the older sizing).
func matchSeatsFor(room *state.Room) int {
	if room.QuickMatch || room.MaxPlayers < 2 {
		return 0
	}
	for _, seat := range room.Players {
		if seat.User == nil || !canLateJoin(seat.User) {
			return 0
		}
	}
	return room.MaxPlayers
}

func matchPeerFor(u *state.User, slot int) protocol.MatchPeer {
	ip, port := u.GetUDPEndpoint()
	return protocol.MatchPeer{
		UserID:       u.ID,
		Username:     u.Username,
		PublicIP:     ip,
		PublicPort:   port,
		LocalIP:      u.LocalIP,
		Slot:         slot,
		ICESupported: u.ICESupported,
	}
}

func seatSlotOf(room *state.Room, u *state.User) int {
	for _, seat := range room.Players {
		if seat.User != nil && seat.User.ID == u.ID {
			return seat.Slot
		}
	}
	return 0
}

// activeMatchPlayers is everyone still playing in m, in seat order.
func activeMatchPlayers(room *state.Room, m *state.Match) []*state.RoomSeat {
	var out []*state.RoomSeat
	for _, seat := range room.Players {
		if seat.User != nil && seat.User.CurrentMatch == m {
			out = append(out, seat)
		}
	}
	return out
}

func (c *Client) sendMatchAdmitFail(matchID, userID uint64, reason string) {
	c.sendEnvelope(protocol.TypeMatchAdmitFail, "", protocol.MatchAdmitFailPayload{
		MatchID: matchID, UserID: userID, Reason: reason,
	})
}

func sendEnvelopeTo(u *state.User, msgType string, data any) {
	if u == nil || u.Send == nil {
		return
	}
	env, err := json.Marshal(protocol.Envelope{Type: msgType, Data: mustMarshal(data)})
	if err != nil {
		return
	}
	select {
	case u.Send <- env:
	default:
		slog.Warn("late join message dropped (channel full)", "user", u.Username, "type", msgType)
	}
}

func (h *Hub) handleMatchAdmit(c *Client, env protocol.Envelope) {
	var req protocol.MatchAdmitRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	room := c.user.CurrentRoom
	match := c.user.CurrentMatch
	if room == nil || match == nil || match.ID != req.MatchID ||
		room.Host == nil || room.Host.ID != c.user.ID {
		c.sendMatchAdmitFail(req.MatchID, req.UserID, protocol.ReasonNotAuthorized)
		return
	}
	if !roomAcceptsLateJoin(room) || !canLateJoin(c.user) {
		c.sendMatchAdmitFail(req.MatchID, req.UserID, protocol.ReasonLateJoinUnsupported)
		return
	}

	var joiner *state.User
	joinerSlot := 0
	for _, seat := range room.Players {
		if seat.User != nil && seat.User.ID == req.UserID {
			joiner = seat.User
			joinerSlot = seat.Slot
		}
	}
	if joiner == nil || joiner.CurrentMatch != nil {
		c.sendMatchAdmitFail(req.MatchID, req.UserID, protocol.ReasonNotWaiting)
		return
	}
	if !canLateJoin(joiner) || joinerSlot < 1 || joinerSlot > match.Seats {
		c.sendMatchAdmitFail(req.MatchID, req.UserID, protocol.ReasonLateJoinUnsupported)
		return
	}
	// Builds emulate differently (see the v0.9.14/v0.9.15 divergence), so a
	// joiner must run exactly the host's build to stay in sync.
	if joiner.ClientVersion != c.user.ClientVersion {
		c.sendMatchAdmitFail(req.MatchID, req.UserID, protocol.ReasonVersionMismatch)
		return
	}

	if old := match.CurrentAdmission(); old != nil && time.Since(old.StartedAt) > admitStaleAfter {
		h.abortAdmission(room, match, old.ID, "timeout", 0)
	}
	admit, epoch, ok := match.BeginAdmission(admitIDs.Add(1), joiner, joinerSlot)
	if !ok {
		c.sendMatchAdmitFail(req.MatchID, req.UserID, protocol.ReasonAdmitBusy)
		return
	}

	players := activeMatchPlayers(room, match)
	peers := make([]protocol.MatchPeer, 0, len(players)+1)
	for _, seat := range players {
		peers = append(peers, matchPeerFor(seat.User, seat.Slot))
	}
	joinerPeer := matchPeerFor(joiner, joinerSlot)
	peers = append(peers, joinerPeer)

	payload := protocol.MatchAdmitBeginPayload{
		MatchID: match.ID,
		AdmitID: admit.ID,
		Epoch:   epoch,
		Seats:   match.Seats,
		HostID:  c.user.ID,
		Joiner:  joinerPeer,
		Peers:   peers,
	}
	for _, seat := range players {
		sendEnvelopeTo(seat.User, protocol.TypeMatchAdmitBegin, payload)
	}
	sendEnvelopeTo(joiner, protocol.TypeMatchAdmitBegin, payload)

	slog.Info("late join started", "match", match.ID, "admit", admit.ID, "joiner", joiner.Username,
		"slot", joinerSlot, "epoch", epoch, "players", len(players))
}

func (h *Hub) handleMatchAdmitSignal(c *Client, env protocol.Envelope) {
	var sig protocol.MatchAdmitSignalPayload
	if err := json.Unmarshal(env.Data, &sig); err != nil {
		return
	}
	room := c.user.CurrentRoom
	if room == nil {
		return
	}
	match := roomMatch(room)
	if match == nil || match.ID != sig.MatchID {
		return
	}
	admit := match.CurrentAdmission()
	if admit == nil || admit.ID != sig.AdmitID {
		return
	}
	if len(sig.Data) > admitMaxChunkB64 || sig.Count < 0 || sig.Count > admitMaxChunkCount ||
		sig.Index < 0 || (sig.Count > 0 && sig.Index >= sig.Count) {
		return
	}

	isHost := room.Host != nil && room.Host.ID == c.user.ID
	isJoiner := admit.Joiner != nil && admit.Joiner.ID == c.user.ID
	isPlayer := c.user.CurrentMatch == match
	sig.FromUserID = c.user.ID

	switch sig.Step {
	case protocol.AdmitStepManifest, protocol.AdmitStepState:
		if isHost {
			sendEnvelopeTo(admit.Joiner, protocol.TypeMatchAdmitSignal, sig)
		}
	case protocol.AdmitStepReady:
		if isJoiner {
			sendEnvelopeTo(room.Host, protocol.TypeMatchAdmitSignal, sig)
		}
	case protocol.AdmitStepCut:
		if isHost {
			for _, seat := range activeMatchPlayers(room, match) {
				if seat.User.ID != c.user.ID {
					sendEnvelopeTo(seat.User, protocol.TypeMatchAdmitSignal, sig)
				}
			}
		}
	case protocol.AdmitStepCutReady:
		if isPlayer && !isHost {
			sendEnvelopeTo(room.Host, protocol.TypeMatchAdmitSignal, sig)
		}
	case protocol.AdmitStepGo:
		if isHost {
			h.commitAdmission(room, match, admit, sig)
		}
	case protocol.AdmitStepAbort:
		if isHost || isJoiner || isPlayer {
			h.abortAdmission(room, match, admit.ID, sig.Reason, c.user.ID)
		}
	}
}

// commitAdmission makes the joiner a match player and tells everyone to switch
// to the new session.
func (h *Hub) commitAdmission(room *state.Room, match *state.Match, admit *state.Admission, sig protocol.MatchAdmitSignalPayload) {
	if match.EndAdmission(admit.ID) == nil {
		return
	}
	joiner := admit.Joiner
	if joiner == nil || joiner.CurrentRoom != room || joiner.CurrentMatch != nil {
		// The joiner left after the players stopped at the cut. The players
		// still switch sessions; the empty seat simply stays idle.
		for _, seat := range activeMatchPlayers(room, match) {
			if seat.User.ID != sig.FromUserID {
				sendEnvelopeTo(seat.User, protocol.TypeMatchAdmitSignal, sig)
			}
		}
		return
	}

	for _, seat := range activeMatchPlayers(room, match) {
		if seat.User.ID != sig.FromUserID {
			sendEnvelopeTo(seat.User, protocol.TypeMatchAdmitSignal, sig)
		}
	}
	sendEnvelopeTo(joiner, protocol.TypeMatchAdmitSignal, sig)

	match.AddPlayer(joiner)
	joiner.CurrentMatch = match
	joiner.SetState(state.UserStatePlaying)

	slog.Info("late join committed", "match", match.ID, "admit", admit.ID, "joiner", joiner.Username,
		"slot", admit.Slot)

	h.broadcastRoomState(room)
	h.broadcastRoomList()
	h.broadcastPresenceUpdated(joiner)
}

// abortAdmission ends the hand-off and tells everyone involved except
// exceptUserID (0 tells everyone, for a server-side abort).
func (h *Hub) abortAdmission(room *state.Room, match *state.Match, admitID uint64, reason string, exceptUserID uint64) {
	admit := match.EndAdmission(admitID)
	if admit == nil {
		return
	}
	sig := protocol.MatchAdmitSignalPayload{
		MatchID:    match.ID,
		AdmitID:    admit.ID,
		Step:       protocol.AdmitStepAbort,
		FromUserID: exceptUserID,
		Reason:     clampString(reason, 64),
	}
	for _, seat := range activeMatchPlayers(room, match) {
		if seat.User.ID != exceptUserID {
			sendEnvelopeTo(seat.User, protocol.TypeMatchAdmitSignal, sig)
		}
	}
	if admit.Joiner != nil && admit.Joiner.ID != exceptUserID {
		sendEnvelopeTo(admit.Joiner, protocol.TypeMatchAdmitSignal, sig)
	}
	joinerName := ""
	if admit.Joiner != nil {
		joinerName = admit.Joiner.Username
	}
	slog.Info("late join aborted", "match", match.ID, "admit", admit.ID, "joiner", joinerName,
		"reason", sig.Reason, "by", exceptUserID)
}

// abortAdmissionFor cancels a hand-off that u is joining through, before u
// leaves the room or the match ends.
func (h *Hub) abortAdmissionFor(room *state.Room, u *state.User, reason string) {
	match := roomMatch(room)
	if match == nil {
		return
	}
	admit := match.CurrentAdmission()
	if admit == nil || admit.Joiner == nil || admit.Joiner.ID != u.ID {
		return
	}
	h.abortAdmission(room, match, admit.ID, reason, 0)
}
