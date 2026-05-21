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
