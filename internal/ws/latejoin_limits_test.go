package ws

import (
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// beginHandOff starts a match with host and guest, seats joiner in the room
// and has the host start adding them.
func beginHandOff(t *testing.T) (*lateJoinFixture, *state.Match, *state.Admission) {
	t.Helper()
	f := newLateJoinFixture(t, 3)
	f.user("host", true)
	f.user("guest", true)
	f.user("joiner", true)
	match := f.startMatch(3, "host", "guest")
	f.send("joiner", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: f.users["joiner"].ID})
	admit := match.CurrentAdmission()
	if admit == nil {
		t.Fatal("host could not start the late join")
	}
	for _, name := range []string{"host", "guest", "joiner"} {
		f.drain(name)
	}
	return f, match, admit
}

func TestLateJoinRelayStopsAtTheBlobLimit(t *testing.T) {
	for _, tc := range []struct {
		step     string
		rawLimit int
	}{
		{protocol.AdmitStepManifest, admitMaxManifestBytes},
		{protocol.AdmitStepState, admitMaxStateBytes},
	} {
		t.Run(tc.step, func(t *testing.T) {
			f, match, admit := beginHandOff(t)
			chunk := strings.Repeat("A", admitMaxChunkB64)
			limit := admitRelayLimit(tc.step)
			if limit < tc.rawLimit {
				t.Fatalf("relay limit %d is below the raw limit %d", limit, tc.rawLimit)
			}

			// Everything up to the limit reaches the joiner...
			fits := limit / len(chunk)
			for i := 0; i < fits; i++ {
				f.send("host", protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{
					MatchID: match.ID, AdmitID: admit.ID, Step: tc.step,
					Index: i % admitMaxChunkCount, Count: admitMaxChunkCount, Data: chunk,
				})
				if got := len(f.signals("joiner")); got != 1 {
					t.Fatalf("chunk %d of %d: joiner got %d signals, want 1", i, fits, got)
				}
			}
			if match.CurrentAdmission() == nil {
				t.Fatal("hand-off ended before the limit")
			}

			// ...and the chunk that passes it ends the hand-off instead.
			f.send("host", protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{
				MatchID: match.ID, AdmitID: admit.ID, Step: tc.step,
				Index: 0, Count: admitMaxChunkCount, Data: chunk,
			})
			if match.CurrentAdmission() != nil {
				t.Fatal("hand-off kept running past the limit")
			}
			for _, name := range []string{"host", "guest", "joiner"} {
				sigs := f.signals(name)
				if len(sigs) != 1 || sigs[0].Step != protocol.AdmitStepAbort ||
					sigs[0].Reason != tc.step+"_too_large" || sigs[0].Data != "" {
					t.Fatalf("%s got %+v, want one abort", name, sigs)
				}
			}
		})
	}
}

func TestLateJoinSignalsCarryNoData(t *testing.T) {
	f, match, admit := beginHandOff(t)

	// A cut goes to every player; it must not double as a way to push data
	// at them.
	f.send("host", protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{
		MatchID: match.ID, AdmitID: admit.ID, Step: protocol.AdmitStepCut, Frame: 100,
		Index: 0, Count: 1, Data: strings.Repeat("A", 1024),
	})
	if sigs := f.signals("guest"); len(sigs) != 0 {
		t.Fatalf("guest got a cut carrying data: %+v", sigs)
	}

	f.send("host", protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{
		MatchID: match.ID, AdmitID: admit.ID, Step: protocol.AdmitStepCut, Frame: 100,
	})
	if sigs := f.signals("guest"); len(sigs) != 1 || sigs[0].Frame != 100 {
		t.Fatalf("guest got %+v, want the plain cut", sigs)
	}
}
