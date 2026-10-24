package web

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/ranked"
	"github.com/jay-day/rmgk-lobby/internal/rating"
)

const testConfig = `{"ladders":[{"id":"remix","name":"Smash Remix 1v1","moduleId":"smashremix",
	"rules":["4 stock"],"rulesets":["remix-1v1-vanilla","remix-1v1-remix"],
	"rulesetNames":{"remix-1v1-vanilla":"Vanilla roster"},
	"rulesetDescriptions":{"remix-1v1-vanilla":"The original 12."},
	"rulesetStages":{"remix-1v1-remix":["Dream Land","Goomba Road"]},
	"bestOf":3,"roms":[{"md5":"2B2D6B295106C54216B7FC7A2F14346E","name":"Remix"}]}]}`

func newTestServer(t *testing.T) (*httptest.Server, *accounts.Store) {
	t.Helper()
	srv, store, _ := newTestServerWithReplays(t)
	return srv, store
}

func newTestServerWithReplays(t *testing.T) (*httptest.Server, *accounts.Store, string) {
	t.Helper()
	store, err := accounts.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg, err := ranked.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	replayDir := t.TempDir()
	New(store, cfg, replayDir).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, replayDir
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return res.StatusCode
}

func TestIndexServedCompressedAndCached(t *testing.T) {
	srv, _ := newTestServer(t)

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("ETag") == "" {
		t.Fatalf("headers = %v", res.Header)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), "RMG-K Ranked") {
		t.Fatal("page body missing title")
	}

	req2, _ := http.NewRequest("GET", srv.URL+"/rankings", nil)
	req2.Header.Set("If-None-Match", res.Header.Get("ETag"))
	res2, err := http.DefaultTransport.RoundTrip(req2)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d", res2.StatusCode)
	}

	if res3, _ := http.Get(srv.URL + "/nope"); res3.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path = %d", res3.StatusCode)
	}
}

func TestOverview(t *testing.T) {
	srv, _ := newTestServer(t)
	var ov struct {
		Ladders []struct {
			ID       string
			Rules    []string
			Rulesets []struct {
				ID, Name, Description string
				Stages                []string
			}
		}
		Tiers  []struct{ Name string }
		Glicko struct {
			Rating, Deviation, MinDeviation, Tau, RatingPeriodHours float64
		}
	}
	if code := getJSON(t, srv.URL+"/api/ranked/overview", &ov); code != 200 {
		t.Fatalf("status %d", code)
	}
	if g := ov.Glicko; g.Rating != 1500 || g.Deviation != 350 || g.MinDeviation != 45 || g.Tau != 0.5 || g.RatingPeriodHours != 24 {
		t.Fatalf("glicko = %+v", g)
	}
	if len(ov.Ladders) != 1 || ov.Ladders[0].Rules[0] != "4 stock" || len(ov.Tiers) == 0 {
		t.Fatalf("overview = %+v", ov)
	}
	rs := ov.Ladders[0].Rulesets
	if rs[0].Name != "Vanilla roster" || rs[0].Description != "The original 12." || rs[1].Name != "remix-1v1-remix" ||
		rs[0].Stages == nil || len(rs[0].Stages) != 0 || len(rs[1].Stages) != 2 || rs[1].Stages[1] != "Goomba Road" {
		t.Fatalf("rulesets = %+v", rs)
	}
}

func TestLeaderboardPlayerAndSets(t *testing.T) {
	srv, store := newTestServer(t)
	ace, _ := store.Register("Ace", "password123")
	rookie, _ := store.Register("rookie", "password123")
	// Ace is established, rookie still in placement
	store.SeedRating(ace.ID, "remix", "remix-1v1-vanilla", rating.Rating{Rating: 1700, Deviation: 60, Volatility: 0.06})
	_, err := store.RecordSet(accounts.SetRecord{
		Ladder: "remix", MatchID: 1, BootID: "b", Status: accounts.SetRated, WinnerID: ace.ID,
		RulesetID: "remix-1v1-vanilla",
		ResultJSON: `{"1":{"games":[{"players":[{"slot":1,"character":29},{"slot":2,"character":0}]},` +
			`{"players":[{"slot":1,"character":29},{"slot":2,"character":5}]}]}}`,
		Players: []accounts.SetPlayer{{AccountID: ace.ID, Slot: 1, GamesWon: 2}, {AccountID: rookie.ID, Slot: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var board struct {
		Players []struct {
			Rank        int
			Username    string
			Provisional bool
			Wins        int
		}
	}
	if code := getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=remix", &board); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(board.Players) != 2 || board.Players[0].Username != "Ace" || board.Players[0].Rank != 1 ||
		board.Players[1].Rank != 0 || !board.Players[1].Provisional {
		t.Fatalf("leaderboard = %+v", board.Players)
	}
	if code := getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=nope", nil); code != 404 {
		t.Fatalf("unknown ladder = %d", code)
	}
	// the remix roster is a separate ranking: nobody has played it
	board.Players = nil
	if code := getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=remix&ruleset=remix-1v1-remix", &board); code != 200 ||
		len(board.Players) != 0 {
		t.Fatalf("remix roster = %d %+v", code, board.Players)
	}
	if code := getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=remix&ruleset=nope", nil); code != 404 {
		t.Fatalf("unknown roster = %d", code)
	}

	var player struct {
		Username  string
		Standings []struct {
			Rank, Wins           int
			Ruleset, RulesetName string
		}
		History    map[string][]struct{ Rating int }
		Characters []struct{ Character, Sets int }
	}
	if code := getJSON(t, srv.URL+"/api/ranked/player?name=ACE", &player); code != 200 {
		t.Fatalf("status %d", code)
	}
	if player.Username != "Ace" || len(player.Standings) != 1 || player.Standings[0].Rank != 1 ||
		player.Standings[0].RulesetName != "Vanilla roster" ||
		len(player.History["remix/remix-1v1-vanilla"]) != 1 || len(player.Characters) != 1 ||
		player.Characters[0].Character != 29 || player.Characters[0].Sets != 1 {
		t.Fatalf("player = %+v", player)
	}

	// the sets list, filtered
	type setsPage struct {
		Total int
		Sets  []struct {
			Winner    string
			HasReplay bool
			ReplayURL string
			Players   []struct {
				Username   string
				Characters []int
			}
		}
	}
	var page setsPage
	if code := getJSON(t, srv.URL+"/api/ranked/sets?player=rookie", &page); code != 200 || page.Total != 1 ||
		page.Sets[0].Winner != "Ace" || page.Sets[0].HasReplay || page.Sets[0].ReplayURL != "" {
		t.Fatalf("rookie's sets = %d %+v", code, page)
	}
	if got := page.Sets[0].Players[1].Characters; len(got) != 2 || got[0] != 0 || got[1] != 5 {
		t.Fatalf("rookie characters = %v", got)
	}
	for _, c := range []struct {
		query string
		total int
	}{
		{"player=Ace&result=win", 1}, {"player=Ace&result=loss", 0}, {"player=rookie&character=5", 1},
		{"player=Ace&character=5", 0}, {"character=0", 1}, {"ruleset=remix-1v1-remix", 0},
		{"player=Ace&opponent=rookie", 1}, {"player=Ace&offset=1", 1},
	} {
		page = setsPage{}
		if code := getJSON(t, srv.URL+"/api/ranked/sets?"+c.query, &page); code != 200 || page.Total != c.total {
			t.Errorf("sets?%s = %d, total %d (want %d)", c.query, code, page.Total, c.total)
		}
	}
	for query, want := range map[string]int{"player=ghost": 404, "result=win": 400, "opponent=Ace": 400,
		"player=Ace&result=draw": 400, "character=x": 400, "ruleset=nope": 404} {
		if code := getJSON(t, srv.URL+"/api/ranked/sets?"+query, nil); code != want {
			t.Errorf("sets?%s = %d, want %d", query, code, want)
		}
	}
	if code := getJSON(t, srv.URL+"/api/ranked/player?name=ghost", nil); code != 404 {
		t.Fatalf("missing player = %d", code)
	}

	var sets struct{ Sets []struct{ Status string } }
	if code := getJSON(t, srv.URL+"/api/ranked/sets?limit=5", &sets); code != 200 || len(sets.Sets) != 1 {
		t.Fatalf("sets = %d %+v", code, sets)
	}
}

func TestSetDetailAndReplay(t *testing.T) {
	srv, store, dir := newTestServerWithReplays(t)
	a, _ := store.Register("Ace", "password123")
	b, _ := store.Register("Bee", "password123")
	setID, _, err := store.RecordSetWithID(accounts.SetRecord{
		Ladder: "remix", MatchID: 1, BootID: "b", Status: accounts.SetRated, WinnerID: a.ID,
		RulesetID: "remix-1v1-vanilla", ModuleVersion: "0.4.0", RomMD5: "2B2D6B295106C54216B7FC7A2F14346E",
		Players: []accounts.SetPlayer{{AccountID: a.ID, Slot: 1, GamesWon: 2}, {AccountID: b.ID, Slot: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/ranked/sets/" + strconv.FormatInt(setID, 10)

	var detail struct {
		RulesetName string
		HasReplay   bool
		Replay      *struct{ URL string }
	}
	if code := getJSON(t, base, &detail); code != 200 || detail.RulesetName != "Vanilla roster" ||
		detail.HasReplay || detail.Replay != nil {
		t.Fatalf("detail before replay = %d %+v", code, detail)
	}
	if res, _ := http.Get(base + "/replay"); res.StatusCode != 404 {
		t.Fatalf("missing replay = %d", res.StatusCode)
	}

	data := []byte("krec bytes")
	os.WriteFile(accounts.ReplayFile(dir, setID), data, 0o644)
	if err := store.AddReplay(accounts.ReplayInfo{SetID: setID, Size: int64(len(data)), SHA256: "ab",
		Bookmarks: `[{"record":5,"label":"Game 1","kind":"game_start","game":1}]`, UploaderID: a.ID}); err != nil {
		t.Fatal(err)
	}
	var withReplay struct {
		HasReplay bool
		Replay    struct {
			URL           string
			ModuleID      string
			ModuleVersion string
			Bookmarks     []struct{ Record int }
		}
	}
	getJSON(t, base, &withReplay)
	if !withReplay.HasReplay || withReplay.Replay.ModuleID != "smashremix" || withReplay.Replay.ModuleVersion != "0.4.0" ||
		len(withReplay.Replay.Bookmarks) != 1 || withReplay.Replay.Bookmarks[0].Record != 5 {
		t.Fatalf("detail with replay = %+v", withReplay)
	}
	// the sets list says where to download it
	var list struct {
		Sets []struct {
			HasReplay bool
			ReplayURL string
		}
	}
	if getJSON(t, srv.URL+"/api/ranked/sets", &list); len(list.Sets) != 1 || !list.Sets[0].HasReplay ||
		list.Sets[0].ReplayURL != withReplay.Replay.URL {
		t.Fatalf("list with replay = %+v", list)
	}
	res, err := http.Get(srv.URL + withReplay.Replay.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != string(data) || res.Header.Get("ETag") != `"ab"` {
		t.Fatalf("replay download = %d %q %v", res.StatusCode, body, res.Header)
	}
	if code := getJSON(t, srv.URL+"/api/ranked/sets/999", nil); code != 404 {
		t.Fatalf("unknown set = %d", code)
	}
}

func TestLeaderboardRegions(t *testing.T) {
	srv, store := newTestServer(t)
	names := []string{"Euro", "Yank", "Kiwi", "Brit"}
	regions := map[string]string{"Euro": "eu-west", "Yank": "us-east", "Kiwi": "oceania", "Brit": "eu-west"}
	ratings := map[string]float64{"Euro": 1800, "Yank": 1900, "Kiwi": 1600, "Brit": 1700}
	ids := map[string]int64{}
	for _, n := range names {
		acct, _ := store.Register(n, "password123")
		ids[n] = acct.ID
	}
	for i, pair := range [][2]string{{"Euro", "Yank"}, {"Kiwi", "Brit"}} {
		if _, err := store.RecordSet(accounts.SetRecord{Ladder: "remix", MatchID: uint64(i + 1), BootID: "b",
			Status: accounts.SetRated, WinnerID: ids[pair[0]], RulesetID: "remix-1v1-vanilla",
			Players: []accounts.SetPlayer{
				{AccountID: ids[pair[0]], Slot: 1, GamesWon: 2, Region: regions[pair[0]]},
				{AccountID: ids[pair[1]], Slot: 2, Region: regions[pair[1]]}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		store.SeedRating(ids[n], "remix", "remix-1v1-vanilla", rating.Rating{Rating: ratings[n], Deviation: 60, Volatility: 0.06})
	}

	type row struct {
		Rank      int
		WorldRank int
		Username  string
		Region    string
	}
	var board struct{ Players []row }
	getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=remix", &board)
	if len(board.Players) != 4 || board.Players[0].Username != "Yank" || board.Players[0].Region != "na" {
		t.Fatalf("worldwide = %+v", board.Players)
	}
	board.Players = nil
	getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=remix&region=eu", &board)
	if len(board.Players) != 2 || board.Players[0] != (row{1, 2, "Euro", "eu"}) || board.Players[1] != (row{2, 3, "Brit", "eu"}) {
		t.Fatalf("Europe = %+v", board.Players)
	}
	if code := getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=remix&region=mars", nil); code != 404 {
		t.Fatalf("unknown region = %d", code)
	}

	var player struct {
		Region    string
		Standings []struct {
			Rank       int
			RegionRank int
		}
	}
	getJSON(t, srv.URL+"/api/ranked/player?name=Brit", &player)
	if player.Region != "eu" || len(player.Standings) != 1 || player.Standings[0].Rank != 3 || player.Standings[0].RegionRank != 2 {
		t.Fatalf("Brit = %+v", player)
	}
	var overview struct{ Regions []struct{ ID, Name string } }
	getJSON(t, srv.URL+"/api/ranked/overview", &overview)
	if len(overview.Regions) != 5 || overview.Regions[2].ID != "eu" {
		t.Fatalf("overview regions = %+v", overview.Regions)
	}
}

func TestLeaderboardCharacters(t *testing.T) {
	srv, store := newTestServer(t)
	ace, _ := store.Register("Ace", "password123")
	foe, _ := store.Register("Foe", "password123")
	// Ace: Falco in 3 sets (once with Fox too), then Link, Mario and Kirby once each
	sets := [][]int{{29}, {29, 1}, {29}, {5}, {0}, {8}}
	for i, chars := range sets {
		games := []string{}
		for _, c := range chars {
			games = append(games, fmt.Sprintf(`{"players":[{"slot":1,"character":%d},{"slot":2,"character":3}]}`, c))
		}
		if _, err := store.RecordSet(accounts.SetRecord{Ladder: "remix", MatchID: uint64(i + 1), BootID: "b",
			Status: accounts.SetRated, WinnerID: ace.ID, RulesetID: "remix-1v1-vanilla",
			ResultJSON: `{"Ace":{"games":[` + strings.Join(games, ",") + `]}}`,
			Players:    []accounts.SetPlayer{{AccountID: ace.ID, Slot: 1, GamesWon: 2}, {AccountID: foe.ID, Slot: 2}}}); err != nil {
			t.Fatal(err)
		}
	}
	type share struct{ ID, Sets, Percent int }
	var board struct {
		Players []struct {
			Username       string
			Characters     []share
			MoreCharacters int
		}
	}
	getJSON(t, srv.URL+"/api/ranked/leaderboard?ladder=remix", &board)
	var aceRow, foeRow = board.Players[0], board.Players[1]
	if aceRow.Username != "Ace" {
		aceRow, foeRow = foeRow, aceRow
	}
	if len(aceRow.Characters) != 3 || aceRow.Characters[0] != (share{29, 3, 50}) || aceRow.MoreCharacters != 2 {
		t.Fatalf("Ace = %+v", aceRow)
	}
	if len(foeRow.Characters) != 1 || foeRow.Characters[0] != (share{3, 6, 100}) || foeRow.MoreCharacters != 0 {
		t.Fatalf("Foe = %+v", foeRow)
	}
	// the player page lists every character of each roster, once
	var player struct {
		Standings []struct {
			Ruleset    string
			Characters []share
		}
	}
	getJSON(t, srv.URL+"/api/ranked/player?name=Ace", &player)
	if len(player.Standings) != 1 || len(player.Standings[0].Characters) != 5 {
		t.Fatalf("Ace's standings = %+v", player.Standings)
	}
	seen := map[int]bool{}
	for _, c := range player.Standings[0].Characters {
		if seen[c.ID] {
			t.Fatalf("character %d listed twice", c.ID)
		}
		seen[c.ID] = true
	}
}

func TestDiscordConfigAndModeratorUnlink(t *testing.T) {
	srv, store := newTestServer(t)
	// without Discord the config endpoint is a 404 (RMG-K hides the button)
	if code := getJSON(t, srv.URL+"/api/auth/discord", nil); code != 404 {
		t.Fatalf("discord config without Discord = %d", code)
	}
	ace, _ := store.Register("Ace", "password123")
	if err := store.LinkDiscord(ace.ID, "123", "ace_d", "Ace D"); err != nil {
		t.Fatal(err)
	}
	// nothing about Discord is public
	var player map[string]any
	getJSON(t, srv.URL+"/api/ranked/player?name=Ace", &player)
	for key := range player {
		if strings.Contains(strings.ToLower(key), "discord") {
			t.Fatalf("player page exposes %q", key)
		}
	}
}
