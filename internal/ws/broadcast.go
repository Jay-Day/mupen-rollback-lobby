package ws

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/config"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// broadcast is one live stream of a match's .krec. A single player (the
// broadcaster) streams krec bytes up; the server appends them to an ephemeral
// file and fans them out to spectators. The file is deleted when the broadcast
// ends — we keep it on disk (not in RAM) so a late-joining spectator can be
// served the full history from frame 0, then tail the live edge.
type broadcast struct {
	matchID        uint64
	roomID         uint64
	broadcasterID  uint64
	participantIDs []uint64
	manifest       string // immutable base64 prematch settings from the host
	path           string

	mu         sync.Mutex
	file       *os.File
	spectators map[uint64]*broadcastSpectator
	closed     bool
	liveFrame  int   // broadcaster's latest stamped live krec frame
	written    int64 // spool bytes written, enforced against maxSpoolBytes
	// The audience size last sent to the lobby (-1: none yet), so a change
	// that leaves it the same (a viewer re-subscribing) isn't sent to
	// everyone again. Written under roomListMu and mu.
	published int

	// Latest complete savestate keyframe (compressed bytes) + the first krec input
	// record to consume after restore, so a late spectator fast-forwards only the tail.
	// 0 = none yet (spectators fall back to full-spool replay).
	keyframeFrame int
	keyframeData  []byte
	keyframeAt    time.Time // when it was stored
	keyframeSeq   uint64    // counts stored keyframes, so a waiting viewer can tell a newer one
	// In-progress reassembly of a chunked BROADCAST_KEYFRAME upload.
	kfAsmFrame  int
	kfAsmCount  int
	kfAsmGot    int
	kfAsmBytes  int
	kfAsmChunks [][]byte

	// On demand (lobby.json liveReplayKeyframeOnDemand, and the broadcaster
	// said in BROADCAST_BEGIN that it can): the broadcaster uploads a keyframe
	// only when asked. The cached one serves joiners while it's younger than
	// the lobby's keyframe interval; a later joiner waits while a new one is
	// asked for. Fixed for the broadcast's life, so a config change mid-match
	// can't leave a periodic broadcaster waiting for asks, or the reverse.
	keyframeOnDemand bool
	startedAt        time.Time // frame 0 serves as a keyframe stored at this time
	kfRequestedAt    time.Time // last BROADCAST_KEYFRAME_REQUEST; zero = none outstanding
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
	stopped       bool   // protected by broadcast.mu
	subscribed    uint64 // broadcastManager.subSeq at SPECTATE_START; orders a user's subscriptions

	// An on-demand join waiting for a keyframe newer than waitSeq. Only
	// SPECTATE_BEGIN goes out meanwhile, so the cursors above are untouched
	// when one arrives or waitDeadline passes. Protected by broadcast.mu.
	waitingKeyframe bool
	waitSeq         uint64
	waitSince       time.Time
	waitDeadline    time.Time
}

// keyframeChunkBytes is the raw savestate slice size per keyframe message. base64
// (~+33%) plus the JSON envelope must stay under the server read limit (1 MiB).
const keyframeChunkBytes = 512 * 1024

// New clients use small paced chunks so chat can preempt between them; older
// clients used 512 KiB chunks. Bound both the shape and total decoded bytes.
// The total is the largest keyframe a viewer accepts: a 24 MiB state, or its
// compressed form's 10-byte header (LobbyClient.cpp, MAX_KEYFRAME_ENCODED_BYTES).
// Anything larger would be stored and sent only for every viewer to refuse it.
const (
	maxKeyframeChunks = 512
	maxKeyframeBytes  = 24<<20 + 10
)

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

// keyframeRequestTimeout bounds how long an on-demand join waits for the
// broadcaster's new keyframe. Capture waits for rollback to confirm the frame,
// then the client compresses a few MB and uploads them in paced chunks. Past
// it, the viewer starts from what's cached, and the ask counts as unanswered,
// so the next joiner asks again.
const keyframeRequestTimeout = 10 * time.Second

// broadcastManager owns all live broadcasts, keyed by match id.
type broadcastManager struct {
	hub *Hub
	dir string

	mu      sync.Mutex
	byMatch map[uint64]*broadcast

	// presenceMu orders spectating-presence decisions, so one that scanned
	// subscriptions before a newer change can't land after one that saw it.
	presenceMu sync.Mutex

	// subSeq numbers subscriptions, so spectator chat from an older client
	// watching several streams has a stable one to go to.
	subSeq atomic.Uint64
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

// roomBroadcast reports the current broadcast and audience size for room lists.
func (m *broadcastManager) roomBroadcast(roomID uint64) (matchID uint64, ok bool, viewerCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.byMatch {
		if b.roomID == roomID {
			b.mu.Lock()
			count := len(b.spectators)
			b.mu.Unlock()
			return b.matchID, true, count
		}
	}
	return 0, false, 0
}

// begin registers a new broadcast and opens its spool file. Returns false if
// the match is already being broadcast (first broadcaster wins) or the file
// can't be opened. keyframeOnDemand means the broadcaster uploads keyframes
// when asked rather than on a timer.
func (m *broadcastManager) begin(matchID, roomID, broadcasterID uint64, participantIDs []uint64, manifest string,
	keyframeOnDemand bool) bool {
	// Keep the base64 manifest plus its envelope below the 1 MiB WebSocket limit.
	// Empty is allowed for older broadcasters; new viewers reject missing settings.
	const maxManifestBytes = 512 * 1024
	if len(manifest) > base64.StdEncoding.EncodedLen(maxManifestBytes) {
		return false
	}
	if manifest != "" {
		decoded, err := base64.StdEncoding.DecodeString(manifest)
		if err != nil || len(decoded) > maxManifestBytes {
			return false
		}
	}
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
		matchID:          matchID,
		roomID:           roomID,
		broadcasterID:    broadcasterID,
		participantIDs:   append([]uint64(nil), participantIDs...),
		manifest:         manifest,
		path:             path,
		file:             f,
		spectators:       make(map[uint64]*broadcastSpectator),
		keyframeOnDemand: keyframeOnDemand,
		startedAt:        time.Now(),
		published:        -1,
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
	if chunkCount <= 0 || chunkCount > maxKeyframeChunks || chunkIndex < 0 || chunkIndex >= chunkCount ||
		len(raw) == 0 || len(raw) > keyframeChunkBytes {
		return
	}

	// (Re)start reassembly when a new keyframe begins (frame or shape changed).
	if b.kfAsmChunks == nil || b.kfAsmFrame != frame || b.kfAsmCount != chunkCount {
		b.kfAsmFrame = frame
		b.kfAsmCount = chunkCount
		b.kfAsmChunks = make([][]byte, chunkCount)
		b.kfAsmGot = 0
		b.kfAsmBytes = 0
	}
	if b.kfAsmChunks[chunkIndex] == nil {
		if b.kfAsmBytes+len(raw) > maxKeyframeBytes {
			b.kfAsmChunks = nil
			b.kfAsmGot = 0
			b.kfAsmBytes = 0
			return
		}
		b.kfAsmChunks[chunkIndex] = raw
		b.kfAsmGot++
		b.kfAsmBytes += len(raw)
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
	b.keyframeAt = time.Now()
	b.keyframeSeq++
	b.kfAsmChunks = nil
	b.kfAsmGot = 0
	b.kfAsmBytes = 0
	// Any new keyframe answers an outstanding ask: it's as fresh as the one
	// asked for, and the cache is fresh again until it ages out.
	b.kfRequestedAt = time.Time{}
	waiting := 0
	for _, sc := range b.spectators {
		if !sc.waitingKeyframe {
			continue
		}
		waiting++
		select {
		case sc.wake <- struct{}{}:
		default:
		}
	}
	slog.Info("broadcast keyframe stored", "match", matchID, "frame", frame, "bytes", len(buf),
		"waiting_viewers", waiting)
}

// keyframeMaxAge is how long a cached keyframe keeps serving viewers who join
// an on-demand broadcast: the lobby's keyframe interval, read at each join.
func (m *broadcastManager) keyframeMaxAge() time.Duration {
	if m.hub == nil {
		return time.Duration(config.Defaults().LiveReplayKeyframeSeconds) * time.Second
	}
	return time.Duration(m.hub.cfg().LiveReplayKeyframeSeconds) * time.Second
}

// keyframeFresh reports whether an on-demand broadcast can serve a joiner from
// what it has. Until the first keyframe, frame 0 stands in for one stored when
// the broadcast began: replaying from the start then covers no more than a
// cached keyframe could leave to fast-forward, and the broadcaster's emulator
// may still be booting, too early to capture one. Call with b.mu held.
func (b *broadcast) keyframeFresh(now time.Time, maxAge time.Duration) bool {
	if b.keyframeFrame > 0 && len(b.keyframeData) > 0 {
		return now.Sub(b.keyframeAt) < maxAge
	}
	return now.Sub(b.startedAt) < maxAge
}

// claimKeyframeRequest reports whether the caller should ask the broadcaster
// for a keyframe, and records the ask. One ask is outstanding at a time, so
// everyone joining meanwhile waits for the same keyframe; an ask unanswered
// for keyframeRequestTimeout no longer counts. Call with b.mu held.
func (b *broadcast) claimKeyframeRequest(now time.Time) bool {
	if !b.kfRequestedAt.IsZero() && now.Sub(b.kfRequestedAt) < keyframeRequestTimeout {
		return false
	}
	b.kfRequestedAt = now
	return true
}

// requestKeyframe sends BROADCAST_KEYFRAME_REQUEST for an ask claimed at `at`.
// Call without broadcast locks. A full broadcaster queue releases the claim,
// so a waiting viewer's pump asks again on its next tick.
func (m *broadcastManager) requestKeyframe(b *broadcast, at time.Time) {
	var broadcaster *Client
	if m.hub != nil {
		broadcaster = m.hub.getClient(b.broadcasterID)
	}
	if broadcaster == nil {
		return // disconnecting: ending the broadcast ends the wait too
	}
	if broadcaster.trySendEnvelope(protocol.TypeBroadcastKeyframeRequest, "",
		protocol.BroadcastKeyframeRequestPayload{MatchID: b.matchID}) {
		slog.Info("broadcast keyframe requested", "match", b.matchID)
		return
	}
	b.mu.Lock()
	if b.kfRequestedAt.Equal(at) {
		b.kfRequestedAt = time.Time{}
	}
	b.mu.Unlock()
}

// attachKeyframe starts a spectator that has been sent nothing but
// SPECTATE_BEGIN from the cached keyframe: the savestate, the krec header, then
// the spool tail from the keyframe's next input record. The spectator restores
// the savestate and consumes that record first, so catch-up is bounded to the
// tail regardless of how long the match has run. It returns the krec bytes
// that are history at this point, or false when there's no keyframe or its
// record isn't in the spool (yet). Call with b.mu held.
func (b *broadcast) attachKeyframe(sc *broadcastSpectator) (int64, bool) {
	if b.keyframeFrame <= 0 || len(b.keyframeData) == 0 {
		return 0, false
	}
	data, err := os.ReadFile(b.path)
	if err != nil {
		slog.Warn("broadcast: keyframe spool read failed", "path", b.path, "err", err)
	}
	off := krecFrameOffset(data, b.keyframeFrame)
	if off <= 0 {
		return 0, false
	}
	hs := 272
	if len(data) >= 4 && string(data[0:4]) == "KRC1" {
		hs = 400
	}
	sc.headerSize = int64(hs)
	sc.tailStart = off
	sc.inHeader = true
	sc.keyframeFrame = b.keyframeFrame
	sc.keyframeData = b.keyframeData
	return int64(hs) + b.written - off, true
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

// notifyViewerCount snapshots and publishes the latest audience size in the
// same order as room lists. Call without broadcast locks: membership may change
// while waiting to publish, so read the count only after acquiring roomListMu.
func (m *broadcastManager) notifyViewerCount(matchID uint64) {
	if m.hub == nil {
		return
	}
	m.hub.roomListMu.Lock()
	defer m.hub.roomListMu.Unlock()
	m.mu.Lock()
	b := m.byMatch[matchID]
	m.mu.Unlock()
	if b == nil {
		return
	}
	b.mu.Lock()
	payload := protocol.BroadcastViewerCountPayload{
		MatchID:     b.matchID,
		ViewerCount: len(b.spectators),
	}
	unchanged := b.published == payload.ViewerCount
	b.published = payload.ViewerCount
	b.mu.Unlock()
	if unchanged {
		return // everyone already has this count
	}
	m.hub.broadcastToAll(protocol.TypeBroadcastViewerCount, payload, 0)
}

// pumpSpectator runs one ordered producer per viewer. It reads only from the
// durable spool and advances its cursor only after an envelope was accepted by
// the connection queue. Queue pressure therefore delays this spectator without
// ever dropping a byte or blocking the broadcaster and other viewers.
func (m *broadcastManager) pumpSpectator(b *broadcast, userID uint64, sc *broadcastSpectator) {
	ticker := time.NewTicker(spectateRetryInterval)
	defer ticker.Stop()
	// Every way a subscription ends (stop, match over, read error,
	// replacement) ends this pump, so presence is reconciled here.
	defer m.syncSpectatingPresence(userID)

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
	viewersChanged := false
	var askAt time.Time
	b.mu.Lock()
	defer func() {
		b.mu.Unlock()
		if viewersChanged {
			m.notifyViewerCount(b.matchID)
		}
		if !askAt.IsZero() {
			m.requestKeyframe(b, askAt)
		}
	}()
	if b.closed || sc.stopped || b.spectators[userID] != sc {
		return false, false
	}
	if !sc.beginSent {
		if !sc.client.trySendEnvelopeReserved(protocol.TypeSpectateBegin, "",
			protocol.SpectateBeginPayload{MatchID: b.matchID, Manifest: b.manifest}, spectateQueueReserve) {
			return false, true
		}
		sc.beginSent = true
		return true, true
	}

	// An on-demand join holds its stream until the keyframe it's waiting for
	// arrives (keyframe wakes this pump) or its deadline passes (the ticker).
	if sc.waitingKeyframe {
		now := time.Now()
		if b.keyframeSeq != sc.waitSeq {
			if history, ok := b.attachKeyframe(sc); ok {
				sc.waitingKeyframe = false
				slog.Info("spectate start", "mode", "keyframe-on-demand", "match", b.matchID,
					"user", sc.client.user.Username, "history_bytes", history,
					"waited_ms", now.Sub(sc.waitSince).Milliseconds())
				return true, true
			}
			// Its input record hasn't reached the spool yet; the next
			// BROADCAST_DATA wakes this pump to look again.
		}
		if now.Before(sc.waitDeadline) {
			// Ask again only while nothing newer has been stored: once it
			// has, the wait is just for its input record to reach the spool.
			if b.keyframeSeq == sc.waitSeq && b.claimKeyframeRequest(now) {
				askAt = now // the last ask timed out or never reached the broadcaster
			}
			return false, true
		}
		// No keyframe in time: start from what's cached, as a periodic
		// broadcast would.
		sc.waitingKeyframe = false
		mode, history := "full-fallback", b.written
		if n, ok := b.attachKeyframe(sc); ok {
			mode, history = "keyframe-fallback", n
		}
		slog.Info("spectate start", "mode", mode, "match", b.matchID,
			"user", sc.client.user.Username, "history_bytes", history,
			"waited_ms", now.Sub(sc.waitSince).Milliseconds())
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
		viewersChanged = true
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
// chunks form a gap-free, duplicate-free stream. On an on-demand broadcast
// whose cached keyframe is too old, the spectator is registered but held
// after SPECTATE_BEGIN while the broadcaster is asked for a new keyframe.
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

	maxAge := m.keyframeMaxAge()
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
		client:     c,
		done:       make(chan struct{}),
		wake:       make(chan struct{}, 1),
		subscribed: m.subSeq.Add(1),
	}
	mode := "full"
	historyBytes := b.written
	now := time.Now()
	asked := false

	// Prefer the keyframe path (see attachKeyframe). Fall back to the full
	// spool (frame 0) otherwise, e.g. during startup before any keyframe
	// exists. An on-demand broadcast first makes sure what it has is fresh.
	if b.keyframeOnDemand && !b.keyframeFresh(now, maxAge) {
		sc.waitingKeyframe = true
		sc.waitSeq = b.keyframeSeq
		sc.waitSince = now
		sc.waitDeadline = now.Add(keyframeRequestTimeout)
		asked = b.claimKeyframeRequest(now)
	} else if history, ok := b.attachKeyframe(sc); ok {
		mode = "keyframe"
		if b.keyframeOnDemand {
			mode = "keyframe-cached"
		}
		historyBytes = history
	}

	waiting := sc.waitingKeyframe // the pump owns it once unlocked
	b.spectators[c.user.ID] = sc
	viewers := len(b.spectators)
	b.mu.Unlock()
	if asked {
		m.requestKeyframe(b, now)
	}
	if m.hub != nil {
		m.hub.modlog.GameSpectators(matchID, viewers)
	}
	m.notifyViewerCount(matchID)
	go m.pumpSpectator(b, c.user.ID, sc)
	m.syncSpectatingPresence(c.user.ID)
	if waiting {
		// The pump logs "spectate start" once it knows where the stream starts.
		slog.Info("spectate waiting for keyframe", "match", matchID, "user", c.user.Username,
			"asked", asked)
		return
	}
	slog.Info("spectate start", "mode", mode, "match", matchID, "user", c.user.Username,
		"history_bytes", historyBytes)
}

// syncSpectatingPresence shows a lobby-idle user as "spectating" while they
// watch any live stream, and returns them to idle once they watch none. Like
// AFK, it only takes over the "doing nothing" states: a seated, searching or
// playing user keeps their real state. Local players watch with the PC they
// sit at, as they go AFK with it. Call with no broadcast locks held.
func (m *broadcastManager) syncSpectatingPresence(userID uint64) {
	if m.hub == nil {
		return
	}
	m.presenceMu.Lock()
	defer m.presenceMu.Unlock()
	c := m.hub.getClient(userID)
	if c == nil {
		return // disconnected: presence removal already covers them
	}

	watching := m.isWatching(userID)
	for _, u := range c.user.Group() {
		var changed bool
		if watching {
			changed = u.CompareAndSetState([]state.UserState{
				state.UserStateIdle, state.UserStateBrowsing, state.UserStateAway,
			}, state.UserStateSpectating)
		} else {
			changed = u.CompareAndSetState([]state.UserState{state.UserStateSpectating}, state.UserStateIdle)
		}
		if changed {
			m.hub.broadcastPresenceUpdated(u)
		}
	}
}

// settleLobbyState is where a user lands when a room or search ends without
// them: spectating if they still watch a live stream, idle otherwise. Call
// with no broadcast locks held.
func (m *broadcastManager) settleLobbyState(u *state.User) {
	if m == nil {
		u.SetState(state.UserStateIdle)
		return
	}
	m.presenceMu.Lock()
	defer m.presenceMu.Unlock()
	if m.isWatching(u.Primary().ID) {
		u.SetState(state.UserStateSpectating)
	} else {
		u.SetState(state.UserStateIdle)
	}
}

// isWatching reports whether userID is subscribed to any live broadcast.
// Call with no broadcast locks held.
func (m *broadcastManager) isWatching(userID uint64) bool {
	m.mu.Lock()
	live := make([]*broadcast, 0, len(m.byMatch))
	for _, b := range m.byMatch {
		live = append(live, b)
	}
	m.mu.Unlock()
	for _, b := range live {
		b.mu.Lock()
		watching := !b.closed && b.spectators[userID] != nil
		b.mu.Unlock()
		if watching {
			return true
		}
	}
	return false
}

func (m *broadcastManager) spectateStop(userID, matchID uint64) {
	m.mu.Lock()
	b := m.byMatch[matchID]
	m.mu.Unlock()
	if b == nil {
		return
	}
	viewersChanged := false
	b.mu.Lock()
	if sc := b.spectators[userID]; sc != nil {
		stopBroadcastSpectator(sc)
		delete(b.spectators, userID)
		viewersChanged = true
	}
	b.mu.Unlock()
	if viewersChanged {
		m.notifyViewerCount(matchID)
	}
}

// spectatorChat relays a chat line to everyone on a live replay's chat: its
// viewers, the sender included so it echoes like room chat, and the match's
// players, who can answer. A viewer whose client still says the players can't read
// this keeps that promise: their lines reach the other viewers only. A sender who's
// neither watching nor playing a match being broadcast is ignored. toMatchID is
// the match the line is for, zero when the client doesn't say (see chatBroadcastOf).
//
// It reports the match and room of the broadcast the line reached (for the
// moderators' log), ok false when it went nowhere.
func (m *broadcastManager) spectatorChat(c *Client, toMatchID uint64, payload protocol.ChatMsgPayload) (matchID, roomID uint64, ok bool) {
	b, fromPlayer := m.chatBroadcastOf(c, toMatchID)
	if b == nil {
		return 0, 0, false
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, 0, false
	}
	recipients := make(map[*Client]struct{}, len(b.spectators)+maxRoomPlayers)
	for _, sc := range b.spectators {
		recipients[sc.client] = struct{}{}
	}
	b.mu.Unlock()
	if c.user.SpectatorChat {
		for _, player := range m.chatPlayersOf(b) {
			recipients[player] = struct{}{}
		}
	}
	payload.FromPlayer = fromPlayer
	payload.MatchID = b.matchID // a client watching several files the line under this one
	for recipient := range recipients {
		recipient.sendEnvelope(protocol.TypeChatMsg, "", payload)
	}
	return b.matchID, b.roomID, true
}

// chatBroadcastOf finds the broadcast whose chat c's line is for, and whether c is
// on it as one of the match's players rather than as a viewer. A line naming its
// match (toMatchID) goes there if c plays or watches it, and otherwise nowhere:
// never to another stream c watches. One that doesn't, from an older client, goes
// to the match c plays, else to the stream c subscribed to most recently, which
// is the one an older client shows if it never stopped an earlier one.
func (m *broadcastManager) chatBroadcastOf(c *Client, toMatchID uint64) (*broadcast, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if match := c.user.CurrentMatch; match != nil && c.user.SpectatorChat && (toMatchID == 0 || toMatchID == match.ID) {
		if b := m.byMatch[match.ID]; b != nil {
			return b, true
		}
	}
	if toMatchID != 0 {
		b := m.byMatch[toMatchID]
		if b == nil {
			return nil, false
		}
		b.mu.Lock()
		watching := b.spectators[c.user.ID] != nil
		b.mu.Unlock()
		if !watching {
			return nil, false
		}
		return b, false
	}
	var latest *broadcast
	var latestSub uint64
	for _, b := range m.byMatch {
		b.mu.Lock()
		if sc := b.spectators[c.user.ID]; sc != nil && (latest == nil || sc.subscribed > latestSub) {
			latest, latestSub = b, sc.subscribed
		}
		b.mu.Unlock()
	}
	return latest, false
}

// chatPlayersOf returns the connections playing b's match right now whose clients
// can show its chat. Local players go through their owner's connection.
func (m *broadcastManager) chatPlayersOf(b *broadcast) []*Client {
	room := m.hub.state.GetRoom(b.roomID)
	if room == nil {
		return nil
	}
	var out []*Client
	for _, seat := range room.Players {
		u := seat.User
		if u == nil || u.Owner != nil || !u.SpectatorChat || u.CurrentMatch == nil || u.CurrentMatch.ID != b.matchID {
			continue
		}
		if player := m.hub.getClient(u.ID); player != nil {
			out = append(out, player)
		}
	}
	return out
}

// chatTarget names the broadcast a spectator-chat line from c would reach, for
// the moderators' log of lines blocked before they were sent; zero for none.
func (m *broadcastManager) chatTarget(c *Client, toMatchID uint64) (matchID, roomID uint64) {
	if m == nil {
		return 0, 0
	}
	if b, _ := m.chatBroadcastOf(c, toMatchID); b != nil {
		return b.matchID, b.roomID
	}
	return 0, 0
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
		m.hub.roomListMu.Lock()
		notified := make(map[uint64]struct{}, len(b.participantIDs)+1)
		for _, userID := range b.participantIDs {
			if _, exists := notified[userID]; exists {
				continue
			}
			notified[userID] = struct{}{}
			if participant := m.hub.getClient(userID); participant != nil {
				participant.sendEnvelope(protocol.TypeBroadcastViewerCount, "",
					protocol.BroadcastViewerCountPayload{MatchID: b.matchID, ViewerCount: 0})
			}
		}
		if _, exists := notified[b.broadcasterID]; !exists {
			if broadcaster := m.hub.getClient(b.broadcasterID); broadcaster != nil {
				broadcaster.sendEnvelope(protocol.TypeBroadcastViewerCount, "",
					protocol.BroadcastViewerCountPayload{MatchID: b.matchID, ViewerCount: 0})
			}
		}
		m.hub.roomListMu.Unlock()
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
		viewersChanged := false
		b.mu.Lock()
		if sc := b.spectators[userID]; sc != nil {
			stopBroadcastSpectator(sc)
			delete(b.spectators, userID)
			viewersChanged = true
		}
		b.mu.Unlock()
		if viewersChanged {
			m.notifyViewerCount(b.matchID)
		}
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
	// One live replay per match: clients send BROADCAST_BEGIN once, at
	// MATCH_BEGIN, and each one (with the BROADCAST_END that follows) reaches
	// every player twice. There is no failure reply; the host's client never
	// asks again anyway.
	if match.LiveReplayStarted() {
		if log, more := liveReplayLog.Allow(c.ip); log {
			slog.Info("BROADCAST_BEGIN refused: this match's live replay already started",
				"user", c.user.Username, "ip", c.ip, "match", match.ID, "more_since_last_line", more)
		}
		return
	}
	participantIDs := make([]uint64, 0, len(match.Players))
	for _, player := range match.Players {
		if player != nil {
			participantIDs = append(participantIDs, player.ID)
		}
	}
	if h.broadcasts.begin(match.ID, match.RoomID, c.user.ID, participantIDs, req.Manifest, req.KeyframeOnDemand) {
		match.MarkLiveReplayStarted()
		slog.Info("broadcast begin", "match", match.ID, "by", c.user.Username,
			"keyframe_on_demand", req.KeyframeOnDemand)
		// Older hosts do not publish the pre-match room setting. Once their
		// broadcast actually starts, reconcile it so newer room members never
		// display an unchecked Live Replay box for an active stream.
		if room.SetLiveReplayEnabled(true) {
			h.broadcastRoomState(room)
		}
		h.broadcasts.notifyViewerCount(match.ID)
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
	// Each start streams the keyframe and the match so far (megabytes) and
	// reaches every player (viewer count, presence). A refused client closes
	// its viewer ("Couldn't watch: <reason>"), so end any subscription it has
	// to this match too, keeping both sides in agreement.
	if ok, retry := h.allowAction(c, ratelimit.ActionSpectate); !ok {
		h.broadcasts.spectateStop(c.user.ID, req.MatchID)
		c.sendEnvelope(protocol.TypeSpectateFail, "", protocol.SpectateFailPayload{
			MatchID: req.MatchID, Reason: tooFast("You're opening live replays too fast.", retry),
		})
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
