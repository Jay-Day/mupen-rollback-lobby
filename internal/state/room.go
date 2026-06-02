package state

import (
	"sync"
	"time"
)

type Visibility string

const (
	VisibilityPublic     Visibility = "public"
	VisibilityInviteOnly Visibility = "invite_only"
)

type RoomState string

const (
	RoomStateWaiting  RoomState = "waiting"
	RoomStateStarting RoomState = "starting"
	RoomStateInGame   RoomState = "in_game"
	RoomStateFinished RoomState = "finished"
)

// Room is a Kaillera-style lobby room.
// Mutate via helper methods or hold mu directly.
type Room struct {
	mu sync.RWMutex

	ID   uint64
	Name string
	Host *User

	RomName   string
	RomMD5    string
	RomRegion string

	MaxPlayers int
	Delay      int
	Prediction int
	Visibility Visibility
	Password   string

	Players    []*RoomSeat
	Spectators []*User

	State     RoomState
	CreatedAt time.Time
	StartedAt time.Time // set when the match goes in-game; zero until then
}

type RoomSeat struct {
	User *User
	Slot int
}

func (r *Room) Snapshot() (id, hostID uint64, name string, players, max int, st RoomState) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id = r.ID
	if r.Host != nil {
		hostID = r.Host.ID
	}
	name = r.Name
	players = len(r.Players)
	max = r.MaxPlayers
	st = r.State
	return
}

func (r *Room) AddPlayer(u *User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Players) >= r.MaxPlayers {
		return ErrRoomFull
	}
	// Pick the lowest free slot in [1..MaxPlayers] so leavers' slots get
	// reused without shifting the surviving players' slot numbers. Slot
	// stability matters because GekkoNet keys actors by slot at MATCH_BEGIN,
	// and clients render seat rows by slot.
	used := make(map[int]bool, len(r.Players))
	for _, seat := range r.Players {
		used[seat.Slot] = true
	}
	slot := 0
	for s := 1; s <= r.MaxPlayers; s++ {
		if !used[s] {
			slot = s
			break
		}
	}
	r.Players = append(r.Players, &RoomSeat{User: u, Slot: slot})
	return nil
}

func (r *Room) RemovePlayer(userID uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, seat := range r.Players {
		if seat.User != nil && seat.User.ID == userID {
			r.Players = append(r.Players[:i], r.Players[i+1:]...)
			return true
		}
	}
	return false
}

func (r *Room) SetState(s RoomState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.State = s
}

// MarkStarted stamps the match start time (used for the Ongoing Matches duration).
func (r *Room) MarkStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.StartedAt = time.Now()
}

// StartedAtUnixMs returns the match start time in unix milliseconds, or 0 if the
// match hasn't started.
func (r *Room) StartedAtUnixMs() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.StartedAt.IsZero() {
		return 0
	}
	return r.StartedAt.UnixMilli()
}

// PlayerUsernames returns the seated players' usernames in slot order.
func (r *Room) PlayerUsernames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.Players))
	for _, seat := range r.Players {
		if seat.User != nil {
			names = append(names, seat.User.Username)
		}
	}
	return names
}
