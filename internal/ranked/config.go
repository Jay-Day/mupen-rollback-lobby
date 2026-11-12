// Package ranked holds the server-side ranked configuration: which ROMs can
// be played ranked, on which ladder, with which rulesets, and the tier bands.
//
// It is loaded from a JSON file (RMGK_RANKED_PATH, default ranked.json) so
// ladders and tiers can change without a release. Without the file, ranked
// rooms are unavailable and the lobby behaves as before.
package ranked

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/jay-day/rmgk-lobby/internal/gameoptions"
	"github.com/jay-day/rmgk-lobby/internal/rating"
)

// Rom is one ROM build allowed on a ladder.
type Rom struct {
	MD5  string `json:"md5"`
	Name string `json:"name"`
}

// Ladder is one rating pool, e.g. Smash Remix 1v1.
type Ladder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ModuleID is the RMG-K ranked game module that runs the set.
	ModuleID string `json:"moduleId"`
	// Rulesets lists allowed module ruleset ids; the first is the default.
	Rulesets []string `json:"rulesets"`
	// RulesetNames optionally gives rulesets display names (e.g. a roster).
	RulesetNames map[string]string `json:"rulesetNames,omitempty"`
	// RulesetDescriptions optionally explains each ruleset (rankings page).
	RulesetDescriptions map[string]string `json:"rulesetDescriptions,omitempty"`
	// RulesetStages optionally lists each ruleset's stages in stage select
	// order (rankings page). The game module enforces them, so keep this in
	// sync with it.
	RulesetStages map[string][]string `json:"rulesetStages,omitempty"`
	// RulesetOptions names, per ruleset, the game options its matches are
	// played with (ids from the game options packs), in the order clients
	// apply them. The lobby sends their ROM patches in MATCH_BEGIN; hosts
	// can't change them.
	RulesetOptions map[string][]string `json:"rulesetOptions,omitempty"`
	// TeamOptions (team ladders) names the game option that makes port "2",
	// "3" or "4" P1's teammate. A match whose teams the lobby matched gets
	// it after the ruleset's options.
	TeamOptions map[string]string `json:"teamOptions,omitempty"`
	// Rules lists the match rules every ruleset on the ladder shares, for
	// the rankings page (e.g. "4 stock, 8 minutes").
	Rules []string `json:"rules,omitempty"`
	// BestOf is the length of every set on the ladder (default 3).
	BestOf SetLength `json:"bestOf"`
	Roms   []Rom     `json:"roms"`
	// TeamSize is players per side: 1 for 1v1 (the default), 2 for 2v2
	// team battles. Team ladders are rated with OpenSkill.
	TeamSize int `json:"teamSize,omitempty"`
	// Placement and tiers (optional): players stay in placement while their
	// ± is above PlacementDeviation, and tiers come from rating -
	// TierDeviations × ±. Defaults: 110 and 2 for 1v1, 175 and 1 for teams.
	PlacementDeviation float64 `json:"placementDeviation,omitempty"`
	TierDeviations     float64 `json:"tierDeviations,omitempty"`
}

// SetLength is a set's length in games (best of N). Configs from when the
// host picked the length list several; the first, the old default, is used.
type SetLength int

func (n *SetLength) UnmarshalJSON(data []byte) error {
	var list []int
	if err := json.Unmarshal(data, &list); err == nil {
		*n = 0
		if len(list) > 0 {
			*n = SetLength(list[0])
		}
		return nil
	}
	var v int
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("bestOf must be a number: %w", err)
	}
	*n = SetLength(v)
	return nil
}

// Policy is the ladder's placement and tier rule.
func (l *Ladder) Policy() rating.Policy {
	p := rating.SoloPolicy
	if l.Teams() {
		p = rating.TeamPolicy
	}
	if l.PlacementDeviation > 0 {
		p.PlacementDeviation = l.PlacementDeviation
	}
	if l.TierDeviations > 0 {
		p.TierDeviations = l.TierDeviations
	}
	return p
}

// Policy is the placement and tier rule of a ladder by id (1v1's for an
// unknown ladder).
func (c *Config) Policy(ladderID string) rating.Policy {
	if l, ok := c.Ladder(ladderID); ok {
		return l.Policy()
	}
	return rating.SoloPolicy
}

// Players is how many players a set on this ladder needs.
func (l *Ladder) Players() int { return 2 * l.TeamSize }

// Teams reports whether this is a team ladder.
func (l *Ladder) Teams() bool { return l.TeamSize > 1 }

// Config is the whole ranked configuration.
type Config struct {
	Ladders []Ladder     `json:"ladders"`
	Tiers   rating.Tiers `json:"tiers"`

	byMD5 map[string][]*Ladder // a ROM can have one ladder per team size
	byID  map[string]*Ladder

	// The game options each ladder names, resolved against the packs for
	// each of its ROMs by UseGameOptions: per ruleset, and per teammate
	// port ("2"-"4"). Never changed afterwards.
	rulesetOptions map[optionsKey][]*gameoptions.Option
	teamOptions    map[optionsKey]*gameoptions.Option
}

type optionsKey struct {
	ladder, md5, name string
}

// teammatePorts are the ports P1's teammate can have in a 2v2 set.
var teammatePorts = []string{"2", "3", "4"}

// Load reads the config at path. A missing file yields an empty config with
// ranked disabled.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Parse(nil)
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse validates and indexes a config. nil data yields an empty config.
func Parse(data []byte) (*Config, error) {
	c := &Config{}
	// tolerate a UTF-8 byte order mark (Windows editors add one)
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(data) > 0 {
		if err := json.Unmarshal(data, c); err != nil {
			return nil, err
		}
	}
	c.Tiers = c.Tiers.Normalized()
	c.byMD5 = make(map[string][]*Ladder)
	c.byID = make(map[string]*Ladder)
	for i := range c.Ladders {
		l := &c.Ladders[i]
		if l.ID == "" || l.ModuleID == "" {
			return nil, fmt.Errorf("ladder %d: id and moduleId are required", i)
		}
		if _, dup := c.byID[l.ID]; dup {
			return nil, fmt.Errorf("duplicate ladder id %q", l.ID)
		}
		if len(l.Rulesets) == 0 {
			return nil, fmt.Errorf("ladder %q: at least one ruleset is required", l.ID)
		}
		for id := range l.RulesetStages {
			if _, ok := l.Ruleset(id); !ok || id == "" {
				return nil, fmt.Errorf("ladder %q: rulesetStages names unknown ruleset %q", l.ID, id)
			}
		}
		if l.BestOf == 0 {
			l.BestOf = 3
		}
		if l.TeamSize == 0 {
			l.TeamSize = 1
		}
		if l.TeamSize != 1 && l.TeamSize != 2 {
			return nil, fmt.Errorf("ladder %q: teamSize %d must be 1 or 2", l.ID, l.TeamSize)
		}
		if l.BestOf < 1 || l.BestOf%2 == 0 {
			return nil, fmt.Errorf("ladder %q: bestOf %d must be a positive odd number", l.ID, l.BestOf)
		}
		if err := l.checkGameOptionNames(); err != nil {
			return nil, fmt.Errorf("ladder %q: %w", l.ID, err)
		}
		for _, rom := range l.Roms {
			md5 := strings.ToUpper(strings.TrimSpace(rom.MD5))
			if len(md5) != 32 {
				return nil, fmt.Errorf("ladder %q: invalid md5 %q", l.ID, rom.MD5)
			}
			for _, other := range c.byMD5[md5] {
				if other.TeamSize == l.TeamSize {
					return nil, fmt.Errorf("md5 %s is on ladders %q and %q", md5, other.ID, l.ID)
				}
			}
			c.byMD5[md5] = append(c.byMD5[md5], l)
		}
		c.byID[l.ID] = l
	}
	return c, nil
}

// checkGameOptionNames checks rulesetOptions and teamOptions as written;
// UseGameOptions checks them against the packs.
func (l *Ladder) checkGameOptionNames() error {
	for ruleset, ids := range l.RulesetOptions {
		if _, ok := l.Ruleset(ruleset); !ok || ruleset == "" {
			return fmt.Errorf("rulesetOptions names unknown ruleset %q", ruleset)
		}
		for i, id := range ids {
			if !gameoptions.ValidOptionID(id) {
				return fmt.Errorf("rulesetOptions[%q]: %q isn't an option id", ruleset, id)
			}
			for _, earlier := range ids[:i] {
				if earlier == id {
					return fmt.Errorf("rulesetOptions[%q] names %q twice", ruleset, id)
				}
			}
		}
	}
	if len(l.TeamOptions) == 0 {
		return nil
	}
	if !l.Teams() {
		return errors.New("teamOptions is for team ladders")
	}
	for port, id := range l.TeamOptions {
		if !slices.Contains(teammatePorts, port) {
			return fmt.Errorf("teamOptions: %q isn't a teammate port (2, 3 or 4)", port)
		}
		if !gameoptions.ValidOptionID(id) {
			return fmt.Errorf("teamOptions[%q]: %q isn't an option id", port, id)
		}
	}
	for _, port := range teammatePorts {
		if _, ok := l.TeamOptions[port]; !ok {
			return fmt.Errorf("teamOptions needs an option for each teammate port (2, 3 and 4); %s is missing", port)
		}
	}
	return nil
}

// UseGameOptions checks every ladder's rulesetOptions and teamOptions
// against the game options packs, for each ROM on the ladder: every option
// must be offered for the ROM, and a ruleset's options, with each team
// option, must fit in one match (no more than 8, 64 KiB of patches, no two
// changing the same bytes). It keeps their patches for GameOptions. Call it
// once, before the config is used.
func (c *Config) UseGameOptions(packs *gameoptions.Set) error {
	c.rulesetOptions = make(map[optionsKey][]*gameoptions.Option)
	c.teamOptions = make(map[optionsKey]*gameoptions.Option)
	for i := range c.Ladders {
		l := &c.Ladders[i]
		if len(l.RulesetOptions) == 0 && len(l.TeamOptions) == 0 {
			continue
		}
		for _, rom := range l.Roms {
			md5 := normalizeMD5(rom.MD5)
			for _, ruleset := range l.Rulesets {
				ids := l.RulesetOptions[ruleset]
				opts, err := packs.Resolve(md5, ids)
				if err != nil {
					return fmt.Errorf("ladder %q, ruleset %q, ROM %s: %w", l.ID, ruleset, md5, err)
				}
				if len(ids) > 0 {
					c.rulesetOptions[optionsKey{l.ID, md5, ruleset}] = opts
				}
				for _, port := range teammatePorts {
					team, ok := l.TeamOptions[port]
					if !ok {
						continue
					}
					withTeam, err := packs.Resolve(md5, append(slices.Clone(ids), team))
					if err != nil {
						return fmt.Errorf("ladder %q, ruleset %q with teamOptions[%q], ROM %s: %w", l.ID, ruleset, port, md5, err)
					}
					c.teamOptions[optionsKey{l.ID, md5, port}] = withTeam[len(withTeam)-1]
				}
			}
		}
	}
	return nil
}

// GameOptions returns the game options a ranked set on a ladder's ruleset is
// played with on the ROM: the ruleset's, then (team ladders with
// teamOptions) the one that makes port teammate P1's teammate; teammate 0
// leaves that out. ok is false when the ladder names options UseGameOptions
// didn't resolve for this ROM, which leaves the ruleset unplayable.
func (c *Config) GameOptions(ladderID, rulesetID, romMD5 string, teammate int) ([]*gameoptions.Option, bool) {
	l, ok := c.byID[ladderID]
	if !ok {
		return nil, false
	}
	md5 := normalizeMD5(romMD5)
	var out []*gameoptions.Option
	if len(l.RulesetOptions[rulesetID]) > 0 {
		opts, ok := c.rulesetOptions[optionsKey{l.ID, md5, rulesetID}]
		if !ok {
			return nil, false
		}
		out = append(out, opts...)
	}
	if teammate != 0 && len(l.TeamOptions) > 0 {
		opt, ok := c.teamOptions[optionsKey{l.ID, md5, strconv.Itoa(teammate)}]
		if !ok {
			return nil, false
		}
		out = append(out, opt)
	}
	return out, true
}

func normalizeMD5(md5 string) string {
	return strings.ToUpper(strings.TrimSpace(md5))
}

// Enabled reports whether any ladder is configured.
func (c *Config) Enabled() bool { return len(c.Ladders) > 0 }

// LadderForRom returns the 1v1 ladder a ROM can be played ranked on (or its
// only ladder).
func (c *Config) LadderForRom(md5 string) (*Ladder, bool) {
	ladders := c.byMD5[strings.ToUpper(strings.TrimSpace(md5))]
	for _, l := range ladders {
		if l.TeamSize == 1 {
			return l, true
		}
	}
	if len(ladders) > 0 {
		return ladders[0], true
	}
	return nil, false
}

// LaddersForRom returns every ladder a ROM can be played ranked on.
func (c *Config) LaddersForRom(md5 string) []*Ladder {
	return c.byMD5[strings.ToUpper(strings.TrimSpace(md5))]
}

// RomLadder returns the ladder with this id when the ROM is on it, or the
// ROM's 1v1 ladder when id is empty.
func (c *Config) RomLadder(md5, id string) (*Ladder, bool) {
	if id == "" {
		return c.LadderForRom(md5)
	}
	for _, l := range c.LaddersForRom(md5) {
		if l.ID == id {
			return l, true
		}
	}
	return nil, false
}

// Ladder returns a ladder by id.
func (c *Config) Ladder(id string) (*Ladder, bool) {
	l, ok := c.byID[id]
	return l, ok
}

// Ruleset returns the requested ruleset if allowed ("" = default).
func (l *Ladder) Ruleset(id string) (string, bool) {
	if id == "" {
		return l.Rulesets[0], true
	}
	for _, r := range l.Rulesets {
		if r == id {
			return r, true
		}
	}
	return "", false
}

// RulesetName returns a ruleset's display name, falling back to its id.
func (l *Ladder) RulesetName(id string) string {
	if name := l.RulesetNames[id]; name != "" {
		return name
	}
	return id
}
