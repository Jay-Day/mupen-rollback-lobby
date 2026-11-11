package ws

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/rating"
)

func (f *rankedFixture) quickMatch(c *Client, ranked *protocol.RankedRoomRequest) {
	f.t.Helper()
	f.hub.handleQuickMatchJoin(c, envelope(f.t, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, Ranked: ranked,
	}))
}

// lastQuickMatchStatus returns the newest QUICK_MATCH_STATUS sent to c.
func lastQuickMatchStatus(t *testing.T, c *Client) (protocol.QuickMatchStatusPayload, bool) {
	t.Helper()
	statuses := drain(c, protocol.TypeQuickMatchStatus)
	if len(statuses) == 0 {
		return protocol.QuickMatchStatusPayload{}, false
	}
	var out protocol.QuickMatchStatusPayload
	json.Unmarshal(statuses[len(statuses)-1].Data, &out)
	return out, true
}

func TestRankedQuickMatchPairsSameSettings(t *testing.T) {
	f := newRankedFixture(t)
	a, b := f.connect("alice", true), f.connect("bob", true)
	casual := f.connect("carol", true)

	f.quickMatch(a, &protocol.RankedRoomRequest{Enabled: true})
	if status, _ := lastQuickMatchStatus(t, a); !status.Searching || !status.Ranked || status.QueueSize != 1 {
		t.Fatalf("status = %+v", status)
	}
	// a casual searcher never pairs with a ranked one
	f.quickMatch(casual, nil)
	if a.user.CurrentRoom != nil || casual.user.CurrentRoom != nil {
		t.Fatal("casual and ranked searchers paired")
	}
	if status, _ := lastQuickMatchStatus(t, casual); !status.Searching || status.Ranked || status.QueueSize != 1 {
		t.Fatalf("casual status = %+v", status)
	}

	f.quickMatch(b, &protocol.RankedRoomRequest{Enabled: true})
	room := a.user.CurrentRoom
	if room == nil || b.user.CurrentRoom != room {
		t.Fatal("ranked searchers with the same settings didn't pair")
	}
	settings := room.GetRanked()
	if settings == nil || settings.LadderID != "remix" || settings.RulesetID != "remix-1v1-vanilla" || settings.BestOf != 3 {
		t.Fatalf("room ranked = %+v", settings)
	}
	if room.Host != a.user || room.MaxPlayers != 2 || room.Name != "Ranked Quick Match: alice vs bob" {
		t.Fatalf("room = host %s, %d seats, %q", room.Host.Username, room.MaxPlayers, room.Name)
	}
	if problem := f.hub.rankedStartCheck(room, settings); problem != "" {
		t.Fatalf("start check: %s", problem)
	}
	for _, c := range []*Client{a, b} {
		if status, ok := lastQuickMatchStatus(t, c); !ok || status.Searching {
			t.Fatalf("%s still searching: %+v", c.user.Username, status)
		}
	}
	if casual.user.CurrentRoom != nil || !f.hub.matchmaker.contains(casual.user.ID) {
		t.Fatal("the casual searcher left the queue")
	}
}

// Rosters split the queue. Set length and skill aren't choices, so an older
// client's request for another length or a tier range still pairs, and the
// set gets the ladder's length.
func TestRankedQuickMatchNeedsSameRoster(t *testing.T) {
	f := newRankedFixture(t)
	a, b, c := f.connect("alice", true), f.connect("bob", true), f.connect("carol", true)

	f.quickMatch(a, &protocol.RankedRoomRequest{Enabled: true})
	f.quickMatch(c, &protocol.RankedRoomRequest{Enabled: true, RulesetID: "remix-1v1-remix"})
	if a.user.CurrentRoom != nil || c.user.CurrentRoom != nil {
		t.Fatal("searchers paired across rosters")
	}
	f.hub.handleQuickMatchJoin(b, envelope(t, map[string]any{
		"rom":    protocol.RomInfo{Name: "Remix", MD5: testRomMD5},
		"ranked": map[string]any{"enabled": true, "bestOf": 5, "maxTierGap": 1, "allowProvisional": false},
	}))
	room := b.user.CurrentRoom
	if room == nil || room != a.user.CurrentRoom {
		t.Fatal("searchers on the same roster didn't pair")
	}
	if settings := room.GetRanked(); settings.BestOf != 3 {
		t.Fatalf("set length = %d, want the ladder's 3", settings.BestOf)
	}
}

func TestRankedQuickMatchRequiresAccount(t *testing.T) {
	f := newRankedFixture(t)
	guest := f.connect("guest", false)
	f.quickMatch(guest, &protocol.RankedRoomRequest{Enabled: true})
	if f.hub.matchmaker.contains(guest.user.ID) {
		t.Fatal("a guest queued for ranked")
	}
	status, ok := lastQuickMatchStatus(t, guest)
	if !ok || status.Searching || !status.Ranked {
		t.Fatalf("guest status = %+v (sent %v)", status, ok)
	}
}

// Any two players pair, however far apart their tiers and whether or not
// they're still in placement.
func TestRankedQuickMatchIgnoresSkill(t *testing.T) {
	f := newRankedFixture(t)
	strong, weak, newbie := f.connect("strong", true), f.connect("weak", true), f.connect("newbie", true)
	seed := func(c *Client, r float64) {
		if err := f.store.SeedRating(c.user.AccountID, "remix", "remix-1v1-vanilla",
			rating.Rating{Rating: r, Deviation: 60, Volatility: 0.06}); err != nil {
			t.Fatal(err)
		}
	}
	seed(strong, 1850)
	seed(weak, 1250)
	s1 := f.hub.standing(strong.user.AccountID, "remix", "remix-1v1-vanilla")
	s2 := f.hub.standing(weak.user.AccountID, "remix", "remix-1v1-vanilla")
	if s1.TierIndex-s2.TierIndex < 2 || s1.Provisional || s2.Provisional {
		t.Fatalf("seeded standings %+v / %+v aren't two settled tiers apart", s1, s2)
	}

	f.quickMatch(strong, &protocol.RankedRoomRequest{Enabled: true})
	f.quickMatch(weak, &protocol.RankedRoomRequest{Enabled: true})
	if weak.user.CurrentRoom == nil || weak.user.CurrentRoom != strong.user.CurrentRoom {
		t.Fatal("players two tiers apart didn't pair")
	}
	if problem := f.hub.rankedStartCheck(weak.user.CurrentRoom, weak.user.CurrentRoom.GetRanked()); problem != "" {
		t.Fatalf("start check: %s", problem)
	}

	vet := f.connect("vet", true)
	seed(vet, 1850)
	f.quickMatch(vet, &protocol.RankedRoomRequest{Enabled: true})
	f.quickMatch(newbie, &protocol.RankedRoomRequest{Enabled: true})
	if newbie.user.CurrentRoom == nil || newbie.user.CurrentRoom != vet.user.CurrentRoom {
		t.Fatal("a player in placement didn't pair with a settled one")
	}
}

func TestRankedQuickMatchTeams(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	players := []*Client{f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)}
	req := &protocol.RankedRoomRequest{Enabled: true, LadderID: "remix-2v2"}
	for _, p := range players[:3] {
		f.quickMatch(p, req)
	}
	if status, _ := lastQuickMatchStatus(t, players[2]); status.QueueSize != 3 {
		t.Fatalf("pool size = %d", status.QueueSize)
	}
	// a 1v1 searcher on the same ROM doesn't fill a 2v2 match
	solo := f.connect("solo", true)
	f.quickMatch(solo, &protocol.RankedRoomRequest{Enabled: true})
	for _, p := range []*Client{players[0], players[1], players[2], solo} {
		if p.user.CurrentRoom != nil {
			t.Fatalf("%s placed with fewer than four 2v2 searchers", p.user.Username)
		}
	}
	f.quickMatch(players[3], req)
	room := players[0].user.CurrentRoom
	if room == nil || room.MaxPlayers != 4 || room.GetRanked().TeamSize != 2 {
		t.Fatalf("2v2 room = %+v", room)
	}
	for _, p := range players {
		if p.user.CurrentRoom != room {
			t.Fatalf("%s isn't in the 2v2 room", p.user.Username)
		}
	}
	if !strings.HasPrefix(room.Name, "Ranked Quick Match 2v2: ") {
		t.Fatalf("room name %q", room.Name)
	}
	if problem := f.hub.rankedStartCheck(room, room.GetRanked()); problem != "" {
		t.Fatalf("start check: %s", problem)
	}
	if solo.user.CurrentRoom != nil || !f.hub.matchmaker.contains(solo.user.ID) {
		t.Fatal("the 1v1 searcher left the queue")
	}
}
