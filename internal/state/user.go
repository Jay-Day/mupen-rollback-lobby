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

	PingToServer uint16

	CurrentRoom  *Room
	CurrentMatch *Match

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
