package ws

import "testing"

func TestUsernameLengthBoundaries(t *testing.T) {
	tests := []struct {
		username string
		want     bool
	}{
		{"A", false},
		{"AB", true},
		{"12345678901234567890", true},
		{"123456789012345678901", false},
		{"valid_name-1.2", true},
		{"invalid name", false},
	}

	for _, test := range tests {
		t.Run(test.username, func(t *testing.T) {
			if got := isValidUsername(test.username); got != test.want {
				t.Fatalf("isValidUsername(%q) = %v, want %v", test.username, got, test.want)
			}
		})
	}
}
