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

	// Ping-probe flood window, and the last time we introduced this client to
	// each target. Same read-loop-only rule as chatTimes, so no lock.
	probeTimes   []time.Time
	introducedAt map[uint64]time.Time
}

const (
	chatBurst  = 5 // max chat messages per chatWindow
	chatWindow = 5 * time.Second

	// A seated 4-player room refreshes 3 peers every 3s, so ~5 requests per 5s
	// is the honest ceiling; 12 leaves room for that plus a user clicking
	// around the lobby without ever letting one client hammer the server.
	probeBurst  = 12
	probeWindow = 5 * time.Second

	// How often one client may cause us to poke a given target. The probe reply
	// still goes out when this trips — only the *unsolicited* push to the target
	// is suppressed, so a spammer can't make someone else emit packets on
	// demand. Introduction only needs to bootstrap the NAT mapping; once both
	// sides are probing each other the 3s seat refresh keeps it open.
	introduceCooldown = 10 * time.Second
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

// allowProbeRequest records a PING_PROBE_REQUEST and reports whether it's
// within the flood limit. Called only from this client's read loop.
func (c *Client) allowProbeRequest(now time.Time) bool {
	cutoff := now.Add(-probeWindow)
	kept := c.probeTimes[:0]
	for _, t := range c.probeTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.probeTimes = kept
	if len(c.probeTimes) >= probeBurst {
		return false
	}
	c.probeTimes = append(c.probeTimes, now)
	return true
}

// allowIntroduce reports whether we may push an unsolicited PING_PROBE_INCOMING
// about this client to targetID, and records the attempt when we may. Keyed per
// target so hammering one victim is bounded even while the caller stays inside
// the overall probe budget. Called only from this client's read loop.
func (c *Client) allowIntroduce(targetID uint64, now time.Time) bool {
	if c.introducedAt == nil {
		c.introducedAt = make(map[uint64]time.Time)
	}
	if last, ok := c.introducedAt[targetID]; ok && now.Sub(last) < introduceCooldown {
		return false
	}
	c.introducedAt[targetID] = now
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

// trySendEnvelope marshals data as the inner payload and attempts to enqueue a
// single envelope. Callers that require reliable ordered delivery can retain
// their cursor and retry when this returns false.
func (c *Client) trySendEnvelope(msgType, id string, data any) bool {
	payload, err := json.Marshal(data)
	if err != nil {
		return false
	}
	env, err := json.Marshal(protocol.Envelope{Type: msgType, ID: id, Data: payload})
	if err != nil {
		return false
	}
	select {
	case c.send <- env:
		return true
	default:
		return false
	}
}

// trySendEnvelopeReserved leaves room for latency-sensitive lobby/control
// messages while a replay history pump is filling the same connection queue.
func (c *Client) trySendEnvelopeReserved(msgType, id string, data any, reserve int) bool {
	if reserve > 0 && cap(c.send) > reserve && len(c.send) >= cap(c.send)-reserve {
		return false
	}
	return c.trySendEnvelope(msgType, id, data)
}

// sendEnvelope is best-effort for ordinary lobby notifications. Replay data
// uses trySendEnvelopeReserved and retries from its on-disk cursor instead of
// silently advancing past a full queue.
func (c *Client) sendEnvelope(msgType, id string, data any) {
	_ = c.trySendEnvelope(msgType, id, data)
}

func (c *Client) sendFail(msgType, id, reason string) {
	c.sendEnvelope(msgType, id, protocol.HelloFailResponse{Reason: reason})
}

func isValidUsername(s string) bool {
	return usernameRegexp.MatchString(s)
}
