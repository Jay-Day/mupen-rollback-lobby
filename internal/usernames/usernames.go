// Package usernames holds the rules for player names: what a name may contain,
// which spelling the lobby keeps, and when two names count as the same person.
//
// Names are limited to scripts the in-game overlay can draw correctly. It has no
// text shaping or right-to-left layout, so Arabic, Hebrew, Thai and the Indic
// scripts stay out until it does. Chat isn't limited this way.
package usernames

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"
	"golang.org/x/text/unicode/norm"
)

const (
	MinChars = 2
	MaxChars = 20
	// MaxBytes is what fits a name in a replay header: 32 bytes per player, one
	// of them the terminator. In practice 10 Japanese, Chinese or Korean characters.
	MaxBytes = 31
	// maxMarks caps combining marks in a row, which is enough for real text and
	// keeps stacked "zalgo" names out of everyone's player list.
	maxMarks = 2
)

// ErrInvalid means a name breaks the rules above.
var ErrInvalid = errors.New("invalid username")

var (
	displayProfile = precis.UsernameCasePreserved
	keyProfile     = precis.NewIdentifier(precis.FoldWidth, precis.FoldCase(), precis.Norm(norm.NFC), precis.BidiRule)
)

// Scripts allowed together in one name, as a bit per script.
const (
	scriptLatin = 1 << iota
	scriptGreek
	scriptCyrillic
	scriptHan
	scriptKana
	scriptHangul
)

var scriptTables = []struct {
	bit   int
	table *unicode.RangeTable
}{
	{scriptLatin, unicode.Latin},
	{scriptGreek, unicode.Greek},
	{scriptCyrillic, unicode.Cyrillic},
	{scriptHan, unicode.Han},
	{scriptKana, unicode.Hiragana},
	{scriptKana, unicode.Katakana},
	{scriptHangul, unicode.Hangul},
}

// The mixes people actually write: Latin alongside Japanese or Korean. Latin with
// Cyrillic or Greek is the classic way to fake someone's name, so those stand alone.
var allowedMixes = []int{
	scriptLatin | scriptHan | scriptKana,
	scriptLatin | scriptHan | scriptHangul,
	scriptGreek,
	scriptCyrillic,
}

// Normalize returns the spelling the lobby keeps for raw, with accents composed
// and fullwidth forms folded, or ErrInvalid if raw can't be a name.
func Normalize(raw string) (string, error) {
	name, err := displayProfile.String(raw)
	if err != nil {
		return "", ErrInvalid
	}
	if n := utf8.RuneCountInString(name); n < MinChars || n > MaxChars || len(name) > MaxBytes {
		return "", ErrInvalid
	}

	scripts, marks := 0, 0
	for i, r := range name {
		if unicode.Is(unicode.Mn, r) {
			marks++
			if i == 0 || marks > maxMarks || !unicode.Is(unicode.Inherited, r) {
				return "", ErrInvalid
			}
			continue
		}
		marks = 0
		bit, ok := scriptOf(r)
		if !ok {
			return "", ErrInvalid
		}
		scripts |= bit
	}
	for _, mix := range allowedMixes {
		if scripts&^mix == 0 {
			return name, nil
		}
	}
	return "", ErrInvalid
}

// scriptOf returns r's script bit, or 0 for a character that goes with any script.
func scriptOf(r rune) (int, bool) {
	switch {
	case r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		return 0, true
	case r == 'ー', r == '・': // written in katakana, but Unicode files them as shared
		return scriptKana, true
	}
	if unicode.IsLetter(r) {
		for _, s := range scriptTables {
			if unicode.Is(s.table, r) {
				return s.bit, true
			}
		}
	}
	return 0, false
}

// Key is what a name is matched by: case, width and accent encoding don't count.
// A string that couldn't be a name still gets a key; it just won't match any name.
func Key(name string) string {
	if key, err := keyProfile.String(name); err == nil {
		return key
	}
	return strings.ToLower(strings.TrimSpace(name))
}

// Same reports whether two strings name the same player.
func Same(a, b string) bool { return Key(a) == Key(b) }

// Skeleton is how a name looks, so that Bob spelled with a Cyrillic о has the same
// skeleton as the real one (UTS #39, plus case folding, since names ignore case).
func Skeleton(name string) string {
	skeleton := strings.ToLower(mapConfusables(norm.NFD.String(name)))
	return norm.NFD.String(mapConfusables(skeleton))
}

func mapConfusables(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if target, ok := confusables[r]; ok {
			b.WriteString(target)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsASCII reports whether name is plain ASCII. Two ASCII names never count as
// lookalikes: "Paul" and "PauI" have always been allowed side by side.
func IsASCII(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Latinize reduces text to what it reads as in plain Latin letters, for matching
// against the chat filter's word list: compatibility forms fold, accents and
// invisible characters go, and non-ASCII lookalikes become their ASCII letters.
// ASCII itself stays put, because the filter has its own reading of 1 and 0.
func Latinize(s string) string {
	if IsASCII(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFD.String(norm.NFKC.String(s)) {
		switch {
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Cf, r):
			continue
		case r >= utf8.RuneSelf:
			if target, ok := confusables[r]; ok && IsASCII(target) {
				b.WriteString(target)
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}
