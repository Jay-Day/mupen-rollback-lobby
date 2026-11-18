package modlog

import "log/slog"

// Everything here is one Info line in the server log, keyed so it can be
// found with grep: "msg":"chat", "msg":"lobby event", "msg":"game event",
// "msg":"moderator action". Nothing is kept in memory.

// Conn is a lobby connection.
type Conn struct {
	UserID   uint64
	IP       string
	Username string
	Version  string
	Country  string
}

// ConnectionOpened records a connection. The hub's "hello accepted" line
// already has the name, id and IP; this adds the version and country. It
// returns the user id for ConnectionClosed.
func (l *Logger) ConnectionOpened(c Conn) int64 {
	slog.Info("connection opened", "user", c.Username, "id", c.UserID, "ip", c.IP,
		"version", c.Version, "country", c.Country)
	return int64(c.UserID)
}

// ConnectionClosed is covered by the hub's "client cleanup" line.
func (l *Logger) ConnectionClosed(connID int64) {}

// Why a chat line didn't reach anyone ("" = delivered).
const (
	Delivered     = ""
	BlockedFilter = "filter"
	BlockedMuted  = "muted"
	BlockedFlood  = "flood"
)

// Chat channels.
const (
	ChannelLobby    = "lobby"
	ChannelRoom     = "room"
	ChannelSpectate = "spectate"
)

// ChatMessage is one chat line, delivered or blocked.
type ChatMessage struct {
	Channel  string // lobby, room, spectate
	RoomID   uint64
	RoomName string
	MatchID  uint64
	UserID   uint64
	Name     string
	IP       string
	Text     string
	Admin    bool
	Blocked  string
	// Suppressed: blocked lines from the same address before this one that
	// weren't logged (the lobby keeps a sample of them).
	Suppressed int
}

// Chat records a message.
func (l *Logger) Chat(m ChatMessage) {
	args := []any{"channel", m.Channel, "user", m.Name, "id", m.UserID, "ip", m.IP, "text", m.Text}
	if m.RoomID != 0 {
		args = append(args, "room", m.RoomID, "room_name", m.RoomName)
	}
	if m.MatchID != 0 {
		args = append(args, "match", m.MatchID)
	}
	if m.Admin {
		args = append(args, "admin", true)
	}
	if m.Blocked != "" {
		args = append(args, "blocked", m.Blocked)
	}
	if m.Suppressed > 0 {
		args = append(args, "suppressed", m.Suppressed)
	}
	slog.Info("chat", args...)
}

// Event types.
const (
	EventRoomCreated  = "room_created"
	EventRoomJoined   = "room_joined"
	EventRoomLeft     = "room_left"
	EventRoomKick     = "room_kick"
	EventRoomClosed   = "room_closed"
	EventGameStarted  = "game_started"
	EventGameEnded    = "game_ended"
	EventPlayerJoined = "player_joined"
	EventPlayerLeft   = "player_left"
	EventPunchFailed  = "punch_failed"
	EventModeration   = "moderation"
)

// Event is something that happened in a room or to a player.
type Event struct {
	Type    string
	RoomID  uint64
	MatchID uint64
	Name    string // who it's about, "" when nobody in particular
	IP      string
	Text    string
}

// Event records e.
func (l *Logger) Event(e Event) {
	args := []any{"type", e.Type, "text", e.Text}
	if e.RoomID != 0 {
		args = append(args, "room", e.RoomID)
	}
	if e.MatchID != 0 {
		args = append(args, "match", e.MatchID)
	}
	if e.Name != "" {
		args = append(args, "user", e.Name)
	}
	if e.IP != "" {
		args = append(args, "ip", e.IP)
	}
	slog.Info("lobby event", args...)
}

// Player is one seat in a game.
type Player struct {
	UserID uint64
	Name   string
	IP     string
	Slot   int
}

// Game is a match as it starts.
type Game struct {
	MatchID  uint64
	RoomID   uint64
	RoomName string
	Game     string // the ROM's name
	RomMD5   string
	Players  []Player
}

// GameStarted records a match start and its players, with their IPs.
func (l *Logger) GameStarted(g Game) {
	players := make([]string, 0, len(g.Players))
	for _, p := range g.Players {
		players = append(players, p.Name+"@"+p.IP)
	}
	slog.Info("game event", "type", EventGameStarted, "match", g.MatchID, "room", g.RoomID,
		"room_name", g.RoomName, "game", g.Game, "players", players)
}

// GameConnected: the players' peer connection came up. The hub logs it.
func (l *Logger) GameConnected(matchID uint64) {}

// GamePunchFailed records a player reporting they couldn't reach a peer.
func (l *Logger) GamePunchFailed(matchID uint64, reporter, peer string) {
	slog.Info("game event", "type", EventPunchFailed, "match", matchID, "user", reporter, "peer", peer)
}

// GamePlayerJoined records a late join.
func (l *Logger) GamePlayerJoined(matchID uint64, p Player) {
	slog.Info("game event", "type", EventPlayerJoined, "match", matchID, "user", p.Name, "id", p.UserID,
		"ip", p.IP, "slot", p.Slot)
}

// GamePlayerLeft records a player leaving a running match.
func (l *Logger) GamePlayerLeft(matchID, userID uint64, how string) {
	slog.Info("game event", "type", EventPlayerLeft, "match", matchID, "id", userID, "how", leftText(how))
}

func leftText(how string) string {
	switch how {
	case "dropped":
		return "closed the game"
	case "left":
		return "left the room"
	case "disconnect":
		return "disconnected"
	case "kicked":
		return "was kicked by the host"
	case "admin_closed":
		return "was removed by a moderator"
	case "":
		return "left the game"
	}
	return "left the game (" + how + ")"
}

// GameSpectators is a live replay's audience; it changes too often to log.
func (l *Logger) GameSpectators(matchID uint64, viewers int) {}

// Why a game ended.
const (
	EndFinished         = "finished"
	EndHostLeft         = "host left"
	EndHostDisconnected = "host disconnected"
	EndPlayerLeft       = "player left"
	EndPlayerDisconnect = "player disconnected"
	EndModerator        = "closed by moderator"
	EndAbandoned        = "abandoned"
	EndPunchFailed      = "punch failed"
)

// GameEnded records why a match ended.
func (l *Logger) GameEnded(matchID uint64, reason string) {
	slog.Info("game event", "type", EventGameEnded, "match", matchID, "reason", reason)
}

// RoomClosed records a room going away (and, when one was running, why its
// match ended).
func (l *Logger) RoomClosed(roomID uint64, name, endReason, text string) {
	if roomID == 0 {
		return
	}
	if text == "" {
		text = "Room \"" + name + "\" closed"
	}
	slog.Info("lobby event", "type", EventRoomClosed, "room", roomID, "room_name", name, "text", text,
		"end_reason", endReason)
}
