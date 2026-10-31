package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// games builds counted games from their winners (slots, or teams in a team set).
func games(team bool, winners ...int) []protocol.MatchResultGame {
	out := make([]protocol.MatchResultGame, 0, len(winners))
	for i, w := range winners {
		g := protocol.MatchResultGame{Game: i + 1, WinnerSlot: w, Stage: 6}
		if team {
			// the lowest slot of the winning team (1+3 vs 2+4)
			g.WinnerTeam, g.WinnerSlot = w, w
		}
		out = append(out, g)
	}
	return out
}

// live is the game under way: stocks and percent per slot, in slot order.
func live(inGame bool, teams []int, stocksPercent ...[2]int) *protocol.MatchLiveGame {
	l := &protocol.MatchLiveGame{Game: 3, InGame: inGame, Teams: teams}
	for i, sp := range stocksPercent {
		l.Players = append(l.Players, protocol.MatchLivePlayer{Slot: i + 1, Stocks: sp[0], Percent: sp[1]})
	}
	return l
}

func (f *rankedFixture) progress(c *Client, matchID uint64, g []protocol.MatchResultGame) {
	f.t.Helper()
	f.hub.handleMatchProgress(c, envelope(f.t, protocol.MatchProgressRequest{MatchID: matchID, Games: g}))
}

func (f *rankedFixture) abandon(c *Client, matchID uint64, leftSlot int, g []protocol.MatchResultGame,
	l *protocol.MatchLiveGame) {
	f.t.Helper()
	f.hub.handleMatchAbandoned(c, envelope(f.t, protocol.MatchAbandonedRequest{
		MatchID: matchID, LeftSlot: leftSlot, Games: g, Live: l, RulesetHash: "abc", Integrity: "ok",
	}))
}

// leftSet1v1 plays the given games (both players confirm them), has the
// player on leftSlot leave, and returns the result the other one gets.
func leftSet1v1(t *testing.T, leftSlot int, played []protocol.MatchResultGame,
	l *protocol.MatchLiveGame) protocol.RankedResultPayload {
	t.Helper()
	f := newRankedFixture(t)
	host, guest, matchID := startRankedMatch(t, f)
	if len(played) > 0 {
		f.progress(host, matchID, played)
		f.progress(guest, matchID, played)
	}
	leaver, stayer := guest, host
	if leftSlot == 1 {
		leaver, stayer = host, guest
	}
	f.hub.handleMatchFinished(leaver, protocol.Envelope{})
	f.abandon(stayer, matchID, leftSlot, played, l)
	res := rankedResultFor(t, stayer)
	if res.LeftUsername != leaver.user.Username {
		t.Fatalf("leftUsername = %q", res.LeftUsername)
	}
	return res
}

func TestRankedLeftSetRules(t *testing.T) {
	tests := []struct {
		name     string
		leftSlot int
		played   []protocol.MatchResultGame
		live     *protocol.MatchLiveGame
		status   string
		reason   string
		winner   int
	}{
		{"leaves during game 1", 2, nil, live(true, nil, [2]int{3, 40}, [2]int{1, 90}), "voided", "left_in_game_1", 0},
		{"the loser of game 1 leaves", 2, games(false, 1), nil, "rated", "forfeit", 1},
		{"the winner of game 1 leaves", 1, games(false, 1), nil, "voided", "leader_left", 0},
		{"behind 0-1 in game 2, ahead in it", 1, games(false, 2), live(true, nil, [2]int{4, 0}, [2]int{1, 0}), "rated", "forfeit", 2},
		{"game 3, fewer stocks", 2, games(false, 1, 2), live(true, nil, [2]int{2, 80}, [2]int{1, 10}), "rated", "forfeit", 1},
		{"game 3, more stocks", 2, games(false, 1, 2), live(true, nil, [2]int{1, 10}, [2]int{2, 80}), "voided", "leader_left", 0},
		{"game 3, level stocks, more damage", 2, games(false, 1, 2), live(true, nil, [2]int{2, 30}, [2]int{2, 31}), "rated", "forfeit", 1},
		{"game 3, level stocks, less damage", 2, games(false, 1, 2), live(true, nil, [2]int{2, 31}, [2]int{2, 30}), "voided", "leader_left", 0},
		{"game 3, dead even", 1, games(false, 1, 2), live(true, nil, [2]int{4, 0}, [2]int{4, 0}), "rated", "forfeit", 2},
		{"before game 3 starts", 1, games(false, 1, 2), live(false, nil), "rated", "forfeit", 2},
		{"game 3, the module can't tell", 2, games(false, 1, 2), nil, "voided", "player_left", 0},
		{"the set was already over", 2, games(false, 1, 1), nil, "rated", "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := leftSet1v1(t, tt.leftSlot, tt.played, tt.live)
			if res.Status != tt.status || res.Reason != tt.reason || res.WinnerSlot != tt.winner {
				t.Fatalf("result = %s %q winner %d, want %s %q winner %d",
					res.Status, res.Reason, res.WinnerSlot, tt.status, tt.reason, tt.winner)
			}
			if tt.status == "rated" {
				for _, p := range res.Players {
					if (p.Slot == tt.winner) != (p.After.Rating > p.Before.Rating) {
						t.Fatalf("rating moved the wrong way for slot %d: %+v", p.Slot, p)
					}
				}
			}
		})
	}
}

// Games count only as far as the leaver's own client confirmed them.
func TestRankedLeftSetNeedsTheLeaversConfirmation(t *testing.T) {
	f := newRankedFixture(t)
	host, guest, matchID := startRankedMatch(t, f)
	// the guest never confirmed game 1, which the host says it won
	f.hub.handleMatchFinished(guest, protocol.Envelope{})
	f.abandon(host, matchID, 2, games(false, 1), nil)
	if res := rankedResultFor(t, host); res.Status != "voided" || res.Reason != "left_in_game_1" {
		t.Fatalf("result = %+v", res)
	}

	f = newRankedFixture(t)
	host, guest, matchID = startRankedMatch(t, f)
	// the reports disagree on who won game 1
	f.progress(guest, matchID, games(false, 2))
	f.hub.handleMatchFinished(guest, protocol.Envelope{})
	f.abandon(host, matchID, 2, games(false, 1), nil)
	if res := rankedResultFor(t, host); res.Status != "disputed" || res.Reason != "reports_disagree" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRankedLeftSetReports(t *testing.T) {
	// no report from the player who stayed: voided after the timeout
	old := rankedLeftReportTimeout
	rankedLeftReportTimeout = 50 * time.Millisecond
	t.Cleanup(func() { rankedLeftReportTimeout = old })
	f := newRankedFixture(t)
	host, guest, matchID := startRankedMatch(t, f)
	f.progress(host, matchID, games(false, 1))
	f.progress(guest, matchID, games(false, 1))
	f.hub.handleMatchFinished(guest, protocol.Envelope{})
	if len(drain(host, protocol.TypeRankedResult)) != 0 {
		t.Fatal("judged without the stayer's report")
	}
	// a report that names the wrong leaver doesn't count
	f.abandon(host, matchID, 1, games(false, 1), nil)
	var res protocol.RankedResultPayload
	waitFor(t, "the result after the timeout", func() bool {
		msgs := drain(host, protocol.TypeRankedResult)
		if len(msgs) > 0 {
			json.Unmarshal(msgs[0].Data, &res)
		}
		return len(msgs) > 0
	})
	if res.Status != "voided" || res.Reason != "player_left" {
		t.Fatalf("result = %+v", res)
	}

	// a report that arrives before the lobby sees the leave waits for it
	f = newRankedFixture(t)
	host, guest, matchID = startRankedMatch(t, f)
	f.progress(host, matchID, games(false, 1))
	f.progress(guest, matchID, games(false, 1))
	f.abandon(host, matchID, 2, games(false, 1), nil)
	if len(drain(host, protocol.TypeRankedResult)) != 0 {
		t.Fatal("judged before the player left")
	}
	f.hub.handleMatchFinished(guest, protocol.Envelope{})
	if res := rankedResultFor(t, host); res.Status != "rated" || res.Reason != "forfeit" || res.WinnerSlot != 1 {
		t.Fatalf("result = %+v", res)
	}

	// a kicked player didn't choose to leave
	f = newRankedFixture(t)
	host, guest, matchID = startRankedMatch(t, f)
	f.progress(host, matchID, games(false, 1))
	f.progress(guest, matchID, games(false, 1))
	f.hub.rankedPlayerLeft(matchID, guest.user.ID, "kicked")
	if res := rankedResultFor(t, host); res.Status != "voided" || res.Reason != "player_left" {
		t.Fatalf("result = %+v", res)
	}
}

// The set was decided and the winner reported it; the loser left before
// reporting. It's rated as played.
func TestRankedLeftAfterTheSetWasDecided(t *testing.T) {
	f := newRankedFixture(t)
	host, guest, matchID := startRankedMatch(t, f)
	f.progress(host, matchID, games(false, 1, 1))
	f.progress(guest, matchID, games(false, 1, 1))
	f.hub.handleMatchResult(host, envelope(t, result(matchID, 1, []int{2, 0})))
	f.hub.handleMatchFinished(guest, protocol.Envelope{})
	if res := rankedResultFor(t, host); res.Status != "rated" || res.Reason != "" || res.WinnerSlot != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRankedLeftTeamSet(t *testing.T) {
	teams := []int{1, 2, 1, 2}
	play := func(t *testing.T, played []protocol.MatchResultGame, leaver int, l *protocol.MatchLiveGame) protocol.RankedResultPayload {
		t.Helper()
		f := newRankedFixtureWith(t, testTeamsConfig)
		players, matchID := startTeamMatch(t, f)
		for _, p := range players {
			f.progress(p, matchID, played)
		}
		f.hub.handleMatchFinished(players[leaver-1], protocol.Envelope{})
		for slot, p := range players {
			if slot+1 != leaver {
				if l == nil {
					l = live(false, teams)
				}
				f.abandon(p, matchID, leaver, played, l)
			}
		}
		return rankedResultFor(t, players[0])
	}

	// team 2 lost game 1 and P2 left: the whole team forfeits, P4 included
	res := play(t, games(true, 1), 2, nil)
	if res.Status != "rated" || res.Reason != "forfeit" || res.WinnerTeam != 1 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range res.Players {
		if (p.Team == 1) != (p.After.Rating > p.Before.Rating) {
			t.Fatalf("rating moved the wrong way for P%d (team %d): %+v", p.Slot, p.Team, p)
		}
	}

	// P1 left while team 1 was ahead in the set
	if res := play(t, games(true, 1), 1, nil); res.Status != "voided" || res.Reason != "leader_left" {
		t.Fatalf("result = %+v", res)
	}

	// game 3: team stocks decide (P1 2 + P3 0 against P2 1 + P4 2)
	l := live(true, teams, [2]int{2, 40}, [2]int{1, 10}, [2]int{0, 0}, [2]int{2, 70})
	if res := play(t, games(true, 1, 2), 3, l); res.Status != "rated" || res.Reason != "forfeit" || res.WinnerTeam != 2 {
		t.Fatalf("result = %+v", res)
	}
}

// A 2v2 forfeit counts as losing the games the team didn't play.
func TestForfeitedGames(t *testing.T) {
	if got := forfeitedGames([]int{1}, 1, 2); len(got) != 2 || got[1] != 1 {
		t.Fatalf("forfeitedGames = %v", got)
	}
	if got := forfeitedGames([]int{2, 1}, 1, 2); len(got) != 3 || got[2] != 1 {
		t.Fatalf("forfeitedGames = %v", got)
	}
}
