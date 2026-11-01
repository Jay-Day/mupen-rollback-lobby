package state

import (
	"sync"
	"sync/atomic"

	"github.com/jay-day/rmgk-lobby/internal/usernames"
)

// State is the central in-memory store for all lobby data.
// Restart wipes everything; this is intentional (no DB, Kaillera-style).
type State struct {
	mu sync.RWMutex

	nextUserID  atomic.Uint64
	nextRoomID  atomic.Uint64
	nextMatchID atomic.Uint64

	users     map[uint64]*User
	usernames map[string]*User          // by usernames.Key
	lookalike map[string]lookalikeUsers // by usernames.Skeleton
	rooms     map[uint64]*Room
	matches   map[uint64]*Match
}

func New() *State {
	return &State{
		users:     make(map[uint64]*User),
		usernames: make(map[string]*User),
		lookalike: make(map[string]lookalikeUsers),
		rooms:     make(map[uint64]*Room),
		matches:   make(map[uint64]*Match),
	}
}

func (s *State) NextUserID() uint64  { return s.nextUserID.Add(1) }
func (s *State) NextRoomID() uint64  { return s.nextRoomID.Add(1) }
func (s *State) NextMatchID() uint64 { return s.nextMatchID.Add(1) }

// -- Users --

// lookalikeUsers counts the online names that share one skeleton.
type lookalikeUsers struct{ ascii, other int }

func (s *State) AddUser(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := usernames.Key(u.Username)
	if _, exists := s.usernames[key]; exists {
		return ErrUsernameTaken
	}
	// A name that only looks like someone else's is theirs too. Two plain ASCII
	// names are exempt: this rule isn't meant to split "Paul" from "PauI".
	skeleton := usernames.Skeleton(u.Username)
	alike := s.lookalike[skeleton]
	ascii := usernames.IsASCII(u.Username)
	if alike.other > 0 || (!ascii && alike.ascii > 0) {
		return ErrUsernameTaken
	}
	if ascii {
		alike.ascii++
	} else {
		alike.other++
	}
	s.lookalike[skeleton] = alike
	s.users[u.ID] = u
	s.usernames[key] = u
	return nil
}

func (s *State) RemoveUser(id uint64) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil
	}
	delete(s.users, id)
	delete(s.usernames, usernames.Key(u.Username))
	skeleton := usernames.Skeleton(u.Username)
	alike := s.lookalike[skeleton]
	if usernames.IsASCII(u.Username) {
		alike.ascii--
	} else {
		alike.other--
	}
	if alike.ascii <= 0 && alike.other <= 0 {
		delete(s.lookalike, skeleton)
	} else {
		s.lookalike[skeleton] = alike
	}
	return u
}

func (s *State) GetUser(id uint64) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[id]
}

func (s *State) GetUserByName(name string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usernames[usernames.Key(name)]
}

func (s *State) ListUsers() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out
}

func (s *State) UserCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// -- Rooms --

func (s *State) AddRoom(r *Room) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rooms[r.ID] = r
}

func (s *State) RemoveRoom(id uint64) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		return nil
	}
	delete(s.rooms, id)
	return r
}

func (s *State) GetRoom(id uint64) *Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms[id]
}

func (s *State) ListRooms() []*Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		out = append(out, r)
	}
	return out
}

// -- Matches --

func (s *State) AddMatch(m *Match) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matches[m.ID] = m
}

func (s *State) RemoveMatch(id uint64) *Match {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.matches[id]
	if !ok {
		return nil
	}
	delete(s.matches, id)
	return m
}

func (s *State) GetMatch(id uint64) *Match {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.matches[id]
}

func (s *State) ListMatches() []*Match {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Match, 0, len(s.matches))
	for _, m := range s.matches {
		out = append(out, m)
	}
	return out
}
