package state

import (
	"sync"
	"time"
)

type MatchState string

const (
	MatchStateConnecting MatchState = "connecting"
	MatchStateActive     MatchState = "active"
	MatchStateFinished   MatchState = "finished"
)

type Match struct {
	mu sync.RWMutex

	ID        uint64
	RoomID    uint64
	Players   []*User
	StartedAt time.Time
	State     MatchState

	// Seats is the controller port count every client emulates (0: sized by
	// the highest occupied seat, so nobody can join mid-match).
	Seats int
	// Epoch counts the rollback sessions this match has run; each late join
	// moves every client to a new one.
	Epoch int
	// Admit is the late join hand-off in progress, if any.
	Admit *Admission
}

// Admission is a late join hand-off: the host adding Joiner to the match.
type Admission struct {
	ID        uint64
	Joiner    *User
	Slot      int
	StartedAt time.Time
}

// BeginAdmission starts a hand-off unless one is already running, and returns
// it along with the epoch the clients switch to.
func (m *Match) BeginAdmission(id uint64, joiner *User, slot int) (*Admission, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Admit != nil {
		return nil, 0, false
	}
	m.Epoch++
	m.Admit = &Admission{ID: id, Joiner: joiner, Slot: slot, StartedAt: time.Now()}
	return m.Admit, m.Epoch, true
}

// CurrentAdmission returns the running hand-off, or nil.
func (m *Match) CurrentAdmission() *Admission {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Admit
}

// EndAdmission clears the hand-off if it is still the one with this id.
func (m *Match) EndAdmission(id uint64) *Admission {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Admit == nil || m.Admit.ID != id {
		return nil
	}
	a := m.Admit
	m.Admit = nil
	return a
}

// AddPlayer appends u to the roster unless it is already there.
func (m *Match) AddPlayer(u *User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.Players {
		if p == u {
			return
		}
	}
	m.Players = append(m.Players, u)
}

func (m *Match) SetState(s MatchState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.State = s
}

func (m *Match) GetState() MatchState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.State
}
