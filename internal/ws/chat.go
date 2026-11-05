package ws

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	channelLobby      = "lobby"
	channelRoom       = "room"
	channelSpectate   = "spectate" // one live replay's viewers, and the players of its match
	channelPM         = "pm"       // one person to another; live only, never stored
	lobbyHistorySize  = 50
	maxChatMessageLen = 500
)

// chatHistory keeps a fixed-size ring of recent messages per channel.
// Only the "lobby" channel is persisted; room and PM messages are live-only.
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
	return clipRunes(s, maxChatMessageLen)
}

// bidiControls reorder the text around them. In a line that also shows someone's
// name, that lets a message rearrange what other players read.
var bidiControls = strings.NewReplacer(
	"\u202a", "", "\u202b", "", "\u202c", "", "\u202d", "", "\u202e", "",
	"\u2066", "", "\u2067", "", "\u2068", "", "\u2069", "",
)

// cleanText makes free text a client sent safe to pass on to everyone else.
func cleanText(s string) string {
	return bidiControls.Replace(strings.ToValidUTF8(s, "\ufffd"))
}

// validChannel reports whether a channel name is accepted right now.
func validChannel(c string) bool {
	return c == channelLobby || c == channelRoom || c == channelSpectate || c == channelPM
}

// sendToUser pushes a message to the connection u plays through. Dropped when
// that connection's queue is full, like other lobby notifications.
func sendToUser(u *state.User, msgType string, data any) {
	send := u.Primary().Send
	if send == nil {
		return
	}
	env, err := json.Marshal(protocol.Envelope{Type: msgType, Data: mustMarshal(data)})
	if err != nil {
		return
	}
	select {
	case send <- env:
	default:
		slog.Warn("message dropped (queue full)", "type", msgType, "to", u.Username)
	}
}

// deliverPrivateMessage routes a PM to its recipient and echoes it to the
// sender, whose client shows it from that echo. The flood limit, mute and chat
// filter have already passed in handleChatSend.
func (h *Hub) deliverPrivateMessage(c *Client, id string, req protocol.ChatSendRequest, msg protocol.ChatMsgPayload) {
	fail := func(reason string) {
		c.sendEnvelope(protocol.TypeChatFail, id, protocol.ChatFailPayload{
			Channel: channelPM, ToUserID: req.ToUserID, Reason: reason,
		})
	}
	target := h.state.GetUser(req.ToUserID)
	if target == nil {
		fail(protocol.ReasonUserGone)
		return
	}
	if target.Primary() == c.user {
		fail(protocol.ReasonSamePC)
		return
	}
	if !target.Primary().Social {
		fail(protocol.ReasonNeedsUpdate)
		return
	}
	// Replying to someone who wrote to a local player goes out as that player,
	// so the other side never sees the PC's owner answer.
	if req.AsUserID != 0 {
		as := h.state.GetUser(req.AsUserID)
		if as == nil || as.Primary() != c.user {
			fail(protocol.ReasonInvalidPayload)
			return
		}
		msg.FromUserID = as.ID
		msg.FromUsername = as.Username
		// The shield is the moderator's, not their guests'.
		msg.Admin = msg.Admin && as == c.user
	}
	msg.ToUserID = target.ID
	msg.ToUsername = target.Username
	sendToUser(target, protocol.TypeChatMsg, msg)
	c.sendEnvelope(protocol.TypeChatMsg, id, msg)
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
