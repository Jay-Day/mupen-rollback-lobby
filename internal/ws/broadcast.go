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
	liveFrame  int   // broadcaster's latest stamped live krec frame
	written    int64 // spool bytes written, enforced against maxSpoolBytes

	// Latest complete savestate keyframe (compressed bytes) + the krec input-frame
	// index it corresponds to, so a late spectator can restore here and fast-forward
	// only the tail. 0 frame = none yet (spectators fall back to full-spool replay).
	keyframeFrame int
	keyframeData  []byte
	// In-progress reassembly of a chunked BROADCAST_KEYFRAME upload.
	kfAsmFrame  int
	kfAsmCount  int
	kfAsmGot    int
	kfAsmChunks [][]byte
}

// keyframeChunkBytes is the raw savestate slice size per keyframe message. base64
// (~+33%) plus the JSON envelope must stay under the server read limit (1 MiB).
const keyframeChunkBytes = 512 * 1024

// maxKeyframeChunks bounds the chunk count a broadcaster may declare. A real
// savestate (~17 MB uncompressed) is ~33 chunks; anything beyond this is a
// hostile client trying to make the server allocate a huge reassembly buffer.
const maxKeyframeChunks = 64

// maxSpoolBytes caps one broadcast's spool file. A krec is input records — even
// an hours-long 4-player match is a few MB — so hitting this means a hostile or
// runaway broadcaster, and the broadcast is ended rather than filling the disk.
const maxSpoolBytes = 64 << 20

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
	if b.closed || b.broadcasterID != senderID {
		b.mu.Unlock()
		return
	}
	b.written += int64(len(raw))
	if b.written > maxSpoolBytes {
		// Can't finish while holding b.mu (finish re-locks it); end outside.
		b.mu.Unlock()
		slog.Warn("broadcast: spool size limit hit, ending broadcast",
			"match", matchID, "bytes", b.written)
		if m.endByMatch(matchID) {
			m.hub.broadcastRoomList()
		}
		return
	}
	defer b.mu.Unlock()
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

// keyframe reassembles a chunked savestate keyframe uploaded by the broadcaster
// and, once complete, commits it as the latest keyframe (frame + bytes). Only the
// registered broadcaster may write.
func (m *broadcastManager) keyframe(senderID, matchID uint64, frame, chunkIndex, chunkCount int, b64 string) {
	m.mu.Lock()
	b := m.byMatch[matchID]
	m.mu.Unlock()
	if b == nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.broadcasterID != senderID {
		return
	}
	if chunkCount <= 0 || chunkCount > maxKeyframeChunks || chunkIndex < 0 || chunkIndex >= chunkCount {
		return
	}

	// (Re)start reassembly when a new keyframe begins (frame or shape changed).
	if b.kfAsmChunks == nil || b.kfAsmFrame != frame || b.kfAsmCount != chunkCount {
		b.kfAsmFrame = frame
		b.kfAsmCount = chunkCount
		b.kfAsmChunks = make([][]byte, chunkCount)
		b.kfAsmGot = 0
	}
	if b.kfAsmChunks[chunkIndex] == nil {
		b.kfAsmChunks[chunkIndex] = raw
		b.kfAsmGot++
	}
	if b.kfAsmGot < b.kfAsmCount {
		return
	}

	// Complete — assemble and commit, then drop the in-progress buffers.
	total := 0
	for _, c := range b.kfAsmChunks {
		total += len(c)
	}
	buf := make([]byte, 0, total)
	for _, c := range b.kfAsmChunks {
		buf = append(buf, c...)
	}
	b.keyframeFrame = frame
	b.keyframeData = buf
	b.kfAsmChunks = nil
	b.kfAsmGot = 0
	slog.Info("broadcast keyframe stored", "match", matchID, "frame", frame, "bytes", len(buf))
}

// krecFrameOffset returns the byte offset of the (frame)-th input (0x12) record in
// a krec buffer, or -1 if not present yet. Mirrors the client's record grammar:
// KRC1=400 / KRC0=272 header, then 0x12 <u16 len> data | 0x14 nick\0 +4 | 0x08 nick\0 msg\0.
func krecFrameOffset(data []byte, frame int) int64 {
	n := len(data)
	if frame < 0 || n < 272 {
		return -1
	}
	hs := 272
	if n >= 4 && string(data[0:4]) == "KRC1" {
		hs = 400
	}
	if n < hs {
		return -1
	}
	count := 0
	p := hs
	for p < n {
		switch data[p] {
		case 0x12:
			if p+3 > n {
				return -1
			}
			if count == frame {
				return int64(p)
			}
			rlen := int(data[p+1]) | int(data[p+2])<<8
			p += 3 + rlen
			count++
		case 0x14:
			z := p + 1
			for z < n && data[z] != 0 {
				z++
			}
			if z >= n || z+1+4 > n {
				return -1
			}
			p = z + 1 + 4
		case 0x08:
			z := p + 1
			for z < n && data[z] != 0 {
				z++
			}
			if z >= n {
				return -1
			}
			z2 := z + 1
			for z2 < n && data[z2] != 0 {
				z2++
			}
			if z2 >= n {
				return -1
			}
			p = z2 + 1
		default:
			return -1
		}
	}
	return -1
}

// sendKeyframeChunks streams a stored savestate to one spectator as chunked
// SPECTATE_KEYFRAME messages it can reassemble (same chunk size as the uplink).
func sendKeyframeChunks(c *Client, matchID uint64, frame int, data []byte) {
	total := (len(data) + keyframeChunkBytes - 1) / keyframeChunkBytes
	if total < 1 {
		total = 1
	}
	for i := 0; i < total; i++ {
		start := i * keyframeChunkBytes
		end := start + keyframeChunkBytes
		if end > len(data) {
			end = len(data)
		}
		c.sendEnvelope(protocol.TypeSpectateKeyframe, "", protocol.SpectateKeyframePayload{
			MatchID:    matchID,
			Frame:      frame,
			ChunkIndex: i,
			ChunkCount: total,
			Data:       base64.StdEncoding.EncodeToString(data[start:end]),
		})
	}
}

// sendSpectateDataChunked streams a krec byte slice to one spectator as one or more
// SPECTATE_DATA messages, each small enough to stay under the WS read limit. The
// spectator appends chunks in order, so splitting at arbitrary byte boundaries is
// fine — a record straddling two chunks completes when the second arrives. Sending
// the whole spool as a single message blows past the 1 MiB limit on a long match and
// gets silently dropped, leaving the spectator with no krec at all.
func sendSpectateDataChunked(c *Client, matchID uint64, data []byte, liveFrame int) {
	for off := 0; off < len(data); off += keyframeChunkBytes {
		end := off + keyframeChunkBytes
		if end > len(data) {
			end = len(data)
		}
		c.sendEnvelope(protocol.TypeSpectateData, "", protocol.SpectateDataPayload{
			MatchID: matchID,
			Data:    base64.StdEncoding.EncodeToString(data[off:end]),
			Frame:   liveFrame,
		})
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

	// Prefer the keyframe path: if we hold a complete keyframe whose frame is
	// present in the spool, send the savestate + only the krec [header + tail from
	// that frame]. The spectator restores the savestate and its first replayed
	// input is that frame, so catch-up is bounded to the tail regardless of how
	// long the match has run. Fall back to the full spool (frame 0) otherwise —
	// e.g. in the first ~60s before any keyframe exists.
	if b.keyframeFrame > 0 && len(b.keyframeData) > 0 && len(data) > 0 {
		if off := krecFrameOffset(data, b.keyframeFrame); off > 0 {
			hs := 272
			if len(data) >= 4 && string(data[0:4]) == "KRC1" {
				hs = 400
			}
			tail := make([]byte, 0, hs+(len(data)-int(off)))
			tail = append(tail, data[:hs]...)   // header so the spectator can parse it
			tail = append(tail, data[off:]...)  // krec from the keyframe frame onward

			sendKeyframeChunks(c, matchID, b.keyframeFrame, b.keyframeData)
			sendSpectateDataChunked(c, matchID, tail, b.liveFrame)
			b.spectators[c.user.ID] = c
			slog.Info("spectate start (keyframe)", "match", matchID, "user", c.user.Username,
				"frame", b.keyframeFrame, "savestate_bytes", len(b.keyframeData), "tail_bytes", len(tail))
			return
		}
	}

	if len(data) > 0 {
		// Chunked so a long match's full history (well over the 1 MiB WS limit) is
		// actually delivered instead of dropped. Frame = live edge so the joiner can
		// fast-forward to it.
		sendSpectateDataChunked(c, matchID, data, b.liveFrame)
	}
	b.spectators[c.user.ID] = c
	slog.Info("spectate start (full)", "match", matchID, "user", c.user.Username, "history_bytes", len(data))
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

func (h *Hub) handleBroadcastKeyframe(c *Client, env protocol.Envelope) {
	var req protocol.BroadcastKeyframeRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	h.broadcasts.keyframe(c.user.ID, req.MatchID, req.Frame, req.ChunkIndex, req.ChunkCount, req.Data)
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
