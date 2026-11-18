package ws

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

var vanillaOnly = protocol.RoomUpdateGameOptionsRequest{
	Options: []protocol.GameOption{{ID: "vanilla-only", Label: "Vanilla only"}},
}

func (f *lateJoinFixture) gameOptionsUser(name string) *state.User {
	u := f.user(name, true)
	u.GameOptions = true
	return u
}

func (f *lateJoinFixture) setGameOptions(name string, req any) {
	f.t.Helper()
	f.send(name, protocol.TypeRoomUpdateGameOptions, req)
}

func optionIDs(opts []state.GameOption) string {
	ids := make([]string, 0, len(opts))
	for _, opt := range opts {
		ids = append(ids, opt.ID)
	}
	return strings.Join(ids, ",")
}

func roomListFrom(t *testing.T, envs []protocol.Envelope) protocol.RoomListPayload {
	t.Helper()
	env, ok := findEnvelope(envs, protocol.TypeRoomList)
	if !ok {
		t.Fatal("no ROOM_LIST")
	}
	var list protocol.RoomListPayload
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func TestHelloTakesTheGameOptionsCapability(t *testing.T) {
	for _, capable := range []bool{true, false} {
		h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
		c := &Client{hub: h, send: make(chan []byte, 64)}
		raw, _ := json.Marshal(map[string]any{
			"username": "mira", "clientVersion": "vdev-test", "gameOptions": capable,
		})
		if err := h.handleHello(c, protocol.Envelope{Type: protocol.TypeHello, Data: raw}); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if c.user.GameOptions != capable {
			t.Fatalf("HELLO gameOptions %v: user has %v", capable, c.user.GameOptions)
		}
	}
}

func TestGameOptionsAreOffUntilTheHostTurnsThemOn(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.gameOptionsUser("guest")
	f.openRoom(2, "host", "guest")
	// Connected, so the room list reaches them.
	for _, name := range []string{"host", "guest"} {
		f.h.clients[f.users[name].ID] = f.client(name)
	}

	initial := roomStatePayload(f.room)
	if initial.GameOptions != nil || !initial.GameOptionsOffered {
		t.Fatalf("a new room: options %+v, offered %v", initial.GameOptions, initial.GameOptionsOffered)
	}
	// Off means left out, for the room as a whole and in the room list.
	if raw, _ := json.Marshal(initial); strings.Contains(string(raw), `"gameOptions":[`) ||
		!strings.Contains(string(raw), `"gameOptionsOffered":true`) {
		t.Fatalf("room state JSON: %s", raw)
	}
	if raw, _ := json.Marshal(f.h.buildRoomListPayload()); strings.Contains(string(raw), "gameOptions") {
		t.Fatalf("room list JSON names game options: %s", raw)
	}

	f.setGameOptions("guest", vanillaOnly)
	if f.room.GameOptions != nil {
		t.Fatal("a non-host turned game options on")
	}

	f.drain("host")
	f.drain("guest")
	f.setGameOptions("host", vanillaOnly)
	if optionIDs(f.room.GameOptions) != "vanilla-only" {
		t.Fatalf("the host could not turn game options on: %+v", f.room.GameOptions)
	}
	for _, name := range []string{"host", "guest"} {
		envs := f.drain(name)
		st := roomStateFrom(t, envs)
		if len(st.GameOptions) != 1 || st.GameOptions[0] != vanillaOnly.Options[0] {
			t.Fatalf("%s room state game options = %+v", name, st.GameOptions)
		}
		for _, seat := range st.Players {
			if !seat.GameOptions {
				t.Fatalf("%s sees seat %+v without the capability", name, seat)
			}
		}
		list := roomListFrom(t, envs)
		if len(list.Rooms) != 1 || len(list.Rooms[0].GameOptions) != 1 ||
			list.Rooms[0].GameOptions[0] != vanillaOnly.Options[0] {
			t.Fatalf("%s room list = %+v", name, list.Rooms)
		}
	}
	if raw, _ := json.Marshal(roomStatePayload(f.room)); !strings.Contains(string(raw),
		`"gameOptions":[{"id":"vanilla-only","label":"Vanilla only"}]`) {
		t.Fatalf("room state JSON: %s", raw)
	}

	// The same list again changes nothing and isn't rebroadcast.
	f.setGameOptions("host", vanillaOnly)
	if _, ok := findEnvelope(f.drain("guest"), protocol.TypeRoomState); ok {
		t.Fatal("an unchanged list was rebroadcast")
	}

	// An empty list turns them all off.
	f.sendNextRoomListAtOnce()
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{}})
	if f.room.GameOptions != nil {
		t.Fatalf("the host could not turn game options off: %+v", f.room.GameOptions)
	}
	envs := f.drain("guest")
	if st := roomStateFrom(t, envs); st.GameOptions != nil {
		t.Fatalf("guest still sees %+v", st.GameOptions)
	}
	if list := roomListFrom(t, envs); list.Rooms[0].GameOptions != nil {
		t.Fatalf("room list still shows %+v", list.Rooms[0].GameOptions)
	}
}

func TestGameOptionsReplaceTheWholeList(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.openRoom(2, "host")

	two := protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		{ID: "vanilla-only", Label: "Vanilla only"}, {ID: "no-items", Label: "No items"},
	}}
	f.setGameOptions("host", two)
	if optionIDs(f.room.GameOptions) != "vanilla-only,no-items" {
		t.Fatalf("options = %+v", f.room.GameOptions)
	}
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		{ID: "no-items", Label: "No items"},
	}})
	if optionIDs(f.room.GameOptions) != "no-items" {
		t.Fatalf("options after replacing = %+v", f.room.GameOptions)
	}
	// A label change alone is a change.
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		{ID: "no-items", Label: "Items off"},
	}})
	if len(f.room.GameOptions) != 1 || f.room.GameOptions[0].Label != "Items off" {
		t.Fatalf("options after relabelling = %+v", f.room.GameOptions)
	}
}

func TestGameOptionsRejectABadList(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.openRoom(2, "host")
	f.setGameOptions("host", vanillaOnly)

	one := func(id, label string) protocol.RoomUpdateGameOptionsRequest {
		return protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{{ID: id, Label: label}}}
	}
	nine := protocol.RoomUpdateGameOptionsRequest{}
	for i := 0; i < gameOptionsMax+1; i++ {
		nine.Options = append(nine.Options, protocol.GameOption{ID: fmt.Sprintf("opt-%d", i), Label: "Option"})
	}
	bad := map[string]any{
		"nine options":    nine,
		"empty id":        one("", "Vanilla only"),
		"upper-case id":   one("Vanilla-only", "Vanilla only"),
		"underscore id":   one("vanilla_only", "Vanilla only"),
		"space in id":     one("vanilla only", "Vanilla only"),
		"non-ASCII id":    one("vanillé", "Vanilla only"),
		"33-byte id":      one(strings.Repeat("a", gameOptionIDMax+1), "Vanilla only"),
		"empty label":     one("vanilla-only", ""),
		"blank label":     one("vanilla-only", " \t "),
		"49-rune label":   one("vanilla-only", strings.Repeat("バ", gameOptionLabelMax+1)),
		"newline":         one("vanilla-only", "Vanilla\nonly"),
		"carriage return": one("vanilla-only", "Vanilla\ronly"),
		"tab":             one("vanilla-only", "Vanilla\tonly"),
		"bell":            one("vanilla-only", "Vanilla\aonly"),
		"C1 control":      one("vanilla-only", "Vanilla\u0085only"),
		"line separator":  one("vanilla-only", "Vanilla only"),
		"bidi override":   one("vanilla-only", "Vanilla ‮ylno"),
		"bad UTF-8":       json.RawMessage("{\"options\":[{\"id\":\"vanilla-only\",\"label\":\"Vanilla \xff\"}]}"),
		"duplicate id": protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
			{ID: "vanilla-only", Label: "Vanilla only"}, {ID: "vanilla-only", Label: "Again"},
		}},
		"options not a list": json.RawMessage(`{"options":"vanilla-only"}`),
		"id not a string":    json.RawMessage(`{"options":[{"id":7,"label":"Vanilla only"}]}`),
	}
	for name, req := range bad {
		f.setGameOptions("host", req)
		// Refused whole: the list the host had stays, nothing is fixed up.
		if optionIDs(f.room.GameOptions) != "vanilla-only" || f.room.GameOptions[0].Label != "Vanilla only" {
			t.Fatalf("%s: options now %+v", name, f.room.GameOptions)
		}
	}

	// At the limits is fine, and the label is trimmed.
	eight := protocol.RoomUpdateGameOptionsRequest{}
	for i := 0; i < gameOptionsMax; i++ {
		eight.Options = append(eight.Options, protocol.GameOption{ID: fmt.Sprintf("opt-%d", i), Label: "Option"})
	}
	eight.Options[0] = protocol.GameOption{
		ID: strings.Repeat("a", gameOptionIDMax), Label: "  " + strings.Repeat("バ", gameOptionLabelMax) + "\n",
	}
	f.setGameOptions("host", eight)
	if len(f.room.GameOptions) != gameOptionsMax {
		t.Fatalf("eight options at the limits were refused: %+v", f.room.GameOptions)
	}
	if got := f.room.GameOptions[0].Label; got != strings.Repeat("バ", gameOptionLabelMax) {
		t.Fatalf("label = %q, want it trimmed", got)
	}
}

func TestGameOptionLabelsGoThroughTheFilter(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	f.gameOptionsUser("host")
	f.openRoom(2, "host")

	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		{ID: "vanilla-only", Label: "Vanilla only"}, {ID: "rude", Label: "badword mode"},
	}})
	if f.room.GameOptions != nil {
		t.Fatalf("a filtered label got in: %+v", f.room.GameOptions)
	}
	f.setGameOptions("host", vanillaOnly)
	if optionIDs(f.room.GameOptions) != "vanilla-only" {
		t.Fatalf("a clean label was refused: %+v", f.room.GameOptions)
	}
}

func TestGameOptionsCantChangeMidMatchOrInQuickMatch(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	host := f.gameOptionsUser("host")
	f.gameOptionsUser("guest")
	f.openRoom(2, "host", "guest")
	f.setGameOptions("host", vanillaOnly)

	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch == nil {
		t.Fatal("match did not start")
	}
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{})
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		{ID: "no-items", Label: "No items"},
	}})
	if optionIDs(f.room.GameOptions) != "vanilla-only" {
		t.Fatalf("game options changed mid-match: %+v", f.room.GameOptions)
	}
	// The running match's options stay on show.
	if st := roomStatePayload(f.room); len(st.GameOptions) != 1 {
		t.Fatalf("in-game room state options = %+v", st.GameOptions)
	}
	if list := f.h.buildRoomListPayload(); len(list.Rooms) != 1 || len(list.Rooms[0].GameOptions) != 1 {
		t.Fatalf("in-game room list = %+v", list.Rooms)
	}

	q := newLateJoinFixture(t, 2)
	q.gameOptionsUser("a")
	q.gameOptionsUser("b")
	q.openRoom(2, "a", "b")
	q.room.QuickMatch = true
	q.setGameOptions("a", vanillaOnly)
	if q.room.GameOptions != nil {
		t.Fatal("a quick match room took game options")
	}
	if st := roomStatePayload(q.room); st.GameOptionsOffered || st.GameOptions != nil {
		t.Fatalf("a quick match room offers game options: %+v", st)
	}

	// A challenge room has them, like save data.
	p := newLateJoinFixture(t, 2)
	p.gameOptionsUser("a")
	p.gameOptionsUser("b")
	p.openRoom(2, "a", "b")
	p.room.Private = true
	p.setGameOptions("a", vanillaOnly)
	if optionIDs(p.room.GameOptions) != "vanilla-only" || !roomStatePayload(p.room).GameOptionsOffered {
		t.Fatalf("a challenge room refused game options: %+v", p.room.GameOptions)
	}
}

func TestGameOptionsNeedEveryPlayersClient(t *testing.T) {
	f := newLateJoinFixture(t, 3)
	host := f.gameOptionsUser("host")
	old := f.user("old", true) // a client from before game options
	f.openRoom(3, "host", "old")

	// Allowed with them seated, as save data is: the start is what waits.
	f.setGameOptions("host", vanillaOnly)
	if f.room.GameOptions == nil {
		t.Fatal("game options were refused with an old client seated")
	}
	for _, seat := range roomStatePayload(f.room).Players {
		if seat.UserID == old.ID && seat.GameOptions {
			t.Fatal("an old client shows the game options capability")
		}
		if seat.UserID == host.ID && !seat.GameOptions {
			t.Fatal("the host's seat lacks the game options capability")
		}
	}

	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch != nil {
		t.Fatal("started a match with game options and a client that can't take them")
	}

	// Once they leave, the host can start.
	f.send("old", protocol.TypeRoomLeave, struct{}{})
	if old.CurrentRoom != nil {
		t.Fatal("the old client could not leave")
	}
	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch == nil {
		t.Fatal("could not start once the old client left")
	}

	// Or with the options off, the same players can play.
	g := newLateJoinFixture(t, 2)
	gHost := g.gameOptionsUser("host")
	g.user("old", true)
	g.openRoom(2, "host", "old")
	g.setGameOptions("host", vanillaOnly)
	g.send("host", protocol.TypeRoomStart, struct{}{})
	if gHost.CurrentMatch != nil {
		t.Fatal("started with game options and an old client")
	}
	g.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{})
	g.send("host", protocol.TypeRoomStart, struct{}{})
	if gHost.CurrentMatch == nil {
		t.Fatal("could not start without game options")
	}
}

func TestGameOptionsCapabilityCoversLocalPlayers(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host").GameOptions = true
	owner := f.capableUser("mira")
	owner.GameOptions = true
	sam := f.addLocal("mira", "sam")
	if !sam.GameOptions {
		t.Fatal("a local player doesn't share its owner's capability")
	}
	room := f.createRoom("host", 4)
	f.joinWithPing("mira", room, 0)
	f.setGameOptions("host", vanillaOnly)
	f.send("host", protocol.TypeRoomStart, struct{}{})
	if f.users["host"].CurrentMatch == nil || sam.CurrentMatch == nil {
		t.Fatal("a capable PC with a local player couldn't start a match with game options")
	}
}

func TestGameOptionsKeepOldClientsOutOfALateJoin(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.gameOptionsUser("host")
	f.gameOptionsUser("guest")
	old := f.user("old", true)
	late := f.gameOptionsUser("late")
	f.openRoom(4, "host", "guest")
	f.room.LateJoinEnabled = true
	f.setGameOptions("host", vanillaOnly)
	f.send("host", protocol.TypeRoomStart, struct{}{})
	match := f.users["host"].CurrentMatch
	if match == nil || match.Seats == 0 {
		t.Fatalf("match = %+v, want one open to late joiners", match)
	}

	// An old client may still come in to watch and chat.
	f.send("old", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	if old.CurrentRoom != f.room {
		t.Fatal("the old client could not enter the room to watch and chat")
	}
	f.drain("host")
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: old.ID})
	env, ok := findEnvelope(f.drain("host"), protocol.TypeMatchAdmitFail)
	if !ok {
		t.Fatal("added a client that can't take game options to a match with them on")
	}
	var fail protocol.MatchAdmitFailPayload
	if err := json.Unmarshal(env.Data, &fail); err != nil {
		t.Fatal(err)
	}
	if fail.Reason != protocol.ReasonLateJoinUnsupported {
		t.Fatalf("reason = %q", fail.Reason)
	}

	// A capable joiner is let in.
	f.send("late", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	if late.CurrentRoom != f.room {
		t.Fatal("the capable joiner could not enter the room")
	}
	f.drain("host")
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: late.ID})
	if _, ok := findEnvelope(f.drain("host"), protocol.TypeMatchAdmitBegin); !ok {
		t.Fatal("a capable joiner wasn't admitted")
	}
}

func TestGameOptionsLastTheRoomsLife(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	host := f.gameOptionsUser("host")
	guest := f.gameOptionsUser("guest")
	f.openRoom(2, "host", "guest")
	f.setGameOptions("host", vanillaOnly)

	// They survive a match: the host ends it and the room waits again.
	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch == nil {
		t.Fatal("match did not start")
	}
	f.send("host", protocol.TypeMatchFinished, struct{}{})
	if f.room.State != state.RoomStateWaiting {
		t.Fatalf("room state %s after the host ended the match", f.room.State)
	}
	if optionIDs(f.room.GameOptions) != "vanilla-only" {
		t.Fatalf("options after a match = %+v", f.room.GameOptions)
	}
	f.drain("guest")
	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch == nil {
		t.Fatal("the next match did not start")
	}
	f.send("host", protocol.TypeMatchFinished, struct{}{})

	// And the host can change them between matches.
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{})
	if f.room.GameOptions != nil {
		t.Fatalf("options not cleared between matches: %+v", f.room.GameOptions)
	}

	// The host never changes: when they leave, the room and its options go.
	f.setGameOptions("host", vanillaOnly)
	f.send("host", protocol.TypeRoomLeave, struct{}{})
	if guest.CurrentRoom != nil || f.h.state.GetRoom(f.room.ID) != nil {
		t.Fatal("the room outlived its host")
	}
	f.send("guest", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "next", Rom: protocol.RomInfo{Name: "rom", MD5: "abc"}, MaxPlayers: 2,
	})
	if guest.CurrentRoom == nil || guest.CurrentRoom.GameOptions != nil {
		t.Fatal("a new room doesn't start with no game options")
	}
}

// sendNextRoomListAtOnce lets the hub's next room list out straight away, as
// if roomListMinIntervalMs had passed: the fixture's hub doesn't Run, so a
// held-back list would never go out.
func (f *lateJoinFixture) sendNextRoomListAtOnce() {
	f.h.roomListMu.Lock()
	f.h.roomListLast = time.Time{}
	f.h.roomListMu.Unlock()
}
