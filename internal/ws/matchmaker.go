package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	qmDefaultDelay      = 2
	qmDefaultPrediction = 7
	qmDefaultPacing     = 1 // smooth/asymmetric — the only model current clients run
	qmDefaultMaxPlayers = 2

	// Warmup window between pairing and auto-start. Long enough for both clients
	// to render the in-room view, probe each other, and publish their local
	// rollback settings.
	qmWarmup = 5 * time.Second
)

// qmEntry is a queued searcher: when they enqueued and the ROM they want.
type qmEntry struct {
	enqueuedAt time.Time
	rom        protocol.RomInfo
}

// matchmaker tracks users searching for a quick match.
// Pairing rule: same ROM (by MD5), then lowest combined pingToServer, ties FIFO.
type matchmaker struct {
	mu    sync.Mutex
	queue map[uint64]qmEntry // userId → entry
}

func newMatchmaker() *matchmaker {
	return &matchmaker{
		queue: make(map[uint64]qmEntry),
	}
}

func (m *matchmaker) add(userID uint64, rom protocol.RomInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.queue[userID]; !exists {
		m.queue[userID] = qmEntry{enqueuedAt: time.Now(), rom: rom}
	}
}

// romOf returns the ROM a queued user is searching for.
func (m *matchmaker) romOf(userID uint64) (protocol.RomInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.queue[userID]
	return e.rom, ok
}

func (m *matchmaker) remove(userID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.queue, userID)
}

func (m *matchmaker) contains(userID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.queue[userID]
	return ok
}

func (m *matchmaker) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queue)
}

// pickOpponent returns the userId of the best opponent for `searcher` who is
// queued for the same ROM (matched by MD5), or 0. Pings come from getPing(uid)
// (returns user's pingToServer, or 0 if unknown).
func (m *matchmaker) pickOpponent(searcherID uint64, searcherPing uint16, romMd5 string, getPing func(uint64) uint16) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	var bestID uint64
	bestScore := uint32(math.MaxUint32)
	var bestEnq time.Time
	for id, e := range m.queue {
		if id == searcherID {
			continue
		}
		// Only pair players searching for the same ROM.
		if !strings.EqualFold(e.rom.MD5, romMd5) {
			continue
		}
		score := uint32(searcherPing) + uint32(getPing(id))
		if score < bestScore || (score == bestScore && (bestEnq.IsZero() || e.enqueuedAt.Before(bestEnq))) {
			bestID = id
			bestScore = score
			bestEnq = e.enqueuedAt
		}
	}
	return bestID
}

// -- Hub integration --

func (h *Hub) handleQuickMatchJoin(c *Client, env protocol.Envelope) {
	if c.user.CurrentRoom != nil {
		// Already in a room — can't queue
		slog.Debug("QUICK_MATCH_JOIN rejected: in a room", "user", c.user.Username)
		return
	}

	var req protocol.QuickMatchJoinRequest
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &req); err != nil {
			slog.Warn("QUICK_MATCH_JOIN invalid payload", "user", c.user.Username, "err", err)
			return
		}
	}
	if strings.TrimSpace(req.Rom.MD5) == "" {
		// No ROM selected — can't scope the search, so don't queue.
		slog.Warn("QUICK_MATCH_JOIN rejected: no rom md5", "user", c.user.Username)
		return
	}

	if h.matchmaker.contains(c.user.ID) {
		return // already queued
	}

	h.matchmaker.add(c.user.ID, req.Rom)
	c.user.SetState(state.UserStateSearching)
	h.broadcastPresenceUpdated(c.user)

	c.sendEnvelope(protocol.TypeQuickMatchStatus, env.ID, protocol.QuickMatchStatusPayload{
		Searching: true,
		QueueSize: h.matchmaker.size(),
	})

	slog.Info("quick match joined", "user", c.user.Username,
		"rom", req.Rom.Name, "md5", req.Rom.MD5, "queue", h.matchmaker.size())
	h.quickMatchAttempt(c.user)
}

func (h *Hub) handleQuickMatchCancel(c *Client, env protocol.Envelope) {
	if !h.matchmaker.contains(c.user.ID) {
		return
	}
	h.matchmaker.remove(c.user.ID)
	c.user.SetState(state.UserStateIdle)
	h.broadcastPresenceUpdated(c.user)

	c.sendEnvelope(protocol.TypeQuickMatchStatus, env.ID, protocol.QuickMatchStatusPayload{
		Searching: false,
		QueueSize: h.matchmaker.size(),
	})
	slog.Info("quick match cancelled", "user", c.user.Username, "queue", h.matchmaker.size())
}

// dequeueQuickMatch drops a user's quick-match queue entry when they enter a
// room by other means, and resets their client's search toggle. Without this
// the stale entry sits in the queue until pairing fires and corrupts their
// room state (see quickMatchAttempt).
func (h *Hub) dequeueQuickMatch(c *Client) {
	if !h.matchmaker.contains(c.user.ID) {
		return
	}
	h.matchmaker.remove(c.user.ID)
	c.sendEnvelope(protocol.TypeQuickMatchStatus, "", protocol.QuickMatchStatusPayload{
		Searching: false,
		QueueSize: h.matchmaker.size(),
	})
	slog.Info("quick match dequeued: entered a room", "user", c.user.Username)
}

// quickMatchAttempt runs the pairing logic on behalf of `searcher`.
// If an opponent is found, both are removed from the queue and dropped into
// an auto-created room which then auto-starts.
func (h *Hub) quickMatchAttempt(searcher *state.User) {
	searcherRom, ok := h.matchmaker.romOf(searcher.ID)
	if !ok {
		return // no longer queued
	}

	opponentID := h.matchmaker.pickOpponent(searcher.ID, searcher.PingToServer, searcherRom.MD5, func(uid uint64) uint16 {
		u := h.state.GetUser(uid)
		if u == nil {
			return 0
		}
		return u.PingToServer
	})
	if opponentID == 0 {
		return // nobody searching for the same ROM yet
	}
	opponent := h.state.GetUser(opponentID)
	if opponent == nil {
		h.matchmaker.remove(opponentID)
		return
	}

	// A queued user who has since entered a room by other means is a stale
	// entry: pairing them would overwrite their CurrentRoom pointer and orphan
	// their seat, leaving behind a room nobody can dissolve — the "ghost match
	// that sits in the list until a server restart" bug. Drop the stale side
	// and let the live side keep searching.
	if searcher.CurrentRoom != nil {
		slog.Warn("quick match dropped stale queue entry", "user", searcher.Username)
		h.matchmaker.remove(searcher.ID)
		return
	}
	if opponent.CurrentRoom != nil {
		slog.Warn("quick match dropped stale queue entry", "user", opponent.Username)
		h.matchmaker.remove(opponentID)
		return
	}

	// Capture the host's ROM before popping them. Both sides matched on MD5, so
	// either RomInfo names the same game; use the host's so the room is
	// host-authoritative (matches normal Create Room behaviour).
	hostRom, ok := h.matchmaker.romOf(opponentID)
	if !ok {
		hostRom = searcherRom
	}

	// Pop both from the queue
	h.matchmaker.remove(searcher.ID)
	h.matchmaker.remove(opponentID)

	// Tell both clients they've stopped searching. Without this the client's
	// Quick Match button stays in its "Cancel" toggle state for the rest of the
	// session (it only clears on QUICK_MATCH_STATUS{searching:false} or
	// disconnect), so after the game ends and they leave the room, clicking
	// Quick Match sends a cancel instead of a new join and they can't requeue.
	for _, uid := range []uint64{searcher.ID, opponentID} {
		if cl := h.getClient(uid); cl != nil {
			cl.sendEnvelope(protocol.TypeQuickMatchStatus, "", protocol.QuickMatchStatusPayload{
				Searching: false,
				QueueSize: h.matchmaker.size(),
			})
		}
	}

	// Build the room with the earlier-enqueued player as host.
	hostUser, joinerUser := opponent, searcher

	room := &state.Room{
		ID:         h.state.NextRoomID(),
		Name:       fmt.Sprintf("Quick Match: %s vs %s", hostUser.Username, joinerUser.Username),
		Host:       hostUser,
		RomName:    hostRom.Name,
		RomMD5:     hostRom.MD5,
		RomRegion:  hostRom.Region,
		MaxPlayers: qmDefaultMaxPlayers,
		Delay:      qmDefaultDelay,
		Prediction: qmDefaultPrediction,
		Pacing:     qmDefaultPacing,
		// Quick Match opens on Auto delay and Default prediction for both seats;
		// each client publishes its resolved local pair during warmup.
		DelayAuto:      true,
		PredictionAuto: true,
		Visibility:     state.VisibilityInviteOnly,
		State:          state.RoomStateWaiting,
		CreatedAt:      time.Now(),
	}

	slog.Info("quick match paired", "rom", hostRom.Name, "md5", hostRom.MD5,
		"host", hostUser.Username, "joiner", joinerUser.Username, "room", room.ID)
	// Seat through the choke point: if a racing handler seated either user
	// between the pair-time check above and here, this refuses instead of
	// corrupting their membership — the room object is simply discarded.
	if err := h.seatUser(hostUser, room, state.UserStateInRoom); err != nil {
		slog.Warn("quick match aborted: host could not be seated",
			"user", hostUser.Username, "err", err)
		return
	}
	if err := h.seatUser(joinerUser, room, state.UserStateInRoom); err != nil {
		slog.Warn("quick match aborted: joiner could not be seated",
			"user", joinerUser.Username, "err", err)
		h.unseatUser(hostUser, room)
		return
	}

	h.state.AddRoom(room)

	// Send ROOM_JOIN_OK to both so their UIs switch into the in-room view.
	payload := roomStatePayload(room)
	if hostClient := h.getClient(hostUser.ID); hostClient != nil {
		hostClient.sendEnvelope(protocol.TypeRoomJoinOK, "", payload)
	}
	if joinerClient := h.getClient(joinerUser.ID); joinerClient != nil {
		joinerClient.sendEnvelope(protocol.TypeRoomJoinOK, "", payload)
	}

	// Punch the pair against each other immediately. The warmup below is only
	// long enough to be useful if a real ping lands inside it, and a one-sided
	// probe usually won't — so the Auto delay would fall back to the region
	// estimate on exactly the path that most needs a measurement.
	h.introduceRoomPeers(room, joinerUser)

	// Don't start instantly. Give both clients a short warmup in the in-room
	// view: seats + live peer ping render, and the host's Auto delay resolves
	// from that ping and pushes ROOM_UPDATE_DELAY before the match locks in.
	// The match then auto-starts with the tuned delay.
	for _, u := range []*state.User{hostUser, joinerUser} {
		h.broadcastPresenceUpdated(u)
	}
	h.broadcastRoomState(room)
	h.broadcastRoomList()
	h.sendRoomSystemChat(room,
		fmt.Sprintf("Match found! Measuring connection — game starts in %d seconds.",
			int(qmWarmup/time.Second)))
	h.scheduleQuickMatchStart(room)
}

// scheduleQuickMatchStart auto-starts the paired match after the warmup window,
// but only if the room is still valid — a player may have left during warmup,
// dissolving the room or emptying a seat.
func (h *Hub) scheduleQuickMatchStart(room *state.Room) {
	roomID := room.ID
	time.AfterFunc(qmWarmup, func() {
		r := h.state.GetRoom(roomID)
		if r == nil {
			return
		}
		_, _, _, players, _, st := r.Snapshot()
		if st != state.RoomStateWaiting || players < minRoomPlayers {
			slog.Info("quick match warmup aborted", "room", roomID, "state", st, "players", players)
			return
		}
		// host-only validation doesn't apply to a system-created room.
		h.startMatchInRoom(r, "quick-match")
	})
}

// sendRoomSystemChat pushes an informational chat line to a room's members,
// attributed to a synthetic sender (userID 0) so it renders like a normal room
// message on every client without needing a dedicated system-message type.
func (h *Hub) sendRoomSystemChat(room *state.Room, message string) {
	h.broadcastToRoom(room, protocol.TypeChatMsg, protocol.ChatMsgPayload{
		Channel:      channelRoom,
		FromUserID:   0,
		FromUsername: "Quick Match",
		Message:      message,
		ServerTime:   nowMs(),
	})
}

// getClient returns the WS client for a given user id, or nil if not connected.
func (h *Hub) getClient(userID uint64) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[userID]
}
