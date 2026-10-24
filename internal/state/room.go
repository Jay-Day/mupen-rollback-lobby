package state

import (
	"strings"
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
	Delay      int // legacy host delay; current values live on RoomSeat
	Prediction int // host-controlled, room-wide rollback prediction window
	// Pacing is the time-sync model (0 = aggressive/symmetric default,
	// 1 = smooth/asymmetric). Host-set; broadcast so every seat runs the same
	// model. No Auto concept — it's a discrete choice, not ping-derived.
	Pacing int
	// Legacy compatibility flags for clients that still render shared settings.
	DelayAuto         bool
	PredictionAuto    bool
	LiveReplayEnabled bool // host's room-wide choice for streaming the next match
	Visibility        Visibility
	Password          string
	// QuickMatch rooms are paired by the matchmaker; nobody joins them mid-match.
	QuickMatch bool

	Players    []*RoomSeat
	Spectators []*User

	State     RoomState
	CreatedAt time.Time
	StartedAt time.Time // set when the match goes in-game; zero until then

	// Ranked is non-nil while the room is ranked. Read it via GetRanked.
	Ranked *RankedSettings

	// peerPings: measured round trips (ms) reported by players, per pair
	peerPings map[[2]uint64]int

	kickedUserIDs   map[uint64]struct{} // active connection identity
	kickedUsernames map[string]struct{} // survives reconnects with the same name
}

// RankedSettings are a ranked room's server-validated set settings.
type RankedSettings struct {
	LadderID  string
	ModuleID  string
	RulesetID string
	BestOf    int
	// TeamSize is players per side: 1 (1v1) or 2 (2v2 team battle).
	TeamSize int
	// MaxPing (ms) between players; 0 = any.
	MaxPing int
	// Teammates (team sets): users the lobby matched as teammates, a chosen
	// or kept team of a quick match. Each pair must play on one team.
	Teammates [][2]uint64
}

func pingPair(a, b uint64) [2]uint64 {
	if a > b {
		a, b = b, a
	}
	return [2]uint64{a, b}
}

// SetPeerPing records a measured round trip between two players.
func (r *Room) SetPeerPing(a, b uint64, rttMs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.peerPings == nil {
		r.peerPings = make(map[[2]uint64]int)
	}
	r.peerPings[pingPair(a, b)] = rttMs
}

// PeerPing returns the last measured round trip between two players.
func (r *Room) PeerPing(a, b uint64) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rtt, ok := r.peerPings[pingPair(a, b)]
	return rtt, ok
}

// Players is how many players a ranked set needs.
func (s *RankedSettings) Players() int {
	if s.TeamSize < 1 {
		return 2
	}
	return 2 * s.TeamSize
}

// GetRanked returns a copy of the ranked settings, nil when unranked.
func (r *Room) GetRanked() *RankedSettings {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.Ranked == nil {
		return nil
	}
	copy := *r.Ranked
	return &copy
}

// SetRanked replaces the ranked settings (nil = unranked). Turning ranked on
// sizes the room for the set: two seats for 1v1, four for 2v2.
func (r *Room) SetRanked(settings *RankedSettings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Ranked = settings
	if settings != nil {
		r.MaxPlayers = settings.Players()
	}
}

type RoomSeat struct {
	User       *User
	Slot       int
	FrameDelay int
	DelayAuto  bool
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
	r.Players = append(r.Players, &RoomSeat{
		User:       u,
		Slot:       slot,
		FrameDelay: r.Delay,
		DelayAuto:  r.DelayAuto,
	})
	return nil
}

// SetPlayerDelay updates one seated player's independently chosen input delay.
// Callers validate range and room state; false means the user was not seated or
// the published value was unchanged.
func (r *Room) SetPlayerDelay(userID uint64, delay int, delayAuto bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, seat := range r.Players {
		if seat.User == nil || seat.User.ID != userID {
			continue
		}
		if seat.FrameDelay == delay && seat.DelayAuto == delayAuto {
			return false
		}
		seat.FrameDelay = delay
		seat.DelayAuto = delayAuto
		return true
	}
	return false
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

// SetPrediction updates the host-controlled room prediction window. Callers
// enforce host ownership, range, and waiting-room state.
func (r *Room) SetPrediction(prediction int, predictionAuto bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Prediction == prediction && r.PredictionAuto == predictionAuto {
		return false
	}
	r.Prediction = prediction
	r.PredictionAuto = predictionAuto
	return true
}

// SetRollbackSettings updates the legacy shared settings message. Delay is
// mirrored only for older clients; prediction and pacing remain room-wide.
func (r *Room) SetRollbackSettings(delay, prediction, pacing int, delayAuto, predictionAuto bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Delay = delay
	r.Prediction = prediction
	r.Pacing = pacing
	r.DelayAuto = delayAuto
	r.PredictionAuto = predictionAuto
}

// SetLiveReplayEnabled updates the host's room-wide streaming choice and
// reports whether it changed. The caller is responsible for host/state checks.
func (r *Room) SetLiveReplayEnabled(enabled bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.LiveReplayEnabled == enabled {
		return false
	}
	r.LiveReplayEnabled = enabled
	return true
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

// MarkKicked bars a user from rejoining this room. The set belongs to the Room,
// so it disappears automatically only when the room itself is destroyed.
func (r *Room) MarkKicked(userID uint64, username string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.kickedUserIDs == nil {
		r.kickedUserIDs = make(map[uint64]struct{})
	}
	if r.kickedUsernames == nil {
		r.kickedUsernames = make(map[string]struct{})
	}
	r.kickedUserIDs[userID] = struct{}{}
	r.kickedUsernames[strings.ToLower(strings.TrimSpace(username))] = struct{}{}
}

// WasKicked reports whether a user is barred from this room. Username matching
// keeps the ban effective if the kicked client disconnects and receives a new
// transient user ID before trying to rejoin.
func (r *Room) WasKicked(userID uint64, username string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, kicked := r.kickedUserIDs[userID]; kicked {
		return true
	}
	_, kicked := r.kickedUsernames[strings.ToLower(strings.TrimSpace(username))]
	return kicked
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
