package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/coder/websocket"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

var (
	usernameRegexp     = regexp.MustCompile(`^[A-Za-z0-9_\-\.]{3,16}$`)
	errInvalidUsername = errors.New("invalid username")
)

// Client wraps one WebSocket connection plus its outbound queue.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	ip   string

	user *state.User
	send chan []byte

	// Moderation: set once a connection authenticates via ADMIN_AUTH.
	isModerator bool
	adminName   string
	authFails   int // failed ADMIN_AUTH attempts; disconnect at maxAuthFails

	// Chat flood window. Only touched from this client's read loop, so no lock.
	chatTimes []time.Time
}

const (
	chatBurst  = 5 // max chat messages per chatWindow
	chatWindow = 5 * time.Second
)

// allowChat records a chat attempt and reports whether it's within the flood
// limit. Called only from this client's read loop.
func (c *Client) allowChat(now time.Time) bool {
	cutoff := now.Add(-chatWindow)
	kept := c.chatTimes[:0]
	for _, t := range c.chatTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.chatTimes = kept
	if len(c.chatTimes) >= chatBurst {
		return false
	}
	c.chatTimes = append(c.chatTimes, now)
	return true
}

// notice sends a MOD_NOTICE to this client. severity is "info"/"warn"/"error".
func (c *Client) notice(severity, text string) {
	c.sendEnvelope(protocol.TypeModNotice, "", protocol.ModNoticePayload{
		Text:     text,
		Severity: severity,
	})
}

// disconnect best-effort delivers a final notice, then closes the connection,
// which unblocks readLoop and triggers the normal unregister/cleanup path.
func (c *Client) disconnect(reason string) {
	if reason != "" {
		c.notice("error", reason)
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

// readLoop drives the lifetime of one client.
// It enforces the HELLO timeout, then loops reading messages until disconnect.
func (c *Client) readLoop(ctx context.Context) {
	helloCtx, cancel := context.WithTimeout(ctx, helloTimeout)
	_, helloRaw, err := c.conn.Read(helloCtx)
	cancel()
	if err != nil {
		slog.Debug("hello timeout/error", "ip", c.ip, "err", err)
		return
	}

	var hello protocol.Envelope
	if err := json.Unmarshal(helloRaw, &hello); err != nil || hello.Type != protocol.TypeHello {
		c.sendFail(protocol.TypeHelloFail, hello.ID, protocol.ReasonInvalidHello)
		return
	}
	if err := c.hub.handleHello(c, hello); err != nil {
		return
	}

	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		c.hub.dispatch(c, env)
	}
}

// writeLoop drains the send channel to the WS connection.
// Closes when ctx is cancelled or the channel is closed.
func (c *Client) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeWait)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// sendEnvelope marshals data as the inner payload and emits a single envelope.
// Drops the message silently if the outbound queue is full.
func (c *Client) sendEnvelope(msgType, id string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	env, err := json.Marshal(protocol.Envelope{Type: msgType, ID: id, Data: payload})
	if err != nil {
		return
	}
	select {
	case c.send <- env:
	default:
	}
}

func (c *Client) sendFail(msgType, id, reason string) {
	c.sendEnvelope(msgType, id, protocol.HelloFailResponse{Reason: reason})
}

func isValidUsername(s string) bool {
	return usernameRegexp.MatchString(s)
}
