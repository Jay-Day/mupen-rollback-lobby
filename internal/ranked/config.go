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
	"strings"

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
	// Rules lists the match rules every ruleset on the ladder shares, for
	// the rankings page (e.g. "4 stock, 8 minutes").
	Rules []string `json:"rules,omitempty"`
	// BestOf lists allowed set lengths; the first is the default.
	BestOf []int `json:"bestOf"`
	Roms   []Rom `json:"roms"`
	// TeamSize is players per side: 1 for 1v1 (the default), 2 for 2v2
	// team battles. Team ladders are rated with OpenSkill.
	TeamSize int `json:"teamSize,omitempty"`
	// Placement and tiers (optional): players stay in placement while their
	// ± is above PlacementDeviation, and tiers come from rating -
	// TierDeviations × ±. Defaults: 110 and 2 for 1v1, 175 and 1 for teams.
	PlacementDeviation float64 `json:"placementDeviation,omitempty"`
	TierDeviations     float64 `json:"tierDeviations,omitempty"`
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
}

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
		if len(l.BestOf) == 0 {
			l.BestOf = []int{3}
		}
		if l.TeamSize == 0 {
			l.TeamSize = 1
		}
		if l.TeamSize != 1 && l.TeamSize != 2 {
			return nil, fmt.Errorf("ladder %q: teamSize %d must be 1 or 2", l.ID, l.TeamSize)
		}
		for _, n := range l.BestOf {
			if n < 1 || n%2 == 0 {
				return nil, fmt.Errorf("ladder %q: bestOf %d must be a positive odd number", l.ID, n)
			}
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

// SetLength returns the requested best-of if allowed (0 = default).
func (l *Ladder) SetLength(bestOf int) (int, bool) {
	if bestOf == 0 {
		return l.BestOf[0], true
	}
	for _, n := range l.BestOf {
		if n == bestOf {
			return n, true
		}
	}
	return 0, false
}
