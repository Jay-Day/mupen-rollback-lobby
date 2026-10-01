package moderation

import (
	"strings"
)

// Filter matches messages against a curated hate-speech blocklist (racial and
// anti-LGBTQ/anti-trans slurs — NOT general profanity).
//
// It matches on a WHOLE-WORD basis after normalizing each word, which is the
// conservative choice: it deliberately avoids substring matching so innocent
// words are never censored for containing a slur as a fragment ("raccoon" is
// not "coon", "suspicious" is not "spic", "the shed" is not "heshe").
//
// Per word it defeats the common evasions:
//   - leetspeak: '1'->i, '@'->a, '$'->s, ... (n1gg3r -> nigger)
//   - stretched letters: runs of 3+ identical letters collapse to one, while
//     legitimate double letters are preserved (niiiigger -> nigger, but the
//     real double-g in "nigger" and the "oo" in "coon" stay intact, so "con"
//     never collapses into "coon")
//   - simple plurals/possessives: a trailing s/z is also tried (niggers, spics)
//
// What it intentionally does NOT catch: letters spaced apart ("n i g g e r").
// Auto-detecting that without censoring normal sentences is unreliable, so that
// case is left to human moderators (mute/kick/ban).
type Filter struct {
	terms map[string]struct{} // normalized, de-leeted, run-collapsed terms
	// embedded holds the subset of terms long enough to be matched as
	// substrings inside a single token (see CheckName). Short terms stay
	// whole-word only because they appear inside too many innocent words.
	embedded []string
}

// minEmbeddedLen is the minimum canonical length a term needs before it is
// also matched as a substring of a username token.
const minEmbeddedLen = 5

// leet maps common substitution characters back to their letter.
var leet = map[rune]rune{
	'0': 'o', '1': 'i', '!': 'i', '|': 'i', '3': 'e', '4': 'a', '@': 'a',
	'5': 's', '$': 's', '7': 't', '+': 't', '8': 'b', '9': 'g', '6': 'g',
	'2': 'z', '£': 'e', '€': 'e',
}

// normalize lowercases, applies the leet map, and replaces every non-letter
// rune with a space. The result is a letters-and-spaces string.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if m, ok := leet[r]; ok {
			r = m
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// collapseRuns reduces runs of 3 or more identical letters to a single letter,
// while leaving runs of 1 or 2 untouched. This canonicalizes stretched spam
// ("niiigger" -> "nigger") without conflating words whose only difference is a
// legitimate double letter ("con" stays "con", "coon" stays "coon").
func collapseRuns(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(runes))
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && runes[j] == runes[i] {
			j++
		}
		runLen := j - i
		if runLen >= 3 {
			b.WriteRune(runes[i])
		} else {
			for k := 0; k < runLen; k++ {
				b.WriteRune(runes[i])
			}
		}
		i = j
	}
	return b.String()
}

// canon normalizes a single word (no spaces expected) to its match key.
func canon(word string) string {
	return collapseRuns(strings.ReplaceAll(normalize(word), " ", ""))
}

// NewFilter builds a Filter from raw blocklist terms (one slur per entry).
func NewFilter(terms []string) *Filter {
	f := &Filter{terms: make(map[string]struct{})}
	for _, t := range terms {
		key := canon(t)
		if key != "" {
			f.terms[key] = struct{}{}
		}
	}
	for key := range f.terms {
		if len(key) >= minEmbeddedLen {
			f.embedded = append(f.embedded, key)
		}
	}
	return f
}

// Size reports how many distinct terms are loaded.
func (f *Filter) Size() int { return len(f.terms) }

// match reports whether a single canonicalized token is blocked, also trying a
// trailing-s/z stripped form to catch simple plurals/possessives.
func (f *Filter) match(tok string) bool {
	if tok == "" {
		return false
	}
	if _, ok := f.terms[tok]; ok {
		return true
	}
	if n := len(tok); n > 3 {
		if last := tok[n-1]; last == 's' || last == 'z' {
			if _, ok := f.terms[tok[:n-1]]; ok {
				return true
			}
		}
	}
	return false
}

// Check reports whether msg contains a blocked word. The returned string is the
// offending (normalized) token, for server-side logging only — never echo it back.
func (f *Filter) Check(msg string) (string, bool) {
	if f == nil || len(f.terms) == 0 {
		return "", false
	}
	for _, w := range strings.Fields(normalize(msg)) {
		tok := collapseRuns(w)
		if f.match(tok) {
			return tok, true
		}
	}
	return "", false
}

// CheckName applies the chat filter to a username and additionally scans each
// token for embedded long terms. Usernames are a single run of characters, so
// the whole-word rule alone lets "unclefaggot" through even though "faggot" is
// blocked. Only terms of minEmbeddedLen+ letters are matched this way; short
// ones ("fag", "spic", "coon") remain whole-word to keep names like "Stefagio"
// or "Raccoon" allowed. The returned string is for server-side logging only.
func (f *Filter) CheckName(name string) (string, bool) {
	if f == nil || len(f.terms) == 0 {
		return "", false
	}
	if tok, hit := f.Check(name); hit {
		return tok, true
	}
	for _, w := range strings.Fields(normalize(name)) {
		tok := collapseRuns(w)
		for _, term := range f.embedded {
			if strings.Contains(tok, term) {
				return term, true
			}
		}
	}
	return "", false
}
