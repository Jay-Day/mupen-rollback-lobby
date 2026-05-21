package state

import "testing"

func newTestUser(id uint64, name string) *User {
	return &User{ID: id, Username: name}
}

// A simple full → leave middle → join sequence should never produce two seats
// with the same Slot. GekkoNet keys actors by slot, so a collision silently
// breaks 3-/4-player rollback.
func TestAddPlayerReusesFreedSlot(t *testing.T) {
	r := &Room{MaxPlayers: 4}

	for i := uint64(1); i <= 3; i++ {
		if err := r.AddPlayer(newTestUser(i, "")); err != nil {
			t.Fatalf("AddPlayer %d: %v", i, err)
		}
	}
	// Initial seating: slots 1,2,3.
	for i, seat := range r.Players {
		if got, want := seat.Slot, i+1; got != want {
			t.Fatalf("initial seat %d: slot=%d want %d", i, got, want)
		}
	}

	// Player 2 leaves; freed slot is 2.
	if !r.RemovePlayer(2) {
		t.Fatal("RemovePlayer(2) returned false")
	}

	// New joiner must take slot 2, not slot 3 (collision) or slot 4 (skip).
	if err := r.AddPlayer(newTestUser(99, "")); err != nil {
		t.Fatalf("AddPlayer after leave: %v", err)
	}

	seen := map[int]uint64{}
	for _, seat := range r.Players {
		if prev, dup := seen[seat.Slot]; dup {
			t.Fatalf("slot %d assigned to both user %d and %d", seat.Slot, prev, seat.User.ID)
		}
		seen[seat.Slot] = seat.User.ID
	}
	if seen[2] != 99 {
		t.Fatalf("new joiner did not take freed slot 2; got slot map %v", seen)
	}
}

// Filling 4 seats then having one leave and rejoin still respects MaxPlayers
// and exhausts all 4 distinct slots.
func TestAddPlayerFillsAllSlotsAfterChurn(t *testing.T) {
	r := &Room{MaxPlayers: 4}
	for i := uint64(1); i <= 4; i++ {
		if err := r.AddPlayer(newTestUser(i, "")); err != nil {
			t.Fatalf("AddPlayer %d: %v", i, err)
		}
	}
	if err := r.AddPlayer(newTestUser(5, "")); err != ErrRoomFull {
		t.Fatalf("5th AddPlayer: want ErrRoomFull, got %v", err)
	}

	r.RemovePlayer(1)
	r.RemovePlayer(3)
	if err := r.AddPlayer(newTestUser(10, "")); err != nil {
		t.Fatalf("AddPlayer 10: %v", err)
	}
	if err := r.AddPlayer(newTestUser(11, "")); err != nil {
		t.Fatalf("AddPlayer 11: %v", err)
	}

	slots := map[int]bool{}
	for _, seat := range r.Players {
		if slots[seat.Slot] {
			t.Fatalf("duplicate slot %d after churn", seat.Slot)
		}
		slots[seat.Slot] = true
		if seat.Slot < 1 || seat.Slot > 4 {
			t.Fatalf("slot %d out of range", seat.Slot)
		}
	}
	if len(slots) != 4 {
		t.Fatalf("expected 4 distinct slots, got %d (%v)", len(slots), slots)
	}
}
