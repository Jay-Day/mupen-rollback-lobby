package state

import "testing"

func TestAddUserMatchesNamesByKeyAndLook(t *testing.T) {
	s := New()
	var nextID uint64
	add := func(name string) error {
		nextID++
		return s.AddUser(&User{ID: nextID, Username: name})
	}

	if err := add("Bob"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Bob", "bob", "ＢＯＢ", "Воb" /* Cyrillic В, о */} {
		if err := add(name); err != ErrUsernameTaken {
			t.Errorf("add %q next to Bob: %v, want taken", name, err)
		}
	}

	// Plain ASCII names that merely resemble each other have always been allowed.
	if err := add("B0b"); err != nil {
		t.Errorf("add B0b next to Bob: %v", err)
	}

	if err := add("Вов2"); err != nil {
		t.Errorf("add Вов2: %v", err)
	}
	if err := add("José"); err != nil {
		t.Fatal(err)
	}
	if err := add("JOSÉ"); err != ErrUsernameTaken {
		t.Errorf("add JOSÉ next to José: %v, want taken", err)
	}
	if got := s.GetUserByName("JOSÉ"); got == nil || got.Username != "José" {
		t.Errorf("GetUserByName(JOSÉ) = %v", got)
	}

	// The other way round: a lookalike online blocks the ASCII name it imitates.
	if err := add("Ѕаm"); err != nil { // Cyrillic Ѕ, а
		t.Fatal(err)
	}
	if err := add("Sam"); err != ErrUsernameTaken {
		t.Errorf("add Sam next to its lookalike: %v, want taken", err)
	}

	// Leaving frees both the name and its look.
	var bob, b0b *User
	for _, u := range s.ListUsers() {
		switch u.Username {
		case "Bob":
			bob = u
		case "B0b":
			b0b = u
		}
	}
	s.RemoveUser(bob.ID)
	if err := add("Воb"); err != ErrUsernameTaken {
		t.Errorf("add Воb while B0b is online: %v, want taken", err)
	}
	s.RemoveUser(b0b.ID)
	if err := add("Воb"); err != nil {
		t.Errorf("add Воb after the ASCII names left: %v", err)
	}
	if err := add("bob"); err != ErrUsernameTaken {
		t.Errorf("add bob next to its lookalike: %v, want taken", err)
	}
}

func TestKickSurvivesNameSpelling(t *testing.T) {
	r := &Room{}
	r.MarkKicked(1, "José")
	if !r.WasKicked(2, "JOSÉ") {
		t.Error("a kicked name came back under another spelling")
	}
	if r.WasKicked(2, "Jose") {
		t.Error("a different name counted as kicked")
	}
}
