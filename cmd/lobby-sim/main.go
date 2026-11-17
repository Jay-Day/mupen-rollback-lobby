// Command lobby-sim keeps a handful of made-up players online in a local
// lobby, so the moderator site's Live page has something to show: two rooms
// (one in a running, broadcast match with a spectator watching), lobby, room
// and spectator chat, and two players sharing an address. It runs until
// stopped (Ctrl+C) or for --duration.
//
//	go run ./cmd/lobby-sim --url ws://127.0.0.1:8080/ws
//
// The lobby only believes the TCP address, so with --spread (the default,
// for a loopback URL) each player connects from its own loopback address
// (127.0.0.10, 127.0.0.11, ...): all of 127.0.0.0/8 is this machine, and the
// lobby's connect limits leave loopback alone. A player that's disconnected
// (kicked, banned) says so and tries once to come back after --rejoin,
// printing whether the lobby let it in. --flood adds a player who now and
// then sends too much too fast (logged as flood-blocked chat).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

type envelope struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// the players, their addresses and what they do
type role int

const (
	idle role = iota
	hostPlaying
	guestPlaying
	hostWaiting
	guestWaiting
	spectator
	flooder
)

type sim struct {
	name string
	ip   string // loopback source address with --spread
	role role
}

var cast = []sim{
	{"KirbyKing", "127.0.0.10", hostPlaying},
	{"FoxMcLoud", "127.0.0.11", guestPlaying},
	{"PikaPal", "127.0.0.12", guestPlaying},
	{"RainbowRoad", "127.0.0.13", hostWaiting},
	{"BlueShell", "127.0.0.14", guestWaiting},
	{"YoshiEgg", "127.0.0.15", spectator},
	{"DKJungle", "127.0.0.16", idle},
	{"LuigiTime", "127.0.0.30", idle},
	{"MarioTime", "127.0.0.30", idle}, // same address as LuigiTime: siblings
	{"Zoe64", "127.0.0.17", idle},
}

var floodCast = sim{"BIueShell", "127.0.0.40", flooder} // a look-alike of BlueShell

var lobbyLines = []string{
	"anyone up for Smash Remix?", "who wants some Kart?", "rainbow road is cursed lol", "lf 2v2 teammate",
	"rollback feels smooth tonight", "need one more for Mario Party 2", "brb", "gg everyone",
}
var roomLines = []string{"glhf", "gg", "one more?", "nice edgeguard", "Dream Land pls", "that was close", "lag on my end sorry"}
var spectateLines = []string{"that recovery!", "close one", "clean combo", "GG", "what a comeback"}

type player struct {
	sim
	url    string
	spread bool
	rejoin time.Duration
	rng    *rand.Rand

	mu      sync.Mutex
	conn    *websocket.Conn
	roomID  atomic.Uint64
	seated  atomic.Int32 // players in the host's room
	started atomic.Bool
}

// shared state the players read: room ids and the match being broadcast
type world struct {
	mu          sync.Mutex
	playingRoom uint64
	waitingRoom uint64
	liveMatch   uint64
	cond        *sync.Cond
}

func newWorld() *world {
	w := &world{}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *world) wait(get func() uint64) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	for get() == 0 {
		w.cond.Wait()
	}
	return get()
}

func (w *world) set(do func()) {
	w.mu.Lock()
	do()
	w.mu.Unlock()
	w.cond.Broadcast()
}

func main() {
	var (
		lobbyURL  = flag.String("url", "ws://127.0.0.1:8080/ws", "lobby WebSocket URL")
		count     = flag.Int("players", len(cast), fmt.Sprintf("how many of the cast to run (3-%d)", len(cast)))
		spread    = flag.Bool("spread", true, "connect each player from its own loopback address (loopback URLs only)")
		flood     = flag.Bool("flood", false, "add a player who floods the lobby chat now and then")
		duration  = flag.Duration("duration", 0, "stop after this long (0 = until Ctrl+C)")
		rejoin    = flag.Duration("rejoin", 10*time.Second, "after being disconnected, try once to come back after this long")
		chatEvery = flag.Duration("chat-every", 20*time.Second, "about how often someone says something")
		version   = flag.String("client-version", "vdev-sim", "RMG-K version reported in HELLO")
	)
	flag.Parse()
	if *count < 3 {
		*count = 3
	}
	if *count > len(cast) {
		*count = len(cast)
	}
	log.SetFlags(log.Ltime)
	if *spread && !loopbackURL(*lobbyURL) {
		log.Printf("--spread needs a loopback lobby URL; every player connects from this machine's address")
		*spread = false
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, *duration)
		defer stop()
	}

	players := append([]sim(nil), cast[:*count]...)
	if *flood {
		players = append(players, floodCast)
	}
	w := newWorld()
	var wg sync.WaitGroup
	for i, s := range players {
		p := &player{sim: s, url: *lobbyURL, spread: *spread, rejoin: *rejoin, rng: rand.New(rand.NewSource(int64(i) + 7))}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.run(ctx, w, *version, *chatEvery)
		}()
		time.Sleep(150 * time.Millisecond)
	}
	<-ctx.Done()
	log.Printf("stopping")
	wg.Wait()
}

func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

func (p *player) logf(format string, args ...any) {
	log.Printf("[%s] %s", p.name, fmt.Sprintf(format, args...))
}

func (p *player) send(ctx context.Context, msgType string, data any) {
	payload, _ := json.Marshal(data)
	raw, _ := json.Marshal(envelope{Type: msgType, Data: payload})
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = conn.Write(wctx, websocket.MessageText, raw)
}

func (p *player) dial(ctx context.Context, version string) error {
	opts := &websocket.DialOptions{}
	if p.spread {
		dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(p.ip)}, Timeout: 10 * time.Second}
		opts.HTTPClient = &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dctx, p.url, opts)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("%s (HTTP %d)", err, resp.StatusCode)
		}
		return err
	}
	conn.SetReadLimit(4 << 20)
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
	p.send(ctx, "HELLO", map[string]any{
		"username": p.name, "clientVersion": version, "romHashes": []string{},
		"iceSupported": true, "lateJoin": true, "saveData": true, "connection": "lan",
	})
	return nil
}

// run plays the role, and once more after a disconnect.
func (p *player) run(ctx context.Context, w *world, version string, chatEvery time.Duration) {
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.rejoin):
			}
			p.logf("trying to come back")
		}
		if err := p.dial(ctx, version); err != nil {
			p.logf("can't connect: %v", err)
			continue
		}
		reason := p.session(ctx, w, chatEvery)
		if ctx.Err() != nil {
			p.mu.Lock()
			p.conn.Close(websocket.StatusNormalClosure, "")
			p.mu.Unlock()
			return
		}
		p.logf("disconnected: %s", reason)
	}
}

// session runs one connection until it closes; it returns what the lobby
// said last.
func (p *player) session(ctx context.Context, w *world, chatEvery time.Duration) string {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go p.heartbeat(sctx)

	notice := "connection closed"
	for {
		p.mu.Lock()
		conn := p.conn
		p.mu.Unlock()
		_, raw, err := conn.Read(sctx)
		if err != nil {
			return notice
		}
		var env envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		switch env.Type {
		case "HELLO_OK":
			var ok struct {
				UserID     uint64 `json:"userId"`
				ObservedIP string `json:"observedIp"`
			}
			json.Unmarshal(env.Data, &ok)
			p.logf("online as user %d from %s", ok.UserID, ok.ObservedIP)
			go p.act(sctx, w, chatEvery)
		case "HELLO_FAIL":
			p.logf("lobby refused the login: %s", env.Data)
		case "MOD_NOTICE":
			var n struct {
				Text     string `json:"text"`
				Severity string `json:"severity"`
			}
			json.Unmarshal(env.Data, &n)
			p.logf("lobby says (%s): %s", n.Severity, n.Text)
			notice = n.Text
		case "ROOM_CREATED":
			var r struct {
				RoomID uint64 `json:"roomId"`
			}
			json.Unmarshal(env.Data, &r)
			p.roomID.Store(r.RoomID)
			p.logf("created room %d", r.RoomID)
			if p.role == hostPlaying {
				w.set(func() { w.playingRoom = r.RoomID })
			} else {
				w.set(func() { w.waitingRoom = r.RoomID })
			}
		case "ROOM_JOIN_OK":
			var r struct {
				ID uint64 `json:"id"`
			}
			json.Unmarshal(env.Data, &r)
			p.roomID.Store(r.ID)
			p.logf("joined room %d", r.ID)
		case "ROOM_STATE":
			var r struct {
				Players []struct{} `json:"players"`
			}
			json.Unmarshal(env.Data, &r)
			p.seated.Store(int32(len(r.Players)))
			// the host starts once both guests sit down
			if p.role == hostPlaying && len(r.Players) >= 3 && p.started.CompareAndSwap(false, true) {
				p.send(sctx, "ROOM_START", map[string]any{})
			}
		case "ROOM_LEFT":
			p.logf("left the room: %s", env.Data)
			p.roomID.Store(0)
		case "MATCH_BEGIN":
			var m struct {
				MatchID uint64 `json:"matchId"`
			}
			json.Unmarshal(env.Data, &m)
			p.logf("match %d started", m.MatchID)
			p.send(sctx, "MATCH_CONNECTED", map[string]any{"matchId": m.MatchID})
			if p.role == hostPlaying {
				p.send(sctx, "BROADCAST_BEGIN", map[string]any{"matchId": m.MatchID, "manifest": ""})
				w.set(func() { w.liveMatch = m.MatchID })
			}
		case "SPECTATE_BEGIN":
			p.logf("watching the live replay")
		}
	}
}

func (p *player) heartbeat(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.send(ctx, "HEARTBEAT", map[string]any{"pingToServer": 20 + p.rng.Intn(60)})
		}
	}
}

func (p *player) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (p *player) chat(ctx context.Context, channel string, lines []string) {
	p.send(ctx, "CHAT_SEND", map[string]any{"channel": channel, "message": lines[p.rng.Intn(len(lines))]})
}

// act does the player's part, then chats now and then.
func (p *player) act(ctx context.Context, w *world, chatEvery time.Duration) {
	p.sleep(ctx, 500*time.Millisecond)
	switch p.role {
	case hostPlaying:
		p.send(ctx, "ROOM_CREATE", map[string]any{"name": "Remix FFA", "rom": map[string]any{"name": "Smash Remix",
			"md5": "B0D5FA1D1A0A6B9C8E1F2D3C4B5A6978"}, "maxPlayers": 4})
		// ROOM_STATE starts it once both guests are in; with only one after
		// 15 seconds, start anyway
		p.sleep(ctx, 3*time.Second)
		p.chat(ctx, "room", []string{"starting once everyone's in"})
		if p.sleep(ctx, 12*time.Second) && p.seated.Load() >= 2 && p.started.CompareAndSwap(false, true) {
			p.send(ctx, "ROOM_START", map[string]any{})
		}
	case guestPlaying:
		room := w.wait(func() uint64 { return w.playingRoom })
		p.sleep(ctx, time.Duration(p.rng.Intn(1500))*time.Millisecond)
		p.send(ctx, "ROOM_JOIN", map[string]any{"roomId": room})
	case hostWaiting:
		p.send(ctx, "ROOM_CREATE", map[string]any{"name": "Kart night", "rom": map[string]any{"name": "Mario Kart 64",
			"md5": "3A67D9986F54EB282924FCA4CB5F6DFF"}, "maxPlayers": 4, "password": "kart"})
	case guestWaiting:
		room := w.wait(func() uint64 { return w.waitingRoom })
		p.send(ctx, "ROOM_JOIN", map[string]any{"roomId": room, "password": "kart"})
	case spectator:
		match := w.wait(func() uint64 { return w.liveMatch })
		p.sleep(ctx, time.Second)
		p.send(ctx, "SPECTATE_START", map[string]any{"matchId": match})
	}
	if !p.sleep(ctx, 2*time.Second) {
		return
	}
	p.chat(ctx, "lobby", lobbyLines)
	for p.sleep(ctx, chatEvery/2+time.Duration(p.rng.Int63n(int64(chatEvery)))) {
		switch {
		case p.role == flooder:
			// more than the lobby's 5 lines in 5 seconds: the rest are dropped
			for i := 0; i < 8; i++ {
				p.send(ctx, "CHAT_SEND", map[string]any{"channel": "lobby", "message": "FREE SKINS AT my-totally-real-site"})
			}
		case p.role == spectator:
			p.chat(ctx, "spectate", spectateLines)
		case p.roomID.Load() != 0:
			if p.rng.Intn(3) == 0 {
				p.chat(ctx, "lobby", lobbyLines)
			} else {
				p.chat(ctx, "room", roomLines)
			}
		default:
			p.chat(ctx, "lobby", lobbyLines)
		}
	}
}
