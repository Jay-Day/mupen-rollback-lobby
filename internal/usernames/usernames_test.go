package usernames

import (
	"strings"
	"testing"
)

func TestNormalizeAccepts(t *testing.T) {
	tests := []struct{ in, want string }{
		{"AB", "AB"},
		{"valid_name-1.2", "valid_name-1.2"},
		{"12345678901234567890", "12345678901234567890"},
		{"José", "José"},
		{"José", "José"}, // composed
		{"Ｊｏｅ", "Joe"},    // fullwidth folds
		{"Łukasz", "Łukasz"},
		{"Nguyễn", "Nguyễn"},
		{"Γιώργος", "Γιώργος"},
		{"Иван", "Иван"},
		{"スマブラ太郎", "スマブラ太郎"},
		{"スーパー・マリオ", "スーパー・マリオ"}, // the long-vowel mark and middle dot
		{"ﾏﾘｵ", "マリオ"},           // halfwidth katakana folds
		{"李小龙", "李小龙"},
		{"김철수", "김철수"},
		{"Ryu日本", "Ryu日本"},
		{"Kim철수", "Kim철수"},
		{"Иван2", "Иван2"},
		{"日本語の長い名前です", "日本語の長い名前です"}, // 10 characters, 30 bytes
	}
	for _, test := range tests {
		got, err := Normalize(test.in)
		if err != nil || got != test.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", test.in, got, err, test.want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	tests := []struct {
		why string
		in  string
	}{
		{"empty", ""},
		{"too short", "A"},
		{"one character after composing", "é"},
		{"too long", "123456789012345678901"},
		{"over the replay's 31 bytes", "日本語の長い名前ですよ"},
		{"space", "two words"},
		{"ideographic space", "日本　語"},
		{"chat-line brackets", "<Bob>"},
		{"slash", "a/b"},
		{"non-ASCII digits", "Bob٣"},
		{"Latin with Cyrillic", "Bоb"},
		{"Latin with Greek", "Bοb"},
		{"Cyrillic with Greek", "Иο"},
		{"Korean with kana", "철수マ"},
		{"Arabic", "محمد"},
		{"Hebrew", "דוד"},
		{"Thai", "สมชาย"},
		{"Devanagari", "राहुल"},
		{"emoji", "Bob😀"},
		{"zero-width joiner", "Bo‍b"},
		{"right-to-left override", "Bo‮b"},
		{"stacked marks", "Bó̂̃̄b"},
		{"leading mark", "́Bob"},
		{"control character", "Bo\x01b"},
		{"invalid UTF-8", "Bo\xffb"},
	}
	for _, test := range tests {
		if got, err := Normalize(test.in); err == nil {
			t.Errorf("%s: Normalize(%q) = %q, want an error", test.why, test.in, got)
		}
	}
}

func TestKeyMatchesAcrossSpellings(t *testing.T) {
	same := [][2]string{
		{"Bob", "bob"},
		{"José", "JOSÉ"},
		{"Straße", "STRASSE"},
		{"ΣΙΣΥΦΟΣ", "σισυφος"},
		{"ＢＯＢ", "bob"},
		{"Иван", "ИВАН"},
	}
	for _, pair := range same {
		if !Same(pair[0], pair[1]) {
			t.Errorf("Same(%q, %q) = false (%q, %q)", pair[0], pair[1], Key(pair[0]), Key(pair[1]))
		}
	}
	if Same("Bob", "Bоb") {
		t.Error("a Cyrillic lookalike has the same key; only its skeleton should match")
	}
	if Key(" two words ") != "two words" {
		t.Errorf("Key of a non-name = %q", Key(" two words "))
	}
}

func TestSkeletonCatchesLookalikes(t *testing.T) {
	alike := [][2]string{
		{"Bob", "Воb"},
		{"BOB", "ВОВ"},
		{"Sam", "Ѕаm"},
		{"pool", "рооl"},
		{"B0b", "Воb"},
		{"Alpha", "Αlpha"},
	}
	for _, pair := range alike {
		if Skeleton(pair[0]) != Skeleton(pair[1]) {
			t.Errorf("Skeleton(%q) = %q, Skeleton(%q) = %q", pair[0], Skeleton(pair[0]), pair[1], Skeleton(pair[1]))
		}
	}
	different := [][2]string{
		{"Bob", "Rob"},
		{"Иван", "Ivan"},
		{"José", "Jose"},
		{"日本", "本日"},
	}
	for _, pair := range different {
		if Skeleton(pair[0]) == Skeleton(pair[1]) {
			t.Errorf("%q and %q share skeleton %q", pair[0], pair[1], Skeleton(pair[0]))
		}
	}
}

func TestLatinize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain ascii 1 0", "plain ascii 1 0"},
		{"ｈｅｌｌｏ", "hello"},
		{"һеllо", "hello"},
		{"héllò", "hello"},
		{"he​llo", "hello"},
		{"𝐡𝐞𝐥𝐥𝐨", "hello"},
	}
	for _, test := range tests {
		if got := Latinize(test.in); got != test.want {
			t.Errorf("Latinize(%q) = %q, want %q", test.in, got, test.want)
		}
	}
	// Real text in other scripts isn't damaged beyond its lookalike letters.
	if got := Latinize("日本語"); got != "日本語" {
		t.Errorf("Latinize(日本語) = %q", got)
	}
	if strings.ContainsAny(Latinize("a‮b"), "‮") {
		t.Error("Latinize kept a bidi control")
	}
}
