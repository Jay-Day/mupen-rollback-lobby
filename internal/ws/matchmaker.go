package ws

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	qmDefaultDelay      = 2
	qmDefaultPrediction = 7
	qmDefaultMaxPlayers = 2
	// TODO: Quick Match currently hardcodes the ROM. Once clients send their
	// preferred ROM list with QUICK_MATCH_JOIN, intersect those and pick a
	// common one. For now both sides must have this ROM in their library.
	qmDefaultRomName = "Super Smash Bros."
)

// matchmaker tracks users searching for a quick match.
// Pairing rule: lowest combined pingToServer, ties broken FIFO.
type matchmaker struct {
	mu    sync.Mutex
	queue map[uint64]time.Time // userId → enqueue time
}

func newMatchmaker() *matchmaker {
	return &matchmaker{
		queue: make(map[uint64]time.Time),
	}
}

func (m *matchmaker) add(userID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.queue[userID]; !exists {
		m.queue[userID] = time.Now()
	}
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

// pickOpponent returns the userId of the best opponent for `searcher`, or 0.
// Pings come from getPing(uid) (returns user's pingToServer, or 0 if unknown).
func (m *matchmaker) pickOpponent(searcherID uint64, searcherPing uint16, getPing func(uint64) uint16) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	var bestID uint64
	bestScore := uint32(math.MaxUint32)
	var bestEnq time.Time
	for id, enq := range m.queue {
		if id == searcherID {
			continue
		}
		score := uint32(searcherPing) + uint32(getPing(id))
		if score < bestScore || (score == bestScore && (bestEnq.IsZero() || enq.Before(bestEnq))) {
			bestID = id
			bestScore = score
			bestEnq = enq
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
	if h.matchmaker.contains(c.user.ID) {
		return // already queued
	}

	h.matchmaker.add(c.user.ID)
	c.user.SetState(state.UserStateSearching)
	h.broadcastPresenceUpdated(c.user)

	c.sendEnvelope(protocol.TypeQuickMatchStatus, env.ID, protocol.QuickMatchStatusPayload{
		Searching: true,
		QueueSize: h.matchmaker.size(),
	})

	slog.Info("quick match joined", "user", c.user.Username, "queue", h.matchmaker.size())
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

// quickMatchAttempt runs the pairing logic on behalf of `searcher`.
// If an opponent is found, both are removed from the queue and dropped into
// an auto-created room which then auto-starts.
func (h *Hub) quickMatchAttempt(searcher *state.User) {
	opponentID := h.matchmaker.pickOpponent(searcher.ID, searcher.PingToServer, func(uid uint64) uint16 {
		u := h.state.GetUser(uid)
		if u == nil {
			return 0
		}
		return u.PingToServer
	})
	if opponentID == 0 {
		return // nobody to match with yet
	}
	opponent := h.state.GetUser(opponentID)
	if opponent == nil {
		h.matchmaker.remove(opponentID)
		return
	}

	// Pop both from the queue
	h.matchmaker.remove(searcher.ID)
	h.matchmaker.remove(opponentID)

	// Build the room with the earlier-enqueued player as host.
	hostUser, joinerUser := opponent, searcher

	room := &state.Room{
		ID:         h.state.NextRoomID(),
		Name:       fmt.Sprintf("Quick Match: %s vs %s", hostUser.Username, joinerUser.Username),
		Host:       hostUser,
		RomName:    qmDefaultRomName,
		MaxPlayers: qmDefaultMaxPlayers,
		Delay:      qmDefaultDelay,
		Prediction: qmDefaultPrediction,
		Visibility: state.VisibilityInviteOnly,
		State:      state.RoomStateWaiting,
		CreatedAt:  time.Now(),
	}
	_ = room.AddPlayer(hostUser)
	_ = room.AddPlayer(joinerUser)

	hostUser.CurrentRoom = room
	joinerUser.CurrentRoom = room
	hostUser.SetState(state.UserStateInRoom)
	joinerUser.SetState(state.UserStateInRoom)

	h.state.AddRoom(room)

	// Send ROOM_JOIN_OK to both so their UIs switch into the in-room view.
	payload := roomStatePayload(room)
	if hostClient := h.getClient(hostUser.ID); hostClient != nil {
		hostClient.sendEnvelope(protocol.TypeRoomJoinOK, "", payload)
	}
	if joinerClient := h.getClient(joinerUser.ID); joinerClient != nil {
		joinerClient.sendEnvelope(protocol.TypeRoomJoinOK, "", payload)
	}

	// Auto-start: matchmaker already enforced both seats are filled, and
	// host-only validation doesn't apply to a system-created room.
	h.startMatchInRoom(room, "quick-match")
}

// getClient returns the WS client for a given user id, or nil if not connected.
func (h *Hub) getClient(userID uint64) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[userID]
}
