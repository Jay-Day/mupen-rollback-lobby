package moderation

import "testing"

// filter built from the embedded default blocklist.
func defaultFilter() *Filter { return NewFilter(DefaultTerms()) }

func TestFilterBlocks(t *testing.T) {
	f := defaultFilter()
	blocked := []string{
		"you are a nigger",
		"n1gger",     // leet
		"niiiigger",  // stretched
		"f4ggot lol", // leet
		"spics",      // plural
		"niggers",    // plural
		"NIGGER",     // case
		"kike",
		"tranny",
		"sand nigger", // two words, second is a slur token
		"g0ok",        // leet o
		"fag",         // short form, whole word
		"fags",        // short form, plural
		"f4g",         // short form, leet
		"faaag",       // short form, stretched (collapses to fag)
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
		"raccoon", // contains "coon"
		"tycoon",
		"cocoon",
		"suspicious", // contains "spic"
		"conspicuous",
		"the shed is open", // "the shed" -> "theshed" (must NOT be heshe)
		"washes her hands",
		"con artist",       // "con" must not be "coon"
		"classic assassin", // "ass"/"spic"-ish fragments
		"bass moon balloon",
		"it's a trap", // trap omitted from default list
		"shotgun",
		"that was clutch",
		"capture the flag", // "flag" contains "fag" — whole-word must not fire
		"flagged the play", // "flagged" contains "fagg"
		"fragged him",      // adjacent fragment
	}
	for _, m := range allowed {
		if tok, hit := f.Check(m); hit {
			t.Errorf("expected ALLOW for %q, but blocked on token %q", m, tok)
		}
	}
}

func TestCheckNameBlocksEmbedded(t *testing.T) {
	f := defaultFilter()
	blocked := []string{
		"unclefaggot",
		"UncleFaggot",
		"uncle_f4ggot", // leet inside a name
		"xXniggerXx",
		"trannyhunter",
		"faggot", // plain whole-word still blocked
		"fag",    // short term, whole word
	}
	for _, n := range blocked {
		if _, hit := f.CheckName(n); !hit {
			t.Errorf("expected BLOCK for name %q, got allow", n)
		}
	}
}

func TestCheckNameAllowsShortEmbedded(t *testing.T) {
	f := defaultFilter()
	allowed := []string{
		"Stefagio",      // contains "fag" but short terms are whole-word only
		"Spicy_Raccoon", // "spic"/"coon" fragments
		"supaniceguy",
		"texas_radio",
		"Terra",
	}
	for _, n := range allowed {
		if term, hit := f.CheckName(n); hit {
			t.Errorf("expected ALLOW for name %q, blocked on %q", n, term)
		}
	}
}
