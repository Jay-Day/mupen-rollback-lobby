// ranked-seed fills a lobby database with a simulated ranked history, for
// trying the rankings site with a realistic number of players.
//
// Every player gets a hidden skill on each roster, an activity level and a
// few main characters. The simulation then plays months of sets between
// players of similar rating through the store's normal rating code, so the
// leaderboards, tiers, placement, inactivity and rating charts all behave as
// they would after real play. A few sets are voided or disputed.
//
// Usage (against a copy, never a production database):
//
//	go run ./cmd/ranked-seed --db seed.db --players 600 --days 150
//
// When the config also has a 2v2 ladder (teamSize 2), about half the
// players play team sets too: four players of similar team rating, split into
// balanced teams on random ports.
//
// Every generated account's password is "password123". With --replay-dir,
// a share of the sets also gets a stored replay: a tiny placeholder .krec
// that downloads like a real one but doesn't play a real match.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ranked"
)

// Character ids by roster (the game's own ids, as the site's CHARACTERS map).
var vanillaCast = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
var remixCast = append(append([]int{}, vanillaCast...),
	29, 30, 31, 32, 33, 34, 38, 52, 55, 56, 57, 58, 59, 62, 63, 64, 65, 68, 72, 73)

type roster struct {
	id   string
	cast []int
}

type player struct {
	id       int64
	name     string
	skill    map[string]float64 // hidden skill per roster (Elo-like scale)
	plays    map[string]bool    // which rosters they queue for
	mains    map[string][]int   // favourite characters per roster
	activity float64            // expected sets per day while active
	joined   int                // day they start
	quits    int                // day they stop (past the end = still active)
	region   string             // home lobby region
}

// seedRegions weights where players connect from.
var seedRegions = []struct {
	region string
	weight float64
}{
	{"us-east", 25}, {"us-central", 15}, {"us-west", 12}, {"sa", 8}, {"eu-west", 18},
	{"eu-east", 7}, {"asia-east", 8}, {"asia-se", 4}, {"oceania", 3},
}

// regionRng draws regions separately, so adding them didn't change the
// players and sets a seed produces.
var regionRng *rand.Rand

func randomRegion() string {
	rng := regionRng
	total := 0.0
	for _, r := range seedRegions {
		total += r.weight
	}
	x := rng.Float64() * total
	for _, r := range seedRegions {
		if x -= r.weight; x < 0 {
			return r.region
		}
	}
	return seedRegions[0].region
}

// setRegion is where a player played one set from: usually home, sometimes
// travelling.
func (p *player) setRegion() string {
	if regionRng.Float64() < 0.08 {
		return randomRegion()
	}
	return p.region
}

// formats is how many rosters (1v1 and 2v2) the player queues for.
func (p *player) formats() int {
	n := 0
	for _, on := range p.plays {
		if on {
			n++
		}
	}
	return max(n, 1)
}

func main() {
	var (
		dbPath     = flag.String("db", "seed.db", "database to fill (created if missing)")
		configPath = flag.String("ranked", "ranked.example.json", "ranked config naming the ladder and rosters")
		count      = flag.Int("players", 600, "players to create")
		days       = flag.Int("days", 150, "days of history, ending now")
		seed       = flag.Int64("seed", 1, "random seed")
		replayDir  = flag.String("replay-dir", "", "store placeholder replays here (the lobby's RMGK_REPLAY_DIR); empty = none")
		replayRate = flag.Float64("replays", 0.6, "share of sets that get a replay, with --replay-dir")
	)
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := ranked.Parse(data)
	if err != nil || len(cfg.Ladders) == 0 {
		log.Fatalf("ranked config: %v", err)
	}
	ladder := cfg.Ladders[0]
	var teamLadder *ranked.Ladder
	for i := range cfg.Ladders {
		if cfg.Ladders[i].Teams() {
			if teamLadder == nil {
				teamLadder = &cfg.Ladders[i]
			}
		} else if ladder.Teams() {
			ladder = cfg.Ladders[i]
		}
	}
	castFor := func(i int, id string) []int {
		if i == 0 || strings.Contains(id, "vanilla") {
			return vanillaCast
		}
		return remixCast
	}
	var rosters, teamRosters []roster
	for i, id := range ladder.Rulesets {
		rosters = append(rosters, roster{id, castFor(i, id)})
	}
	if teamLadder != nil {
		for i, id := range teamLadder.Rulesets {
			teamRosters = append(teamRosters, roster{id, castFor(i, id)})
		}
	}

	store, err := accounts.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	if *replayDir != "" {
		if err := os.MkdirAll(*replayDir, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	defer store.Close()
	rng := rand.New(rand.NewSource(*seed))
	regionRng = rand.New(rand.NewSource(*seed + 1))
	end := time.Now()
	start := end.Add(-time.Duration(*days) * 24 * time.Hour)

	// players
	players := make([]*player, 0, *count)
	names := map[string]bool{}
	for len(players) < *count {
		name := gamerTag(rng)
		if names[strings.ToLower(name)] {
			continue
		}
		names[strings.ToLower(name)] = true
		p := &player{name: name, skill: map[string]float64{}, plays: map[string]bool{}, mains: map[string][]int{}}
		base := rng.NormFloat64() * 230
		for _, r := range rosters {
			// skills on the two rosters are related but not the same
			p.skill[r.id] = 1500 + base*0.8 + rng.NormFloat64()*130
			cast := r.cast
			p.mains[r.id] = []int{cast[rng.Intn(len(cast))], cast[rng.Intn(len(cast))], cast[rng.Intn(len(cast))]}
		}
		switch x := rng.Float64(); {
		case x < 0.25:
			p.plays[rosters[0].id] = true // vanilla only
		case x < 0.40 && len(rosters) > 1:
			p.plays[rosters[1].id] = true // remix only
		default:
			for _, r := range rosters {
				p.plays[r.id] = true
			}
		}
		// team rosters: the matching 1v1 roster's skill and mains, plus teamwork
		teams := rng.Float64() < 0.5
		for i, tr := range teamRosters {
			solo := rosters[min(i, len(rosters)-1)]
			p.skill[tr.id] = p.skill[solo.id] + rng.NormFloat64()*90
			p.mains[tr.id] = p.mains[solo.id]
			p.plays[tr.id] = teams && p.plays[solo.id]
		}
		// heavy-tailed activity: most play a little, a few play a lot
		p.activity = math.Min(6, 0.05+rng.ExpFloat64()*0.45)
		p.joined = rng.Intn(*days * 9 / 10)
		p.quits = *days + 1
		if rng.Float64() < 0.2 {
			p.quits = p.joined + 7 + rng.Intn(*days)
		}
		p.region = randomRegion()
		players = append(players, p)
	}
	sort.Slice(players, func(i, j int) bool { return players[i].joined < players[j].joined })

	log.Printf("creating %d accounts (argon2id, takes a little while)...", len(players))
	for _, p := range players {
		at := start.Add(time.Duration(p.joined)*24*time.Hour - time.Duration(rng.Intn(3600))*time.Second)
		store.SetClock(func() time.Time { return at })
		account, err := store.Register(p.name, "password123")
		if err != nil {
			log.Fatalf("register %s: %v", p.name, err)
		}
		p.id = account.ID
	}

	// days of play
	match := uint64(0)
	replays, teamSets := 0, 0
	counts := map[accounts.SetStatus]int{}
	for day := 0; day < *days; day++ {
		for _, r := range rosters {
			var queue []*player
			for _, p := range players {
				if day < p.joined || day >= p.quits || !p.plays[r.id] {
					continue
				}
				for n := poisson(rng, p.activity/float64(p.formats())); n > 0; n-- {
					queue = append(queue, p)
				}
			}
			rng.Shuffle(len(queue), func(i, j int) { queue[i], queue[j] = queue[j], queue[i] })
			// pair each player with someone close in visible rating (skill filters)
			visible := func(p *player) float64 {
				lr, _ := store.Rating(p.id, ladder.ID, r.id)
				return lr.Rating.Rating
			}
			// the day's sets in time order, as a live lobby records them
			gap := 2 * 86400 / (len(queue)/2 + 1)
			offset := 0
			for len(queue) >= 2 {
				a := queue[0]
				best, bestGap := -1, math.MaxFloat64
				va := visible(a)
				for i := 1; i < len(queue) && i < 40; i++ {
					if queue[i] == a {
						continue
					}
					if gap := math.Abs(visible(queue[i]) - va); gap < bestGap {
						best, bestGap = i, gap
					}
				}
				if best < 0 {
					queue = queue[1:]
					continue
				}
				b := queue[best]
				queue = append(queue[1:best], queue[best+1:]...)

				offset = min(offset+1+rng.Intn(gap), 86399)
				at := start.Add(time.Duration(day)*24*time.Hour + time.Duration(offset)*time.Second)
				if at.After(end) {
					at = end.Add(-time.Minute)
				}
				store.SetClock(func() time.Time { return at })
				match++
				rec := playSet(rng, ladder, r, a, b, match)
				setID, _, err := store.RecordSetWithID(rec)
				if err != nil {
					log.Fatalf("set %d: %v", match, err)
				}
				counts[rec.Status]++
				if *replayDir != "" && rec.Status != accounts.SetVoided && rng.Float64() < *replayRate {
					games := rec.Players[0].GamesWon + rec.Players[1].GamesWon
					if err := storeReplay(store, *replayDir, setID, max(games, 1), a, b, at); err != nil {
						log.Fatalf("replay for set %d: %v", setID, err)
					}
					replays++
				}
			}
		}
		for _, r := range teamRosters {
			var queue []*player
			for _, p := range players {
				if day < p.joined || day >= p.quits || !p.plays[r.id] {
					continue
				}
				for n := poisson(rng, p.activity/float64(p.formats())); n > 0; n-- {
					queue = append(queue, p)
				}
			}
			visible := func(p *player) float64 {
				lr, _ := store.Rating(p.id, teamLadder.ID, r.id)
				return lr.Rating.Rating + rng.NormFloat64()*60
			}
			// similar ratings play together: sort with a little noise, take groups of four
			keys := make(map[*player]float64)
			for _, p := range queue {
				if _, ok := keys[p]; !ok {
					keys[p] = visible(p)
				}
			}
			sort.SliceStable(queue, func(i, j int) bool { return keys[queue[i]] > keys[queue[j]] })
			gap := 2 * 86400 / (len(queue)/4 + 1)
			offset := 0
			for len(queue) >= 4 {
				group := []*player{}
				rest := queue[:0:0]
				for _, p := range queue {
					dup := false
					for _, g := range group {
						dup = dup || g == p
					}
					if len(group) < 4 && !dup {
						group = append(group, p)
					} else {
						rest = append(rest, p)
					}
				}
				queue = rest
				if len(group) < 4 {
					break
				}
				offset = min(offset+1+rng.Intn(gap), 86399)
				at := start.Add(time.Duration(day)*24*time.Hour + time.Duration(offset)*time.Second)
				if at.After(end) {
					at = end.Add(-time.Minute)
				}
				store.SetClock(func() time.Time { return at })
				match++
				// balanced: best and worst against the middle two
				rec := playTeamSet(rng, *teamLadder, r, [2][2]*player{{group[0], group[3]}, {group[1], group[2]}}, match)
				setID, _, err := store.RecordSetWithID(rec)
				if err != nil {
					log.Fatalf("team set %d: %v", match, err)
				}
				counts[rec.Status]++
				teamSets++
				if *replayDir != "" && rec.Status != accounts.SetVoided && rng.Float64() < *replayRate {
					games := rec.Players[0].GamesWon
					for _, pl := range rec.Players {
						if pl.Team != rec.Players[0].Team {
							games += pl.GamesWon
							break
						}
					}
					if err := storeReplay(store, *replayDir, setID, max(games, 1), group[0], group[1], at); err != nil {
						log.Fatalf("replay for set %d: %v", setID, err)
					}
					replays++
				}
			}
		}
		if day%30 == 29 {
			log.Printf("day %d: %d sets so far", day+1, match)
		}
	}
	store.SetClock(time.Now)
	log.Printf("done: %d players, %d sets (%d of them 2v2; %d rated, %d voided, %d disputed, %d with a replay) in %s",
		len(players), match, teamSets, counts[accounts.SetRated], counts[accounts.SetVoided], counts[accounts.SetDisputed],
		replays, *dbPath)
}

// playSet plays one set between a and b on roster r from their hidden skill.
func playSet(rng *rand.Rand, ladder ranked.Ladder, r roster, a, b *player, match uint64) accounts.SetRecord {
	bestOf := int(ladder.BestOf)
	need := bestOf/2 + 1
	// per-game chance from the skill gap, a set is first to `need`
	pGame := 1 / (1 + math.Pow(10, (b.skill[r.id]-a.skill[r.id])/400))
	score := []int{0, 0}
	var games []protocol.MatchResultGame
	charA := a.mains[r.id][weightedMain(rng)]
	charB := b.mains[r.id][weightedMain(rng)]
	for score[0] < need && score[1] < need {
		winner := 2
		if rng.Float64() < pGame {
			winner = 1
		}
		score[winner-1]++
		games = append(games, protocol.MatchResultGame{Game: len(games) + 1, WinnerSlot: winner, Stage: 6,
			Players: []protocol.MatchResultPlayer{
				{Slot: 1, Character: charA, StocksLeft: stocks(rng, winner == 1)},
				{Slot: 2, Character: charB, StocksLeft: stocks(rng, winner == 2)},
			}})
		// the loser sometimes counterpicks
		if rng.Float64() < 0.2 {
			if winner == 1 {
				charB = b.mains[r.id][rng.Intn(3)]
			} else {
				charA = a.mains[r.id][rng.Intn(3)]
			}
		}
	}
	winnerSlot := 1
	if score[1] > score[0] {
		winnerSlot = 2
	}
	report := protocol.MatchResultRequest{MatchID: match, WinnerSlot: winnerSlot, Score: score, Games: games,
		RulesetHash: "seed", Integrity: "ok", RulesetID: r.id, ModuleVersion: "0.6.0"}
	reports := map[string]protocol.MatchResultRequest{a.name: report, b.name: report}

	rec := accounts.SetRecord{Ladder: ladder.ID, MatchID: match, BootID: "seed", Status: accounts.SetRated,
		RulesetID: r.id, ModuleVersion: "0.6.0",
		Players: []accounts.SetPlayer{{AccountID: a.id, Slot: 1, GamesWon: score[0], Region: a.setRegion()},
			{AccountID: b.id, Slot: 2, GamesWon: score[1], Region: b.setRegion()}}}
	if len(ladder.Roms) > 0 {
		rec.RomMD5 = ladder.Roms[0].MD5
	}
	switch x := rng.Float64(); {
	case x < 0.02: // someone left mid-set
		rec.Status, rec.Reason = accounts.SetVoided, "player_left"
		rec.Players[0].GamesWon, rec.Players[1].GamesWon = 0, 0
		reports = map[string]protocol.MatchResultRequest{}
	case x < 0.025: // the clients disagreed
		other := report
		other.Integrity = "desync detected: frame 4410"
		reports[b.name] = other
		rec.Status, rec.Reason = accounts.SetDisputed, "integrity: desync detected: frame 4410"
	default:
		rec.WinnerID = []int64{a.id, b.id}[winnerSlot-1]
	}
	if data, err := json.Marshal(reports); err == nil {
		rec.ResultJSON = string(data)
	}
	return rec
}

// storeReplay saves a placeholder replay for a set: a valid KRC1 header and
// one second of empty input records, so it downloads and parses like a
// real recording.
func storeReplay(store *accounts.Store, dir string, setID int64, games int, a, b *player, at time.Time) error {
	var buf bytes.Buffer
	field := func(text string, size int) {
		f := make([]byte, size)
		copy(f, text)
		buf.Write(f)
	}
	field("KRC1", 4)
	field("RMG-K ranked-seed", 128)
	field("SmashRemix.z64", 128)
	binary.Write(&buf, binary.LittleEndian, uint32(at.Unix()))
	binary.Write(&buf, binary.LittleEndian, int32(1)) // local player
	binary.Write(&buf, binary.LittleEndian, int32(2)) // players
	field(a.name, 32)
	field(b.name, 32)
	field("", 64)
	for i := 0; i < 60; i++ {
		buf.Write([]byte{0x12, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	}
	data := buf.Bytes()
	if err := os.WriteFile(accounts.ReplayFile(dir, setID), data, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	// a bookmark per game, as a real set's (games of about four minutes)
	var marks []protocol.ReplayBookmark
	for g := 1; g <= games; g++ {
		marks = append(marks, protocol.ReplayBookmark{Record: 600 + (g-1)*14400, Label: fmt.Sprintf("Game %d", g),
			Kind: "game_start", Game: g})
	}
	bookmarks, _ := json.Marshal(marks)
	return store.AddReplay(accounts.ReplayInfo{SetID: setID, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		Bookmarks: string(bookmarks), UploaderID: a.id})
}

// playTeamSet plays a 2v2 set on random ports from the players' hidden team
// skill: a game goes to a team with the Elo chance of its average skill.
func playTeamSet(rng *rand.Rand, ladder ranked.Ladder, r roster, sides [2][2]*player, match uint64) accounts.SetRecord {
	bestOf := int(ladder.BestOf)
	need := bestOf/2 + 1
	avg := func(t [2]*player) float64 { return (t[0].skill[r.id] + t[1].skill[r.id]) / 2 }
	pGame := 1 / (1 + math.Pow(10, (avg(sides[1])-avg(sides[0]))/400))

	// any ports: shuffle the four players over slots 1-4
	order := []struct {
		p    *player
		side int
	}{{sides[0][0], 0}, {sides[0][1], 0}, {sides[1][0], 1}, {sides[1][1], 1}}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	// team 1 is the team holding slot 1
	teamOf := func(side int) int {
		if side == order[0].side {
			return 1
		}
		return 2
	}

	score := [2]int{}
	chars := make([]int, 4)
	for slot, o := range order {
		chars[slot] = o.p.mains[r.id][weightedMain(rng)]
	}
	var games []protocol.MatchResultGame
	for score[0] < need && score[1] < need {
		side := 1
		if rng.Float64() < pGame {
			side = 0
		}
		score[side]++
		g := protocol.MatchResultGame{Game: len(games) + 1, WinnerTeam: teamOf(side), Stage: 6}
		for slot, o := range order {
			g.Players = append(g.Players, protocol.MatchResultPlayer{Slot: slot + 1, Character: chars[slot],
				StocksLeft: stocks(rng, o.side == side)})
		}
		games = append(games, g)
	}
	winnerSide := 0
	if score[1] > score[0] {
		winnerSide = 1
	}
	report := protocol.MatchResultRequest{MatchID: match, Score: make([]int, 4), Teams: make([]int, 4),
		WinnerTeam: teamOf(winnerSide), Games: games, RulesetHash: "seed", Integrity: "ok", RulesetID: r.id,
		ModuleVersion: "0.7.0"}
	rec := accounts.SetRecord{Ladder: ladder.ID, MatchID: match, BootID: "seed", Status: accounts.SetRated,
		RulesetID: r.id, ModuleVersion: "0.7.0", WinnerTeam: report.WinnerTeam}
	for _, g := range games {
		rec.GameWinners = append(rec.GameWinners, g.WinnerTeam)
	}
	if len(ladder.Roms) > 0 {
		rec.RomMD5 = ladder.Roms[0].MD5
	}
	reports := map[string]protocol.MatchResultRequest{}
	for slot, o := range order {
		report.Score[slot] = score[o.side]
		report.Teams[slot] = teamOf(o.side)
		rec.Players = append(rec.Players, accounts.SetPlayer{AccountID: o.p.id, Slot: slot + 1,
			GamesWon: score[o.side], Team: teamOf(o.side), Region: o.p.setRegion()})
	}
	for _, o := range order {
		reports[o.p.name] = report
	}
	switch x := rng.Float64(); {
	case x < 0.03: // someone left mid-set
		rec.Status, rec.Reason, rec.WinnerTeam, rec.GameWinners = accounts.SetVoided, "player_left", 0, nil
		reports = map[string]protocol.MatchResultRequest{}
	case x < 0.036: // the reports disagreed
		other := report
		other.Integrity = "desync detected: frame 9120"
		reports[order[3].p.name] = other
		rec.Status, rec.Reason, rec.WinnerTeam, rec.GameWinners = accounts.SetDisputed, "integrity: desync detected: frame 9120", 0, nil
	}
	if data, err := json.Marshal(reports); err == nil {
		rec.ResultJSON = string(data)
	}
	return rec
}

func weightedMain(rng *rand.Rand) int {
	switch x := rng.Float64(); {
	case x < 0.7:
		return 0
	case x < 0.9:
		return 1
	default:
		return 2
	}
}

func stocks(rng *rand.Rand, won bool) int {
	if !won {
		return 0
	}
	return 1 + rng.Intn(4)
}

func poisson(rng *rand.Rand, mean float64) int {
	l, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

var (
	tagStart = []string{"Shadow", "Pixel", "Turbo", "Lucky", "Crimson", "Frosty", "Neon", "Silent", "Mega", "Hyper",
		"Rusty", "Cosmic", "Zero", "Retro", "Wavy", "Salty", "Spicy", "Quiet", "Mystic", "Swift", "Iron", "Lunar",
		"Solar", "Static", "Fuzzy", "Golden", "Dizzy", "Clutch", "Combo", "Ledge", "Shine", "Smash", "Jab", "Spike"}
	tagEnd = []string{"Fox", "Falcon", "Puff", "Ness", "Link", "Kirby", "Yoshi", "Mario", "Luigi", "Pika", "Samus",
		"DK", "Wolf", "Marth", "Sonic", "Goemon", "Wario", "Lucas", "Sheik", "Dash", "Edge", "Knee", "Upair", "Bair",
		"Tech", "Grab", "Hitbox", "Frame", "Stock", "Combo", "Main", "Slayer", "Wizard", "Kid", "Pro", "Gamer"}
)

// gamerTag makes a plausible player name (letters, digits, underscores).
func gamerTag(rng *rand.Rand) string {
	name := tagStart[rng.Intn(len(tagStart))] + tagEnd[rng.Intn(len(tagEnd))]
	switch x := rng.Float64(); {
	case x < 0.35:
		name += fmt.Sprint(rng.Intn(100))
	case x < 0.45:
		name = strings.ToLower(name)
	case x < 0.55:
		name += "_" + fmt.Sprint(64+rng.Intn(36))
	}
	if len(name) > 20 {
		name = name[:20]
	}
	return name
}
