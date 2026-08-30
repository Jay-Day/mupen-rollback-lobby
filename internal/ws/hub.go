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

	// A connection that has sent nothing for this long is dead: clients
	// heartbeat every 15s, so this is four missed beats. Without it, a client
	// that vanishes silently (power loss, network drop) leaves its read loop
	// blocked forever — the user never gets cleaned up, and a room they were
	// hosting sits "in game" until the server restarts.
	clientSilenceTimeout = 60 * time.Second
	reapInterval         = 30 * time.Second
)

// Hub coordinates the set of connected WS clients and dispatches
// messages between them via the central State.
type Hub struct {
	state *state.State

	udpPublicAddr string // host:port advertised to clients in HELLO_OK
	iceConfig     protocol.ICEConfiguration

	mu      sync.Mutex
	clients map[uint64]*Client // keyed by user id

	register   chan *Client
	unregister chan *Client

	lobbyChat  *chatHistory
	matchmaker *matchmaker
	broadcasts *broadcastManager
	mod        *moderation.Manager
}

func NewHub(s *state.State, udpPublicAddr string, iceConfig protocol.ICEConfiguration, mod *moderation.Manager) *Hub {
	h := &Hub{
		state:         s,
		udpPublicAddr: udpPublicAddr,
		iceConfig:     iceConfig,
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
	reap := time.NewTicker(reapInterval)
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reap.C:
			h.reapSilentClients()
			h.reapOrphanRooms()
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

// reapSilentClients force-closes connections that have been silent past
// clientSilenceTimeout. Closing the socket unblocks the connection's read
// loop, which drives the normal unregister/cleanup path — leave room,
// dissolve if host, presence removal — so a vanished client's room dies the
// same way a graceful disconnect's would.
func (h *Hub) reapSilentClients() {
	cutoff := time.Now().Add(-clientSilenceTimeout)
	var stale []*Client
	h.mu.Lock()
	for _, c := range h.clients {
		if c.user != nil && c.user.GetLastSeen().Before(cutoff) {
			stale = append(stale, c)
		}
	}
	h.mu.Unlock()

	for _, c := range stale {
		slog.Info("reaping silent client",
			"user", c.user.Username, "id", c.user.ID,
			"last_seen", c.user.GetLastSeen().Format(time.RFC3339))
		c.disconnect("connection timed out")
	}
}

// reapOrphanRooms removes rooms in which no seated player is still connected.
// The normal leave/dissolve paths should make this a no-op; it's the safety
// net for any path that orphans a seat (and for whatever the next such bug
// is), so a ghost room lasts at most one reap interval instead of until a
// server restart. A room with even one connected player is left alone —
// playing on alone in a room is allowed by design.
func (h *Hub) reapOrphanRooms() {
	h.mu.Lock()
	connected := make(map[uint64]bool, len(h.clients))
	for id := range h.clients {
		connected[id] = true
	}
	h.mu.Unlock()

	removed := 0
	for _, room := range h.state.ListRooms() {
		// Grace period so a room mid-creation (seat added, CurrentRoom pointer
		// not assigned yet) can't be swept by an unluckily timed tick.
		if time.Since(room.CreatedAt) < 2*reapInterval {
			continue
		}
		// Alive means connected AND still actually in this room. The second
		// half matters: an orphaned seat (a bug overwrote the user's
		// CurrentRoom while their seat stayed behind) keeps pointing at a
		// perfectly connected user who may be chatting in the lobby or playing
		// a different game — connectedness alone would call that ghost alive
		// forever.
		alive := false
		for _, seat := range room.Players {
			if seat.User != nil && connected[seat.User.ID] && seat.User.CurrentRoom == room {
				alive = true
				break
			}
		}
		if alive {
			continue
		}
		id, _, name, seats, _, st := room.Snapshot()
		slog.Warn("reaping orphan room", "id", id, "name", name, "seats", seats, "state", st)
		h.state.RemoveRoom(id)
		removed++
	}
	if removed > 0 {
		h.broadcastRoomList()
	}
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

// clampString caps a client-supplied string to n bytes — these are echoed to
// every peer, so don't let one client bloat presence broadcasts.
func clampString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (h *Hub) handleHello(c *Client, env protocol.Envelope) error {
	var req protocol.HelloRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		c.sendFail(protocol.TypeHelloFail, env.ID, protocol.ReasonInvalidPayload)
		return err
	}
	if !isSupportedClientVersion(req.ClientVersion) {
		c.sendFailMessage(protocol.TypeHelloFail, env.ID, protocol.ReasonVersionMismatch,
			minimumClientMessage)
		slog.Info("out-of-date client rejected", "ip", c.ip,
			"version", clampString(req.ClientVersion, 64))
		return errUnsupportedClientVersion
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

	// Resolve a coarse region bucket from the client's public IP, then split
	// the North America country bucket into east/central/west using the
	// client's reported standard UTC offset (country-level IP data cannot
	// tell coasts apart).
	region := geo.RefineNorthAmerica(geo.RegionFromIP(c.ip), req.TzOffsetSeconds)

	user := &state.User{
		ID:            h.state.NextUserID(),
		Username:      req.Username,
		State:         state.UserStateIdle,
		PublicIP:      c.ip,
		LocalIP:       req.LocalIP,
		Region:        region,
		Country:       geo.CountryFromIP(c.ip),
		ClientVersion: clampString(req.ClientVersion, 32),
		Connection:    clampString(req.Connection, 16),
		Anonymous:     req.Anonymous,
		ICESupported:  req.ICESupported,
		JoinedAt:      time.Now(),
		LastSeen:      time.Now(),
		Send:          c.send,
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
		ICE:        h.iceConfig,
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
	case protocol.TypeRoomUpdateDelay:
		h.handleRoomUpdateDelay(c, env)
	case protocol.TypeRoomUpdatePlayerSettings:
		h.handleRoomUpdatePlayerSettings(c, env)
	case protocol.TypeRoomUpdatePrediction:
		h.handleRoomUpdatePrediction(c, env)
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
	case protocol.TypeICESignal:
		h.handleICESignal(c, env)
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

// handleICESignal relays one ICE description/candidate between two users in
// the same room. Authentication supplies FromUserID; clients cannot spoof it
// or use this endpoint to signal arbitrary lobby users.
func (h *Hub) handleICESignal(c *Client, env protocol.Envelope) {
	var req protocol.ICESignalRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	room := c.user.CurrentRoom
	if room == nil || room.ID != req.RoomID || req.TargetUserID == 0 ||
		req.TargetUserID == c.user.ID || !c.user.ICESupported {
		return
	}
	if !validICESignalRequest(req) {
		return
	}

	target := h.state.GetUser(req.TargetUserID)
	if target == nil || target.CurrentRoom != room || !target.ICESupported {
		return
	}
	h.mu.Lock()
	targetClient := h.clients[target.ID]
	h.mu.Unlock()
	if targetClient == nil {
		return
	}
	targetClient.sendEnvelope(protocol.TypeICESignal, env.ID, protocol.ICESignalPayload{
		FromUserID: c.user.ID,
		RoomID:     room.ID,
		Kind:       req.Kind,
		Value:      req.Value,
		Generation: req.Generation,
	})
}

func validICESignalRequest(req protocol.ICESignalRequest) bool {
	switch req.Kind {
	case "description":
		if len(req.Value) == 0 || len(req.Value) > 4096 {
			return false
		}
	case "candidate":
		if len(req.Value) == 0 || len(req.Value) > 512 {
			return false
		}
	case "gathering_done":
		if req.Value != "" {
			return false
		}
	case "restart":
		if req.Generation == 0 || req.Value != "" {
			return false
		}
	default:
		return false
	}
	return true
}

// handlePingProbeRequest looks up the target user's last-known public UDP
// endpoint (registered via the anchor service) and ships it back so the
// requester can fire UDP probes directly. Returns an empty endpoint when
// the target either isn't registered or hasn't yet sent an anchor packet;
// the client treats that as "ping unavailable".
// It also introduces the two sides to each other: the target gets an
// unsolicited PING_PROBE_INCOMING naming the requester, so both punch at once
// instead of the requester's packet dying at the target's NAT.
func (h *Hub) handlePingProbeRequest(c *Client, env protocol.Envelope) {
	var req protocol.PingProbeRequestPayload
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	if req.TargetUserID == c.user.ID {
		return
	}
	// Dropped silently: a client over its budget is either buggy or hostile,
	// and an error reply would just hand it a second message to spam.
	if !c.allowProbeRequest(time.Now()) {
		slog.Debug("ping probe rate limited", "user", c.user.Username, "target", req.TargetUserID)
		return
	}

	target := h.state.GetUser(req.TargetUserID)

	resp := protocol.PingProbeReplyPayload{TargetUserID: req.TargetUserID}
	if target != nil {
		ip, port := target.GetUDPEndpoint()
		if ip != "" && port != 0 {
			resp.TargetEndpoint = fmt.Sprintf("%s:%d", ip, port)
		}
	}
	c.sendEnvelope(protocol.TypePingProbeReply, env.ID, resp)

	if target != nil && resp.TargetEndpoint != "" && c.allowIntroduce(req.TargetUserID, time.Now()) {
		h.introduceTo(target, c.user)
	}
}

// introduceTo tells `to` that `from` wants to measure ping, so it probes back
// and both NAT mappings open together. No-op when `from` has no observed
// endpoint yet (never sent an anchor packet) or `to` isn't connected.
func (h *Hub) introduceTo(to *state.User, from *state.User) {
	if to == nil || from == nil || to.ID == from.ID {
		return
	}
	ip, port := from.GetUDPEndpoint()
	if ip == "" || port == 0 {
		return
	}
	h.mu.Lock()
	target := h.clients[to.ID]
	h.mu.Unlock()
	if target == nil {
		return
	}
	target.sendEnvelope(protocol.TypePingProbeIncoming, "", protocol.PingProbeIncomingPayload{
		FromUserID:   from.ID,
		FromEndpoint: fmt.Sprintf("%s:%d", ip, port),
	})
}

func (h *Hub) handleHeartbeat(c *Client, env protocol.Envelope) {
	var req protocol.HeartbeatRequest
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &req)
	}
	if req.PingToServer > 0 {
		c.user.SetPingToServer(req.PingToServer)
	}

	// AFK presence. Only the two "doing nothing" states flip to away — a
	// seated/searching/playing user keeps their real state (those states are
	// set and cleared by their own flows, and an AFK label would clobber
	// them). Returning from away lands on idle; if they were actually
	// browsing, the next room-list interaction restores that on its own.
	switch st := c.user.GetState(); {
	case req.Away && (st == state.UserStateIdle || st == state.UserStateBrowsing):
		c.user.SetState(state.UserStateAway)
		h.broadcastPresenceUpdated(c.user)
	case !req.Away && st == state.UserStateAway:
		c.user.SetState(state.UserStateIdle)
		h.broadcastPresenceUpdated(c.user)
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
		ID:            u.ID,
		Username:      u.Username,
		State:         string(u.GetState()),
		Region:        u.Region,
		Country:       u.Country,
		ClientVersion: u.ClientVersion,
		Connection:    u.Connection,
		PingToServer:  u.PingToServer,
	}
	// Withholding happens here, at the single choke point every presence
	// message flows through, so no other broadcast path can leak the country.
	// Region stays: clients need it for region-based ping estimation, and the
	// relayed Anonymous flag tells them not to render it as a location badge.
	if u.Anonymous {
		pu.Country = ""
		pu.Anonymous = true
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
