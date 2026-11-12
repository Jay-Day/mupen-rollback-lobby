package ranked

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/gameoptions"
)

const sample = `{
  "ladders": [{
    "id": "smashremix-1v1", "name": "Smash Remix 1v1", "moduleId": "smashremix",
    "rulesets": ["remix-1v1-standard"], "bestOf": 5,
    "roms": [{"md5": "2b2d6b295106c54216b7fc7a2f14346e", "name": "Smash Remix 2.0.1"}]
  }]
}`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.Enabled() {
		t.Fatal("not enabled")
	}
	l, ok := c.LadderForRom("2B2D6B295106C54216B7FC7A2F14346E")
	if !ok || l.ID != "smashremix-1v1" {
		t.Fatalf("LadderForRom = %v, %v", l, ok)
	}
	if r, ok := l.Ruleset(""); !ok || r != "remix-1v1-standard" {
		t.Fatalf("default ruleset = %q", r)
	}
	if _, ok := l.Ruleset("anything-goes"); ok {
		t.Fatal("unknown ruleset accepted")
	}
	if l.BestOf != 5 {
		t.Fatalf("bestOf = %d", l.BestOf)
	}
	if len(c.Tiers.Bands) == 0 {
		t.Fatal("default tiers missing")
	}
}

// Set lengths used to be the host's choice from a list; such a config keeps
// loading with its first (default) length.
func TestParseSetLength(t *testing.T) {
	for config, want := range map[string]SetLength{
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"]}]}`:                3,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"bestOf":[3,5]}]}`: 3,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"bestOf":[5]}]}`:   5,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"bestOf":[]}]}`:    3,
	} {
		c, err := Parse([]byte(config))
		if err != nil {
			t.Fatalf("Parse(%s): %v", config, err)
		}
		if got := c.Ladders[0].BestOf; got != want {
			t.Errorf("Parse(%s): bestOf = %d, want %d", config, got, want)
		}
	}
}

func TestParseAcceptsByteOrderMark(t *testing.T) {
	if c, err := Parse(append([]byte("\xef\xbb\xbf"), sample...)); err != nil || !c.Enabled() {
		t.Fatalf("Parse with BOM = %v, %v", c, err)
	}
}

func TestParseRejectsBadConfig(t *testing.T) {
	bad := []string{
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":[]}]}`,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"bestOf":2}]}`,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"bestOf":[2,3]}]}`,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"bestOf":"3"}]}`,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"roms":[{"md5":"abc"}]}]}`,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"]},{"id":"x","moduleId":"m","rulesets":["r"]}]}`,
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"rulesetStages":{"other":["Dream Land"]}}]}`,
	}
	for _, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

func TestLoadMissingFileDisablesRanked(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || c.Enabled() {
		t.Fatalf("Load(missing) = %v, %v", c, err)
	}
	path := filepath.Join(t.TempDir(), "ranked.json")
	os.WriteFile(path, []byte(sample), 0o644)
	if c, err := Load(path); err != nil || !c.Enabled() {
		t.Fatalf("Load = %v, %v", c, err)
	}
}

func TestTeamLadders(t *testing.T) {
	cfg, err := Parse([]byte(`{"ladders":[
		{"id":"one","moduleId":"m","rulesets":["r1"],"roms":[{"md5":"2b2d6b295106c54216b7fc7a2f14346e"}]},
		{"id":"two","moduleId":"m","rulesets":["r2"],"teamSize":2,"roms":[{"md5":"2B2D6B295106C54216B7FC7A2F14346E"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	md5 := "2b2d6b295106c54216b7fc7a2f14346e"
	if l, ok := cfg.LadderForRom(md5); !ok || l.ID != "one" || l.Players() != 2 || l.Teams() {
		t.Fatalf("default ladder = %+v", l)
	}
	if l, ok := cfg.RomLadder(md5, "two"); !ok || l.ID != "two" || l.Players() != 4 || !l.Teams() {
		t.Fatalf("team ladder = %+v", l)
	}
	if _, ok := cfg.RomLadder("00000000000000000000000000000000", "two"); ok {
		t.Fatal("team ladder found for another ROM")
	}
	if len(cfg.LaddersForRom(md5)) != 2 {
		t.Fatal("expected two ladders for the ROM")
	}

	for _, bad := range []string{
		`{"ladders":[{"id":"a","moduleId":"m","rulesets":["r"],"teamSize":3}]}`,
		`{"ladders":[{"id":"a","moduleId":"m","rulesets":["r"],"teamSize":2,"roms":[{"md5":"2b2d6b295106c54216b7fc7a2f14346e"}]},
		  {"id":"b","moduleId":"m","rulesets":["r"],"teamSize":2,"roms":[{"md5":"2b2d6b295106c54216b7fc7a2f14346e"}]}]}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := Load("../../ranked.example.json"); err != nil {
		t.Fatalf("example config: %v", err)
	}
}

const remixMD5 = "2B2D6B295106C54216B7FC7A2F14346E"

// optionsConfig has the example's rulesets with the options the DESIGN
// assigns them, on a ladder per team size.
const optionsConfig = `{"ladders":[
	{"id":"one","moduleId":"m","rulesets":["v1","r1"],"roms":[{"md5":"2b2d6b295106c54216b7fc7a2f14346e"}],
	 "rulesetOptions":{"v1":["ranked-rules","vanilla-only","stages-dream-land"],"r1":["ranked-rules","stages-remix-ranked"]}},
	{"id":"two","moduleId":"m","rulesets":["v2","r2","plain"],"teamSize":2,"roms":[{"md5":"` + remixMD5 + `"}],
	 "rulesetOptions":{"v2":["ranked-rules","ranked-teams","vanilla-only","stages-dream-land"],"r2":["ranked-rules","ranked-teams","stages-remix-ranked"]},
	 "teamOptions":{"2":"teams-p1p2","3":"teams-p1p3","4":"teams-p1p4"}}]}`

func fixturePacks(t *testing.T) *gameoptions.Set {
	t.Helper()
	set, err := gameoptions.LoadDir("../gameoptions/testdata")
	if err != nil || len(set.Packs()) != 1 {
		t.Fatalf("fixture packs = %+v, %v", set, err)
	}
	return set
}

func optionIDs(opts []*gameoptions.Option) string {
	ids := make([]string, len(opts))
	for i, opt := range opts {
		ids[i] = opt.ID
	}
	return strings.Join(ids, ",")
}

func TestParseChecksGameOptionNames(t *testing.T) {
	ladder := func(fields string) string {
		return `{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],` + fields + `}]}`
	}
	if _, err := Parse([]byte(optionsConfig)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, bad := range []string{
		ladder(`"rulesetOptions":{"other":["a"]}`),
		ladder(`"rulesetOptions":{"":["a"]}`),
		ladder(`"rulesetOptions":{"r":["Ranked-Rules"]}`),
		ladder(`"rulesetOptions":{"r":[""]}`),
		ladder(`"rulesetOptions":{"r":["a","b","a"]}`),
		ladder(`"rulesetOptions":{"r":"a"}`),
		ladder(`"teamOptions":{"2":"a","3":"b","4":"c"}`), // a 1v1 ladder
		ladder(`"teamSize":2,"teamOptions":{"2":"a","3":"b"}`),
		ladder(`"teamSize":2,"teamOptions":{"1":"a","2":"a","3":"b","4":"c"}`),
		ladder(`"teamSize":2,"teamOptions":{"2":"a","3":"b","4":"C"}`),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestUseGameOptions(t *testing.T) {
	cfg, err := Parse([]byte(optionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	// until the packs are checked, rulesets with options can't be played
	if _, ok := cfg.GameOptions("one", "v1", remixMD5, 0); ok {
		t.Fatal("options before UseGameOptions")
	}
	if opts, ok := cfg.GameOptions("two", "plain", remixMD5, 0); !ok || len(opts) != 0 {
		t.Fatalf("a ruleset without options = %v, %v", opts, ok)
	}
	if err := cfg.UseGameOptions(fixturePacks(t)); err != nil {
		t.Fatalf("UseGameOptions: %v", err)
	}

	for _, tc := range []struct {
		ladder, ruleset string
		teammate        int
		want            string
	}{
		{"one", "v1", 0, "ranked-rules,vanilla-only,stages-dream-land"},
		{"one", "r1", 0, "ranked-rules,stages-remix-ranked"},
		{"one", "v1", 3, "ranked-rules,vanilla-only,stages-dream-land"}, // a 1v1 ladder has no team options
		{"two", "v2", 0, "ranked-rules,ranked-teams,vanilla-only,stages-dream-land"},
		{"two", "v2", 2, "ranked-rules,ranked-teams,vanilla-only,stages-dream-land,teams-p1p2"},
		{"two", "r2", 3, "ranked-rules,ranked-teams,stages-remix-ranked,teams-p1p3"},
		{"two", "r2", 4, "ranked-rules,ranked-teams,stages-remix-ranked,teams-p1p4"},
		{"two", "plain", 4, "teams-p1p4"},
	} {
		opts, ok := cfg.GameOptions(tc.ladder, tc.ruleset, "  2b2d6b295106c54216b7fc7a2f14346e ", tc.teammate)
		if !ok || optionIDs(opts) != tc.want {
			t.Errorf("%s/%s teammate %d = %s, %v; want %s", tc.ladder, tc.ruleset, tc.teammate, optionIDs(opts), ok, tc.want)
		}
	}
	opts, _ := cfg.GameOptions("one", "v1", remixMD5, 0)
	if opts[0].Label != "Ranked rules" || len(opts[0].Patches) != 2 || opts[0].Patches[1].Offset != 0x02F00200 {
		t.Fatalf("ranked-rules = %+v", opts[0])
	}
	if _, ok := cfg.GameOptions("one", "v1", "00000000000000000000000000000000", 0); ok {
		t.Fatal("options for a ROM that isn't on the ladder")
	}
	if _, ok := cfg.GameOptions("three", "v1", remixMD5, 0); ok {
		t.Fatal("options for an unknown ladder")
	}
	if _, ok := cfg.GameOptions("two", "v2", remixMD5, 5); ok {
		t.Fatal("a team option for port 5")
	}
}

// A config naming options that don't exist for one of the ladder's ROMs, or
// that can't be on together, fails to load.
func TestUseGameOptionsRefusesWhatCantBePlayed(t *testing.T) {
	packs := fixturePacks(t)
	for name, config := range map[string]string{
		"missing option": `{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"roms":[{"md5":"` + remixMD5 + `"}],
			"rulesetOptions":{"r":["ranked-rules","no-such-option"]}}]}`,
		"second ROM without a pack": `{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],
			"roms":[{"md5":"` + remixMD5 + `"},{"md5":"00112233445566778899AABBCCDDEEFF"}],
			"rulesetOptions":{"r":["ranked-rules"]}}]}`,
		"overlapping options": `{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"roms":[{"md5":"` + remixMD5 + `"}],
			"rulesetOptions":{"r":["stages-dream-land","stages-remix-ranked"]}}]}`,
		"team option overlaps the ruleset's": `{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"teamSize":2,
			"roms":[{"md5":"` + remixMD5 + `"}],"rulesetOptions":{"r":["ranked-rules","teams-p1p2"]},
			"teamOptions":{"2":"teams-p1p3","3":"teams-p1p3","4":"teams-p1p4"}}]}`,
		"missing team option": `{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"teamSize":2,
			"roms":[{"md5":"` + remixMD5 + `"}],"teamOptions":{"2":"teams-p1p2","3":"teams-p1p3","4":"teams-p1p5"}}]}`,
	} {
		cfg, err := Parse([]byte(config))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		if err := cfg.UseGameOptions(packs); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

// The example config names the real option ids. Once the shipped pack has
// them, it must load against it.
func TestExampleConfigAgainstShippedPacks(t *testing.T) {
	cfg, err := Load("../../ranked.example.json")
	if err != nil {
		t.Fatal(err)
	}
	packs, err := gameoptions.LoadDir("../../gameoptions")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := packs.Option(remixMD5, "ranked-rules"); !ok {
		t.Skip("the shipped Smash Remix pack has no ranked options yet")
	}
	if err := cfg.UseGameOptions(packs); err != nil {
		t.Fatalf("example config against gameoptions/: %v", err)
	}
}
