package ws

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
		{"スマブラ太郎", true},
		{"日本語の長い名前です", true},
		{"日本語の長い名前ですよ", false},
	}

	for _, test := range tests {
		t.Run(test.username, func(t *testing.T) {
			if got := isValidUsername(test.username); got != test.want {
				t.Fatalf("isValidUsername(%q) = %v, want %v", test.username, got, test.want)
			}
		})
	}
}

func TestClampStringKeepsWholeCharacters(t *testing.T) {
	if got := clampString("日本語", 7); got != "日本" {
		t.Errorf("clampString cut to %q", got)
	}
	if got := clampString("abc", 2); got != "ab" {
		t.Errorf("clampString cut to %q", got)
	}
	if got := clampString("abc", 9); got != "abc" {
		t.Errorf("clampString changed %q", got)
	}
}

func TestTruncateMessageCountsCharacters(t *testing.T) {
	full := strings.Repeat("語", maxChatMessageLen)
	if got := truncateMessage(full); got != full {
		t.Errorf("a %d-character message lost text: %d characters left", maxChatMessageLen, utf8.RuneCountInString(got))
	}
	got := truncateMessage(full + "語語")
	if utf8.RuneCountInString(got) != maxChatMessageLen || !utf8.ValidString(got) {
		t.Errorf("overlong message became %d characters, valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
}

func TestCleanText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"日本語 Привет", "日本語 Привет"},
		{"a‮b⁦c", "abc"},
		{"bad\xffbyte", "bad�byte"},
		{"👨‍👩‍👧", "👨‍👩‍👧"}, // joiners are how emoji combine
	}
	for _, test := range tests {
		if got := cleanText(test.in); got != test.want {
			t.Errorf("cleanText(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}
