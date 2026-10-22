package accounts

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRegisterLogin(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.Register("Player_1", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password err = %v", err)
	}
	acct, err := s.Register("Player_1", "correct horse")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := s.Register("player_1", "another password"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("case-insensitive duplicate err = %v", err)
	}

	got, err := s.Login("PLAYER_1", "correct horse")
	if err != nil || got.ID != acct.ID || got.Username != "Player_1" {
		t.Fatalf("Login = %+v, %v", got, err)
	}
	if _, err := s.Login("Player_1", "wrong password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password err = %v", err)
	}
	if _, err := s.Login("nobody", "whatever1"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user err = %v", err)
	}

	if ok, _ := s.IsRegistered("player_1"); !ok {
		t.Fatal("IsRegistered(player_1) = false")
	}
	if ok, _ := s.IsRegistered("guest"); ok {
		t.Fatal("IsRegistered(guest) = true")
	}
}

func TestSessions(t *testing.T) {
	s := openTestStore(t)
	acct, _ := s.Register("tokenuser", "password123")

	token, err := s.CreateSession(acct.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got, err := s.SessionAccount(token); err != nil || got.ID != acct.ID {
		t.Fatalf("SessionAccount = %+v, %v", got, err)
	}
	if _, err := s.SessionAccount("bogus"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("bogus token err = %v", err)
	}

	// expired
	s.now = func() time.Time { return time.Now().Add(SessionTTL + time.Hour) }
	if _, err := s.SessionAccount(token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired token err = %v", err)
	}
	s.now = time.Now

	token2, _ := s.CreateSession(acct.ID)
	if err := s.RevokeSession(token2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAccount(token2); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked token err = %v", err)
	}
}

func TestRecordRatedSet(t *testing.T) {
	s := openTestStore(t)
	a, _ := s.Register("alice", "password123")
	b, _ := s.Register("bob", "password123")

	rec := SetRecord{
		Ladder: "smashremix-1v1", MatchID: 7, BootID: "boot1",
		Status: SetRated, WinnerID: a.ID,
		Players: []SetPlayer{{AccountID: a.ID, Slot: 1, GamesWon: 2}, {AccountID: b.ID, Slot: 2, GamesWon: 1}},
	}
	out, err := s.RecordSet(rec)
	if err != nil {
		t.Fatalf("RecordSet: %v", err)
	}
	if out[0].After.Rating <= out[0].Before.Rating || out[1].After.Rating >= out[1].Before.Rating {
		t.Fatalf("outcomes = %+v", out)
	}

	ra, _ := s.Rating(a.ID, "smashremix-1v1", "")
	rb, _ := s.Rating(b.ID, "smashremix-1v1", "")
	if ra.SetsPlayed != 1 || rb.SetsPlayed != 1 || ra.Rating.Rating != out[0].After.Rating {
		t.Fatalf("stored ratings = %+v / %+v", ra, rb)
	}
	if other, _ := s.Rating(a.ID, "other-ladder", ""); other.SetsPlayed != 0 || other.Rating.Rating != 1500 {
		t.Fatalf("ladders not independent: %+v", other)
	}

	if _, err := s.RecordSet(rec); err == nil {
		t.Fatal("recording the same match twice succeeded")
	}
}

func TestRecordVoidedSetKeepsRatings(t *testing.T) {
	s := openTestStore(t)
	a, _ := s.Register("alice", "password123")
	b, _ := s.Register("bob", "password123")

	out, err := s.RecordSet(SetRecord{
		Ladder: "l", MatchID: 1, BootID: "boot", Status: SetVoided, Reason: "player_left",
		Players: []SetPlayer{{AccountID: a.ID, Slot: 1, GamesWon: 1}, {AccountID: b.ID, Slot: 2}},
	})
	if err != nil {
		t.Fatalf("RecordSet: %v", err)
	}
	if out[0].After != out[0].Before {
		t.Fatal("voided set changed a rating")
	}
	if ra, _ := s.Rating(a.ID, "l", ""); ra.SetsPlayed != 0 {
		t.Fatalf("voided set counted: %+v", ra)
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Register("keeper", "password123")
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Login("keeper", "password123"); err != nil {
		t.Fatalf("login after reopen: %v", err)
	}
}
