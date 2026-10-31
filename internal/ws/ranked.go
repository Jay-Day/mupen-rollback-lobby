package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/geo"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/rating"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// After the first MATCH_RESULT of a set, how long the other players have to
// send theirs before the set is voided. A variable so tests can shorten it.
var rankedResultTimeout = 60 * time.Second

func (h *Hub) rankedEnabled() bool {
	return h.accounts != nil && h.rankedConfig != nil && h.rankedConfig.Enabled()
}

func standingFrom(lr accounts.LadderRating, tiers rating.Tiers, policy rating.Policy) protocol.LadderStanding {
	place := tiers.PlaceWith(lr.Rating, policy)
	return protocol.LadderStanding{
		Ladder:      lr.Ladder,
		Ruleset:     lr.Ruleset,
		Rating:      int(math.Round(lr.Rating.Rating)),
		Deviation:   int(math.Round(lr.Rating.Deviation)),
		Tier:        place.Tier,
		TierIndex:   place.TierIndex,
		Provisional: place.Provisional,
		SetsPlayed:  lr.SetsPlayed,
	}
}

// standing returns an account's standing on one roster of a ladder, nil for
// guests or on error.
func (h *Hub) standing(accountID int64, ladderID, rulesetID string) *protocol.LadderStanding {
	if accountID == 0 || h.accounts == nil || h.rankedConfig == nil {
		return nil
	}
	lr, err := h.accounts.Rating(accountID, ladderID, rulesetID)
	if err != nil {
		slog.Warn("rating lookup failed", "account", accountID, "ladder", ladderID, "ruleset", rulesetID, "err", err)
		return nil
	}
	s := standingFrom(lr, h.rankedConfig.Tiers, h.rankedConfig.Policy(ladderID))
	return &s
}

// resolveRankedSettings validates a ranked request against the room's ROM.
func (h *Hub) resolveRankedSettings(romMD5 string, req *protocol.RankedRoomRequest) (*state.RankedSettings, string, string) {
	if !h.rankedEnabled() {
		return nil, protocol.ReasonRankedUnavailable, "Ranked play isn't available on this server."
	}
	ladder, ok := h.rankedConfig.RomLadder(romMD5, req.LadderID)
	if !ok {
		if req.LadderID != "" {
			return nil, protocol.ReasonRankedUnavailable, "This ROM isn't on that ranked ladder."
		}
		return nil, protocol.ReasonRankedUnavailable, "This ROM isn't on a ranked ladder."
	}
	ruleset, ok := ladder.Ruleset(req.RulesetID)
	if !ok {
		return nil, protocol.ReasonInvalidPayload, "Unknown ranked ruleset."
	}
	maxPing := req.MaxPing
	if maxPing < 0 {
		maxPing = 0
	} else if maxPing > maxRankedPing {
		maxPing = maxRankedPing
	}
	return &state.RankedSettings{
		LadderID:  ladder.ID,
		ModuleID:  ladder.ModuleID,
		RulesetID: ruleset,
		BestOf:    int(ladder.BestOf),
		TeamSize:  ladder.TeamSize,
		MaxPing:   maxPing,
	}, "", ""
}

// maxRankedPing caps the max ping setting (ms).
const maxRankedPing = 500

// rankedEligibility reports why u can't play in a ranked room hosted by host
// ("" when eligible). The host always can.
func (h *Hub) rankedEligibility(u *state.User, settings *state.RankedSettings, host *state.User) (string, string) {
	if !u.Verified() {
		return protocol.ReasonNotVerified, u.Username + " needs to log into an account to play ranked."
	}
	if host == nil || host.ID == u.ID {
		return "", ""
	}
	if estimate := geo.EstimatedRTT(u.Region, host.Region); settings.MaxPing > 0 && estimate > settings.MaxPing {
		return protocol.ReasonTooFar, fmt.Sprintf("%s is too far away for this room (about %d ms; it allows %d ms).",
			u.Username, estimate, settings.MaxPing)
	}
	return "", ""
}

// roomState is roomStatePayload plus what needs the account store: ladder
// names and each verified seat's standing.
func (h *Hub) roomState(room *state.Room) protocol.RoomStatePayload {
	payload := roomStatePayload(room)
	if h.rankedConfig == nil {
		return payload
	}
	ladder, hasLadder := h.rankedConfig.LadderForRom(room.RomMD5)
	if payload.Ranked != nil {
		ladder, hasLadder = h.rankedConfig.RomLadder(room.RomMD5, payload.Ranked.Ladder)
	}
	if payload.Ranked != nil && hasLadder {
		payload.Ranked.LadderName = ladder.Name
		payload.Ranked.RulesetName = ladder.RulesetName(payload.Ranked.RulesetID)
	}
	if !hasLadder {
		return payload
	}
	// standings on the room's roster (the ladder's default before it's ranked)
	ruleset, _ := ladder.Ruleset("")
	if payload.Ranked != nil {
		ruleset = payload.Ranked.RulesetID
	}
	for i := range payload.Players {
		seat := &payload.Players[i]
		if u := h.state.GetUser(seat.UserID); u != nil && u.Verified() {
			seat.Standing = h.standing(u.AccountID, ladder.ID, ruleset)
		}
	}
	return payload
}

// handleRoomUpdateRanked turns ranked on or off for a waiting room.
func (h *Hub) handleRoomUpdateRanked(c *Client, env protocol.Envelope) {
	var req protocol.RankedRoomRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	room := c.user.CurrentRoom
	if room == nil {
		return
	}
	if room.Host == nil || room.Host.ID != c.user.ID {
		slog.Debug("ROOM_UPDATE_RANKED rejected: not host", "user", c.user.Username, "room", room.ID)
		return
	}
	if room.State != state.RoomStateWaiting {
		c.notice("warn", "Ranked can only be changed between games.")
		return
	}

	if !req.Enabled {
		if room.GetRanked() == nil {
			return
		}
		room.SetRanked(nil)
	} else {
		settings, reason, message := h.resolveRankedSettings(room.RomMD5, &req)
		if reason != "" {
			c.notice("warn", message)
			return
		}
		if len(room.Players) > settings.Players() {
			c.notice("warn", fmt.Sprintf("This ranked format seats %d players; too many are in the room.", settings.Players()))
			return
		}
		for _, seat := range room.Players {
			if seat.User == nil {
				continue
			}
			if reason, message := h.rankedEligibility(seat.User, settings, room.Host); reason != "" {
				c.notice("warn", message)
				return
			}
		}
		room.SetRanked(settings)
	}

	ranked := room.GetRanked()
	slog.Info("room ranked updated", "room", room.ID, "ranked", ranked != nil, "by", c.user.Username)
	h.broadcastRoomState(room)
	h.broadcastRoomList()
}

// rankedStartCheck validates a ranked room right before its match starts.
func (h *Hub) rankedStartCheck(room *state.Room, settings *state.RankedSettings) string {
	seated := 0
	var players []*state.User
	for _, seat := range room.Players {
		if seat.User == nil {
			continue
		}
		seated++
		players = append(players, seat.User)
		if reason, message := h.rankedEligibility(seat.User, settings, room.Host); reason != "" {
			return message
		}
	}
	// measured pings, where the players reported them
	if settings.MaxPing > 0 {
		for i, a := range players {
			for _, b := range players[:i] {
				if rtt, ok := room.PeerPing(a.ID, b.ID); ok && rtt > settings.MaxPing {
					return fmt.Sprintf("%s and %s measured %d ms; this room allows %d ms.",
						b.Username, a.Username, rtt, settings.MaxPing)
				}
			}
		}
	}
	if seated != settings.Players() {
		if settings.TeamSize > 1 {
			return fmt.Sprintf("A ranked %dv%d set needs exactly %d players.", settings.TeamSize, settings.TeamSize, settings.Players())
		}
		return "A ranked set needs exactly two players."
	}
	return ""
}

//
// Set results
//

type rankedParticipant struct {
	userID    uint64
	accountID int64
	slot      int
	username  string
	region    string
}

type pendingSet struct {
	matchID      uint64
	settings     state.RankedSettings
	participants []rankedParticipant
	reports      map[uint64]protocol.MatchResultRequest
	timer        *time.Timer
	quitter      uint64 // the player who left mid-set (see ranked_left.go)
	// requiredTeams: each slot's team (index 0 = slot 1) when the lobby
	// matched teammates; all 0 when the players pick teams in the game
	requiredTeams [4]int
	// progress: each player's latest MATCH_PROGRESS, the games counted so far
	progress map[uint64][]protocol.MatchResultGame
	// abandoned: MATCH_ABANDONED reports, the set as of a player leaving it
	abandoned map[uint64]protocol.MatchAbandonedRequest
	// left is set once a player left mid-set: the set is judged from the
	// others' reports (judgeLeftSet) instead of waiting for every result
	left *leftSet
	// verdict: the result judgeLeftSet decided, used in place of the reports
	verdict *protocol.MatchResultRequest
}

func (p *pendingSet) participant(userID uint64) (rankedParticipant, bool) {
	for _, pp := range p.participants {
		if pp.userID == userID {
			return pp, true
		}
	}
	return rankedParticipant{}, false
}

// rankedSets tracks ranked matches until every player has reported. The zero
// value is ready to use.
type rankedSets struct {
	mu   sync.Mutex
	sets map[uint64]*pendingSet
}

// take removes and returns a pending set.
func (r *rankedSets) take(matchID uint64) *pendingSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.sets[matchID]
	if set != nil {
		delete(r.sets, matchID)
		if set.timer != nil {
			set.timer.Stop()
		}
	}
	return set
}

// beginRankedSet registers a ranked match and returns its MATCH_BEGIN info.
func (h *Hub) beginRankedSet(match *state.Match, room *state.Room, settings *state.RankedSettings) *protocol.RankedMatchInfo {
	set := &pendingSet{
		matchID:   match.ID,
		settings:  *settings,
		reports:   make(map[uint64]protocol.MatchResultRequest),
		progress:  make(map[uint64][]protocol.MatchResultGame),
		abandoned: make(map[uint64]protocol.MatchAbandonedRequest),
	}
	info := &protocol.RankedMatchInfo{
		Ladder: settings.LadderID, ModuleID: settings.ModuleID,
		RulesetID: settings.RulesetID, BestOf: settings.BestOf, TeamSize: settings.TeamSize,
	}
	slots := make(map[uint64]int, len(room.Players))
	for _, seat := range room.Players {
		if seat.User != nil {
			slots[seat.User.ID] = seat.Slot
		}
	}
	set.requiredTeams = requiredTeams(settings, slots)
	for _, seat := range room.Players {
		if seat.User == nil {
			continue
		}
		set.participants = append(set.participants, rankedParticipant{
			userID: seat.User.ID, accountID: seat.User.AccountID,
			slot: seat.Slot, username: seat.User.Username, region: seat.User.Region,
		})
		info.Players = append(info.Players, protocol.RankedMatchPlayer{
			Slot: seat.Slot, UserID: seat.User.ID, Username: seat.User.Username,
			Standing: h.standing(seat.User.AccountID, settings.LadderID, settings.RulesetID),
			Team:     set.requiredTeams[seat.Slot-1],
		})
	}
	sort.Slice(info.Players, func(i, j int) bool { return info.Players[i].Slot < info.Players[j].Slot })

	h.rankedSets.mu.Lock()
	if h.rankedSets.sets == nil {
		h.rankedSets.sets = make(map[uint64]*pendingSet)
	}
	h.rankedSets.sets[match.ID] = set
	h.rankedSets.mu.Unlock()

	slog.Info("ranked set started", "match", match.ID, "ladder", settings.LadderID,
		"ruleset", settings.RulesetID, "bestOf", settings.BestOf, "requiredTeams", set.requiredTeams)
	return info
}

// requiredTeams gives each seated slot its team (index 0 = slot 1) when the
// lobby matched teammates: each pair shares a team, everyone else is on the
// other, and team 1 holds the lowest slot. All 0 when no pair is seated or
// the teams wouldn't come out even.
func requiredTeams(settings *state.RankedSettings, slots map[uint64]int) [4]int {
	var teams [4]int
	if settings.TeamSize < 2 {
		return teams
	}
	group := make(map[int]int, len(slots)) // slot -> pair number, 0 = no pair
	for _, slot := range slots {
		if slot < 1 || slot > 4 {
			return teams
		}
		group[slot] = 0
	}
	pairs := 0
	for _, pair := range settings.Teammates {
		a, okA := slots[pair[0]]
		b, okB := slots[pair[1]]
		if okA && okB {
			pairs++
			group[a], group[b] = pairs, pairs
		}
	}
	if pairs == 0 {
		return teams
	}
	lowest := 5
	for slot := range group {
		lowest = min(lowest, slot)
	}
	count := [3]int{}
	for slot, g := range group {
		team := 2
		if g == group[lowest] {
			team = 1
		}
		teams[slot-1] = team
		count[team]++
	}
	if count[1] != settings.TeamSize || count[2] != settings.TeamSize {
		return [4]int{}
	}
	return teams
}

// handleMatchResult stores one player's report and finalizes the set once
// every player has reported.
func (h *Hub) handleMatchResult(c *Client, env protocol.Envelope) {
	var req protocol.MatchResultRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}

	h.rankedSets.mu.Lock()
	set := h.rankedSets.sets[req.MatchID]
	if set == nil {
		h.rankedSets.mu.Unlock()
		return
	}
	if _, ok := set.participant(c.user.ID); !ok {
		h.rankedSets.mu.Unlock()
		slog.Warn("MATCH_RESULT from non-participant", "match", req.MatchID, "user", c.user.Username)
		return
	}
	if _, dup := set.reports[c.user.ID]; dup {
		h.rankedSets.mu.Unlock()
		return
	}
	set.reports[c.user.ID] = req
	if set.left != nil {
		// a player already left: this result is one of the stayers' reports
		ready := set.leftReportsComplete()
		if ready {
			delete(h.rankedSets.sets, set.matchID)
			if set.timer != nil {
				set.timer.Stop()
			}
		}
		h.rankedSets.mu.Unlock()
		slog.Info("ranked result reported after a player left", "match", req.MatchID, "user", c.user.Username,
			"winnerSlot", req.WinnerSlot, "complete", ready)
		if ready {
			h.finishLeftSet(set)
		}
		return
	}
	complete := len(set.reports) == len(set.participants)
	if complete {
		// claim the set under the lock so the timeout can't finalize it too
		delete(h.rankedSets.sets, set.matchID)
		if set.timer != nil {
			set.timer.Stop()
		}
	} else if set.timer == nil {
		matchID := set.matchID
		set.timer = time.AfterFunc(rankedResultTimeout, func() {
			if expired := h.rankedSets.take(matchID); expired != nil {
				h.finalizeRankedSet(expired, accounts.SetVoided, "result_timeout")
			}
		})
	}
	h.rankedSets.mu.Unlock()

	slog.Info("ranked result reported", "match", req.MatchID, "user", c.user.Username,
		"winnerSlot", req.WinnerSlot, "complete", complete)
	if !complete {
		return
	}
	status, reason := judgeRankedSet(set)
	h.finalizeRankedSet(set, status, reason)
}

// rankedPlayerLeft handles a player leaving a pending set before reporting
// its result (leaving after reporting is the normal end of a set). The set
// is then judged from the other players' reports of it as of that moment
// (see ranked_left.go). A kicked player didn't choose to leave: the set is
// voided.
func (h *Hub) rankedPlayerLeft(matchID, userID uint64, reason string) {
	h.rankedSets.mu.Lock()
	set := h.rankedSets.sets[matchID]
	if set == nil {
		h.rankedSets.mu.Unlock()
		return
	}
	_, isParticipant := set.participant(userID)
	_, reported := set.reports[userID]
	if !isParticipant || reported {
		h.rankedSets.mu.Unlock()
		return
	}
	if reason == "kicked" && set.left == nil {
		h.rankedSets.mu.Unlock()
		if set = h.rankedSets.take(matchID); set != nil {
			set.quitter = userID
			h.finalizeRankedSet(set, accounts.SetVoided, "player_left")
		}
		return
	}
	ready := set.playerLeft(userID)
	if ready {
		delete(h.rankedSets.sets, set.matchID)
		if set.timer != nil {
			set.timer.Stop()
		}
	} else if set.left.userID == userID {
		// the first player to leave: wait for the others' reports
		if set.timer != nil {
			set.timer.Stop()
		}
		matchID := set.matchID
		set.timer = time.AfterFunc(rankedLeftReportTimeout, func() {
			if expired := h.rankedSets.take(matchID); expired != nil {
				h.finishLeftSet(expired)
			}
		})
	}
	h.rankedSets.mu.Unlock()
	if ready {
		h.finishLeftSet(set)
	}
}

// judgeRankedSet compares every player's report. Only a set every player
// agrees on, with a valid winner and no integrity problem, is rated.
func judgeRankedSet(set *pendingSet) (accounts.SetStatus, string) {
	var reference *protocol.MatchResultRequest
	for _, p := range set.participants {
		report, ok := set.reports[p.userID]
		if !ok {
			return accounts.SetVoided, "missing_report"
		}
		if report.Integrity != "ok" {
			return accounts.SetDisputed, "integrity: " + report.Integrity
		}
		if reference == nil {
			reference = &report
			continue
		}
		if !sameResult(*reference, report) {
			return accounts.SetDisputed, "reports_disagree"
		}
	}

	needed := set.settings.BestOf/2 + 1
	score := normalizedScore(reference.Score)
	if set.settings.TeamSize > 1 {
		return judgeTeamSet(set, reference, score, needed)
	}
	winnerFound := false
	for _, p := range set.participants {
		won := score[p.slot-1]
		if p.slot == reference.WinnerSlot {
			winnerFound = true
			if won != needed {
				return accounts.SetDisputed, "invalid_result"
			}
		} else if won >= needed {
			return accounts.SetDisputed, "invalid_result"
		}
	}
	if !winnerFound {
		return accounts.SetDisputed, "invalid_result"
	}
	return accounts.SetRated, ""
}

// judgeTeamSet checks a team set's agreed result: every player on team 1 or
// 2, TeamSize players each, team 1 holding the lowest slot, and the winning
// team (and only it) at the needed games.
func judgeTeamSet(set *pendingSet, reference *protocol.MatchResultRequest, score [4]int, needed int) (accounts.SetStatus, string) {
	teams := normalizedScore(reference.Teams)
	count := [3]int{}
	lowest := 5
	for _, p := range set.participants {
		team := teams[p.slot-1]
		if team != 1 && team != 2 {
			return accounts.SetDisputed, "invalid_teams"
		}
		count[team]++
		if p.slot < lowest {
			lowest = p.slot
		}
	}
	if count[1] != set.settings.TeamSize || count[2] != set.settings.TeamSize || teams[lowest-1] != 1 {
		return accounts.SetDisputed, "invalid_teams"
	}
	// teammates the lobby matched (a chosen or kept team) played together
	if set.requiredTeams != ([4]int{}) {
		for _, p := range set.participants {
			if teams[p.slot-1] != set.requiredTeams[p.slot-1] {
				return accounts.SetDisputed, "teams_not_as_matched"
			}
		}
	}
	if reference.WinnerTeam != 1 && reference.WinnerTeam != 2 {
		return accounts.SetDisputed, "invalid_result"
	}
	for _, p := range set.participants {
		won := score[p.slot-1]
		if teams[p.slot-1] == reference.WinnerTeam {
			if won != needed {
				return accounts.SetDisputed, "invalid_result"
			}
		} else if won >= needed {
			return accounts.SetDisputed, "invalid_result"
		}
	}
	return accounts.SetRated, ""
}

func normalizedScore(score []int) [4]int {
	var out [4]int
	copy(out[:], score)
	return out
}

func sameResult(a, b protocol.MatchResultRequest) bool {
	if a.WinnerSlot != b.WinnerSlot || a.RulesetHash != b.RulesetHash ||
		normalizedScore(a.Score) != normalizedScore(b.Score) || len(a.Games) != len(b.Games) ||
		a.WinnerTeam != b.WinnerTeam || normalizedScore(a.Teams) != normalizedScore(b.Teams) {
		return false
	}
	for i := range a.Games {
		if a.Games[i].WinnerSlot != b.Games[i].WinnerSlot || a.Games[i].WinnerTeam != b.Games[i].WinnerTeam {
			return false
		}
	}
	return true
}

// finalizeRankedSet records the set and tells its players the outcome.
func (h *Hub) finalizeRankedSet(set *pendingSet, status accounts.SetStatus, reason string) {
	// Any report carries the score (rated sets: all reports agree); a set a
	// player left has the verdict judgeLeftSet decided.
	reference := set.verdict
	for _, p := range set.participants {
		if reference != nil {
			break
		}
		if report, ok := set.reports[p.userID]; ok {
			reference = &report
		}
	}
	var score, teams [4]int
	winnerSlot, winnerTeam := 0, 0
	if reference != nil {
		score = normalizedScore(reference.Score)
		if set.settings.TeamSize > 1 {
			teams = normalizedScore(reference.Teams)
		}
		if status == accounts.SetRated {
			if set.settings.TeamSize > 1 {
				winnerTeam = reference.WinnerTeam
			} else {
				winnerSlot = reference.WinnerSlot
			}
		}
	}

	rec := accounts.SetRecord{
		Ladder: set.settings.LadderID, MatchID: set.matchID, BootID: h.bootID,
		Status: status, Reason: reason, RulesetID: set.settings.RulesetID,
	}
	if reference != nil {
		rec.ModuleVersion = reference.ModuleVersion
		rec.RomMD5 = reference.RomMD5
	}
	rec.WinnerTeam = winnerTeam
	if winnerTeam != 0 && reference != nil {
		// every game of a team set is rated
		for _, g := range reference.Games {
			rec.GameWinners = append(rec.GameWinners, g.WinnerTeam)
		}
		if reason == reasonForfeit {
			rec.GameWinners = forfeitedGames(rec.GameWinners, winnerTeam, set.settings.BestOf/2+1)
		}
	}
	for _, p := range set.participants {
		games, team := 0, 0
		if p.slot >= 1 && p.slot <= 4 {
			games, team = score[p.slot-1], teams[p.slot-1]
		}
		rec.Players = append(rec.Players, accounts.SetPlayer{AccountID: p.accountID, Slot: p.slot, GamesWon: games,
			Team: team, Region: p.region})
		if winnerSlot != 0 && p.slot == winnerSlot {
			rec.WinnerID = p.accountID
		}
	}
	// each player's report, by username, so moderators can see who claimed what
	if reportsJSON, err := json.Marshal(set.recordReports()); err == nil {
		rec.ResultJSON = string(reportsJSON)
	}

	payload := protocol.RankedResultPayload{
		MatchID: set.matchID, Status: string(status), Reason: reason, WinnerSlot: winnerSlot, WinnerTeam: winnerTeam,
	}
	if set.left != nil {
		payload.LeftUsername = set.left.username
	}
	if h.accounts != nil {
		setID, outcomes, err := h.accounts.RecordSetWithID(rec)
		payload.SetID = setID
		if err != nil {
			slog.Error("recording ranked set failed", "match", set.matchID, "err", err)
			payload.Status, payload.Reason, payload.WinnerSlot = string(accounts.SetVoided), "server_error", 0
		} else {
			tiers := h.rankedConfig.Tiers
			for i, p := range set.participants {
				board := accounts.LadderRating{Ladder: rec.Ladder, Ruleset: rec.RulesetID}
				board.Rating = outcomes[i].Before
				policy := h.rankedConfig.Policy(rec.Ladder)
				before := standingFrom(board, tiers, policy)
				after := before
				if status == accounts.SetRated {
					board.Rating = outcomes[i].After
					after = standingFrom(board, tiers, policy)
				}
				if current := h.standing(p.accountID, rec.Ladder, rec.RulesetID); current != nil {
					after.SetsPlayed = current.SetsPlayed
					before.SetsPlayed = current.SetsPlayed
					if status == accounts.SetRated {
						before.SetsPlayed--
					}
				}
				team := 0
				if p.slot >= 1 && p.slot <= 4 {
					team = teams[p.slot-1]
				}
				payload.Players = append(payload.Players, protocol.RankedResultPlayer{
					Slot: p.slot, Team: team, Username: p.username, Before: before, After: after,
				})
			}
		}
	}

	slog.Info("ranked set finalized", "match", set.matchID, "status", payload.Status,
		"reason", payload.Reason, "winnerSlot", payload.WinnerSlot)

	for _, p := range set.participants {
		u := h.state.GetUser(p.userID)
		if u == nil || u.Send == nil {
			continue
		}
		data, err := json.Marshal(protocol.Envelope{Type: protocol.TypeRankedResult, Data: mustMarshal(payload)})
		if err != nil {
			continue
		}
		select {
		case u.Send <- data:
		default:
		}
	}
	h.quickMatchSetEnded(set, teams)
}
