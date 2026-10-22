package state

import (
	"sync"
	"time"
)

type UserState string

const (
	UserStateIdle       UserState = "idle"
	UserStateBrowsing   UserState = "browsing"
	UserStateHosting    UserState = "hosting"
	UserStateInRoom     UserState = "in_room"
	UserStateSearching  UserState = "searching"
	UserStatePlaying    UserState = "playing"
	UserStateSpectating UserState = "spectating"
	UserStateAway       UserState = "away"
)

// User represents a connected lobby user.
// All fields are protected by mu; access via the helper methods or hold the lock.
type User struct {
	mu sync.RWMutex

	ID       uint64
	Username string
	State    UserState

	PublicIP      string
	UDPPublicIP   string
	UDPPublicPort uint16
	LocalIP       string
	Region        string
	Country       string // ISO 3166-1 alpha-2, "" when unknown; drives client flags
	Anonymous     bool   // withhold Country from presence at the user's request
	ClientVersion string // self-reported build, shown in peer tooltips
	Connection    string // self-reported transport medium ("wifi"/"lan"/...)
	ICESupported  bool   // room-scoped libjuice signaling/transport support

	// Set once at HELLO when the user logged into a registered account;
	// read-only afterwards. Guests have AccountID 0.
	AccountID    int64
	SessionToken string // remember-me token issued at HELLO, for ACCOUNT_LOGOUT

	PingToServer uint16

	CurrentRoom  *Room
	CurrentMatch *Match
	// Live replay being watched while State is spectating (0 = none).
	WatchingMatchID uint64

	JoinedAt time.Time
	LastSeen time.Time

	// Send is the outbound message channel owned by the WS client goroutine.
	// The hub writes serialized JSON envelopes here; the client's writeLoop drains it.
	Send chan<- []byte
}

func (u *User) GetState() UserState {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.State
}

func (u *User) SetState(s UserState) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.State = s
}

// StartWatching marks the user as spectating matchID. Only a user doing
// nothing else becomes "spectating": a seated, searching or playing user keeps
// that state. Reports whether the presence changed.
func (u *User) StartWatching(matchID uint64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch u.State {
	case UserStateIdle, UserStateBrowsing, UserStateAway, UserStateSpectating:
	default:
		return false
	}
	changed := u.State != UserStateSpectating || u.WatchingMatchID != matchID
	u.State = UserStateSpectating
	u.WatchingMatchID = matchID
	return changed
}

// StopWatching ends watching matchID (watching another match is kept) and
// returns a spectating user to idle. Reports whether the presence changed.
func (u *User) StopWatching(matchID uint64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.WatchingMatchID != matchID {
		return false
	}
	u.WatchingMatchID = 0
	if u.State != UserStateSpectating {
		return false
	}
	u.State = UserStateIdle
	return true
}

// WatchingMatch returns the live replay the user is spectating, 0 if none.
func (u *User) WatchingMatch() uint64 {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.State != UserStateSpectating {
		return 0
	}
	return u.WatchingMatchID
}

func (u *User) GetUDPEndpoint() (string, uint16) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.UDPPublicIP, u.UDPPublicPort
}

func (u *User) SetUDPEndpoint(ip string, port uint16) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.UDPPublicIP = ip
	u.UDPPublicPort = port
}

func (u *User) Touch() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.LastSeen = time.Now()
}

func (u *User) GetLastSeen() time.Time {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.LastSeen
}

func (u *User) SetPingToServer(rtt uint16) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.PingToServer = rtt
}

// Verified reports whether the user is logged into a registered account.
func (u *User) Verified() bool { return u.AccountID != 0 }
