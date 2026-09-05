package ws

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	spectators map[uint64]*broadcastSpectator
	closed     bool
	liveFrame  int   // broadcaster's latest stamped live krec frame
	written    int64 // spool bytes written, enforced against maxSpoolBytes

	// Latest complete savestate keyframe (compressed bytes) + the first krec input
	// record to consume after restore, so a late spectator fast-forwards only the tail.
	// 0 = none yet (spectators fall back to full-spool replay).
	keyframeFrame int
	keyframeData  []byte
	// In-progress reassembly of a chunked BROADCAST_KEYFRAME upload.
	kfAsmFrame  int
	kfAsmCount  int
	kfAsmGot    int
	kfAsmChunks [][]byte
}

// broadcastSpectator owns an ordered cursor into the broadcast spool. Logical
// offsets are contiguous in the krec stream delivered to this viewer. They are
// normally identical to spool offsets; for a keyframe join they cover the krec
// header followed by the spool tail at tailStart.
type broadcastSpectator struct {
	client *Client
	done   chan struct{}
	wake   chan struct{}

	streamOffset int64
	spoolOffset  int64
	headerSize   int64
	tailStart    int64
	inHeader     bool

	keyframeFrame int
	keyframeData  []byte
	keyframeChunk int
	beginSent     bool
	stopped       bool // protected by broadcast.mu
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

// History is paced through the ordinary client send queue. Smaller chunks cap
// queued replay memory, while the reserve prevents a large catch-up from
// starving chat, heartbeat, and match-control messages on the same connection.
const (
	spectateDataChunkBytes = 64 << 10
	spectateQueueReserve   = 8
	spectateRetryInterval  = 20 * time.Millisecond
)

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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR|os.O_APPEND, 0o644)
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
		spectators:    make(map[uint64]*broadcastSpectator),
	}
	return true
}

// data appends a krec chunk to the spool file and wakes each spectator's spool
// pump. A slow/full client queue never advances that spectator's cursor, so a
// later pump attempt retries the exact missing bytes instead of skipping them.
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
	newWritten := b.written + int64(len(raw))
	if newWritten > maxSpoolBytes {
		// Can't finish while holding b.mu (finish re-locks it); end outside.
		b.mu.Unlock()
		slog.Warn("broadcast: spool size limit hit, ending broadcast",
			"match", matchID, "bytes", newWritten)
		if m.endByMatch(matchID) {
			m.hub.broadcastRoomList()
		}
		return
	}
	written, writeErr := b.file.Write(raw)
	if writeErr != nil || written != len(raw) {
		b.mu.Unlock()
		slog.Warn("broadcast: spool write failed; ending broadcast", "match", matchID,
			"bytes", len(raw), "written", written, "err", writeErr)
		if m.endByMatch(matchID) {
			m.hub.broadcastRoomList()
		}
		return
	}
	b.written = newWritten
	if frame > b.liveFrame {
		b.liveFrame = frame // track the live edge for spectate fast-forward
	}
	for _, sc := range b.spectators {
		select {
		case sc.wake <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
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
// a krec buffer. It also returns EOF when frame is exactly one past the final complete
// record, allowing a freshly captured keyframe to wait for its next live input.
// Otherwise it returns -1. Mirrors the client's record grammar:
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
	if count == frame {
		return int64(p) // the next input is one past the current complete tail
	}
	return -1
}

func stopBroadcastSpectator(sc *broadcastSpectator) {
	if sc == nil || sc.stopped {
		return
	}
	sc.stopped = true
	close(sc.done)
}

// notifyViewerCountLocked publishes the current authoritative audience size to
// the broadcaster and every active viewer. Call with b.mu held so membership
// mutations and their notifications retain the same order.
func (m *broadcastManager) notifyViewerCountLocked(b *broadcast) {
	if m.hub == nil {
		return
	}
	payload := protocol.BroadcastViewerCountPayload{
		MatchID:     b.matchID,
		ViewerCount: len(b.spectators),
	}
	if broadcaster := m.hub.getClient(b.broadcasterID); broadcaster != nil {
		broadcaster.sendEnvelope(protocol.TypeBroadcastViewerCount, "", payload)
	}
	for userID, sc := range b.spectators {
		if userID != b.broadcasterID {
			sc.client.sendEnvelope(protocol.TypeBroadcastViewerCount, "", payload)
		}
	}
}

// pumpSpectator runs one ordered producer per viewer. It reads only from the
// durable spool and advances its cursor only after an envelope was accepted by
// the connection queue. Queue pressure therefore delays this spectator without
// ever dropping a byte or blocking the broadcaster and other viewers.
func (m *broadcastManager) pumpSpectator(b *broadcast, userID uint64, sc *broadcastSpectator) {
	ticker := time.NewTicker(spectateRetryInterval)
	defer ticker.Stop()

	for {
		for {
			sent, active := m.pumpSpectatorChunk(b, userID, sc)
			if !active {
				return
			}
			if !sent {
				break
			}
		}

		select {
		case <-sc.done:
			return
		case <-sc.wake:
		case <-ticker.C:
		}
	}
}

// pumpSpectatorChunk attempts one keyframe or krec message. Both cursor state
// and enqueue happen while holding b.mu, keeping stream order atomic with
// spectate stop/end and with replacement subscriptions from the same user.
func (m *broadcastManager) pumpSpectatorChunk(b *broadcast, userID uint64, sc *broadcastSpectator) (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || sc.stopped || b.spectators[userID] != sc {
		return false, false
	}
	if !sc.beginSent {
		if !sc.client.trySendEnvelopeReserved(protocol.TypeSpectateBegin, "",
			protocol.SpectateBeginPayload{MatchID: b.matchID}, spectateQueueReserve) {
			return false, true
		}
		sc.beginSent = true
		return true, true
	}

	if len(sc.keyframeData) > 0 {
		total := (len(sc.keyframeData) + spectateDataChunkBytes - 1) / spectateDataChunkBytes
		if sc.keyframeChunk < total {
			start := sc.keyframeChunk * spectateDataChunkBytes
			end := start + spectateDataChunkBytes
			if end > len(sc.keyframeData) {
				end = len(sc.keyframeData)
			}
			if !sc.client.trySendEnvelopeReserved(protocol.TypeSpectateKeyframe, "",
				protocol.SpectateKeyframePayload{
					MatchID:    b.matchID,
					Frame:      sc.keyframeFrame,
					ChunkIndex: sc.keyframeChunk,
					ChunkCount: total,
					Data:       base64.StdEncoding.EncodeToString(sc.keyframeData[start:end]),
				}, spectateQueueReserve) {
				return false, true
			}
			sc.keyframeChunk++
			return true, true
		}
		sc.keyframeData = nil
	}

	// A keyframe stream first reproduces the krec header, then jumps its spool
	// cursor to the keyframe's input record. Its public stream offset remains
	// contiguous across that intentional source-file jump.
	if sc.inHeader && sc.spoolOffset >= sc.headerSize {
		sc.spoolOffset = sc.tailStart
		sc.inHeader = false
	}
	limit := b.written
	if sc.inHeader {
		limit = sc.headerSize
	}
	if sc.spoolOffset >= limit {
		return false, true // caught up; wait for the next broadcast write
	}

	n := limit - sc.spoolOffset
	if n > spectateDataChunkBytes {
		n = spectateDataChunkBytes
	}
	raw := make([]byte, int(n))
	read, err := b.file.ReadAt(raw, sc.spoolOffset)
	if err != nil || read != len(raw) {
		slog.Warn("spectate spool read failed", "match", b.matchID, "user", userID,
			"offset", sc.spoolOffset, "bytes", len(raw), "read", read, "err", err)
		stopBroadcastSpectator(sc)
		delete(b.spectators, userID)
		sc.client.sendEnvelope(protocol.TypeSpectateEnd, "", protocol.SpectateEndPayload{
			MatchID: b.matchID, Reason: "stream_read_error",
		})
		m.notifyViewerCountLocked(b)
		return false, false
	}

	if !sc.client.trySendEnvelopeReserved(protocol.TypeSpectateData, "",
		protocol.SpectateDataPayload{
			MatchID: b.matchID,
			Data:    base64.StdEncoding.EncodeToString(raw),
			Frame:   b.liveFrame,
			Offset:  sc.streamOffset,
		}, spectateQueueReserve) {
		return false, true
	}
	sc.spoolOffset += n
	sc.streamOffset += n
	return true, true
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
	if b.closed {
		b.mu.Unlock()
		c.sendEnvelope(protocol.TypeSpectateFail, "", protocol.SpectateFailPayload{
			MatchID: matchID, Reason: "ended",
		})
		return
	}

	if old := b.spectators[c.user.ID]; old != nil {
		stopBroadcastSpectator(old)
	}
	sc := &broadcastSpectator{
		client: c,
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
	}
	mode := "full"
	historyBytes := b.written

	// Prefer the keyframe path: if we hold a complete keyframe whose next input is
	// locatable in the spool, send the savestate + only the krec [header + tail from
	// that input]. The spectator restores the savestate and consumes that record
	// first, so catch-up is bounded to the tail regardless of how
	// long the match has run. Fall back to the full spool (frame 0) otherwise —
	// e.g. during startup before any keyframe exists.
	if b.keyframeFrame > 0 && len(b.keyframeData) > 0 {
		data, err := os.ReadFile(b.path)
		if err != nil {
			slog.Warn("broadcast: keyframe spool read failed", "path", b.path, "err", err)
		}
		if off := krecFrameOffset(data, b.keyframeFrame); off > 0 {
			hs := 272
			if len(data) >= 4 && string(data[0:4]) == "KRC1" {
				hs = 400
			}
			sc.headerSize = int64(hs)
			sc.tailStart = off
			sc.inHeader = true
			sc.keyframeFrame = b.keyframeFrame
			sc.keyframeData = b.keyframeData
			mode = "keyframe"
			historyBytes = int64(hs) + b.written - off
		}
	}

	b.spectators[c.user.ID] = sc
	m.notifyViewerCountLocked(b)
	b.mu.Unlock()
	go m.pumpSpectator(b, c.user.ID, sc)
	slog.Info("spectate start", "mode", mode, "match", matchID, "user", c.user.Username,
		"history_bytes", historyBytes)
}

func (m *broadcastManager) spectateStop(userID, matchID uint64) {
	m.mu.Lock()
	b := m.byMatch[matchID]
	m.mu.Unlock()
	if b == nil {
		return
	}
	b.mu.Lock()
	if sc := b.spectators[userID]; sc != nil {
		stopBroadcastSpectator(sc)
		delete(b.spectators, userID)
		m.notifyViewerCountLocked(b)
	}
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
	b.spectators = make(map[uint64]*broadcastSpectator)
	for _, sc := range specs {
		stopBroadcastSpectator(sc)
	}
	b.mu.Unlock()

	for _, sc := range specs {
		sc.client.sendEnvelope(protocol.TypeSpectateEnd, "", protocol.SpectateEndPayload{
			MatchID: b.matchID, Reason: reason,
		})
	}
	if m.hub != nil {
		if broadcaster := m.hub.getClient(b.broadcasterID); broadcaster != nil {
			broadcaster.sendEnvelope(protocol.TypeBroadcastViewerCount, "",
				protocol.BroadcastViewerCountPayload{MatchID: b.matchID, ViewerCount: 0})
		}
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
		if sc := b.spectators[userID]; sc != nil {
			stopBroadcastSpectator(sc)
			delete(b.spectators, userID)
			m.notifyViewerCountLocked(b)
		}
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
		c.sendEnvelope(protocol.TypeBroadcastViewerCount, "", protocol.BroadcastViewerCountPayload{
			MatchID: match.ID, ViewerCount: 0,
		})
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
