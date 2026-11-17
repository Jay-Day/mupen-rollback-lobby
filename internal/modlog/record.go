package modlog

import (
	"database/sql"
	"fmt"
	"strings"
)

// What the hub records. Every method returns at once (the row is written
// later) and does nothing on a nil *Logger.

// Conn is a lobby connection at HELLO.
type Conn struct {
	UserID   uint64
	IP       string
	Username string
	Version  string
	Country  string
}

// ConnectionOpened records a connection and returns its row id (0 when it
// couldn't be queued), for ConnectionClosed.
func (l *Logger) ConnectionOpened(c Conn) int64 {
	if l == nil {
		return 0
	}
	now := l.now().Unix()
	nameKey := key(c.Username)
	return l.insert(tConnections, func(id int64) func(tx *sql.Tx) error {
		return func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO connections (id, user_id, ip, username, version, country, connected)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, int64(c.UserID), c.IP, c.Username, c.Version, c.Country, now); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT OR IGNORE INTO connection_names (conn_id, name, name_key, local, added)
				VALUES (?, ?, ?, 0, ?)`, id, c.Username, nameKey, now)
			return err
		}
	})
}

// ConnectionClosed records the disconnect.
func (l *Logger) ConnectionClosed(connID int64) {
	if l == nil || connID == 0 {
		return
	}
	now := l.now().Unix()
	l.enqueue(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE connections SET disconnected = MAX(connected, ?) WHERE id = ? AND disconnected = 0`, now, connID)
		return err
	})
}

// Chat blocked values.
const (
	Delivered     = ""
	BlockedFilter = "filter"
	BlockedMuted  = "muted"
	BlockedFlood  = "flood"
)

// Chat channels, as the lobby names them.
const (
	ChannelLobby    = "lobby"
	ChannelRoom     = "room"
	ChannelSpectate = "spectate"
)

// ChatMessage is one message sent on a channel, delivered or blocked.
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
	if l == nil {
		return
	}
	now := l.now().Unix()
	nameKey, fold := key(m.Name), strings.ToLower(m.Text)
	l.enqueue(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO chat (time, channel, room_id, room_name, match_id, user_id, name, name_key,
			ip, text, text_fold, admin, blocked, suppressed)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			now, m.Channel, int64(m.RoomID), m.RoomName, int64(m.MatchID), int64(m.UserID), m.Name, nameKey,
			m.IP, m.Text, fold, boolInt(m.Admin), m.Blocked, m.Suppressed)
		return err
	})
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

// Event is one line of a room's, game's or person's timeline.
type Event struct {
	Type    string
	RoomID  uint64
	MatchID uint64
	Name    string // who it's about, "" when nobody in particular
	IP      string
	Text    string
}

// Event records a timeline line.
func (l *Logger) Event(e Event) {
	if l == nil {
		return
	}
	l.enqueue(l.eventOp(e))
}

func (l *Logger) eventOp(e Event) func(tx *sql.Tx) error {
	now := l.now().Unix()
	nameKey := key(e.Name)
	return func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO events (time, type, room_id, match_id, name, name_key, ip, text)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, now, e.Type, int64(e.RoomID), int64(e.MatchID), e.Name, nameKey, e.IP, e.Text)
		return err
	}
}

// Player is one seat of a game.
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

// GameStarted records a match start and its players.
func (l *Logger) GameStarted(g Game) {
	if l == nil || g.MatchID == 0 {
		return
	}
	now := l.now().Unix()
	l.gmu.Lock()
	defer l.gmu.Unlock()
	if _, open := l.games[g.MatchID]; open {
		return
	}
	names := make([]string, 0, len(g.Players))
	for _, p := range g.Players {
		names = append(names, fmt.Sprintf("%s (P%d)", p.Name, p.Slot))
	}
	started := l.eventOp(Event{Type: EventGameStarted, RoomID: g.RoomID, MatchID: g.MatchID,
		Text: "Game started: " + strings.Join(names, ", ")})
	id := l.insert(tGames, func(id int64) func(tx *sql.Tx) error {
		return func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO games (id, match_id, room_id, room_name, game, rom_md5, started)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, int64(g.MatchID), int64(g.RoomID), g.RoomName, g.Game, g.RomMD5, now); err != nil {
				return err
			}
			for _, p := range g.Players {
				if err := insertPlayer(tx, id, p, now, 0); err != nil {
					return err
				}
			}
			return started(tx)
		}
	})
	if id == 0 {
		return
	}
	open := &openGame{id: id, roomID: g.RoomID, players: map[uint64]*openPlayer{}}
	for _, p := range g.Players {
		open.players[p.UserID] = &openPlayer{name: p.Name}
	}
	l.games[g.MatchID] = open
}

func insertPlayer(tx *sql.Tx, gameID int64, p Player, joined, late int64) error {
	_, err := tx.Exec(`INSERT INTO game_players (game_id, user_id, name, name_key, ip, slot, joined, joined_late)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(game_id, user_id) DO UPDATE SET joined_late = excluded.joined_late, left_at = 0,
			slot = excluded.slot`,
		gameID, int64(p.UserID), p.Name, key(p.Name), p.IP, p.Slot, joined, late)
	return err
}

// GameConnected notes that a client reported the match connected.
func (l *Logger) GameConnected(matchID uint64) {
	if l == nil {
		return
	}
	l.gmu.Lock()
	defer l.gmu.Unlock()
	if g := l.games[matchID]; g != nil {
		g.connected = true
	}
}

// GamePunchFailed notes a client's report that it couldn't reach a peer.
func (l *Logger) GamePunchFailed(matchID uint64, reporter, peer string) {
	if l == nil {
		return
	}
	l.gmu.Lock()
	defer l.gmu.Unlock()
	g := l.games[matchID]
	if g == nil {
		return
	}
	first := !g.punchFailed
	g.punchFailed = true
	if !first {
		return
	}
	text := reporter + " couldn't connect"
	if peer != "" {
		text += " to " + peer
	}
	l.enqueue(l.eventOp(Event{Type: EventPunchFailed, RoomID: g.roomID, MatchID: matchID, Name: reporter, Text: text}))
}

// GamePlayerJoined records a player added to a running match (late join).
func (l *Logger) GamePlayerJoined(matchID uint64, p Player) {
	if l == nil {
		return
	}
	now := l.now().Unix()
	l.gmu.Lock()
	defer l.gmu.Unlock()
	g := l.games[matchID]
	if g == nil {
		return
	}
	if existing := g.players[p.UserID]; existing != nil && !existing.left {
		return
	}
	g.players[p.UserID] = &openPlayer{name: p.Name}
	gameID := g.id
	event := l.eventOp(Event{Type: EventPlayerJoined, RoomID: g.roomID, MatchID: matchID, Name: p.Name, IP: p.IP,
		Text: fmt.Sprintf("%s joined the game (P%d)", p.Name, p.Slot)})
	l.enqueue(func(tx *sql.Tx) error {
		if err := insertPlayer(tx, gameID, p, now, now); err != nil {
			return err
		}
		return event(tx)
	})
}

// GamePlayerLeft records a player leaving a running match. how is what the
// lobby calls it: dropped, left, disconnect, kicked, admin_closed.
func (l *Logger) GamePlayerLeft(matchID, userID uint64, how string) {
	if l == nil {
		return
	}
	now := l.now().Unix()
	l.gmu.Lock()
	defer l.gmu.Unlock()
	g := l.games[matchID]
	if g == nil {
		return
	}
	p := g.players[userID]
	if p == nil || p.left {
		return
	}
	p.left = true
	gameID := g.id
	event := l.eventOp(Event{Type: EventPlayerLeft, RoomID: g.roomID, MatchID: matchID, Name: p.name,
		Text: p.name + " " + leftText(how)})
	l.enqueue(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE game_players SET left_at = ? WHERE game_id = ? AND user_id = ?`,
			now, gameID, int64(userID)); err != nil {
			return err
		}
		return event(tx)
	})
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

// GameSpectators records a live replay's audience; the game keeps its peak.
func (l *Logger) GameSpectators(matchID uint64, viewers int) {
	if l == nil {
		return
	}
	l.gmu.Lock()
	defer l.gmu.Unlock()
	g := l.games[matchID]
	if g == nil || viewers <= g.spectators {
		return
	}
	g.spectators = viewers
	gameID := g.id
	l.enqueue(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE games SET spectators = MAX(spectators, ?) WHERE id = ?`, viewers, gameID)
		return err
	})
}

// End reasons the hub gives (and the logger adds "punch failed" and
// "server restarted"/"server stopped").
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

// GameEnded records the end of a match. A match that never connected after a
// punch failure ends as "punch failed" whatever closed it.
func (l *Logger) GameEnded(matchID uint64, reason string) {
	if l == nil {
		return
	}
	l.gmu.Lock()
	defer l.gmu.Unlock()
	l.endLocked(matchID, reason)
}

func (l *Logger) endLocked(matchID uint64, reason string) {
	g := l.games[matchID]
	if g == nil {
		return
	}
	delete(l.games, matchID)
	if !g.connected && g.punchFailed {
		reason = EndPunchFailed
	}
	now := l.now().Unix()
	gameID := g.id
	event := l.eventOp(Event{Type: EventGameEnded, RoomID: g.roomID, MatchID: matchID, Text: "Game ended: " + reason})
	l.enqueue(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE games SET ended = MAX(started, ?), end_reason = ? WHERE id = ? AND ended = 0`,
			now, reason, gameID); err != nil {
			return err
		}
		return event(tx)
	})
}

// RoomClosed records a room going away; any match still running in it ends
// with endReason.
func (l *Logger) RoomClosed(roomID uint64, name, endReason, text string) {
	if l == nil || roomID == 0 {
		return
	}
	l.gmu.Lock()
	for matchID, g := range l.games {
		if g.roomID == roomID {
			l.endLocked(matchID, endReason)
		}
	}
	l.gmu.Unlock()
	if text == "" {
		text = "Room \"" + name + "\" closed"
	}
	l.Event(Event{Type: EventRoomClosed, RoomID: roomID, Text: text})
}

// OpenGames counts matches still running, for tests and status.
func (l *Logger) OpenGames() int {
	if l == nil {
		return 0
	}
	l.gmu.Lock()
	defer l.gmu.Unlock()
	return len(l.games)
}
