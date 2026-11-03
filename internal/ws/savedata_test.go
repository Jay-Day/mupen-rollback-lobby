package ws

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

var testSave = protocol.RoomUpdateSaveDataRequest{
	Enabled: true,
	Label:   "Everything unlocked",
	Kinds:   []string{"SRAM"},
	Bytes:   0x8000,
	Hash:    "0123456789abcdef",
}

func roomStateFrom(t *testing.T, envs []protocol.Envelope) protocol.RoomStatePayload {
	t.Helper()
	env, ok := findEnvelope(envs, protocol.TypeRoomState)
	if !ok {
		t.Fatal("no ROOM_STATE")
	}
	var st protocol.RoomStatePayload
	if err := json.Unmarshal(env.Data, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSaveDataIsOffUntilTheHostPicksASave(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.user("host", true).SaveData = true
	f.user("guest", true).SaveData = true
	f.openRoom(2, "host", "guest")

	initial := roomStatePayload(f.room)
	if initial.SaveData == nil || initial.SaveData.Enabled {
		t.Fatalf("a new room starts with save data: %+v", initial.SaveData)
	}
	// On the wire even when off, so clients can tell the room has the option.
	if raw, _ := json.Marshal(initial); !strings.Contains(string(raw), `"saveData":{"enabled":false}`) {
		t.Fatalf("room state JSON lacks saveData off: %s", raw)
	}

	f.send("guest", protocol.TypeRoomUpdateSaveData, testSave)
	if f.room.SaveData != nil {
		t.Fatal("a non-host turned save data on")
	}

	f.drain("host")
	f.drain("guest")
	f.send("host", protocol.TypeRoomUpdateSaveData, testSave)
	if f.room.SaveData == nil {
		t.Fatal("the host could not turn save data on")
	}
	for _, name := range []string{"host", "guest"} {
		st := roomStateFrom(t, f.drain(name))
		if st.SaveData == nil || !st.SaveData.Enabled || st.SaveData.Label != testSave.Label ||
			st.SaveData.Hash != testSave.Hash || len(st.SaveData.Kinds) != 1 || st.SaveData.Bytes != testSave.Bytes {
			t.Fatalf("%s room state save data = %+v", name, st.SaveData)
		}
		for _, seat := range st.Players {
			if !seat.SaveData {
				t.Fatalf("%s sees seat %+v without the capability", name, seat)
			}
		}
	}

	// The same pick again changes nothing and isn't rebroadcast.
	f.send("host", protocol.TypeRoomUpdateSaveData, testSave)
	if _, ok := findEnvelope(f.drain("guest"), protocol.TypeRoomState); ok {
		t.Fatal("an unchanged save data pick was rebroadcast")
	}

	f.send("host", protocol.TypeRoomUpdateSaveData, protocol.RoomUpdateSaveDataRequest{Enabled: false})
	if f.room.SaveData != nil {
		t.Fatal("the host could not turn save data off")
	}
}

func TestSaveDataRejectsABadDescription(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	f.user("host", true).SaveData = true
	f.user("guest", true).SaveData = true
	f.openRoom(2, "host", "guest")

	bad := []protocol.RoomUpdateSaveDataRequest{
		{Enabled: true, Label: "x", Kinds: []string{"Rumble"}, Bytes: 10, Hash: testSave.Hash},
		{Enabled: true, Label: "x", Kinds: []string{"SRAM"}, Bytes: 0, Hash: testSave.Hash},
		{Enabled: true, Label: "x", Kinds: []string{"SRAM"}, Bytes: saveDataBytesMax + 1, Hash: testSave.Hash},
		{Enabled: true, Label: "x", Kinds: []string{"SRAM"}, Bytes: 10, Hash: "not-hex-at-all!!"},
	}
	for _, req := range bad {
		f.send("host", protocol.TypeRoomUpdateSaveData, req)
		if f.room.SaveData != nil {
			t.Fatalf("accepted %+v", req)
		}
	}

	long := testSave
	long.Label = strings.Repeat("セーブ", 40)
	long.Kinds = []string{"SRAM", "SRAM", "EEPROM"}
	f.send("host", protocol.TypeRoomUpdateSaveData, long)
	if f.room.SaveData == nil {
		t.Fatal("rejected a long label")
	}
	if got := []rune(f.room.SaveData.Label); len(got) != saveDataLabelMax {
		t.Fatalf("label kept %d characters, want %d", len(got), saveDataLabelMax)
	}
	if strings.Join(f.room.SaveData.Kinds, ",") != "SRAM,EEPROM" {
		t.Fatalf("kinds = %v", f.room.SaveData.Kinds)
	}
}

func TestSaveDataNeedsEveryPlayersClient(t *testing.T) {
	f := newLateJoinFixture(t, 3)
	host := f.user("host", true)
	host.SaveData = true
	old := f.user("old", true) // a client from before save data
	f.openRoom(3, "host", "old")
	f.room.LateJoinEnabled = true

	f.send("host", protocol.TypeRoomUpdateSaveData, testSave)
	st := roomStatePayload(f.room)
	for _, seat := range st.Players {
		if seat.UserID == old.ID && seat.SaveData {
			t.Fatal("an old client shows the save data capability")
		}
	}

	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch != nil {
		t.Fatal("started a save data match with a client that can't take the save")
	}

	// Off again, the same players can play.
	f.send("host", protocol.TypeRoomUpdateSaveData, protocol.RoomUpdateSaveDataRequest{Enabled: false})
	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch == nil {
		t.Fatal("could not start without save data")
	}
}

func TestSaveDataCantChangeMidMatchOrInQuickMatch(t *testing.T) {
	f := newLateJoinFixture(t, 2)
	host := f.user("host", true)
	host.SaveData = true
	f.user("guest", true).SaveData = true
	f.openRoom(2, "host", "guest")

	f.send("host", protocol.TypeRoomStart, struct{}{})
	if host.CurrentMatch == nil {
		t.Fatal("match did not start")
	}
	f.send("host", protocol.TypeRoomUpdateSaveData, testSave)
	if f.room.SaveData != nil {
		t.Fatal("save data changed mid-match")
	}

	q := newLateJoinFixture(t, 2)
	q.user("a", true).SaveData = true
	q.user("b", true).SaveData = true
	q.openRoom(2, "a", "b")
	q.room.QuickMatch = true
	q.send("a", protocol.TypeRoomUpdateSaveData, testSave)
	if q.room.SaveData != nil {
		t.Fatal("a quick match room took save data")
	}
	if st := roomStatePayload(q.room); st.SaveData != nil {
		t.Fatalf("a quick match room offers save data: %+v", st.SaveData)
	}
}

func TestSaveDataKeepsOldClientsOutOfALateJoin(t *testing.T) {
	f := newLateJoinFixture(t, 3)
	f.user("host", true).SaveData = true
	f.user("guest", true).SaveData = true
	old := f.user("old", true)
	f.openRoom(3, "host", "guest")
	f.room.LateJoinEnabled = true
	f.send("host", protocol.TypeRoomUpdateSaveData, testSave)
	f.send("host", protocol.TypeRoomStart, struct{}{})
	match := f.users["host"].CurrentMatch
	if match == nil || match.Seats == 0 {
		t.Fatalf("match = %+v, want one open to late joiners", match)
	}

	f.send("old", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	if old.CurrentRoom != f.room {
		t.Fatal("the old client could not enter the room to watch and chat")
	}
	f.drain("host")
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: old.ID})
	env, ok := findEnvelope(f.drain("host"), protocol.TypeMatchAdmitFail)
	if !ok {
		t.Fatal("added a client that can't take the save to a save data match")
	}
	var fail protocol.MatchAdmitFailPayload
	if err := json.Unmarshal(env.Data, &fail); err != nil {
		t.Fatal(err)
	}
	if fail.Reason != protocol.ReasonLateJoinUnsupported {
		t.Fatalf("reason = %q", fail.Reason)
	}
}

var _ = state.RoomStateWaiting
