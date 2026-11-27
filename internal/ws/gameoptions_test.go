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
	tooMany := protocol.RoomUpdateGameOptionsRequest{}
	for i := 0; i < gameOptionsMax+1; i++ {
		tooMany.Options = append(tooMany.Options, protocol.GameOption{ID: fmt.Sprintf("opt-%d", i), Label: "Option"})
	}
	bad := map[string]any{
		"17 options":      tooMany,
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
	atLimits := protocol.RoomUpdateGameOptionsRequest{}
	for i := 0; i < gameOptionsMax; i++ {
		atLimits.Options = append(atLimits.Options, protocol.GameOption{ID: fmt.Sprintf("opt-%d", i), Label: "Option"})
	}
	atLimits.Options[0] = protocol.GameOption{
		ID: strings.Repeat("a", gameOptionIDMax), Label: "  " + strings.Repeat("バ", gameOptionLabelMax) + "\n",
	}
	f.setGameOptions("host", atLimits)
	if len(f.room.GameOptions) != gameOptionsMax {
		t.Fatalf("%d options at the limits were refused: %+v", gameOptionsMax, f.room.GameOptions)
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

// optionsN is a list of n game options with distinct ids.
func optionsN(n int) []protocol.GameOption {
	out := make([]protocol.GameOption, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, protocol.GameOption{ID: fmt.Sprintf("opt-%d", i), Label: fmt.Sprintf("Option %d", i)})
	}
	return out
}

var oneVsOneVanilla = protocol.RoomUpdateGameOptionsRequest{
	Options: []protocol.GameOption{{ID: "vanilla-css", Label: "Vanilla CSS"}, {ID: "tournament-rules", Label: "Tournament rules"}},
	Preset:  &protocol.GameOptionPreset{ID: "1v1-vanilla", Label: "1v1 Vanilla"},
}

func TestHelloTakesHowManyGameOptionsAClientTakes(t *testing.T) {
	cases := []struct {
		capable bool
		max     any // left out of HELLO when nil
		limit   int
	}{
		{true, nil, gameOptionsLegacyMax}, // a client from before presets
		{true, 16, 16},
		{true, 12, 12},
		{true, 4, gameOptionsLegacyMax},
		{true, -3, gameOptionsLegacyMax},
		{true, 1000, gameOptionsMax},
		{false, 16, 0},
	}
	for _, tc := range cases {
		h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
		c := &Client{hub: h, send: make(chan []byte, 64)}
		fields := map[string]any{"username": "mira", "clientVersion": "vdev-test", "gameOptions": tc.capable}
		if tc.max != nil {
			fields["gameOptionsMax"] = tc.max
		}
		raw, _ := json.Marshal(fields)
		if err := h.handleHello(c, protocol.Envelope{Type: protocol.TypeHello, Data: raw}); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if got := gameOptionsLimit(c.user); got != tc.limit {
			t.Fatalf("HELLO gameOptions %v, gameOptionsMax %v: limit %d, want %d", tc.capable, tc.max, got, tc.limit)
		}
	}
}

func TestGameOptionPresetGoesWithTheOptions(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.gameOptionsUser("guest")
	f.openRoom(2, "host", "guest")
	for _, name := range []string{"host", "guest"} {
		f.h.clients[f.users[name].ID] = f.client(name)
	}
	if st := roomStatePayload(f.room); st.GameOptionsMax != gameOptionsMax || st.GameOptionsPreset != nil {
		t.Fatalf("a new room: max %d, preset %+v", st.GameOptionsMax, st.GameOptionsPreset)
	}

	// The label is trimmed like an option's.
	req := oneVsOneVanilla
	req.Preset = &protocol.GameOptionPreset{ID: "1v1-vanilla", Label: "  1v1 Vanilla "}
	f.drain("guest")
	f.sendNextRoomListAtOnce()
	f.setGameOptions("host", req)
	want := protocol.GameOptionPreset{ID: "1v1-vanilla", Label: "1v1 Vanilla"}
	if p := f.room.GameOptionsPreset; p == nil || p.ID != want.ID || p.Label != want.Label {
		t.Fatalf("room preset = %+v", p)
	}
	envs := f.drain("guest")
	if st := roomStateFrom(t, envs); st.GameOptionsPreset == nil || *st.GameOptionsPreset != want || len(st.GameOptions) != 2 {
		t.Fatalf("guest room state: options %+v, preset %+v", st.GameOptions, st.GameOptionsPreset)
	}
	if list := roomListFrom(t, envs); len(list.Rooms) != 1 || list.Rooms[0].GameOptionsPreset == nil ||
		*list.Rooms[0].GameOptionsPreset != want {
		t.Fatalf("room list = %+v", list.Rooms)
	}
	const wire = `"gameOptionsPreset":{"id":"1v1-vanilla","label":"1v1 Vanilla"}`
	if raw, _ := json.Marshal(roomStatePayload(f.room)); !strings.Contains(string(raw), wire) {
		t.Fatalf("room state JSON: %s", raw)
	}
	if raw, _ := json.Marshal(f.h.buildRoomListPayload()); !strings.Contains(string(raw), wire) {
		t.Fatalf("room list JSON: %s", raw)
	}

	// The same options without a preset are custom ones: the preset goes, and
	// that's a change everyone hears about.
	f.sendNextRoomListAtOnce()
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: oneVsOneVanilla.Options})
	if f.room.GameOptionsPreset != nil || len(f.room.GameOptions) != 2 {
		t.Fatalf("custom options: %+v, preset %+v", f.room.GameOptions, f.room.GameOptionsPreset)
	}
	envs = f.drain("guest")
	if st := roomStateFrom(t, envs); st.GameOptionsPreset != nil || len(st.GameOptions) != 2 {
		t.Fatalf("guest still sees preset %+v", st.GameOptionsPreset)
	}
	if list := roomListFrom(t, envs); list.Rooms[0].GameOptionsPreset != nil {
		t.Fatalf("room list still shows preset %+v", list.Rooms[0].GameOptionsPreset)
	}
	if raw, _ := json.Marshal(roomStatePayload(f.room)); strings.Contains(string(raw), "gameOptionsPreset") {
		t.Fatalf("room state JSON names a preset: %s", raw)
	}

	// A new label for the same preset is a change too.
	f.setGameOptions("host", oneVsOneVanilla)
	f.drain("guest")
	relabelled := oneVsOneVanilla
	relabelled.Preset = &protocol.GameOptionPreset{ID: "1v1-vanilla", Label: "One on one"}
	f.setGameOptions("host", relabelled)
	if f.room.GameOptionsPreset == nil || f.room.GameOptionsPreset.Label != "One on one" {
		t.Fatalf("preset after relabelling = %+v", f.room.GameOptionsPreset)
	}
	if _, ok := findEnvelope(f.drain("guest"), protocol.TypeRoomState); !ok {
		t.Fatal("a preset change wasn't broadcast")
	}
	// And the same again isn't.
	f.setGameOptions("host", relabelled)
	if _, ok := findEnvelope(f.drain("guest"), protocol.TypeRoomState); ok {
		t.Fatal("an unchanged preset was rebroadcast")
	}

	// Turning the options off takes the preset with them.
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{}})
	if f.room.GameOptions != nil || f.room.GameOptionsPreset != nil {
		t.Fatalf("after turning off: %+v, preset %+v", f.room.GameOptions, f.room.GameOptionsPreset)
	}
}

func TestGameOptionPresetIsCheckedLikeTheOptions(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.openRoom(2, "host")
	f.setGameOptions("host", oneVsOneVanilla)

	with := func(id, label string) protocol.RoomUpdateGameOptionsRequest {
		return protocol.RoomUpdateGameOptionsRequest{
			Options: oneVsOneVanilla.Options, Preset: &protocol.GameOptionPreset{ID: id, Label: label},
		}
	}
	bad := map[string]any{
		"preset without options": protocol.RoomUpdateGameOptionsRequest{
			Preset: &protocol.GameOptionPreset{ID: "1v1-vanilla", Label: "1v1 Vanilla"},
		},
		"empty id":      with("", "1v1 Vanilla"),
		"upper-case id": with("1V1-vanilla", "1v1 Vanilla"),
		"33-byte id":    with(strings.Repeat("a", gameOptionIDMax+1), "1v1 Vanilla"),
		"empty label":   with("1v1-vanilla", ""),
		"blank label":   with("1v1-vanilla", "   "),
		"49-rune label": with("1v1-vanilla", strings.Repeat("バ", gameOptionLabelMax+1)),
		"newline":       with("1v1-vanilla", "1v1\nVanilla"),
		"bidi override": with("1v1-vanilla", "1v1 ‮allinaV"),
		"preset not an object": json.RawMessage(
			`{"options":[{"id":"vanilla-css","label":"Vanilla CSS"}],"preset":"1v1-vanilla"}`),
	}
	for name, req := range bad {
		f.setGameOptions("host", req)
		// Refused whole: options and preset stay as they were.
		if optionIDs(f.room.GameOptions) != "vanilla-css,tournament-rules" ||
			f.room.GameOptionsPreset == nil || f.room.GameOptionsPreset.ID != "1v1-vanilla" {
			t.Fatalf("%s: options %+v, preset %+v", name, f.room.GameOptions, f.room.GameOptionsPreset)
		}
	}

	// Its label goes through the filter too.
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	f.setGameOptions("host", with("rude", "badword mode"))
	if f.room.GameOptionsPreset == nil || f.room.GameOptionsPreset.ID != "1v1-vanilla" {
		t.Fatalf("a filtered preset label got in: %+v", f.room.GameOptionsPreset)
	}
}

func TestMoreThanEightGameOptionsNeedClientsThatTakeThem(t *testing.T) {
	f := newLateJoinFixture(t, 3)
	host := f.gameOptionsUser("host")
	host.GameOptionsMax = gameOptionsMax
	f.gameOptionsUser("guest").GameOptionsMax = gameOptionsMax
	legacy := f.gameOptionsUser("legacy") // sent gameOptions without gameOptionsMax
	f.openRoom(3, "host", "guest", "legacy")

	// Nine can be picked with them seated, as with any game options: the start is
	// what waits.
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: optionsN(9)})
	if len(f.room.GameOptions) != 9 {
		t.Fatalf("nine options were refused: %+v", f.room.GameOptions)
	}
	for _, seat := range roomStatePayload(f.room).Players {
		want := gameOptionsMax
		if seat.UserID == legacy.ID {
			want = gameOptionsLegacyMax
		}
		if !seat.GameOptions || seat.GameOptionsMax != want {
			t.Fatalf("seat %s: gameOptions %v, gameOptionsMax %d, want %d",
				seat.Username, seat.GameOptions, seat.GameOptionsMax, want)
		}
	}
	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch != nil {
		t.Fatal("started nine game options with a client that takes eight")
	}

	// Eight are fine with everyone.
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: optionsN(8)})
	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch == nil {
		t.Fatal("could not start eight game options with a client that takes eight")
	}

	// A late joiner that takes eight can't be added to a match with nine; one
	// that takes more can.
	g := newLateJoinFixture(t, 4)
	gHost := g.gameOptionsUser("host")
	gHost.GameOptionsMax = gameOptionsMax
	g.gameOptionsUser("guest").GameOptionsMax = gameOptionsMax
	late := g.gameOptionsUser("late")
	newer := g.gameOptionsUser("newer")
	newer.GameOptionsMax = gameOptionsMax
	g.openRoom(4, "host", "guest")
	g.room.LateJoinEnabled = true
	g.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: optionsN(9)})
	g.send("host", protocol.TypeRoomStart, struct{}{})
	match := gHost.CurrentMatch
	if match == nil || match.Seats == 0 {
		t.Fatalf("match = %+v, want one open to late joiners", match)
	}
	g.send("late", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: g.room.ID})
	if late.CurrentRoom != g.room {
		t.Fatal("the joiner that takes eight could not enter the room")
	}
	g.drain("host")
	g.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: late.ID})
	env, ok := findEnvelope(g.drain("host"), protocol.TypeMatchAdmitFail)
	if !ok {
		t.Fatal("added a client that takes eight game options to a match with nine")
	}
	var fail protocol.MatchAdmitFailPayload
	if err := json.Unmarshal(env.Data, &fail); err != nil {
		t.Fatal(err)
	}
	if fail.Reason != protocol.ReasonLateJoinUnsupported {
		t.Fatalf("reason = %q", fail.Reason)
	}
	g.send("newer", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: g.room.ID})
	g.drain("host")
	g.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: newer.ID})
	if _, ok := findEnvelope(g.drain("host"), protocol.TypeMatchAdmitBegin); !ok {
		t.Fatal("a joiner that takes nine game options wasn't admitted")
	}
}

const competitiveCode = "STG-220C-6YA8-2T5V-W0"

var stageList = protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
	{ID: "vanilla-css", Label: "Vanilla CSS"},
	{ID: "stage-list", Label: "Stage list", Param: competitiveCode},
}}

func TestGameOptionParamsTravelWithTheOptions(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.gameOptionsUser("guest")
	f.openRoom(2, "host", "guest")
	for _, name := range []string{"host", "guest"} {
		f.h.clients[f.users[name].ID] = f.client(name)
	}
	if st := roomStatePayload(f.room); !st.GameOptionParams {
		t.Fatal("room state doesn't say the server keeps params")
	}
	if raw, _ := json.Marshal(roomStatePayload(f.room)); !strings.Contains(string(raw), `"gameOptionParams":true`) {
		t.Fatalf("room state JSON: %s", raw)
	}

	f.drain("guest")
	f.sendNextRoomListAtOnce()
	f.setGameOptions("host", stageList)
	if len(f.room.GameOptions) != 2 || f.room.GameOptions[1].Param != competitiveCode || f.room.GameOptions[0].Param != "" {
		t.Fatalf("room options = %+v", f.room.GameOptions)
	}
	envs := f.drain("guest")
	if st := roomStateFrom(t, envs); len(st.GameOptions) != 2 || st.GameOptions[1] != stageList.Options[1] {
		t.Fatalf("guest room state options = %+v", st.GameOptions)
	}
	if list := roomListFrom(t, envs); len(list.Rooms) != 1 || len(list.Rooms[0].GameOptions) != 2 ||
		list.Rooms[0].GameOptions[1].Param != competitiveCode {
		t.Fatalf("room list = %+v", list.Rooms)
	}
	// An option without one has no param on the wire.
	const wire = `"gameOptions":[{"id":"vanilla-css","label":"Vanilla CSS"},` +
		`{"id":"stage-list","label":"Stage list","param":"STG-220C-6YA8-2T5V-W0"}]`
	if raw, _ := json.Marshal(roomStatePayload(f.room)); !strings.Contains(string(raw), wire) {
		t.Fatalf("room state JSON: %s", raw)
	}
	if raw, _ := json.Marshal(f.h.buildRoomListPayload()); !strings.Contains(string(raw), wire) {
		t.Fatalf("room list JSON: %s", raw)
	}

	// A new list alone is a change everyone hears about; the same again isn't.
	other := protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		stageList.Options[0], {ID: "stage-list", Label: "Stage list", Param: "STG-20GC-HPK0"},
	}}
	f.setGameOptions("host", other)
	if f.room.GameOptions[1].Param != "STG-20GC-HPK0" {
		t.Fatalf("param after a change = %+v", f.room.GameOptions)
	}
	if st := roomStateFrom(t, f.drain("guest")); st.GameOptions[1].Param != "STG-20GC-HPK0" {
		t.Fatalf("guest param = %+v", st.GameOptions)
	}
	f.setGameOptions("host", other)
	if _, ok := findEnvelope(f.drain("guest"), protocol.TypeRoomState); ok {
		t.Fatal("an unchanged param was rebroadcast")
	}
	// Dropping it is a change too.
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		stageList.Options[0], {ID: "stage-list", Label: "Stage list"},
	}})
	if f.room.GameOptions[1].Param != "" {
		t.Fatalf("param not dropped: %+v", f.room.GameOptions)
	}

	// Quick match rooms offer no game options, so no params either.
	q := newLateJoinFixture(t, 2)
	q.gameOptionsUser("a")
	q.openRoom(2, "a")
	q.room.QuickMatch = true
	if st := roomStatePayload(q.room); st.GameOptionParams {
		t.Fatal("a quick match room says it keeps params")
	}
}

func TestGameOptionParamsAreChecked(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.openRoom(2, "host")
	f.setGameOptions("host", stageList)

	with := func(param string) protocol.RoomUpdateGameOptionsRequest {
		return protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
			{ID: "stage-list", Label: "Stage list", Param: param},
		}}
	}
	bad := map[string]any{
		"lower case":       with("stg-220c-6ya8-2t5v-w0"),
		"space":            with("STG 220C"),
		"257 characters":   with(strings.Repeat("A", gameOptionParamMax+1)),
		"punctuation":      with("STG-220C!"),
		"underscore":       with("STG_220C"),
		"non-ASCII":        with("STG-ÄÖ"),
		"newline":          with("STG-220C\n"),
		"param not a text": json.RawMessage(`{"options":[{"id":"stage-list","label":"Stage list","param":7}]}`),
	}
	for name, req := range bad {
		f.setGameOptions("host", req)
		// Refused whole: the list the host had stays.
		if len(f.room.GameOptions) != 2 || f.room.GameOptions[1].Param != competitiveCode {
			t.Fatalf("%s: options now %+v", name, f.room.GameOptions)
		}
	}
	// At the limit is fine, and none is fine.
	f.setGameOptions("host", with(strings.Repeat("Z", gameOptionParamMax)))
	if len(f.room.GameOptions) != 1 || len(f.room.GameOptions[0].Param) != gameOptionParamMax {
		t.Fatalf("a %d-character param was refused: %+v", gameOptionParamMax, f.room.GameOptions)
	}
	f.setGameOptions("host", with(""))
	if len(f.room.GameOptions) != 1 || f.room.GameOptions[0].Param != "" {
		t.Fatalf("no param: %+v", f.room.GameOptions)
	}

	// It shows in the room, so it goes through the filter like a label.
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	f.setGameOptions("host", with("STG-BADWORD"))
	if f.room.GameOptions[0].Param != "" {
		t.Fatalf("a filtered param got in: %+v", f.room.GameOptions)
	}
	f.setGameOptions("host", with(competitiveCode))
	if f.room.GameOptions[0].Param != competitiveCode {
		t.Fatalf("a clean param was refused: %+v", f.room.GameOptions)
	}
}

// A client that takes game options but not params still plays a match with
// one: the host's pre-match manifest carries every option's patches, made
// from the param by the host's client. So nothing more is gated.
func TestGameOptionParamsDontKeepGameOptionsClientsOut(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	host := f.gameOptionsUser("host")
	host.GameOptionsMax = gameOptionsMax
	f.gameOptionsUser("legacy") // sent gameOptions without gameOptionsMax
	late := f.gameOptionsUser("late")
	f.openRoom(4, "host", "legacy")
	f.room.LateJoinEnabled = true
	f.setGameOptions("host", stageList)
	f.send("host", protocol.TypeRoomStart, struct{}{})
	match := host.CurrentMatch
	if match == nil {
		t.Fatal("a match with a param didn't start with a client that takes game options")
	}
	f.send("late", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	f.drain("host")
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: late.ID})
	if _, ok := findEnvelope(f.drain("host"), protocol.TypeMatchAdmitBegin); !ok {
		t.Fatal("a late joiner that takes game options wasn't admitted to a match with a param")
	}
	// And the param can't change mid-match.
	f.setGameOptions("host", protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		{ID: "stage-list", Label: "Stage list", Param: "STG-20GC-HPK0"},
	}})
	if len(f.room.GameOptions) != 2 || f.room.GameOptions[1].Param != competitiveCode {
		t.Fatalf("game options changed mid-match: %+v", f.room.GameOptions)
	}
}

func namedList(param string, preset *protocol.GameOptionPreset) protocol.RoomUpdateGameOptionsRequest {
	return protocol.RoomUpdateGameOptionsRequest{Options: []protocol.GameOption{
		{ID: "vanilla-css", Label: "Vanilla CSS"},
		{ID: "stage-list", Label: "Stage list", Param: param, ParamPreset: preset},
	}}
}

func TestGameOptionParamPresetsTravelWithTheParam(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.gameOptionsUser("guest")
	f.openRoom(2, "host", "guest")
	for _, name := range []string{"host", "guest"} {
		f.h.clients[f.users[name].ID] = f.client(name)
	}
	f.drain("guest")
	f.sendNextRoomListAtOnce()
	dreamLand := &protocol.GameOptionPreset{ID: "dream-land", Label: "Dream Land only"}
	f.setGameOptions("host", namedList("STG-20GC-HPK0", dreamLand))
	got := f.room.GameOptions
	if len(got) != 2 || got[1].ParamPresetID != "dream-land" || got[1].ParamPresetLabel != "Dream Land only" ||
		got[0].ParamPresetID != "" {
		t.Fatalf("room options = %+v", got)
	}
	envs := f.drain("guest")
	if st := roomStateFrom(t, envs); len(st.GameOptions) != 2 || st.GameOptions[1].ParamPreset == nil ||
		*st.GameOptions[1].ParamPreset != *dreamLand || st.GameOptions[0].ParamPreset != nil {
		t.Fatalf("guest room state options = %+v", st.GameOptions)
	}
	if list := roomListFrom(t, envs); len(list.Rooms) != 1 || len(list.Rooms[0].GameOptions) != 2 ||
		list.Rooms[0].GameOptions[1].ParamPreset == nil || list.Rooms[0].GameOptions[1].ParamPreset.Label != "Dream Land only" {
		t.Fatalf("room list = %+v", list.Rooms)
	}
	// Next to the param on the wire; options without one have none.
	const wire = `"gameOptions":[{"id":"vanilla-css","label":"Vanilla CSS"},` +
		`{"id":"stage-list","label":"Stage list","param":"STG-20GC-HPK0",` +
		`"paramPreset":{"id":"dream-land","label":"Dream Land only"}}]`
	if raw, _ := json.Marshal(roomStatePayload(f.room)); !strings.Contains(string(raw), wire) {
		t.Fatalf("room state JSON: %s", raw)
	}
	if raw, _ := json.Marshal(f.h.buildRoomListPayload()); !strings.Contains(string(raw), wire) {
		t.Fatalf("room list JSON: %s", raw)
	}

	// A host's saved list goes by its own id and name; a new name alone is a
	// change everyone hears about, the same again isn't.
	saved := &protocol.GameOptionPreset{ID: "user-0a1b2c3d", Label: "Weekly bracket"}
	f.setGameOptions("host", namedList("STG-20GC-HPK0", saved))
	if st := roomStateFrom(t, f.drain("guest")); st.GameOptions[1].ParamPreset == nil || *st.GameOptions[1].ParamPreset != *saved {
		t.Fatalf("guest after a rename = %+v", st.GameOptions)
	}
	f.setGameOptions("host", namedList("STG-20GC-HPK0", saved))
	if _, ok := findEnvelope(f.drain("guest"), protocol.TypeRoomState); ok {
		t.Fatal("an unchanged name was rebroadcast")
	}
	// Dropping the name is a change too; the param stays.
	f.setGameOptions("host", namedList("STG-20GC-HPK0", nil))
	if got := f.room.GameOptions[1]; got.ParamPresetID != "" || got.ParamPresetLabel != "" || got.Param != "STG-20GC-HPK0" {
		t.Fatalf("name not dropped: %+v", got)
	}
	if st := roomStateFrom(t, f.drain("guest")); st.GameOptions[1].ParamPreset != nil {
		t.Fatalf("guest still has a name: %+v", st.GameOptions)
	}
	// The label is trimmed like a preset's.
	f.setGameOptions("host", namedList("STG-20GC-HPK0", &protocol.GameOptionPreset{ID: "user-0a1b2c3d", Label: "  Weekly  "}))
	if got := f.room.GameOptions[1].ParamPresetLabel; got != "Weekly" {
		t.Fatalf("label = %q, want it trimmed", got)
	}
}

func TestGameOptionParamPresetsAreChecked(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.gameOptionsUser("host")
	f.openRoom(2, "host")
	good := namedList(competitiveCode, &protocol.GameOptionPreset{ID: "competitive", Label: "Competitive stages"})
	f.setGameOptions("host", good)
	kept := func(what string) {
		t.Helper()
		if len(f.room.GameOptions) != 2 || f.room.GameOptions[1].ParamPresetID != "competitive" ||
			f.room.GameOptions[1].Param != competitiveCode {
			t.Fatalf("%s: options now %+v", what, f.room.GameOptions)
		}
	}
	kept("a good name")

	bad := map[string]any{
		"no param":          namedList("", &protocol.GameOptionPreset{ID: "dream-land", Label: "Dream Land only"}),
		"no id":             namedList(competitiveCode, &protocol.GameOptionPreset{Label: "Competitive stages"}),
		"upper-case id":     namedList(competitiveCode, &protocol.GameOptionPreset{ID: "Dream-Land", Label: "Dream Land only"}),
		"33-character id":   namedList(competitiveCode, &protocol.GameOptionPreset{ID: strings.Repeat("a", gameOptionIDMax+1), Label: "x"}),
		"no label":          namedList(competitiveCode, &protocol.GameOptionPreset{ID: "dream-land"}),
		"blank label":       namedList(competitiveCode, &protocol.GameOptionPreset{ID: "dream-land", Label: "   "}),
		"49-character name": namedList(competitiveCode, &protocol.GameOptionPreset{ID: "user-0a1b2c3d", Label: strings.Repeat("x", gameOptionLabelMax+1)}),
		"two lines":         namedList(competitiveCode, &protocol.GameOptionPreset{ID: "user-0a1b2c3d", Label: "Weekly\nbracket"}),
		"not an object": json.RawMessage(`{"options":[{"id":"stage-list","label":"Stage list",` +
			`"param":"STG-220C-6YA8-2T5V-W0","paramPreset":"competitive"}]}`),
	}
	for name, req := range bad {
		f.setGameOptions("host", req)
		// Refused whole: the list the host had stays.
		kept(name)
	}
	// At the label limit is fine.
	f.setGameOptions("host", namedList(competitiveCode, &protocol.GameOptionPreset{ID: "user-0a1b2c3d", Label: strings.Repeat("ス", gameOptionLabelMax)}))
	if got := f.room.GameOptions[1].ParamPresetLabel; got != strings.Repeat("ス", gameOptionLabelMax) {
		t.Fatalf("a %d-character name was refused: %q", gameOptionLabelMax, got)
	}

	// It shows in the room, so it goes through the filter like a label.
	f.setGameOptions("host", good)
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	f.setGameOptions("host", namedList(competitiveCode, &protocol.GameOptionPreset{ID: "user-0a1b2c3d", Label: "badword list"}))
	kept("a filtered name")
	f.setGameOptions("host", namedList(competitiveCode, &protocol.GameOptionPreset{ID: "user-0a1b2c3d", Label: "Weekly bracket"}))
	if got := f.room.GameOptions[1].ParamPresetLabel; got != "Weekly bracket" {
		t.Fatalf("a clean name was refused: %+v", f.room.GameOptions)
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
