package ws

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// Ranked set replays. Both players record the same .krec, so the first
// player to offer one uploads it and every later offer is declined — a set
// costs one upload. Replays are a few hundred KB of controller input, stored
// on disk as <dir>/<setId>.krec.
const (
	maxReplayBytes     = 16 << 20
	replayChunkBytes   = 256 << 10
	maxReplayBookmarks = 64
	// An accepted offer that stops sending chunks frees the set for others.
	replayClaimTimeout = 2 * time.Minute
)

type replayUpload struct {
	userID    uint64
	accountID int64
	size      int
	sha256    string
	bookmarks string
	chunks    [][]byte
	received  int
	started   time.Time
}

// replayUploads tracks in-flight uploads by set id. The zero value is ready.
type replayUploads struct {
	mu      sync.Mutex
	pending map[int64]*replayUpload
}

// SetReplayDir enables replay uploads into dir (created if missing).
func (h *Hub) SetReplayDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	h.replayDir = dir
	return nil
}

func (c *Client) declineReplay(setID int64, reason string) {
	c.sendEnvelope(protocol.TypeReplayDecline, "", protocol.ReplayDeclinePayload{SetID: setID, Reason: reason})
}

func validBookmarks(bookmarks []protocol.ReplayBookmark) bool {
	if len(bookmarks) > maxReplayBookmarks {
		return false
	}
	for _, b := range bookmarks {
		if b.Record < 0 || len(b.Label) > 80 || len(b.Kind) > 16 || b.Game < 0 || b.Game > 99 {
			return false
		}
	}
	return true
}

func (h *Hub) handleReplayOffer(c *Client, env protocol.Envelope) {
	var req protocol.ReplayOfferRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	if h.accounts == nil || h.replayDir == "" {
		c.declineReplay(req.SetID, "unavailable")
		return
	}
	if !c.user.Verified() {
		c.declineReplay(req.SetID, "not_participant")
		return
	}
	sum, err := hex.DecodeString(req.SHA256)
	if req.Size <= 0 || req.Size > maxReplayBytes || err != nil || len(sum) != sha256.Size ||
		!validBookmarks(req.Bookmarks) {
		c.declineReplay(req.SetID, "invalid")
		return
	}
	participant, err := h.accounts.IsParticipant(req.SetID, c.user.AccountID)
	if err != nil || !participant {
		c.declineReplay(req.SetID, "not_participant")
		return
	}
	if _, err := h.accounts.Replay(req.SetID); err == nil {
		c.declineReplay(req.SetID, "exists")
		return
	}
	bookmarks, _ := json.Marshal(req.Bookmarks)
	if req.Bookmarks == nil {
		bookmarks = []byte("[]")
	}

	h.replays.mu.Lock()
	if h.replays.pending == nil {
		h.replays.pending = make(map[int64]*replayUpload)
	}
	if existing := h.replays.pending[req.SetID]; existing != nil &&
		existing.userID != c.user.ID && time.Since(existing.started) < replayClaimTimeout {
		h.replays.mu.Unlock()
		c.declineReplay(req.SetID, "in_progress")
		return
	}
	count := (req.Size + replayChunkBytes - 1) / replayChunkBytes
	h.replays.pending[req.SetID] = &replayUpload{
		userID: c.user.ID, accountID: c.user.AccountID, size: req.Size,
		sha256: strings.ToLower(req.SHA256), bookmarks: string(bookmarks),
		chunks: make([][]byte, count), started: time.Now(),
	}
	h.replays.mu.Unlock()

	slog.Info("replay upload accepted", "set", req.SetID, "user", c.user.Username, "bytes", req.Size)
	c.sendEnvelope(protocol.TypeReplayAccept, "", protocol.ReplayAcceptPayload{SetID: req.SetID, ChunkSize: replayChunkBytes})
}

func (h *Hub) handleReplayChunk(c *Client, env protocol.Envelope) {
	var req protocol.ReplayChunkRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}

	h.replays.mu.Lock()
	upload := h.replays.pending[req.SetID]
	if upload == nil || upload.userID != c.user.ID {
		h.replays.mu.Unlock()
		return
	}
	fail := func(reason string) {
		delete(h.replays.pending, req.SetID)
		h.replays.mu.Unlock()
		c.declineReplay(req.SetID, reason)
	}
	if req.Index < 0 || req.Index >= len(upload.chunks) || req.Count != len(upload.chunks) {
		fail("invalid")
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil || len(data) == 0 || len(data) > replayChunkBytes {
		fail("invalid")
		return
	}
	if upload.chunks[req.Index] == nil {
		upload.received++
	}
	upload.chunks[req.Index] = data
	if upload.received < len(upload.chunks) {
		h.replays.mu.Unlock()
		return
	}
	delete(h.replays.pending, req.SetID)
	h.replays.mu.Unlock()

	if err := h.storeReplay(req.SetID, upload); err != nil {
		slog.Warn("replay upload rejected", "set", req.SetID, "user", c.user.Username, "err", err)
		reason := "failed"
		if errors.Is(err, accounts.ErrReplayExists) {
			reason = "exists"
		}
		c.declineReplay(req.SetID, reason)
		return
	}
	slog.Info("replay stored", "set", req.SetID, "user", c.user.Username, "bytes", upload.size)
	c.sendEnvelope(protocol.TypeReplayStored, "", protocol.ReplayStoredPayload{SetID: req.SetID})
}

// storeReplay verifies the assembled bytes and writes them atomically.
func (h *Hub) storeReplay(setID int64, upload *replayUpload) error {
	data := make([]byte, 0, upload.size)
	for _, chunk := range upload.chunks {
		data = append(data, chunk...)
	}
	if len(data) != upload.size {
		return fmt.Errorf("size %d, offered %d", len(data), upload.size)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != upload.sha256 {
		return errors.New("sha256 mismatch")
	}

	if _, err := h.accounts.Replay(setID); err == nil {
		return accounts.ErrReplayExists // never overwrite a stored replay
	}
	path := accounts.ReplayFile(h.replayDir, setID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := h.accounts.AddReplay(accounts.ReplayInfo{
		SetID: setID, Size: int64(upload.size), SHA256: upload.sha256,
		Bookmarks: upload.bookmarks, UploaderID: upload.accountID,
	}); err != nil {
		if !errors.Is(err, accounts.ErrReplayExists) {
			os.Remove(path)
		}
		return err
	}
	return nil
}
