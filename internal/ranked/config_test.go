package ranked

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `{
  "ladders": [{
    "id": "smashremix-1v1", "name": "Smash Remix 1v1", "moduleId": "smashremix",
    "rulesets": ["remix-1v1-standard"], "bestOf": [3, 5],
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
	if n, ok := l.SetLength(0); !ok || n != 3 {
		t.Fatalf("default bestOf = %d", n)
	}
	if _, ok := l.SetLength(7); ok {
		t.Fatal("bestOf 7 accepted")
	}
	if len(c.Tiers.Bands) == 0 {
		t.Fatal("default tiers missing")
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
		`{"ladders":[{"id":"x","moduleId":"m","rulesets":["r"],"bestOf":[2]}]}`,
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
