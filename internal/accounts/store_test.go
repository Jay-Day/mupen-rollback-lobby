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

// Names outside ASCII match the way the lobby matches online names: by
// usernames.Key, with lookalikes taken unless both names are plain ASCII.
func TestRegisterUnicodeNames(t *testing.T) {
	s := openTestStore(t)

	jose, err := s.Register("José", "correct horse")
	if err != nil {
		t.Fatalf("Register(José): %v", err)
	}
	if _, err := s.Register("JOSÉ", "another password"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("upper-case duplicate err = %v", err)
	}
	// the same name with a combining accent, and in fullwidth letters
	if got, err := s.Login("José", "correct horse"); err != nil || got.ID != jose.ID || got.Username != "José" {
		t.Fatalf("Login(decomposed) = %+v, %v", got, err)
	}
	if got, err := s.AccountByName("ｊｏｓé"); err != nil || got.ID != jose.ID {
		t.Fatalf("AccountByName(fullwidth) = %+v, %v", got, err)
	}

	if _, err := s.Register("Bob", "password123"); err != nil {
		t.Fatalf("Register(Bob): %v", err)
	}
	// Bob with a Cyrillic о is Bob's
	if _, err := s.Register("Bоb", "password123"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("lookalike err = %v", err)
	}
	if ok, _ := s.IsRegistered("Bоb"); !ok {
		t.Fatal("IsRegistered(lookalike of Bob) = false")
	}
	// two ASCII names that only look alike stay apart
	if _, err := s.Register("PauI", "password123"); err != nil {
		t.Fatalf("Register(PauI): %v", err)
	}
	if _, err := s.Register("Paul", "password123"); err != nil {
		t.Fatalf("Register(Paul) next to PauI: %v", err)
	}
	if ok, _ := s.IsRegistered("Pau1"); ok {
		t.Fatal("IsRegistered(Pau1) = true; ASCII lookalikes aren't registered names")
	}

	if _, err := s.Register("スマブラ太郎", "password123"); err != nil {
		t.Fatalf("Register(スマブラ太郎): %v", err)
	}
	if ok, _ := s.IsRegistered("スマブラ太郎"); !ok {
		t.Fatal("IsRegistered(スマブラ太郎) = false")
	}
	if found, err := s.SearchAccounts("ブラ", 10); err != nil || len(found) != 1 || found[0].Username != "スマブラ太郎" {
		t.Fatalf("SearchAccounts(ブラ) = %+v, %v", found, err)
	}
	if found, err := s.SearchAccounts("JOS", 10); err != nil || len(found) != 1 || found[0].ID != jose.ID {
		t.Fatalf("SearchAccounts(JOS) = %+v, %v", found, err)
	}
}

// Accounts from before migration 11 get their name keys filled in.
func TestMigrationBackfillsUsernameKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := s.Register("Player_1", "correct horse")
	if _, err := s.db.Exec(`DROP INDEX accounts_username_key; DROP INDEX accounts_username_skeleton;
		ALTER TABLE accounts DROP COLUMN username_key; ALTER TABLE accounts DROP COLUMN username_skeleton;
		PRAGMA user_version = 10`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Login("player_1", "correct horse"); err != nil || got.ID != acct.ID {
		t.Fatalf("Login after backfill = %+v, %v", got, err)
	}
	if _, err := s.Register("PLAYER_1", "another password"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate after backfill err = %v", err)
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
