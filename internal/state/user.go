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
	LateJoin      bool   // can join a running match (MATCH_ADMIT_*)
	LocalPlayers  bool   // reads the peerId routing that local players need
	Social        bool   // can receive private messages and challenges
	SpectatorChat bool   // shares a match's spectator chat with its players
	SaveData      bool   // can play a match that starts from the host's save data
	GameOptions   bool   // can play a match with the host's game options on
	// The PC doesn't take challenges. Set on the connection's owner only; its
	// local players follow it (see ChallengesOff).
	NoChallenges bool
	// The PC doesn't take private messages, except replies (see
	// TakesPrivateMessagesFrom). Owner only, like NoChallenges.
	NoPrivateMessages bool
	// Whom the PC last wrote to, and when: they may answer for a while even with
	// NoPrivateMessages on. Owner only.
	pmSentAt map[uint64]time.Time

	// The games this client has, by upper-case MD5, from HELLO. Set once and
	// only read afterwards.
	RomHashes map[string]struct{}

	PingToServer uint16

	CurrentRoom  *Room
	CurrentMatch *Match

	JoinedAt time.Time
	LastSeen time.Time

	// Send is the outbound message channel owned by the WS client goroutine.
	// The hub writes serialized JSON envelopes here; the client's writeLoop drains it.
	Send chan<- []byte

	// A local player is a user with no connection of its own: Owner is the user
	// whose connection it shares and Send stays nil, so anything sent to the
	// room reaches that PC once, through the owner's seat. Everything else
	// treats it as an ordinary user. Locals are the owner's local players, in
	// the order they were added.
	Owner  *User
	Locals []*User
}

// Primary returns the user whose connection u plays through: u itself, or its
// owner when u is a local player.
func (u *User) Primary() *User {
	if u.Owner != nil {
		return u.Owner
	}
	return u
}

// Group returns everyone on u's connection, the owner first and then the local
// players in the order they were added. They enter, leave and join matches
// together.
func (u *User) Group() []*User {
	owner := u.Primary()
	owner.mu.RLock()
	defer owner.mu.RUnlock()
	out := make([]*User, 0, len(owner.Locals)+1)
	out = append(out, owner)
	return append(out, owner.Locals...)
}

// AddLocal adds a local player to u's connection.
func (u *User) AddLocal(local *User) {
	u.mu.Lock()
	defer u.mu.Unlock()
	local.Owner = u
	u.Locals = append(u.Locals, local)
}

// RemoveLocal takes a local player off u's connection and returns it, or nil if
// userID isn't one of u's.
func (u *User) RemoveLocal(userID uint64) *User {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, local := range u.Locals {
		if local.ID == userID {
			u.Locals = append(u.Locals[:i], u.Locals[i+1:]...)
			return local
		}
	}
	return nil
}

// ChallengesOff reports whether u's PC turned challenges off.
func (u *User) ChallengesOff() bool {
	owner := u.Primary()
	owner.mu.RLock()
	defer owner.mu.RUnlock()
	return owner.NoChallenges
}

// SetChallengesOff records the PC's choice on u's owner and reports whether it
// changed.
func (u *User) SetChallengesOff(off bool) bool {
	owner := u.Primary()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.NoChallenges == off {
		return false
	}
	owner.NoChallenges = off
	return true
}

// PrivateMessageReplyWindow is how long someone who got a private message can
// answer it, even from a PC that turned private messages off.
const PrivateMessageReplyWindow = 10 * time.Minute

// PrivateMessagesOff reports whether u's PC turned private messages off.
func (u *User) PrivateMessagesOff() bool {
	owner := u.Primary()
	owner.mu.RLock()
	defer owner.mu.RUnlock()
	return owner.NoPrivateMessages
}

// SetPrivateMessagesOff records the PC's choice on u's owner and reports whether
// it changed.
func (u *User) SetPrivateMessagesOff(off bool) bool {
	owner := u.Primary()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.NoPrivateMessages == off {
		return false
	}
	owner.NoPrivateMessages = off
	return true
}

// NotePrivateMessageSent records that u's PC wrote to userID at "at", so that
// userID can answer within PrivateMessageReplyWindow.
func (u *User) NotePrivateMessageSent(userID uint64, at time.Time) {
	owner := u.Primary()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.pmSentAt == nil {
		owner.pmSentAt = make(map[uint64]time.Time)
	}
	for id, sent := range owner.pmSentAt {
		if at.Sub(sent) >= PrivateMessageReplyWindow {
			delete(owner.pmSentAt, id)
		}
	}
	owner.pmSentAt[userID] = at
}

// TakesPrivateMessagesFrom reports whether fromUserID can write to u at now: u's
// PC takes private messages, or it wrote to fromUserID within the reply window.
func (u *User) TakesPrivateMessagesFrom(fromUserID uint64, now time.Time) bool {
	owner := u.Primary()
	owner.mu.RLock()
	defer owner.mu.RUnlock()
	if !owner.NoPrivateMessages {
		return true
	}
	sent, ok := owner.pmSentAt[fromUserID]
	return ok && now.Sub(sent) < PrivateMessageReplyWindow
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

// CompareAndSetState sets to only if the current state is one of from, and
// reports whether it did. Check and write share one lock, so a concurrent
// SetState from another flow is never overwritten.
func (u *User) CompareAndSetState(from []UserState, to UserState) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, s := range from {
		if u.State == s {
			u.State = to
			return true
		}
	}
	return false
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
