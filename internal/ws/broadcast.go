package ws

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// broadcast is one live stream of a match's .krec. A single player (the
// broadcaster) streams krec bytes up; the server appends them to an ephemeral
// file and fans them out to spectators. The file is deleted when the broadcast
// ends — we keep it on disk (not in RAM) so a late-joining spectator can be
// served the full history from frame 0, then tail the live edge.
type broadcast struct {
	matchID       uint64
	roomID        uint64
	broadcasterID uint64
	path          string

	mu         sync.Mutex
	file       *os.File
	spectators map[uint64]*Client
	closed     bool
	liveFrame  int // broadcaster's latest stamped live krec frame
}

// broadcastManager owns all live broadcasts, keyed by match id.
type broadcastManager struct {
	hub *Hub
	dir string

	mu      sync.Mutex
	byMatch map[uint64]*broadcast
}

func newBroadcastManager(hub *Hub) *broadcastManager {
	dir := filepath.Join(os.TempDir(), "rmgk-broadcasts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("broadcast: could not create spool dir", "dir", dir, "err", err)
	}
	return &broadcastManager{
		hub:     hub,
		dir:     dir,
		byMatch: make(map[uint64]*broadcast),
	}
}

// roomBroadcast reports whether the given room's current match is being
// broadcast and, if so, the match id (so the room list can flag it and tell
// spectators what to SPECTATE_START).
func (m *broadcastManager) roomBroadcast(roomID uint64) (matchID uint64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.byMatch {
		if b.roomID == roomID {
			return b.matchID, true
		}
	}
	return 0, false
}

// begin registers a new broadcast and opens its spool file. Returns false if
// the match is already being broadcast (first broadcaster wins) or the file
// can't be opened.
func (m *broadcastManager) begin(matchID, roomID, broadcasterID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byMatch[matchID]; exists {
		return false
	}
	path := filepath.Join(m.dir, fmt.Sprintf("match-%d.krec", matchID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Warn("broadcast: open spool file failed", "path", path, "err", err)
		return false
	}
	m.byMatch[matchID] = &broadcast{
		matchID:       matchID,
		roomID:        roomID,
		broadcasterID: broadcasterID,
		path:          path,
		file:          f,
		spectators:    make(map[uint64]*Client),
	}
	return true
}

// data appends a krec chunk to the spool file and forwards it to spectators.
// Only the registered broadcaster may write.
func (m *broadcastManager) data(senderID, matchID uint64, b64 string, frame int) {
	m.mu.Lock()
	b := m.byMatch[matchID]
	m.mu.Unlock()
	if b == nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) == 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.broadcasterID != senderID {
		return
	}
	if frame > b.liveFrame {
		b.liveFrame = frame // track the live edge for spectate fast-forward
	}
	if _, err := b.file.Write(raw); err != nil {
		slog.Warn("broadcast: spool write failed", "match", matchID, "err", err)
	}
	// Forward the same base64 chunk to every spectator. Marshal once.
	if len(b.spectators) == 0 {
		return
	}
	env, err := json.Marshal(protocol.Envelope{
		Type: protocol.TypeSpectateData,
		Data: mustMarshal(protocol.SpectateDataPayload{MatchID: matchID, Data: b64, Frame: b.liveFrame}),
	})
	if err != nil {
		return
	}
	for _, sc := range b.spectators {
		select {
		case sc.send <- env:
		default:
			// Spectator can't keep up; the gap would desync their replay, so
			// drop them rather than feed a corrupt stream.
			slog.Warn("spectate drop (queue full)", "match", matchID, "user", sc.user.ID)
		}
	}
}

// spectateStart serves the krec history-so-far to a new spectator, then
// registers them for the live tail. The snapshot read and the registration
// happen under the broadcast lock, so the history plus all subsequent live
// chunks form a gap-free, duplicate-free stream.
func (m *broadcastManager) spectateStart(c *Client, matchID uint64) {
	m.mu.Lock()
	b := m.byMatch[matchID]
	m.mu.Unlock()
	if b == nil {
		c.sendEnvelope(protocol.TypeSpectateFail, "", protocol.SpectateFailPayload{
			MatchID: matchID, Reason: "not_broadcasting",
		})
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		c.sendEnvelope(protocol.TypeSpectateFail, "", protocol.SpectateFailPayload{
			MatchID: matchID, Reason: "ended",
		})
		return
	}

	data, err := os.ReadFile(b.path)
	if err != nil {
		slog.Warn("broadcast: snapshot read failed", "path", b.path, "err", err)
		data = nil
	}

	c.sendEnvelope(protocol.TypeSpectateBegin, "", protocol.SpectateBeginPayload{MatchID: matchID})
	if len(data) > 0 {
		c.sendEnvelope(protocol.TypeSpectateData, "", protocol.SpectateDataPayload{
			MatchID: matchID,
			Data:    base64.StdEncoding.EncodeToString(data),
			Frame:   b.liveFrame, // live edge so the joiner can fast-forward to it
		})
	}
	b.spectators[c.user.ID] = c
	slog.Info("spectate start", "match", matchID, "user", c.user.Username, "history_bytes", len(data))
}

func (m *broadcastManager) spectateStop(userID, matchID uint64) {
	m.mu.Lock()
	b := m.byMatch[matchID]
	m.mu.Unlock()
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.spectators, userID)
	b.mu.Unlock()
}

// endByMatch ends a broadcast regardless of who triggered it (match is over).
func (m *broadcastManager) endByMatch(matchID uint64) bool {
	m.mu.Lock()
	b := m.byMatch[matchID]
	if b != nil {
		delete(m.byMatch, matchID)
	}
	m.mu.Unlock()
	return m.finish(b, "match_over")
}

// endIfBroadcaster ends a broadcast only if userID is its broadcaster.
func (m *broadcastManager) endIfBroadcaster(matchID, userID uint64) bool {
	m.mu.Lock()
	b := m.byMatch[matchID]
	if b == nil || b.broadcasterID != userID {
		m.mu.Unlock()
		return false
	}
	delete(m.byMatch, matchID)
	m.mu.Unlock()
	return m.finish(b, "broadcaster_left")
}

// finish closes + deletes the spool file and notifies spectators. Returns true
// if it actually ended a live broadcast.
func (m *broadcastManager) finish(b *broadcast, reason string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	b.closed = true
	if b.file != nil {
		_ = b.file.Close()
	}
	if b.path != "" {
		_ = os.Remove(b.path)
	}
	specs := b.spectators
	b.spectators = make(map[uint64]*Client)
	b.mu.Unlock()

	for _, sc := range specs {
		sc.sendEnvelope(protocol.TypeSpectateEnd, "", protocol.SpectateEndPayload{
			MatchID: b.matchID, Reason: reason,
		})
	}
	slog.Info("broadcast end", "match", b.matchID, "reason", reason, "spectators", len(specs))
	return true
}

// onUserGone tears down a disconnecting user's broadcast (if any) and removes
// them from every spectator set.
func (m *broadcastManager) onUserGone(userID uint64) {
	m.mu.Lock()
	var ended []*broadcast
	var others []*broadcast
	for mid, b := range m.byMatch {
		if b.broadcasterID == userID {
			ended = append(ended, b)
			delete(m.byMatch, mid)
		} else {
			others = append(others, b)
		}
	}
	m.mu.Unlock()

	for _, b := range ended {
		m.finish(b, "broadcaster_left")
	}
	for _, b := range others {
		b.mu.Lock()
		delete(b.spectators, userID)
		b.mu.Unlock()
	}
	if len(ended) > 0 {
		m.hub.broadcastRoomList()
	}
}

// -- WS handlers --

func (h *Hub) handleBroadcastBegin(c *Client, env protocol.Envelope) {
	var req protocol.BroadcastBeginRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	match := c.user.CurrentMatch
	if match == nil || match.ID != req.MatchID {
		slog.Debug("BROADCAST_BEGIN rejected: caller not in that match",
			"user", c.user.ID, "match", req.MatchID)
		return
	}
	// Only the room host may broadcast — keeps it to one authoritative stream and
	// matches the host-only checkbox in the client.
	room := c.user.CurrentRoom
	if room == nil || room.Host == nil || room.Host.ID != c.user.ID {
		slog.Debug("BROADCAST_BEGIN rejected: not host", "user", c.user.ID, "match", req.MatchID)
		return
	}
	if h.broadcasts.begin(match.ID, match.RoomID, c.user.ID) {
		slog.Info("broadcast begin", "match", match.ID, "by", c.user.Username)
		h.broadcastRoomList() // flag the room as broadcasting
	}
}

func (h *Hub) handleBroadcastData(c *Client, env protocol.Envelope) {
	var req protocol.BroadcastDataRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	h.broadcasts.data(c.user.ID, req.MatchID, req.Data, req.Frame)
}

func (h *Hub) handleBroadcastEnd(c *Client, env protocol.Envelope) {
	var req protocol.BroadcastEndRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	if h.broadcasts.endIfBroadcaster(req.MatchID, c.user.ID) {
		h.broadcastRoomList()
	}
}

func (h *Hub) handleSpectateStart(c *Client, env protocol.Envelope) {
	var req protocol.SpectateStartRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	h.broadcasts.spectateStart(c, req.MatchID)
}

func (h *Hub) handleSpectateStop(c *Client, env protocol.Envelope) {
	var req protocol.SpectateStopRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	h.broadcasts.spectateStop(c.user.ID, req.MatchID)
}
