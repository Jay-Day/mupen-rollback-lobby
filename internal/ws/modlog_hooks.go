package ws

import (
	"fmt"
	"strings"

	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// What the hub tells the moderators' log (internal/modlog) about rooms and
// games. Connections and chat are logged where they happen (handleHello,
// cleanupClient, handleChatSend). Everything here is a no-op without a log,
// and none of it changes what any client is sent.

// logRoomCreated records a new room; kind is "" for one a host created, or
// what made it ("Quick match").
func (h *Hub) logRoomCreated(room *state.Room, kind string) {
	if h.modlog == nil {
		return
	}
	e := modlog.Event{Type: modlog.EventRoomCreated, RoomID: room.ID}
	if room.Host != nil {
		e.Name = room.Host.Username
		e.IP = room.Host.PublicIP
	}
	where := fmt.Sprintf("%q (%s)", room.Name, room.RomName)
	if kind == "" {
		e.Text = e.Name + " created room " + where
		if room.Password != "" {
			e.Text += " with a password"
		}
	} else {
		e.Text = kind + ": room " + where + " for " + strings.Join(room.PlayerUsernames(), ", ")
	}
	h.modlog.Event(e)
}

func (h *Hub) logRoomJoined(room *state.Room, u *state.User, ip string) {
	if h.modlog == nil {
		return
	}
	h.modlog.Event(modlog.Event{Type: modlog.EventRoomJoined, RoomID: room.ID, Name: u.Username, IP: ip,
		Text: u.Username + " joined the room"})
}

// logRoomLeft records u leaving. Kicks and moderator closes have their own
// lines.
func (h *Hub) logRoomLeft(room *state.Room, u *state.User, reason string) {
	if h.modlog == nil || reason == "kicked" || reason == "admin_closed" {
		return
	}
	text := " left the room"
	if reason == "disconnect" {
		text = " disconnected"
	}
	h.modlog.Event(modlog.Event{Type: modlog.EventRoomLeft, RoomID: room.ID, Name: u.Username, IP: u.PublicIP,
		Text: u.Username + text})
}

// logPlayer is u as a game's player row.
func logPlayer(u *state.User, slot int) modlog.Player {
	return modlog.Player{UserID: u.ID, Name: u.Username, IP: u.PublicIP, Slot: slot}
}

func (h *Hub) logGameStarted(room *state.Room, match *state.Match) {
	if h.modlog == nil {
		return
	}
	g := modlog.Game{MatchID: match.ID, RoomID: room.ID, RoomName: room.Name, Game: room.RomName, RomMD5: room.RomMD5}
	for _, u := range match.Players {
		g.Players = append(g.Players, logPlayer(u, seatSlotOf(room, u)))
	}
	h.modlog.GameStarted(g)
}

// gameEndReason says how a room's match ended when the room went away (or
// its host left) for the lobby's reason.
func gameEndReason(room *state.Room, reason string) string {
	switch reason {
	case "disconnect":
		if room.QuickMatch {
			return modlog.EndPlayerDisconnect // a quick match's "host" is whoever queued first
		}
		return modlog.EndHostDisconnected
	case "left", "dropped":
		if room.QuickMatch {
			return modlog.EndPlayerLeft
		}
		return modlog.EndHostLeft
	case "admin_closed":
		return modlog.EndModerator
	}
	return reason
}

func roomClosedText(reason string) string {
	switch reason {
	case "left":
		return "the host left"
	case "disconnect":
		return "the host disconnected"
	case "admin_closed":
		return "closed by a moderator"
	}
	return reason
}
