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
	// Pacing is the time-sync model (0 = aggressive/symmetric default,
	// 1 = smooth/asymmetric). Host-set; broadcast so every seat runs the same
	// model. No Auto concept — it's a discrete choice, not ping-derived.
	Pacing int
	// DelayAuto/PredictionAuto record whether the host left each value on "Auto"
	// so the state we broadcast lets non-hosts mirror the host's label.
	DelayAuto      bool
	PredictionAuto bool
	Visibility     Visibility
	Password       string

	Players    []*RoomSeat
	Spectators []*User

	State     RoomState
	CreatedAt time.Time
	StartedAt time.Time // set when the match goes in-game; zero until then

	kickedAt map[uint64]time.Time // userID -> last kick time, for a rejoin cooldown
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

// SetRollbackSettings updates the room's delay/prediction/pacing parameters and
// whether delay/prediction are host-Auto-driven. Callers should validate
// range/host/state first; this only mutates under the lock.
func (r *Room) SetRollbackSettings(delay, prediction, pacing int, delayAuto, predictionAuto bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Delay = delay
	r.Prediction = prediction
	r.Pacing = pacing
	r.DelayAuto = delayAuto
	r.PredictionAuto = predictionAuto
}

// SwapSlots exchanges the slot numbers of whoever occupies slotA and slotB. If
// only one slot is occupied, that player simply moves to the other slot.
// Callers should validate host/state/range first; this only mutates under the
// lock. Returns true if a seat actually changed.
func (r *Room) SwapSlots(slotA, slotB int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slotA == slotB {
		return false
	}
	var seatA, seatB *RoomSeat
	for _, seat := range r.Players {
		switch seat.Slot {
		case slotA:
			seatA = seat
		case slotB:
			seatB = seat
		}
	}
	if seatA == nil && seatB == nil {
		return false
	}
	if seatA != nil {
		seatA.Slot = slotB
	}
	if seatB != nil {
		seatB.Slot = slotA
	}
	return true
}

// MarkKicked records that a user was just kicked, starting their rejoin cooldown.
func (r *Room) MarkKicked(userID uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.kickedAt == nil {
		r.kickedAt = make(map[uint64]time.Time)
	}
	r.kickedAt[userID] = time.Now()
}

// KickCoolingDown reports whether the user was kicked within the last `within`.
func (r *Room) KickCoolingDown(userID uint64, within time.Duration) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.kickedAt[userID]
	return ok && time.Since(t) < within
}

// MarkStarted stamps the match start time (used for the Ongoing Matches duration).
func (r *Room) MarkStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.StartedAt = time.Now()
}

// MarkMatchEnded returns the room to its waiting state and clears the old
// match timestamp. Keeping these changes under one lock prevents room-list
// snapshots from briefly showing a waiting room with an old ongoing duration.
func (r *Room) MarkMatchEnded() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.State = RoomStateWaiting
	r.StartedAt = time.Time{}
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
