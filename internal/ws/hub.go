package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jay-day/rmgk-lobby/internal/geo"
	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	helloTimeout    = 5 * time.Second
	writeWait       = 10 * time.Second
	clientSendQueue = 64

	// The client sends HEARTBEAT every 15 seconds. Missing five consecutive
	// heartbeats means the WebSocket is no longer useful even if the OS has not
	// noticed the broken TCP path yet.
	clientStaleAfter = 75 * time.Second
	staleSweepEvery  = 15 * time.Second

	// A match that never reaches MATCH_CONNECTED is a failed launch/punch, not
	// an ongoing game. Active matches have deliberately no age limit.
	connectingMatchTimeout = 2 * time.Minute
)

// Hub coordinates the set of connected WS clients and dispatches
// messages between them via the central State.
type Hub struct {
	state *state.State

	udpPublicAddr string // host:port advertised to clients in HELLO_OK

	mu      sync.Mutex
	clients map[uint64]*Client // keyed by user id

	register   chan *Client
	unregister chan *Client

	lobbyChat  *chatHistory
	matchmaker *matchmaker
	broadcasts *broadcastManager
	mod        *moderation.Manager
}

func NewHub(s *state.State, udpPublicAddr string, mod *moderation.Manager) *Hub {
	h := &Hub{
		state:         s,
		udpPublicAddr: udpPublicAddr,
		clients:       make(map[uint64]*Client),
		register:      make(chan *Client, 32),
		unregister:    make(chan *Client, 32),
		lobbyChat:     newChatHistory(),
		matchmaker:    newMatchmaker(),
		mod:           mod,
	}
	h.broadcasts = newBroadcastManager(h)
	return h
}

// IsBanned reports whether an IP is currently banned (used at connect time).
func (h *Hub) IsBanned(ip string) (moderation.Sanction, bool) {
	if h.mod == nil {
		return moderation.Sanction{}, false
	}
	return h.mod.IsBanned(ip)
}

// Run owns the registration/unregistration channels.
// Spawn this in its own goroutine before calling ServeWS.
func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(staleSweepEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			h.disconnectStaleClients(now)
			h.reapStalledMatches(now)
		case c := <-h.register:
			if c.user == nil {
				continue
			}
			h.mu.Lock()
			h.clients[c.user.ID] = c
			h.mu.Unlock()
		case c := <-h.unregister:
			h.cleanupClient(c)
		}
	}
}

// disconnectStaleClients closes half-open WebSockets that have stopped sending
// application heartbeats. Closing the socket unblocks readLoop, which then uses
// the normal unregister/cleanup path.
func (h *Hub) disconnectStaleClients(now time.Time) {
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.clients))
	for _, c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		if c == nil || c.user == nil {
			continue
		}
		lastSeen := c.user.GetLastSeen()
		if lastSeen.IsZero() || now.Sub(lastSeen) <= clientStaleAfter {
			continue
		}
		slog.Warn("disconnecting stale client",
			"user", c.user.Username, "id", c.user.ID, "silent_for", now.Sub(lastSeen))
		c.disconnect("Connection timed out.")
	}
}

// reapStalledMatches is a periodic backstop for interrupted cleanup paths and
// clients that launch a game but never complete NAT traversal. It never expires
// an active match merely because it has been running a long time.
func (h *Hub) reapStalledMatches(now time.Time) {
	for _, match := range h.state.ListMatches() {
		if match == nil {
			continue
		}
		room := h.state.GetRoom(match.RoomID)
		if room == nil {
			match.SetState(state.MatchStateFinished)
			h.state.RemoveMatch(match.ID)
			h.broadcasts.endByMatch(match.ID)
			continue
		}

		if h.activeMatchPlayers(match) < 2 {
			h.finishMatch(room, match, "watchdog_not_enough_players")
			continue
		}
		if match.GetState() == state.MatchStateConnecting && now.Sub(match.StartedAt) > connectingMatchTimeout {
			h.finishMatch(room, match, "connecting_timeout")
		}
	}
}

func (h *Hub) cleanupClient(c *Client) {
	if c.user == nil {
		return
	}
	h.mu.Lock()
	delete(h.clients, c.user.ID)
	h.mu.Unlock()

	uid := c.user.ID
	slog.Info("client cleanup", "user", c.user.Username, "id", uid)

	// Drop from matchmaker queue if present.
	h.matchmaker.remove(uid)

	// End any broadcast this user was running and drop them from spectator sets.
	h.broadcasts.onUserGone(uid)

	// If the user was in a room, leave it (which dissolves the room if host).
	if c.user.CurrentRoom != nil {
		h.removeUserFromRoom(c.user, c.user.CurrentRoom, "disconnect")
	}

	h.state.RemoveUser(uid)
	// TODO: if user was in a match, send MATCH_PEER_LEFT to peers.

	h.broadcastPresenceRemoved(uid)
}

// ServeWS upgrades an HTTP request to WebSocket and runs the client until disconnect.
// The caller (main.go) has already passed the rate limiter gate.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, ip string) {
	// Reject banned IPs before the WebSocket upgrade so they never connect.
	if s, banned := h.IsBanned(ip); banned {
		http.Error(w, "banned", http.StatusForbidden)
		slog.Info("rejected banned ip", "ip", ip, "until", s.Until, "reason", s.Reason)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// TODO: tighten OriginPatterns before production.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Debug("ws accept failed", "err", err, "ip", ip)
		return
	}

	// Default coder/websocket read limit is 32 KiB. Broadcaster BROADCAST_DATA
	// chunks are small, but raise the ceiling so a batched base64 chunk never
	// trips it and tears down the connection.
	conn.SetReadLimit(1 << 20) // 1 MiB

	c := &Client{
		hub:  h,
		conn: conn,
		ip:   ip,
		send: make(chan []byte, clientSendQueue),
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go c.writeLoop(ctx)
	c.readLoop(ctx)

	_ = conn.Close(websocket.StatusNormalClosure, "")
	h.unregister <- c
}

func (h *Hub) handleHello(c *Client, env protocol.Envelope) error {
	var req protocol.HelloRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		c.sendFail(protocol.TypeHelloFail, env.ID, protocol.ReasonInvalidPayload)
		return err
	}

	if !isValidUsername(req.Username) {
		c.sendFail(protocol.TypeHelloFail, env.ID, protocol.ReasonInvalidPayload)
		return errInvalidUsername
	}
	// Usernames are broadcast to the whole lobby — hold them to the same
	// hate-speech filter as chat.
	if h.mod != nil {
		if term, hit := h.mod.FilterMessage(req.Username); hit {
			c.sendFail(protocol.TypeHelloFail, env.ID, protocol.ReasonInvalidPayload)
			slog.Info("username blocked by filter", "ip", c.ip, "term", term)
			return errInvalidUsername
		}
	}

	// Resolve a coarse region bucket from the client's public IP. This is
	// the only place IPs touch geo data; the result that ships to other
	// clients in PRESENCE is just the bucket name, never a country or city.
	region := geo.RegionFromIP(c.ip)

	user := &state.User{
		ID:        h.state.NextUserID(),
		Username:  req.Username,
		State:     state.UserStateIdle,
		PublicIP:  c.ip,
		LocalIP:   req.LocalIP,
		LocalPort: req.LocalPort,
		Region:    region,
		Country:   geo.CountryFromIP(c.ip),
		JoinedAt:  time.Now(),
		LastSeen:  time.Now(),
		Send:      c.send,
	}

	if err := h.state.AddUser(user); err != nil {
		c.sendFail(protocol.TypeHelloFail, env.ID, protocol.ReasonUsernameTaken)
		return err
	}

	c.user = user
	h.register <- c
	slog.Info("hello accepted", "user", user.Username, "id", user.ID, "ip", c.ip, "total_users", h.state.UserCount())

	resp := protocol.HelloOKResponse{
		UserID:     user.ID,
		ObservedIP: c.ip,
		UDPAnchor:  h.udpPublicAddr,
		Region:     user.Region,
	}
	c.sendEnvelope(protocol.TypeHelloOK, env.ID, resp)

	h.sendPresenceFull(c)
	h.sendRoomListTo(c)
	h.broadcastPresenceAdded(user)
	return nil
}

func (h *Hub) dispatch(c *Client, env protocol.Envelope) {
	if c.user == nil {
		return
	}
	c.user.Touch()

	switch env.Type {
	case protocol.TypeHeartbeat:
		h.handleHeartbeat(c, env)
	case protocol.TypeChatSend:
		h.handleChatSend(c, env)
	case protocol.TypeChatHistory:
		h.handleChatHistory(c, env)
	case protocol.TypeRoomCreate:
		h.handleRoomCreate(c, env)
	case protocol.TypeRoomJoin:
		h.handleRoomJoin(c, env)
	case protocol.TypeRoomLeave:
		h.handleRoomLeave(c, env)
	case protocol.TypeRoomKick:
		h.handleRoomKick(c, env)
	case protocol.TypeRoomStart:
		h.handleRoomStart(c, env)
	case protocol.TypeRoomUpdateSettings:
		h.handleRoomUpdateSettings(c, env)
	case protocol.TypeRoomSwapSlots:
		h.handleRoomSwapSlots(c, env)
	case protocol.TypePingProbeRequest:
		h.handlePingProbeRequest(c, env)
	case protocol.TypeMatchConnected:
		h.handleMatchConnected(c, env)
	case protocol.TypeMatchPunchFailed:
		h.handleMatchPunchFailed(c, env)
	case protocol.TypeMatchFinished:
		h.handleMatchFinished(c, env)
	case protocol.TypeQuickMatchJoin:
		h.handleQuickMatchJoin(c, env)
	case protocol.TypeQuickMatchCancel:
		h.handleQuickMatchCancel(c, env)
	case protocol.TypeBroadcastBegin:
		h.handleBroadcastBegin(c, env)
	case protocol.TypeBroadcastData:
		h.handleBroadcastData(c, env)
	case protocol.TypeBroadcastKeyframe:
		h.handleBroadcastKeyframe(c, env)
	case protocol.TypeBroadcastEnd:
		h.handleBroadcastEnd(c, env)
	case protocol.TypeSpectateStart:
		h.handleSpectateStart(c, env)
	case protocol.TypeSpectateStop:
		h.handleSpectateStop(c, env)
	case protocol.TypeAdminAuth:
		h.handleAdminAuth(c, env)
	case protocol.TypeModAction:
		h.handleModAction(c, env)
	default:
		slog.Debug("unknown message", "type", env.Type, "user", c.user.ID)
	}
}

// handlePingProbeRequest returns every known endpoint candidate for the target.
// TargetEndpoint remains the backward-compatible preferred address: it uses
// the LAN endpoint when both users have the same observed public IP, otherwise
// it uses the public endpoint. Updated clients can race PublicEndpoint and
// LocalEndpoint instead of relying on that heuristic alone.
func (h *Hub) handlePingProbeRequest(c *Client, env protocol.Envelope) {
	var req protocol.PingProbeRequestPayload
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}

	resp := protocol.PingProbeReplyPayload{TargetUserID: req.TargetUserID}
	if target := h.state.GetUser(req.TargetUserID); target != nil {
		ip, port := target.GetUDPEndpoint()
		if ip != "" && port != 0 {
			resp.PublicEndpoint = fmt.Sprintf("%s:%d", ip, port)
			resp.TargetEndpoint = resp.PublicEndpoint
		}

		localIP, localPort := target.GetLocalEndpoint()
		if localIP != "" && localPort != 0 {
			resp.LocalEndpoint = fmt.Sprintf("%s:%d", localIP, localPort)
			if c.user != nil && c.user.PublicIP == target.PublicIP {
				resp.TargetEndpoint = resp.LocalEndpoint
			}
		}
	}
	c.sendEnvelope(protocol.TypePingProbeReply, env.ID, resp)
}

func (h *Hub) handleHeartbeat(c *Client, env protocol.Envelope) {
	var req protocol.HeartbeatRequest
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &req)
	}
	if req.PingToServer > 0 {
		c.user.SetPingToServer(req.PingToServer)
	}
	c.sendEnvelope(protocol.TypeHeartbeatAck, env.ID, protocol.HeartbeatAckResponse{
		ServerTime: time.Now().UnixMilli(),
	})
}

// sendPresenceFull pushes the entire current user list to one client.
func (h *Hub) sendPresenceFull(c *Client) {
	users := h.state.ListUsers()
	payload := protocol.PresenceFullPayload{
		Users: make([]protocol.PresenceUser, 0, len(users)),
	}
	for _, u := range users {
		payload.Users = append(payload.Users, h.presenceUserFromState(u))
	}
	c.sendEnvelope(protocol.TypePresenceFull, "", payload)
}

func (h *Hub) broadcastPresenceAdded(u *state.User) {
	h.broadcastToAll(protocol.TypePresenceDelta, protocol.PresenceDeltaPayload{
		Added: []protocol.PresenceUser{h.presenceUserFromState(u)},
	}, u.ID)
}

func (h *Hub) broadcastPresenceRemoved(userID uint64) {
	h.broadcastToAll(protocol.TypePresenceDelta, protocol.PresenceDeltaPayload{
		Removed: []uint64{userID},
	}, 0)
}

// broadcastToAll sends a message to every connected client except excludeID (0 = none).
func (h *Hub) broadcastToAll(msgType string, data any, excludeID uint64) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	env, err := json.Marshal(protocol.Envelope{Type: msgType, Data: payload})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	slog.Info("broadcast", "type", msgType, "clients", len(h.clients), "exclude", excludeID)
	for id, c := range h.clients {
		if id == excludeID {
			continue
		}
		select {
		case c.send <- env:
		default:
			slog.Warn("broadcast drop (queue full)", "type", msgType, "to", id)
		}
	}
}

func (h *Hub) handleChatSend(c *Client, env protocol.Envelope) {
	var req protocol.ChatSendRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	if !validChannel(req.Channel) {
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		return
	}

	// Per-client flood limit: chat fans out to every connected client, so a
	// spammer amplifies through the whole lobby. Moderators are exempt.
	if !c.isModerator && !c.allowChat(time.Now()) {
		c.notice("warn", "You're sending messages too fast — slow down.")
		return
	}

	if h.mod != nil {
		// Muted users can't send chat (moderators are exempt).
		if !c.isModerator {
			if s, muted := h.mod.IsMuted(c.ip); muted {
				c.notice("warn", "You are muted"+untilSuffix(s.Until)+".")
				return
			}
		}
		// Hate-speech filter: drop the message, notify only the sender. Never
		// echo the matched term back; log it server-side for review.
		if term, hit := h.mod.FilterMessage(msg); hit {
			c.notice("warn", "Your message was blocked by the chat filter.")
			slog.Info("chat blocked", "user", c.user.Username, "id", c.user.ID, "ip", c.ip, "term", term)
			return
		}
	}

	payload := protocol.ChatMsgPayload{
		Channel:      req.Channel,
		FromUserID:   c.user.ID,
		FromUsername: c.user.Username,
		Message:      truncateMessage(msg),
		ServerTime:   nowMs(),
		Admin:        c.isModerator,
	}

	switch req.Channel {
	case channelLobby:
		h.lobbyChat.Append(payload)
		h.broadcastToAll(protocol.TypeChatMsg, payload, 0)
	case channelRoom:
		// Sender must be in a room; broadcast only to that room's members.
		room := c.user.CurrentRoom
		if room == nil {
			return
		}
		h.broadcastToRoom(room, protocol.TypeChatMsg, payload)
	}
}

func (h *Hub) handleChatHistory(c *Client, env protocol.Envelope) {
	var req protocol.ChatHistoryRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	if !validChannel(req.Channel) {
		return
	}
	reply := protocol.ChatHistoryReply{
		Channel:  req.Channel,
		Messages: h.lobbyChat.Snapshot(),
	}
	c.sendEnvelope(protocol.TypeChatHistoryReply, env.ID, reply)
}

func (h *Hub) presenceUserFromState(u *state.User) protocol.PresenceUser {
	pu := protocol.PresenceUser{
		ID:           u.ID,
		Username:     u.Username,
		State:        string(u.GetState()),
		Region:       u.Region,
		Country:      u.Country,
		PingToServer: u.PingToServer,
	}
	if u.CurrentRoom != nil {
		pu.CurrentRoomID = u.CurrentRoom.ID
		pu.CurrentRoomName = u.CurrentRoom.Name
	}
	// Surface the ROM a searching user is queued for, so peers can show it.
	if u.GetState() == state.UserStateSearching {
		if rom, ok := h.matchmaker.romOf(u.ID); ok {
			pu.SearchingRom = rom.Name
		}
	}
	return pu
}
