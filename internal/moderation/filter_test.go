package moderation

import (
	"strings"
	"testing"
)

// filter built from the embedded default blocklist.
func defaultFilter() *Filter { return NewFilter(DefaultTerms()) }

func TestFilterBlocks(t *testing.T) {
	f := defaultFilter()
	blocked := []string{
		"you are a nigger",
		"n1gger",          // leet
		"niiiigger",       // stretched
		"f4ggot lol",      // leet
		"spics",           // plural
		"niggers",         // plural
		"NIGGER",          // case
		"kike",
		"tranny",
		"sand nigger",     // two words, second is a slur token
		"g0ok",            // leet o
		"fag",             // short form, whole word
		"fags",            // short form, plural
		"f4g",             // short form, leet
		"faaag",           // short form, stretched (collapses to fag)
	}
	for _, m := range blocked {
		if _, hit := f.Check(m); !hit {
			t.Errorf("expected BLOCK for %q, got allow", m)
		}
	}
}

func TestFilterAllowsInnocent(t *testing.T) {
	f := defaultFilter()
	allowed := []string{
		"hello good game",
		"raccoon",          // contains "coon"
		"tycoon",
		"cocoon",
		"suspicious",       // contains "spic"
		"conspicuous",
		"the shed is open", // "the shed" -> "theshed" (must NOT be heshe)
		"washes her hands",
		"con artist",       // "con" must not be "coon"
		"classic assassin", // "ass"/"spic"-ish fragments
		"bass moon balloon",
		"it's a trap",      // trap omitted from default list
		"shotgun",
		"that was clutch",
		"capture the flag",  // "flag" contains "fag" — whole-word must not fire
		"flagged the play",  // "flagged" contains "fagg"
		"fragged him",       // adjacent fragment
	}
	for _, m := range allowed {
		if tok, hit := f.Check(m); hit {
			t.Errorf("expected ALLOW for %q, but blocked on token %q", m, tok)
		}
	}
}

// Spellings built from the blocklist itself: each should read as the word it hides.
func TestFilterSeesThroughOtherAlphabets(t *testing.T) {
	f := defaultFilter()
	lookalike := strings.NewReplacer("a", "а", "c", "с", "e", "е", "i", "і", "o", "о", "p", "р", "s", "ѕ", "x", "х", "y", "у")
	spellings := map[string]func(string) string{
		"fullwidth": func(w string) string {
			return strings.Map(func(r rune) rune { return r - 'a' + 'ａ' }, w)
		},
		"cyrillic lookalikes": lookalike.Replace,
		"accents": func(w string) string {
			return strings.ReplaceAll(w, "e", "é")
		},
		"zero-width": func(w string) string {
			return string([]rune(w)[:2]) + "​" + string([]rune(w)[2:])
		},
		"math letters": func(w string) string {
			return strings.Map(func(r rune) rune { return r - 'a' + 0x1D41A }, w)
		},
	}
	for how, spell := range spellings {
		for _, term := range []string{"kike", "tranny", "spic"} {
			word := spell(term)
			if word == term {
				continue
			}
			if _, hit := f.Check("you " + word + " lol"); !hit {
				t.Errorf("%s: %q got through", how, word)
			}
		}
	}
}

func TestFilterLeavesOtherLanguagesAlone(t *testing.T) {
	f := defaultFilter()
	for _, msg := range []string{
		"日本語のチャットです",
		"Привет, как дела?",
		"Γειά σου κόσμε",
		"한국어 채팅 테스트",
		"сор нос рот сон",
		"José y María",
	} {
		if term, hit := f.Check(msg); hit {
			t.Errorf("%q was blocked as %q", msg, term)
		}
	}
}
