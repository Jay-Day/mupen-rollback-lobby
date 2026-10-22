package ws

import (
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

const (
	channelLobby      = "lobby"
	channelRoom       = "room"
	channelSpectate   = "spectate" // viewers of one live replay; never reaches its players
	lobbyHistorySize  = 50
	maxChatMessageLen = 500
)

// chatHistory keeps a fixed-size ring of recent messages per channel.
// Currently only the "lobby" channel is persisted; room and PM messages are live-only.
type chatHistory struct {
	mu       sync.Mutex
	messages []protocol.ChatMsgPayload
}

func newChatHistory() *chatHistory {
	return &chatHistory{
		messages: make([]protocol.ChatMsgPayload, 0, lobbyHistorySize),
	}
}

func (h *chatHistory) Append(msg protocol.ChatMsgPayload) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) >= lobbyHistorySize {
		// Drop oldest by shifting; ring buffers are nicer but this is fine at N=50.
		copy(h.messages, h.messages[1:])
		h.messages = h.messages[:len(h.messages)-1]
	}
	h.messages = append(h.messages, msg)
}

func (h *chatHistory) Snapshot() []protocol.ChatMsgPayload {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]protocol.ChatMsgPayload, len(h.messages))
	copy(out, h.messages)
	return out
}

// truncateMessage clips overlong chat input. We do not reject — be lenient.
func truncateMessage(s string) string {
	if len(s) <= maxChatMessageLen {
		return s
	}
	return s[:maxChatMessageLen]
}

// validChannel reports whether a channel name is accepted right now.
// PM channels will be added later.
func validChannel(c string) bool {
	return c == channelLobby || c == channelRoom || c == channelSpectate
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
