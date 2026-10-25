package ws

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// A player leaving a ranked set before it's over (closing the game, leaving
// the room, losing the connection) decides it from how far it got:
//
//   - before game 1 is counted: void (left_in_game_1)
//   - the leaver's side is behind in games: it forfeits the set (rated,
//     reason forfeit)
//   - it's ahead: void (leader_left)
//   - level, e.g. 1-1 in the third game of a best of 3: it forfeits unless
//     it led the game under way, on stocks and then on percent; leading
//     voids it (leader_left). Between games nobody leads.
//   - a set the games already decided is rated as played
//
// In 2v2 the leaver's whole team forfeits.
//
// The players who stay report the set as of the frame the leaver left
// (MATCH_ABANDONED: their rollback engine drops the leaver there, and nothing
// played after it counts). Every player also reports the games counted so
// far after each game (MATCH_PROGRESS), so a game only counts as far as the
// leaver's own client confirmed it: a stayer alone can't invent wins.

// reasonForfeit is a rated set's reason when the side that left forfeited.
const reasonForfeit = "forfeit"

// How long the players who stay have to report the set after a player left.
// A variable so tests can shorten it.
var rankedLeftReportTimeout = 15 * time.Second

// leftSet is a set's first leaver and who else left after them.
type leftSet struct {
	userID   uint64
	slot     int
	username string
	gone     map[uint64]bool // players who left after them; they can't report
}

// playerLeft records a player leaving (the first one is the leaver) and
// reports whether every player who stayed has already reported the set.
// The caller holds rankedSets.mu.
func (p *pendingSet) playerLeft(userID uint64) bool {
	if p.left == nil {
		pp, _ := p.participant(userID)
		p.left = &leftSet{userID: userID, slot: pp.slot, username: pp.username, gone: map[uint64]bool{}}
		p.quitter = userID
	} else if userID != p.left.userID {
		p.left.gone[userID] = true
	}
	return p.leftReportsComplete()
}

// leftReportsComplete reports whether every player still in the set has
// reported it: as of the leave (MATCH_ABANDONED naming the leaver) or with
// a finished set's result. The caller holds rankedSets.mu.
func (p *pendingSet) leftReportsComplete() bool {
	if p.left == nil {
		return false
	}
	for _, pp := range p.participants {
		if pp.userID == p.left.userID || p.left.gone[pp.userID] {
			continue
		}
		if _, ok := p.reports[pp.userID]; ok {
			continue
		}
		if report, ok := p.abandoned[pp.userID]; ok && report.LeftSlot == p.left.slot {
			continue
		}
		return false
	}
	return true
}

// rankedSetPendingFor reports whether a set the user plays in is still
// waiting for its result.
func (h *Hub) rankedSetPendingFor(userID uint64) bool {
	h.rankedSets.mu.Lock()
	defer h.rankedSets.mu.Unlock()
	for _, set := range h.rankedSets.sets {
		if _, ok := set.participant(userID); ok {
			return true
		}
	}
	return false
}

// handleMatchProgress stores a player's games counted so far.
func (h *Hub) handleMatchProgress(c *Client, env protocol.Envelope) {
	var req protocol.MatchProgressRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	h.rankedSets.mu.Lock()
	defer h.rankedSets.mu.Unlock()
	set := h.rankedSets.sets[req.MatchID]
	if set == nil || set.left != nil {
		return // after a leave only the reports of the set as of then count
	}
	if _, ok := set.participant(c.user.ID); !ok {
		return
	}
	if set.progress == nil {
		set.progress = make(map[uint64][]protocol.MatchResultGame)
	}
	if len(req.Games) >= len(set.progress[c.user.ID]) {
		set.progress[c.user.ID] = req.Games
	}
}

// handleMatchAbandoned stores a stayer's report of the set as of a player
// leaving it, and judges the set once every stayer has reported. A report
// that arrives before the lobby saw the leave waits for it: the leave itself
// must come from the lobby, not from a report.
func (h *Hub) handleMatchAbandoned(c *Client, env protocol.Envelope) {
	var req protocol.MatchAbandonedRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	h.rankedSets.mu.Lock()
	set := h.rankedSets.sets[req.MatchID]
	if set == nil {
		h.rankedSets.mu.Unlock()
		return
	}
	if _, ok := set.participant(c.user.ID); !ok || (set.left != nil && set.left.userID == c.user.ID) {
		h.rankedSets.mu.Unlock()
		return
	}
	if _, dup := set.abandoned[c.user.ID]; dup {
		h.rankedSets.mu.Unlock()
		return
	}
	if set.abandoned == nil {
		set.abandoned = make(map[uint64]protocol.MatchAbandonedRequest)
	}
	set.abandoned[c.user.ID] = req
	ready := set.leftReportsComplete()
	if ready {
		delete(h.rankedSets.sets, set.matchID)
		if set.timer != nil {
			set.timer.Stop()
		}
	}
	h.rankedSets.mu.Unlock()

	slog.Info("ranked set reported as of a player leaving", "match", req.MatchID, "user", c.user.Username,
		"leftSlot", req.LeftSlot, "games", len(req.Games), "live", req.Live != nil, "complete", ready)
	if ready {
		h.finishLeftSet(set)
	}
}

// finishLeftSet judges and records a set a player left.
func (h *Hub) finishLeftSet(set *pendingSet) {
	status, reason := judgeLeftSet(set)
	h.finalizeRankedSet(set, status, reason)
}

// stayerReport is one stayer's account of a set a player left.
type stayerReport struct {
	games []protocol.MatchResultGame
	live  *protocol.MatchLiveGame // nil: unknown, or the set was over
	teams []int
	// a finished set's result: the games decided it
	finished bool

	integrity, rulesetHash, rulesetID, moduleVersion, romMD5 string
	voidedGames                                              int
}

func (p *pendingSet) stayerReports() []stayerReport {
	var out []stayerReport
	for _, pp := range p.participants {
		if p.left == nil || pp.userID == p.left.userID {
			continue
		}
		if r, ok := p.reports[pp.userID]; ok {
			out = append(out, stayerReport{games: r.Games, teams: r.Teams, finished: true, integrity: r.Integrity,
				rulesetHash: r.RulesetHash, rulesetID: r.RulesetID, moduleVersion: r.ModuleVersion,
				romMD5: r.RomMD5, voidedGames: r.VoidedGames})
			continue
		}
		if a, ok := p.abandoned[pp.userID]; ok && a.LeftSlot == p.left.slot {
			report := stayerReport{games: a.Games, live: a.Live, integrity: a.Integrity, rulesetHash: a.RulesetHash,
				rulesetID: a.RulesetID, moduleVersion: a.ModuleVersion, romMD5: a.RomMD5, voidedGames: a.VoidedGames}
			if a.Live != nil {
				report.teams = a.Live.Teams
			}
			out = append(out, report)
		}
	}
	return out
}

func sameGames(a, b []protocol.MatchResultGame) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].WinnerSlot != b[i].WinnerSlot || a[i].WinnerTeam != b[i].WinnerTeam {
			return false
		}
	}
	return true
}

// judgeLeftSet decides a set a player left (see the top of this file) and
// stores the result it rates in set.verdict.
func judgeLeftSet(set *pendingSet) (accounts.SetStatus, string) {
	reports := set.stayerReports()
	if set.left == nil || len(reports) == 0 {
		return accounts.SetVoided, "player_left"
	}
	reference := reports[0]
	for _, r := range reports {
		if r.integrity != "ok" {
			return accounts.SetDisputed, "integrity: " + r.integrity
		}
		if !sameGames(r.games, reference.games) || r.rulesetHash != reference.rulesetHash {
			return accounts.SetDisputed, "reports_disagree"
		}
	}

	// games count as far as the leaver's own client confirmed them
	confirmed := set.progress[set.left.userID]
	counted := reference.games
	if len(confirmed) < len(counted) {
		counted = counted[:len(confirmed)]
	}
	if !sameGames(confirmed[:len(counted)], counted) {
		return accounts.SetDisputed, "reports_disagree"
	}
	unconfirmed := len(counted) < len(reference.games)

	// the sides: slots in 1v1, teams in 2v2
	teamSet := set.settings.TeamSize > 1
	var teams [4]int
	if teamSet {
		teams = normalizedScore(reference.teams)
		if teams == ([4]int{}) {
			teams = set.requiredTeams
		}
		if len(counted) > 0 {
			if teams == ([4]int{}) {
				return accounts.SetVoided, "player_left" // the report doesn't say who's on which team
			}
			if reason := checkTeams(set, teams); reason != "" {
				return accounts.SetDisputed, reason
			}
		}
	}
	sideOf := func(slot int) int {
		if slot < 1 || slot > 4 {
			return 0
		}
		if teamSet {
			return teams[slot-1]
		}
		if _, ok := participantAt(set, slot); ok {
			return slot
		}
		return 0
	}
	leaverSide := sideOf(set.left.slot)
	otherSide := 0
	for _, p := range set.participants {
		if s := sideOf(p.slot); s != 0 && s != leaverSide {
			otherSide = s
		}
	}

	if len(counted) == 0 {
		return accounts.SetVoided, "left_in_game_1"
	}
	if leaverSide == 0 || otherSide == 0 {
		return accounts.SetDisputed, "invalid_result"
	}
	wins := map[int]int{}
	for _, g := range counted {
		winner := g.WinnerSlot
		if teamSet {
			winner = g.WinnerTeam
		}
		side := winner
		if !teamSet {
			side = sideOf(winner)
		}
		if side == 0 || (teamSet && side != 1 && side != 2) {
			return accounts.SetDisputed, "invalid_result"
		}
		wins[side]++
	}

	needed := set.settings.BestOf/2 + 1
	switch {
	case wins[leaverSide] > needed || wins[otherSide] > needed || (wins[leaverSide] == needed && wins[otherSide] == needed):
		return accounts.SetDisputed, "invalid_result"
	case wins[leaverSide] == needed:
		set.verdict = leftVerdict(set, reference, counted, teams, leaverSide)
		return accounts.SetRated, ""
	case wins[otherSide] == needed:
		set.verdict = leftVerdict(set, reference, counted, teams, otherSide)
		return accounts.SetRated, ""
	case wins[leaverSide] < wins[otherSide]:
		set.verdict = leftVerdict(set, reference, counted, teams, otherSide)
		return accounts.SetRated, reasonForfeit
	case wins[leaverSide] > wins[otherSide]:
		return accounts.SetVoided, "leader_left"
	}

	// level: the game under way decides, as every stayer saw it
	if unconfirmed {
		return accounts.SetVoided, "player_left"
	}
	leading := false
	for i, r := range reports {
		if r.live == nil {
			return accounts.SetVoided, "player_left"
		}
		lead := ledLiveGame(r.live, sideOf, leaverSide, otherSide)
		if i > 0 && lead != leading {
			return accounts.SetVoided, "player_left"
		}
		leading = lead
	}
	if leading {
		return accounts.SetVoided, "leader_left"
	}
	set.verdict = leftVerdict(set, reference, counted, teams, otherSide)
	return accounts.SetRated, reasonForfeit
}

func participantAt(set *pendingSet, slot int) (rankedParticipant, bool) {
	for _, p := range set.participants {
		if p.slot == slot {
			return p, true
		}
	}
	return rankedParticipant{}, false
}

// checkTeams checks a team set's teams like judgeTeamSet: every player on
// team 1 or 2, TeamSize each, team 1 holding the lowest slot, and the
// teammates the lobby matched together.
func checkTeams(set *pendingSet, teams [4]int) string {
	count := [3]int{}
	lowest := 5
	for _, p := range set.participants {
		team := teams[p.slot-1]
		if team != 1 && team != 2 {
			return "invalid_teams"
		}
		count[team]++
		lowest = min(lowest, p.slot)
	}
	if count[1] != set.settings.TeamSize || count[2] != set.settings.TeamSize || teams[lowest-1] != 1 {
		return "invalid_teams"
	}
	if set.requiredTeams != ([4]int{}) && teams != set.requiredTeams {
		return "teams_not_as_matched"
	}
	return ""
}

// ledLiveGame reports whether side led the game under way: more stocks left,
// or as many and less damage (on the players still in). Between games
// nobody leads.
func ledLiveGame(live *protocol.MatchLiveGame, sideOf func(int) int, side, other int) bool {
	if !live.InGame {
		return false
	}
	stocks := map[int]int{}
	percent := map[int]int{}
	for _, p := range live.Players {
		s := sideOf(p.Slot)
		if s == 0 || p.Stocks <= 0 {
			continue
		}
		stocks[s] += p.Stocks
		percent[s] += max(0, p.Percent)
	}
	if stocks[side] != stocks[other] {
		return stocks[side] > stocks[other]
	}
	return percent[side] < percent[other]
}

// leftVerdict is the result a left set is rated with: the counted games,
// won by winnerSide (a slot in 1v1, a team in 2v2).
func leftVerdict(set *pendingSet, reference stayerReport, counted []protocol.MatchResultGame, teams [4]int,
	winnerSide int) *protocol.MatchResultRequest {
	verdict := &protocol.MatchResultRequest{
		MatchID: set.matchID, Games: counted, VoidedGames: reference.voidedGames, Integrity: "ok",
		RulesetHash: reference.rulesetHash, RulesetID: reference.rulesetID,
		ModuleVersion: reference.moduleVersion, RomMD5: reference.romMD5,
		Score: scoreOf(counted, set.settings.TeamSize > 1, teams),
	}
	if set.settings.TeamSize > 1 {
		verdict.Teams = teams[:]
		verdict.WinnerTeam = winnerSide
	} else {
		verdict.WinnerSlot = winnerSide
	}
	return verdict
}

// scoreOf counts each slot's games won (a team's, in a team set).
func scoreOf(games []protocol.MatchResultGame, teamSet bool, teams [4]int) []int {
	score := make([]int, 4)
	for _, g := range games {
		for slot := 1; slot <= 4; slot++ {
			if (teamSet && g.WinnerTeam != 0 && teams[slot-1] == g.WinnerTeam) || (!teamSet && g.WinnerSlot == slot) {
				score[slot-1]++
			}
		}
	}
	return score
}

// recordReports is each player's report by username for the set record
// (moderators see who claimed what): results, a stayer's set as of the
// leave, and the games the leaver's client confirmed.
func (p *pendingSet) recordReports() map[string]protocol.MatchResultRequest {
	out := make(map[string]protocol.MatchResultRequest, len(p.participants))
	for _, pp := range p.participants {
		if report, ok := p.reports[pp.userID]; ok {
			out[pp.username] = report
			continue
		}
		if p.left == nil || pp.userID == p.left.userID {
			continue
		}
		if a, ok := p.abandoned[pp.userID]; ok {
			var teams []int
			if a.Live != nil {
				teams = a.Live.Teams
			}
			out[pp.username] = protocol.MatchResultRequest{
				MatchID: a.MatchID, Games: a.Games, VoidedGames: a.VoidedGames, Integrity: a.Integrity,
				RulesetHash: a.RulesetHash, RulesetID: a.RulesetID, ModuleVersion: a.ModuleVersion, RomMD5: a.RomMD5,
				Score: scoreOf(a.Games, p.settings.TeamSize > 1, normalizedScore(teams)), Teams: teams,
			}
		}
	}
	if p.left != nil {
		if games := p.progress[p.left.userID]; len(games) > 0 {
			out[p.left.username+" (left)"] = protocol.MatchResultRequest{MatchID: p.matchID, Games: games}
		}
	}
	return out
}

// forfeitedGames is a team set's game winners with the games the leaving
// team didn't play, lost: every game of a team set is rated, so a forfeit
// counts as losing the rest of the set.
func forfeitedGames(gameWinners []int, winnerTeam, needed int) []int {
	won := 0
	for _, w := range gameWinners {
		if w == winnerTeam {
			won++
		}
	}
	for ; won < needed; won++ {
		gameWinners = append(gameWinners, winnerTeam)
	}
	return gameWinners
}
