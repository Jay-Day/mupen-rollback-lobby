package ws

import (
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/state"
)

func TestNonHostDisconnectEndsTwoPlayerMatch(t *testing.T) {
	h, room, match, users := newActiveMatchTest(t, 2)

	h.removeUserFromRoom(users[1], room, "disconnect")

	if got := h.state.GetMatch(match.ID); got != nil {
		t.Fatalf("match still registered after only one active player remained")
	}
	if room.State != state.RoomStateWaiting {
		t.Fatalf("room state = %q, want waiting", room.State)
	}
	if !room.StartedAt.IsZero() {
		t.Fatalf("room StartedAt was not cleared: %v", room.StartedAt)
	}
	if users[0].CurrentMatch != nil {
		t.Fatal("remaining host still points at ended match")
	}
	if users[0].GetState() != state.UserStateInRoom {
		t.Fatalf("remaining host state = %q, want in_room", users[0].GetState())
	}
}

func TestNonHostDisconnectKeepsThreePlayerMatch(t *testing.T) {
	h, room, match, users := newActiveMatchTest(t, 3)

	h.removeUserFromRoom(users[1], room, "disconnect")

	if got := h.state.GetMatch(match.ID); got != match {
		t.Fatal("three-player match ended even though two active players remained")
	}
	if room.State != state.RoomStateInGame {
		t.Fatalf("room state = %q, want in_game", room.State)
	}
	if users[0].CurrentMatch != match || users[2].CurrentMatch != match {
		t.Fatal("surviving players lost their active match")
	}
}

func TestHostDisconnectRemovesMatchRecord(t *testing.T) {
	h, room, match, users := newActiveMatchTest(t, 2)

	h.removeUserFromRoom(users[0], room, "disconnect")

	if got := h.state.GetRoom(room.ID); got != nil {
		t.Fatal("host disconnect did not dissolve room")
	}
	if got := h.state.GetMatch(match.ID); got != nil {
		t.Fatal("host disconnect left stale match record")
	}
	if users[1].CurrentMatch != nil {
		t.Fatal("surviving player still points at dissolved match")
	}
}

func TestConnectingMatchTimeoutEndsMatch(t *testing.T) {
	h, room, match, users := newActiveMatchTest(t, 2)
	match.SetState(state.MatchStateConnecting)
	match.StartedAt = time.Now().Add(-connectingMatchTimeout - time.Second)

	h.reapStalledMatches(time.Now())

	if got := h.state.GetMatch(match.ID); got != nil {
		t.Fatal("timed-out connecting match still registered")
	}
	if room.State != state.RoomStateWaiting {
		t.Fatalf("room state = %q, want waiting", room.State)
	}
	for _, u := range users {
		if u.CurrentMatch != nil {
			t.Fatalf("user %d still points at timed-out match", u.ID)
		}
	}
}

func TestActiveMatchHasNoAgeTimeout(t *testing.T) {
	h, room, match, _ := newActiveMatchTest(t, 2)
	match.StartedAt = time.Now().Add(-48 * time.Hour)

	h.reapStalledMatches(time.Now())

	if got := h.state.GetMatch(match.ID); got != match {
		t.Fatal("active match was incorrectly expired by age")
	}
	if room.State != state.RoomStateInGame {
		t.Fatalf("room state = %q, want in_game", room.State)
	}
}

func newActiveMatchTest(t *testing.T, playerCount int) (*Hub, *state.Room, *state.Match, []*state.User) {
	t.Helper()
	s := state.New()
	h := NewHub(s, "127.0.0.1:6364", nil)

	users := make([]*state.User, 0, playerCount)
	room := &state.Room{
		ID:         s.NextRoomID(),
		Name:       "test room",
		MaxPlayers: playerCount,
		State:      state.RoomStateInGame,
		StartedAt:  time.Now(),
	}
	for i := 0; i < playerCount; i++ {
		u := &state.User{
			ID:       s.NextUserID(),
			Username: string(rune('a' + i)),
			State:    state.UserStatePlaying,
		}
		if err := s.AddUser(u); err != nil {
			t.Fatalf("AddUser(%d): %v", i, err)
		}
		if err := room.AddPlayer(u); err != nil {
			t.Fatalf("AddPlayer(%d): %v", i, err)
		}
		u.CurrentRoom = room
		users = append(users, u)
	}
	room.Host = users[0]

	match := &state.Match{
		ID:        s.NextMatchID(),
		RoomID:    room.ID,
		Players:   append([]*state.User(nil), users...),
		StartedAt: time.Now(),
		State:     state.MatchStateActive,
	}
	for _, u := range users {
		u.CurrentMatch = match
	}
	s.AddRoom(room)
	s.AddMatch(match)
	return h, room, match, users
}
