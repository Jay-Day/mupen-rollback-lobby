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
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	helloTimeout    = 5 * time.Second
	writeWait       = 10 * time.Second
	clientSendQueue = 64
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
}

func NewHub(s *state.State, udpPublicAddr string) *Hub {
	return &Hub{
		state:         s,
		udpPublicAddr: udpPublicAddr,
		clients:       make(map[uint64]*Client),
		register:      make(chan *Client, 32),
		unregister:    make(chan *Client, 32),
		lobbyChat:     newChatHistory(),
		matchmaker:    newMatchmaker(),
	}
}

// Run owns the registration/unregistration channels.
// Spawn this in its own goroutine before calling ServeWS.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
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
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// TODO: tighten OriginPatterns before production.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Debug("ws accept failed", "err", err, "ip", ip)
		return
	}

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

	// Resolve a coarse region bucket from the client's public IP. This is
	// the only place IPs touch geo data; the result that ships to other
	// clients in PRESENCE is just the bucket name, never a country or city.
	region := geo.RegionFromIP(c.ip)

	user := &state.User{
		ID:       h.state.NextUserID(),
		Username: req.Username,
		State:    state.UserStateIdle,
		PublicIP: c.ip,
		LocalIP:  req.LocalIP,
		Region:   region,
		JoinedAt: time.Now(),
		LastSeen: time.Now(),
		Send:     c.send,
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
	default:
		slog.Debug("unknown message", "type", env.Type, "user", c.user.ID)
	}
}

// handlePingProbeRequest looks up the target user's last-known public UDP
// endpoint (registered via the anchor service) and ships it back so the
// requester can fire UDP probes directly. Returns an empty endpoint when
// the target either isn't registered or hasn't yet sent an anchor packet;
// the client treats that as "ping unavailable".
func (h *Hub) handlePingProbeRequest(c *Client, env protocol.Envelope) {
	var req protocol.PingProbeRequestPayload
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}

	resp := protocol.PingProbeReplyPayload{TargetUserID: req.TargetUserID}
	if target := h.state.GetUser(req.TargetUserID); target != nil {
		ip, port := target.GetUDPEndpoint()
		if ip != "" && port != 0 {
			resp.TargetEndpoint = fmt.Sprintf("%s:%d", ip, port)
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

	payload := protocol.ChatMsgPayload{
		Channel:      req.Channel,
		FromUserID:   c.user.ID,
		FromUsername: c.user.Username,
		Message:      truncateMessage(msg),
		ServerTime:   nowMs(),
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
