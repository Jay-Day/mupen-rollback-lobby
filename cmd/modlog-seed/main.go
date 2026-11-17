// Command modlog-seed fills a moderators' log (internal/modlog) with a few
// made-up days of lobby life, for trying the moderator site and taking
// screenshots: about 40 players (some sharing an IP, a couple of obvious
// alts, a look-alike name, IPv4 and IPv6), lobby, room and spectator chat
// with a few filter-blocked, muted and flood-dropped lines, about 60 games
// with late joiners and early leavers, and some moderator actions in the
// audit log.
//
// Every address is from the documentation ranges (192.0.2.0/24,
// 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32), so none is a real
// person's.
//
//	go run ./cmd/modlog-seed --db modlog.db [--bans bans.json]
//
// --bans writes a bans.json with two bans (it replaces the file). The log
// must not be in use by a running lobby.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
)

type person struct {
	name    string
	ips     []string // the first is home; others are travel/phone
	country string
	version string
	chatty  int // relative chance of talking
	alt     bool
}

// shared home IPs: siblings, a dorm, a LAN group
var people = []person{
	{name: "KirbyKing", ips: []string{"203.0.113.10"}, country: "US", chatty: 5},
	{name: "SamusMains", ips: []string{"203.0.113.11", "2001:db8:44::2"}, country: "US", chatty: 3},
	{name: "FoxMcLoud", ips: []string{"198.51.100.20"}, country: "CA", chatty: 4},
	{name: "PikaPal", ips: []string{"198.51.100.21"}, country: "BR", chatty: 4},
	{name: "YoshiEgg", ips: []string{"2001:db8:1::10"}, country: "SE", chatty: 2},
	{name: "LuigiTime", ips: []string{"203.0.113.30"}, country: "US", chatty: 3},
	{name: "MarioTime", ips: []string{"203.0.113.30"}, country: "US", chatty: 2}, // sibling, same IP
	{name: "DKJungle", ips: []string{"198.51.100.40"}, country: "MX", chatty: 3},
	{name: "NessPK", ips: []string{"2001:db8:2::7"}, country: "JP", chatty: 2},
	{name: "NessPSI", ips: []string{"2001:db8:2::8"}, country: "JP", chatty: 1},
	{name: "JigglyPuff", ips: []string{"192.0.2.15"}, country: "GB", chatty: 3},
	{name: "FalconPunch", ips: []string{"192.0.2.16"}, country: "GB", chatty: 4},
	{name: "Zoe64", ips: []string{"192.0.2.17"}, country: "FR", chatty: 2},
	{name: "WarioWare", ips: []string{"198.51.100.50"}, country: "DE", chatty: 2},
	{name: "Bowser64", ips: []string{"198.51.100.51"}, country: "DE", chatty: 3},
	{name: "ToadRacer", ips: []string{"203.0.113.60"}, country: "AU", chatty: 2},
	{name: "KoopaShell", ips: []string{"203.0.113.61"}, country: "AU", chatty: 2},
	{name: "PeachyKeen", ips: []string{"2001:db8:3::1"}, country: "US", chatty: 3},
	{name: "RainbowRoad", ips: []string{"198.51.100.70"}, country: "US", chatty: 4},
	{name: "RainbowRoad2", ips: []string{"198.51.100.70"}, country: "US", chatty: 5, alt: true}, // alt
	{name: "BananaPeel", ips: []string{"198.51.100.71"}, country: "ES", chatty: 2},
	{name: "BlueShell", ips: []string{"192.0.2.80"}, country: "NL", chatty: 3},
	{name: "BIueShell", ips: []string{"192.0.2.80", "192.0.2.81"}, country: "NL", chatty: 4, alt: true}, // look-alike alt
	{name: "GoldenEyeGuy", ips: []string{"203.0.113.90"}, country: "US", chatty: 2},
	{name: "OddjobMain", ips: []string{"203.0.113.90"}, country: "US", chatty: 2}, // same LAN
	{name: "BanjoBear", ips: []string{"2001:db8:4::22"}, country: "CA", chatty: 2},
	{name: "ConkerSq", ips: []string{"2001:db8:4::23"}, country: "CA", chatty: 1},
	{name: "PeppyHare", ips: []string{"192.0.2.100"}, country: "US", chatty: 2},
	{name: "SlippyToad", ips: []string{"192.0.2.100"}, country: "US", chatty: 1}, // dorm
	{name: "FalcoBird", ips: []string{"192.0.2.100"}, country: "US", chatty: 3},  // dorm
	{name: "MewtwoMagic", ips: []string{"198.51.100.110"}, country: "BR", chatty: 2},
	{name: "MarthMaster", ips: []string{"198.51.100.111"}, country: "CL", chatty: 2},
	{name: "SheikSpeed", ips: []string{"2001:db8:5::5"}, country: "DE", chatty: 1},
	{name: "GanonGrab", ips: []string{"203.0.113.120"}, country: "US", chatty: 3},
	{name: "DarkSamus", ips: []string{"203.0.113.121"}, country: "IT", chatty: 2},
	{name: "MetalMario", ips: []string{"192.0.2.130"}, country: "US", chatty: 2},
	{name: "YoungLink", ips: []string{"192.0.2.131", "2001:db8:6::9"}, country: "PL", chatty: 3},
	{name: "DrMario", ips: []string{"198.51.100.140"}, country: "US", chatty: 1},
	{name: "PolygonMan", ips: []string{"198.51.100.141"}, country: "KR", chatty: 1},
	{name: "FZeroFan", ips: []string{"203.0.113.150"}, country: "US", chatty: 2},
}

var lobbyLines = []string{
	"anyone up for Smash Remix?", "gg everyone", "who wants some Kart?", "rainbow road is cursed lol",
	"lf 2v2 teammate", "is anyone else lagging?", "what version is everyone on?", "brb grabbing food",
	"hosting Remix FFA, come join", "first time on the lobby, hi all", "anyone play GoldenEye here?",
	"rollback feels really smooth tonight", "who's the best Pikachu on here", "need one more for Mario Party 2",
	"quick match is quiet today", "ty for the games", "good night all",
	"did the new version fix the audio thing?", "Diddy Kong Racing anyone?", "Kirby down-B is so annoying",
	"that last game was insane", "anyone from EU on?", "is the tournament still on Saturday?",
}

var roomLines = []string{
	"glhf", "gg", "one more?", "rematch?", "lag on my end sorry", "nice edgeguard", "which stage?",
	"Dream Land pls", "no items?", "3 stock or 4?", "ready", "brb 1 sec", "gg wp", "that was close",
	"your Ness is scary", "how did you survive that", "ok last one", "I'm switching to Fox",
	"Koopa Troopa Beach next", "no blue shells please", "lol that shell", "gg that was fun",
}

var spectateLines = []string{
	"that recovery!", "close one", "go go go", "clean combo", "this is so close", "GG", "what a comeback",
	"nice read", "hype", "how is that legal lol",
}

// mild trash talk the chat filter could plausibly block
var blockedLines = []string{"you're garbage at this", "uninstall the game noob", "ur trash lol", "worst player here"}

type game struct {
	name  string
	md5   string
	seats int
}

var games = []game{
	{name: "Smash Remix", md5: "B0D5FA1D1A0A6B9C8E1F2D3C4B5A6978", seats: 4},
	{name: "Smash Remix", md5: "B0D5FA1D1A0A6B9C8E1F2D3C4B5A6978", seats: 2},
	{name: "Mario Kart 64", md5: "3A67D9986F54EB282924FCA4CB5F6DFF", seats: 4},
	{name: "Super Smash Bros.", md5: "F7C52568A31AADF26E14DC2B6416B2ED", seats: 4},
	{name: "GoldenEye 007", md5: "70C525880240C1E838B8B1BE35666C3B", seats: 4},
	{name: "Mario Party 2", md5: "04840612A35ECE222AFDB2DFBF926409", seats: 4},
	{name: "Diddy Kong Racing", md5: "4F0E07F0EEAC7E5D7CE3A75461888D03", seats: 4},
	{name: "F-Zero X", md5: "753437D0D8ADA1D12F3F9CF0F0A5171F", seats: 4},
}

var roomNames = map[string][]string{
	"Smash Remix":       {"Remix FFA", "Remix 1v1s", "chill Remix", "Remix doubles", "no items FD"},
	"Mario Kart 64":     {"Kart night", "150cc only", "battle mode", "Kart grand prix"},
	"Super Smash Bros.": {"vanilla 64", "Smash 64 FFA", "old school"},
	"GoldenEye 007":     {"Facility slappers", "GoldenEye 4p", "license to kill"},
	"Mario Party 2":     {"Mario Party", "party time"},
	"Diddy Kong Racing": {"DKR races", "Diddy cups"},
	"F-Zero X":          {"F-Zero GP", "Big Blue"},
}

// action is one thing to log at a time.
type action struct {
	at time.Time
	do func()
}

type session struct {
	who        int
	ip         string
	start, end time.Time
	connID     int64
}

func main() {
	var (
		dbPath    = flag.String("db", "modlog.db", "moderators' log to fill (created if missing)")
		bansPath  = flag.String("bans", "", "write a bans.json with two bans here (replacing it)")
		days      = flag.Int("days", 4, "days of history")
		gameCount = flag.Int("games", 60, "games to play")
		seed      = flag.Int64("seed", 64, "random seed")
	)
	flag.Parse()
	rng := rand.New(rand.NewSource(*seed))

	l, err := modlog.Open(*dbPath)
	if err != nil {
		log.Fatalf("modlog: %v", err)
	}
	l.SetRetention(func() modlog.Retention { return modlog.Retention{} }) // keep the history while seeding
	end := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	start := end.Add(-time.Duration(*days) * 24 * time.Hour)
	clock := start
	l.SetClock(func() time.Time { return clock })

	versions := []string{"v0.9.16", "v0.9.16", "v0.9.16", "v0.9.15", "vdev-2412"}
	for i := range people {
		people[i].version = versions[rng.Intn(len(versions))]
	}

	// evening-weighted random times
	randomTime := func() time.Time {
		day := rng.Intn(*days)
		hour := 16 + rng.Intn(9) // 16:00-00:59 mostly
		if rng.Intn(4) == 0 {
			hour = rng.Intn(24)
		}
		t := start.Add(time.Duration(day)*24*time.Hour + time.Duration(hour)*time.Hour + time.Duration(rng.Intn(3600))*time.Second)
		if t.After(end.Add(-2 * time.Hour)) {
			t = end.Add(-2*time.Hour - time.Duration(rng.Intn(7200))*time.Second)
		}
		return t
	}

	sessions := map[int][]*session{}
	// cover makes sure person i is online over [from, to]
	cover := func(i int, from, to time.Time) *session {
		for _, s := range sessions[i] {
			if !s.start.After(to) && !s.end.Before(from) {
				if from.Before(s.start) {
					s.start = from
				}
				if to.After(s.end) {
					s.end = to
				}
				return s
			}
		}
		ip := people[i].ips[0]
		// the first visit is from home; later ones sometimes from elsewhere
		if len(people[i].ips) > 1 && len(sessions[i]) > 0 && rng.Intn(3) == 0 {
			ip = people[i].ips[1+rng.Intn(len(people[i].ips)-1)]
		}
		s := &session{who: i, ip: ip, start: from.Add(-time.Duration(5+rng.Intn(40)) * time.Minute),
			end: to.Add(time.Duration(5+rng.Intn(50)) * time.Minute)}
		if s.end.After(end) {
			s.end = end.Add(-time.Duration(rng.Intn(300)) * time.Second)
		}
		sessions[i] = append(sessions[i], s)
		return s
	}

	var actions []action
	add := func(at time.Time, do func()) { actions = append(actions, action{at: at, do: do}) }
	ids := struct{ room, match uint64 }{room: 400, match: 900}
	nameOf := func(i int) string { return people[i].name }
	userID := func(i int) uint64 { return uint64(10000 + i) }

	// games
	endReasons := []string{modlog.EndFinished, modlog.EndFinished, modlog.EndFinished, modlog.EndFinished,
		modlog.EndFinished, modlog.EndHostLeft, modlog.EndHostDisconnected}
	for g := 0; g < *gameCount; g++ {
		kind := games[rng.Intn(len(games))]
		at := randomTime()
		dur := time.Duration(6+rng.Intn(30)) * time.Minute
		players := 2 + rng.Intn(kind.seats-1)
		quick := kind.seats == 2 && rng.Intn(2) == 0
		picked := rng.Perm(len(people))
		seat := picked[:players]
		ids.room++
		ids.match++
		roomID, matchID := ids.room, ids.match
		names := roomNames[kind.name]
		roomName := names[rng.Intn(len(names))]
		if quick {
			roomName = "Quick Match: " + nameOf(seat[0]) + " vs " + nameOf(seat[1])
		}
		host := seat[0]
		late := -1
		if len(seat) >= 3 && rng.Intn(3) == 0 {
			late = seat[len(seat)-1]
		}
		leaver := -1
		if len(seat) >= 2 && rng.Intn(4) == 0 {
			leaver = seat[1+rng.Intn(len(seat)-1)]
			if leaver == late {
				leaver = -1
			}
		}
		reason := endReasons[rng.Intn(len(endReasons))]
		if quick && reason != modlog.EndFinished {
			reason = modlog.EndPlayerLeft
		}
		punch := rng.Intn(15) == 0
		if punch {
			dur = time.Duration(40+rng.Intn(60)) * time.Second
		}
		ss := map[int]*session{}
		for _, i := range seat {
			ss[i] = cover(i, at.Add(-3*time.Minute), at.Add(dur+2*time.Minute))
		}

		created := at.Add(-time.Duration(60+rng.Intn(120)) * time.Second)
		add(created, func() {
			text := fmt.Sprintf("%s created room %q (%s)", nameOf(host), roomName, kind.name)
			if quick {
				text = fmt.Sprintf("Quick match: room %q (%s) for %s, %s", roomName, kind.name, nameOf(seat[0]), nameOf(seat[1]))
			}
			l.Event(modlog.Event{Type: modlog.EventRoomCreated, RoomID: roomID, Name: nameOf(host), IP: ss[host].ip, Text: text})
		})
		var gamePlayers []modlog.Player
		for n, i := range seat {
			who := i
			p := modlog.Player{UserID: userID(i), Name: nameOf(i), IP: ss[i].ip, Slot: n + 1}
			if i == late {
				continue
			}
			if n > 0 && !quick {
				add(created.Add(time.Duration(10+n*15)*time.Second), func() {
					l.Event(modlog.Event{Type: modlog.EventRoomJoined, RoomID: roomID, Name: nameOf(who), IP: ss[who].ip,
						Text: nameOf(who) + " joined the room"})
				})
			}
			gamePlayers = append(gamePlayers, p)
		}
		add(at, func() {
			l.GameStarted(modlog.Game{MatchID: matchID, RoomID: roomID, RoomName: roomName, Game: kind.name,
				RomMD5: kind.md5, Players: gamePlayers})
			if !punch {
				l.GameConnected(matchID)
			}
		})
		if punch {
			add(at.Add(20*time.Second), func() { l.GamePunchFailed(matchID, nameOf(seat[1]), nameOf(host)) })
		}
		if late >= 0 {
			joinAt := at.Add(dur / 3)
			lateSlot := len(seat)
			add(joinAt.Add(-40*time.Second), func() {
				l.Event(modlog.Event{Type: modlog.EventRoomJoined, RoomID: roomID, Name: nameOf(late), IP: ss[late].ip,
					Text: nameOf(late) + " joined the room"})
			})
			add(joinAt, func() {
				l.GamePlayerJoined(matchID, modlog.Player{UserID: userID(late), Name: nameOf(late), IP: ss[late].ip, Slot: lateSlot})
			})
		}
		if leaver >= 0 && !punch {
			leaveAt := at.Add(dur * 2 / 3)
			how := []string{"dropped", "disconnect", "left"}[rng.Intn(3)]
			add(leaveAt, func() { l.GamePlayerLeft(matchID, userID(leaver), how) })
		}
		// room chat and spectators
		lines := 2 + rng.Intn(8)
		for c := 0; c < lines && !punch; c++ {
			who := seat[rng.Intn(len(seat))]
			t := at.Add(-90*time.Second + time.Duration(rng.Int63n(int64(dur+3*time.Minute))))
			mid := uint64(0)
			if t.After(at) && t.Before(at.Add(dur)) {
				mid = matchID
			}
			text := roomLines[rng.Intn(len(roomLines))]
			add(t, func() {
				l.Chat(modlog.ChatMessage{Channel: "room", RoomID: roomID, RoomName: roomName, MatchID: mid,
					UserID: userID(who), Name: nameOf(who), IP: ss[who].ip, Text: text})
			})
		}
		if rng.Intn(3) == 0 && !punch {
			viewers := 1 + rng.Intn(4)
			add(at.Add(dur/4), func() { l.GameSpectators(matchID, viewers) })
			for c := 0; c < 1+rng.Intn(4); c++ {
				who := picked[len(picked)-1-c]
				s := cover(who, at.Add(dur/4), at.Add(dur/2))
				text := spectateLines[rng.Intn(len(spectateLines))]
				add(at.Add(dur/4+time.Duration(c+1)*45*time.Second), func() {
					l.Chat(modlog.ChatMessage{Channel: "spectate", RoomID: roomID, RoomName: roomName, MatchID: matchID,
						UserID: userID(who), Name: nameOf(who), IP: s.ip, Text: text})
				})
			}
		}
		endAt := at.Add(dur)
		if g == *gameCount/2 {
			reason = modlog.EndModerator
			add(endAt.Add(-time.Second), func() {
				l.Event(modlog.Event{Type: modlog.EventModeration, RoomID: roomID,
					Text: fmt.Sprintf("Wren: Closed room #%d %q: room name", roomID, roomName)})
				l.Audit(modlog.AuditEntry{Admin: "Wren", Action: "closeroom", Source: modlog.SourceSite, IP: "127.0.0.1",
					Detail: fmt.Sprintf("Closed room #%d %q: room name", roomID, roomName)})
			})
		}
		add(endAt, func() {
			l.GameEnded(matchID, reason)
			l.RoomClosed(roomID, roomName, reason, fmt.Sprintf("Room %q closed", roomName))
		})
	}

	// an alt that gets muted, keeps trying, then is banned elsewhere
	altIdx := indexOf("RainbowRoad2")
	altSession := cover(altIdx, end.Add(-30*time.Hour), end.Add(-29*time.Hour))
	muteAt := end.Add(-29*time.Hour - 30*time.Minute)
	add(muteAt.Add(-2*time.Minute), func() {
		l.Chat(modlog.ChatMessage{Channel: "lobby", Name: "RainbowRoad2", IP: altSession.ip, Text: "ur trash lol",
			Blocked: modlog.BlockedFilter})
	})
	add(muteAt, func() {
		detail := "Muted RainbowRoad2 (" + altSession.ip + ") until " + muteAt.Add(24*time.Hour).UTC().Format("2006-01-02 15:04 MST") +
			": rude in chat"
		l.Event(modlog.Event{Type: modlog.EventModeration, Name: "RainbowRoad2", IP: altSession.ip, Text: "Wren: " + detail})
		l.Audit(modlog.AuditEntry{Admin: "Wren", Action: "mute", Detail: detail, Source: modlog.SourceLobby, IP: "198.51.100.200"})
	})
	for k, text := range []string{"why can't I talk", "hello??", "this is so unfair"} {
		add(muteAt.Add(time.Duration(k+1)*time.Minute), func() {
			l.Chat(modlog.ChatMessage{Channel: "lobby", Name: "RainbowRoad2", IP: altSession.ip, Text: text,
				Blocked: modlog.BlockedMuted})
		})
	}
	// a look-alike floods the lobby and is banned
	spamIdx := indexOf("BIueShell")
	// from its second address, so the ban doesn't land on the player it imitates
	spam := &session{who: spamIdx, ip: people[spamIdx].ips[1], start: end.Add(-6 * time.Hour), end: end.Add(-5 * time.Hour)}
	sessions[spamIdx] = append(sessions[spamIdx], spam)
	spamAt := end.Add(-5*time.Hour - 20*time.Minute)
	for k := 0; k < 8; k++ {
		blocked := ""
		if k >= 5 {
			blocked = modlog.BlockedFlood
		}
		add(spamAt.Add(time.Duration(k)*700*time.Millisecond), func() {
			l.Chat(modlog.ChatMessage{Channel: "lobby", Name: "BIueShell", IP: spam.ip, Text: "FREE SKINS AT my-totally-real-site",
				Blocked: blocked})
		})
	}
	add(spamAt.Add(2*time.Minute), func() {
		detail := "Banned BIueShell (" + spam.ip + "): spam, impersonating BlueShell"
		l.Event(modlog.Event{Type: modlog.EventModeration, Name: "BIueShell", IP: spam.ip, Text: "Wren: " + detail})
		l.Audit(modlog.AuditEntry{Admin: "Wren", Action: "ban", Detail: detail, Source: modlog.SourceSite, IP: "127.0.0.1"})
	})
	// someone guessing the moderator password from the lobby
	add(end.Add(-3*time.Hour), func() {
		detail := "Banned 203.0.113.250: " + moderation.AutoBanReason + " (lobby)"
		l.Event(modlog.Event{Type: modlog.EventModeration, IP: "203.0.113.250", Text: moderation.AutoBanBy + ": " + detail})
		l.Audit(modlog.AuditEntry{Admin: moderation.AutoBanBy, Action: "ban", Detail: detail, Source: modlog.SourceAuto,
			IP: "203.0.113.250"})
	})
	for _, a := range []struct {
		at     time.Duration
		admin  string
		action string
		detail string
		source string
		ip     string
	}{
		{-50 * time.Hour, "Juniper", "sign-in", "Signed in to the moderator site", modlog.SourceSite, "127.0.0.1"},
		{-49 * time.Hour, "Juniper", "kick", "Kicked GanonGrab (203.0.113.120): afk in a full room", modlog.SourceSite, "127.0.0.1"},
		{-31 * time.Hour, "Wren", "sign-in", "Signed in to the lobby as WrenPlays", modlog.SourceLobby, "198.51.100.200"},
		{-5*time.Hour - 30*time.Minute, "Wren", "sign-in", "Signed in to the moderator site", modlog.SourceSite, "127.0.0.1"},
		{-2 * time.Hour, "Juniper", "unmute", "Unmuted 198.51.100.70", modlog.SourceSite, "127.0.0.1"},
	} {
		add(end.Add(a.at), func() {
			l.Audit(modlog.AuditEntry{Admin: a.admin, Action: a.action, Detail: a.detail, Source: a.source, IP: a.ip})
		})
	}

	// everyone shows up at least a couple of times
	for i := range people {
		for len(sessions[i]) < 2 {
			t := randomTime()
			cover(i, t, t.Add(time.Duration(10+rng.Intn(80))*time.Minute))
		}
	}

	// connections and lobby chat
	var allSessions []*session
	for i := range people {
		sort.Slice(sessions[i], func(a, b int) bool { return sessions[i][a].start.Before(sessions[i][b].start) })
		merged := sessions[i][:0]
		for _, s := range sessions[i] {
			// one connection at a time per name, as the lobby allows
			if n := len(merged); n > 0 && !s.start.After(merged[n-1].end) && s != spam {
				if s.end.After(merged[n-1].end) {
					merged[n-1].end = s.end
				}
				continue
			}
			merged = append(merged, s)
		}
		sessions[i] = merged
		allSessions = append(allSessions, merged...)
	}
	for _, s := range allSessions {
		p := people[s.who]
		add(s.start, func() {
			s.connID = l.ConnectionOpened(modlog.Conn{UserID: userID(s.who), IP: s.ip, Username: p.name,
				Version: p.version, Country: p.country})
		})
		add(s.end, func() { l.ConnectionClosed(s.connID) })
		lines := rng.Intn(1 + p.chatty*2)
		for c := 0; c < lines; c++ {
			t := s.start.Add(time.Duration(rng.Int63n(int64(s.end.Sub(s.start)))))
			text := lobbyLines[rng.Intn(len(lobbyLines))]
			blocked := ""
			if p.alt && rng.Intn(3) == 0 {
				text, blocked = blockedLines[rng.Intn(len(blockedLines))], modlog.BlockedFilter
			}
			add(t, func() {
				l.Chat(modlog.ChatMessage{Channel: "lobby", UserID: userID(s.who), Name: p.name, IP: s.ip, Text: text,
					Blocked: blocked})
			})
		}
	}

	sort.SliceStable(actions, func(a, b int) bool { return actions[a].at.Before(actions[b].at) })
	for n, a := range actions {
		clock = a.at
		a.do()
		if n%400 == 399 {
			l.Flush()
		}
	}
	clock = time.Now()
	l.Flush()
	if err := l.Close(); err != nil {
		log.Fatalf("close: %v", err)
	}

	if *bansPath != "" {
		now := time.Now()
		bans := []moderation.Sanction{
			{IP: spam.ip, Name: "BIueShell", Reason: "spam, impersonating BlueShell", By: "Wren", Set: spamAt.Add(2 * time.Minute)},
			{IP: "203.0.113.200", Reason: "known ban evader", By: "Juniper", Set: now.Add(-48 * time.Hour),
				Until: now.Add(5 * 24 * time.Hour)},
		}
		raw, _ := json.MarshalIndent(bans, "", "  ")
		if err := os.WriteFile(*bansPath, raw, 0o600); err != nil {
			log.Fatalf("bans: %v", err)
		}
	}
	fmt.Printf("seeded %s: %d people, %d connections, %d games, %d actions (%s to %s)\n", *dbPath, len(people),
		len(allSessions), *gameCount, len(actions), start.Format(time.DateTime), end.Format(time.DateTime))
}

func indexOf(name string) int {
	for i, p := range people {
		if p.name == name {
			return i
		}
	}
	panic("no " + name)
}
