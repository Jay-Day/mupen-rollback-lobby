package ws

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const testCandidate = "candidate:1 1 UDP 2122260223 198.51.100.7 50000 typ host"

// pingCheckUser is a connected, social client that does ping checks.
func (f *lateJoinFixture) pingCheckUser(name string) *state.User {
	u := f.socialUser(name)
	u.PingCheck = true
	return u
}

// sendWithID dispatches like send, with an envelope id to correlate the reply.
func (f *lateJoinFixture) sendWithID(name, id, msgType string, data any) {
	f.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		f.t.Fatalf("json.Marshal: %v", err)
	}
	f.h.dispatch(f.client(name), protocol.Envelope{Type: msgType, ID: id, Data: raw})
}

// probe sends one probe candidate from name to target.
func (f *lateJoinFixture) probe(name string, target uint64) {
	f.t.Helper()
	f.send(name, protocol.TypeICESignal, protocol.ICESignalRequest{
		TargetUserID: target, Kind: "candidate", Value: testCandidate, Probe: true,
	})
}

// probesHeard returns the ICE signals queued for name, draining everything else.
func (f *lateJoinFixture) probesHeard(name string) []protocol.ICESignalPayload {
	f.t.Helper()
	var out []protocol.ICESignalPayload
	for _, env := range f.drain(name) {
		if env.Type != protocol.TypeICESignal {
			continue
		}
		var sig protocol.ICESignalPayload
		if err := json.Unmarshal(env.Data, &sig); err != nil {
			f.t.Fatalf("bad ICE_SIGNAL: %v", err)
		}
		out = append(out, sig)
	}
	return out
}

// heardOneProbeFrom checks name got exactly one probe signal, stamped from.
func (f *lateJoinFixture) heardOneProbeFrom(name string, from uint64) {
	f.t.Helper()
	got := f.probesHeard(name)
	if len(got) != 1 {
		f.t.Fatalf("%s heard %d signals, want 1: %+v", name, len(got), got)
	}
	sig := got[0]
	if !sig.Probe || sig.RoomID != 0 || sig.FromUserID != from || sig.Kind != "candidate" || sig.Value != testCandidate {
		f.t.Fatalf("%s heard %+v, want a probe from %d", name, sig, from)
	}
}

func (f *lateJoinFixture) heardNoProbe(name string) {
	f.t.Helper()
	if got := f.probesHeard(name); len(got) != 0 {
		f.t.Fatalf("%s heard %+v", name, got)
	}
}

func (f *lateJoinFixture) pingGrantCount() int {
	m := f.h.pingChecks
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.grants)
}

// requestPingCheck sends a PING_CHECK_REQUEST and returns its reply.
func (f *lateJoinFixture) requestPingCheck(name string, room *state.Room, password string) protocol.PingCheckReplyPayload {
	f.t.Helper()
	f.drain(name)
	f.sendWithID(name, "42", protocol.TypePingCheckRequest, protocol.PingCheckRequest{RoomID: room.ID, Password: password})
	envs := f.drain(name)
	env, ok := findEnvelope(envs, protocol.TypePingCheckReply)
	if !ok {
		f.t.Fatalf("no PING_CHECK_REPLY in %+v", envs)
	}
	if env.ID != "42" {
		f.t.Fatalf("reply id %q, want the request's", env.ID)
	}
	var reply protocol.PingCheckReplyPayload
	if err := json.Unmarshal(env.Data, &reply); err != nil {
		f.t.Fatalf("bad reply: %v", err)
	}
	if reply.RoomID != room.ID {
		f.t.Fatalf("reply for room %d, want %d", reply.RoomID, room.ID)
	}
	return reply
}

func (f *lateJoinFixture) pingCheckRefused(name string, room *state.Room, password, want string) {
	f.t.Helper()
	got := f.requestPingCheck(name, room, password)
	if got.OK || got.Reason != want || got.PeerUserID != 0 {
		f.t.Fatalf("%s: reply %+v, want refused with %q", name, got, want)
	}
}

func (f *lateJoinFixture) pingCheckEnds(name string) []protocol.PingCheckEndPayload {
	f.t.Helper()
	var out []protocol.PingCheckEndPayload
	for _, env := range f.drain(name) {
		if env.Type == protocol.TypePingCheckEnd {
			var end protocol.PingCheckEndPayload
			if err := json.Unmarshal(env.Data, &end); err != nil {
				f.t.Fatalf("bad PING_CHECK_END: %v", err)
			}
			out = append(out, end)
		}
	}
	return out
}

func (f *lateJoinFixture) endedWith(name string, peer uint64) {
	f.t.Helper()
	got := f.pingCheckEnds(name)
	if len(got) != 1 || got[0].PeerUserID != peer {
		f.t.Fatalf("%s: PING_CHECK_END %+v, want one for %d", name, got, peer)
	}
}

func TestHelloTakesThePingCheckCapability(t *testing.T) {
	for _, capable := range []bool{true, false} {
		h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
		c := &Client{hub: h, send: make(chan []byte, 64), ip: "192.168.1.20"}
		raw, _ := json.Marshal(map[string]any{
			"username": "mira", "clientVersion": "vdev-test", "iceSupported": true, "pingCheck": capable,
		})
		if err := h.handleHello(c, protocol.Envelope{Type: protocol.TypeHello, Data: raw}); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if c.user.PingCheck != capable {
			t.Fatalf("HELLO pingCheck %v: user has %v", capable, c.user.PingCheck)
		}
		ok := payloadOf[protocol.HelloOKResponse](t, drainClient(t, c), protocol.TypeHelloOK)
		if !ok.PingCheck {
			t.Fatal("HELLO_OK doesn't offer ping checks")
		}
		// A private address has no geography: no region, so no estimate.
		if ok.Region != "" || c.user.Region != "" {
			t.Fatalf("region %q / %q for a private address, want none", ok.Region, c.user.Region)
		}
	}
}

func drainClient(t *testing.T, c *Client) []protocol.Envelope {
	t.Helper()
	var out []protocol.Envelope
	for {
		select {
		case b := <-c.send:
			var env protocol.Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatalf("bad envelope: %v", err)
			}
			out = append(out, env)
		default:
			return out
		}
	}
}

func TestProbeSignalWithoutAGrantIsDropped(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.pingCheckUser("mira")
	kart := f.pingCheckUser("kart")
	f.drain("kart")

	f.probe("mira", kart.ID)
	f.heardNoProbe("kart")

	// Nor does a room-scoped signal reach them from outside any shared room.
	f.send("mira", protocol.TypeICESignal, protocol.ICESignalRequest{
		TargetUserID: kart.ID, Kind: "candidate", Value: testCandidate,
	})
	f.heardNoProbe("kart")
	_ = mira
}

func TestJoinGrantRelaysProbesBetweenJoinerAndHost(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	host := f.pingCheckUser("host")
	joiner := f.pingCheckUser("joiner")
	f.pingCheckUser("nosy")
	room := f.createRoom("host", 4)
	for _, name := range []string{"host", "joiner", "nosy"} {
		f.drain(name)
	}

	reply := f.requestPingCheck("joiner", room, "")
	if !reply.OK || reply.PeerUserID != host.ID || reply.ExpiresInMs != pingCheckJoinLife.Milliseconds() || reply.Reason != "" {
		t.Fatalf("reply %+v", reply)
	}
	in := payloadOf[protocol.PingCheckIncomingPayload](t, f.drain("host"), protocol.TypePingCheckIncoming)
	if in.FromUserID != joiner.ID || in.ExpiresInMs != pingCheckJoinLife.Milliseconds() {
		t.Fatalf("incoming %+v", in)
	}

	f.probe("joiner", host.ID)
	f.heardOneProbeFrom("host", joiner.ID)
	f.probe("host", joiner.ID)
	f.heardOneProbeFrom("joiner", host.ID)

	// The grant is theirs alone.
	f.probe("nosy", host.ID)
	f.heardNoProbe("host")
	// A probe signal names no room, and is checked like any other.
	f.send("joiner", protocol.TypeICESignal, protocol.ICESignalRequest{
		TargetUserID: host.ID, RoomID: room.ID, Kind: "candidate", Value: testCandidate, Probe: true,
	})
	f.send("joiner", protocol.TypeICESignal, protocol.ICESignalRequest{
		TargetUserID: host.ID, Kind: "candidate", Probe: true,
	})
	f.heardNoProbe("host")

	// Checking changes nothing in the room.
	if joiner.CurrentRoom != nil || len(room.Players) != 1 {
		t.Fatalf("joiner seated: %v, %d players", joiner.CurrentRoom, len(room.Players))
	}
}

func TestPingCheckForAPasswordRoomNeedsThePassword(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	host := f.pingCheckUser("host")
	f.pingCheckUser("joiner")
	f.send("host", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "room", Rom: testRom, MaxPlayers: 4, Prediction: 7, Password: "secret",
	})
	room := host.CurrentRoom
	if room == nil {
		t.Fatal("room wasn't created")
	}
	f.drain("host")

	f.pingCheckRefused("joiner", room, "", protocol.ReasonWrongPassword)
	f.pingCheckRefused("joiner", room, "Secret", protocol.ReasonWrongPassword)
	if envs := f.drain("host"); len(envs) != 0 {
		t.Fatalf("the host heard about it: %+v", envs)
	}
	if n := f.pingGrantCount(); n != 0 {
		t.Fatalf("%d grants", n)
	}
	f.probe("joiner", host.ID)
	f.heardNoProbe("host")

	if reply := f.requestPingCheck("joiner", room, "secret"); !reply.OK || reply.PeerUserID != host.ID {
		t.Fatalf("right password: %+v", reply)
	}
}

func TestPingCheckRefusals(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.pingCheckUser("host")
	f.pingCheckUser("guest")
	f.pingCheckUser("kicked")
	f.pingCheckUser("laptop").Connection = "wifi"
	f.pingCheckUser("spammer")
	old := f.socialUser("oldclient")
	f.pingCheckUser("oldhost").PingCheck = false

	room := f.createRoom("host", 2)
	f.send("kicked", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.send("host", protocol.TypeRoomKick, protocol.RoomKickRequest{UserID: f.users["kicked"].ID})
	f.pingCheckRefused("kicked", room, "", protocol.ReasonKickedFromRoom)

	f.send("host", protocol.TypeRoomUpdateJoinLimits, protocol.RoomUpdateJoinLimitsRequest{MaxPing: 80, WiredOnly: true})
	f.pingCheckRefused("laptop", room, "", protocol.ReasonWiredOnly)

	f.pingCheckRefused("oldclient", room, "", protocol.ReasonPingCheckUnavailable)
	_ = old

	// Someone in the room doesn't need one.
	f.send("guest", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	if f.users["guest"].CurrentRoom != room {
		t.Fatal("guest didn't join")
	}
	f.pingCheckRefused("guest", room, "", protocol.ReasonAlreadyInRoom)
	f.pingCheckRefused("spammer", room, "", protocol.ReasonRoomFull)

	gone := &state.Room{ID: 999}
	f.pingCheckRefused("spammer", gone, "", protocol.ReasonRoomNotFound)

	oldRoom := f.createRoom("oldhost", 4)
	f.pingCheckRefused("spammer", oldRoom, "", protocol.ReasonPingCheckUnavailable)

	// A challenge's room is private.
	f.pingCheckUser("mira")
	f.pingCheckUser("kart")
	in := f.challenge("mira", "kart")
	f.respond("kart", in.ChallengeID, true)
	f.pingCheckRefused("spammer", f.users["mira"].CurrentRoom, "", protocol.ReasonPrivateRoom)

	// Each request pokes the host, so a connection only gets so many. (One
	// connection: the fixture's send makes a new one each time.)
	c := f.client("spammer")
	raw, _ := json.Marshal(protocol.PingCheckRequest{RoomID: oldRoom.ID})
	var last protocol.PingCheckReplyPayload
	for i := 0; i <= pingCheckBurst; i++ {
		f.h.dispatch(c, protocol.Envelope{Type: protocol.TypePingCheckRequest, ID: strconv.Itoa(i), Data: raw})
		last = payloadOf[protocol.PingCheckReplyPayload](t, f.drain("spammer"), protocol.TypePingCheckReply)
		if i < pingCheckBurst && last.Reason != protocol.ReasonPingCheckUnavailable {
			t.Fatalf("request %d: %+v", i, last)
		}
	}
	if last.OK || last.Reason != protocol.ReasonTooFast {
		t.Fatalf("over the limit: %+v", last)
	}
}

func TestChallengeGrantEndsWithTheChallenge(t *testing.T) {
	saved := pingCheckGrace
	pingCheckGrace = 30 * time.Millisecond
	defer func() { pingCheckGrace = saved }()

	f := newLateJoinFixture(t, 4)
	mira := f.pingCheckUser("mira")
	kart := f.pingCheckUser("kart")
	f.drain("kart")

	f.send("mira", protocol.TypeChallengeSend, protocol.ChallengeSendRequest{ToUserID: kart.ID, Rom: testRom})
	sent := payloadOf[protocol.ChallengeSentPayload](t, f.drain("mira"), protocol.TypeChallengeSent)
	in := payloadOf[protocol.ChallengeIncomingPayload](t, f.drain("kart"), protocol.TypeChallengeIncoming)
	if sent.PingPeerUserID != kart.ID || in.PingPeerUserID != mira.ID {
		t.Fatalf("ping peers: sent %d, incoming %d", sent.PingPeerUserID, in.PingPeerUserID)
	}
	f.probe("mira", kart.ID)
	f.heardOneProbeFrom("kart", mira.ID)
	f.probe("kart", mira.ID)
	f.heardOneProbeFrom("mira", kart.ID)

	f.send("mira", protocol.TypeChallengeCancel, protocol.ChallengeCancelRequest{ChallengeID: sent.ChallengeID})
	f.drain("kart")
	// The grace lets a last sample land.
	f.probe("mira", kart.ID)
	f.heardOneProbeFrom("kart", mira.ID)

	time.Sleep(150 * time.Millisecond)
	f.endedWith("mira", kart.ID)
	f.endedWith("kart", mira.ID)
	if n := f.pingGrantCount(); n != 0 {
		t.Fatalf("%d grants left", n)
	}
	f.probe("mira", kart.ID)
	f.heardNoProbe("kart")
}

func TestAcceptedChallengeGrantEndsAfterTheGrace(t *testing.T) {
	saved := pingCheckGrace
	pingCheckGrace = 30 * time.Millisecond
	defer func() { pingCheckGrace = saved }()

	f := newLateJoinFixture(t, 4)
	mira := f.pingCheckUser("mira")
	kart := f.pingCheckUser("kart")
	in := f.challenge("mira", "kart")
	f.respond("kart", in.ChallengeID, true)
	if kart.CurrentRoom == nil {
		t.Fatal("not seated")
	}
	f.drain("mira")
	f.drain("kart")

	time.Sleep(150 * time.Millisecond)
	f.endedWith("mira", kart.ID)
	f.endedWith("kart", mira.ID)
}

func TestChallengeWithAnOlderClientHasNoPingCheck(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.pingCheckUser("mira")
	old := f.socialUser("oldclient")

	f.send("mira", protocol.TypeChallengeSend, protocol.ChallengeSendRequest{ToUserID: old.ID, Rom: testRom})
	if sent := payloadOf[protocol.ChallengeSentPayload](t, f.drain("mira"), protocol.TypeChallengeSent); sent.PingPeerUserID != 0 {
		t.Fatalf("ping peer %d", sent.PingPeerUserID)
	}
	if in := payloadOf[protocol.ChallengeIncomingPayload](t, f.drain("oldclient"), protocol.TypeChallengeIncoming); in.PingPeerUserID != 0 {
		t.Fatalf("ping peer %d", in.PingPeerUserID)
	}
	if n := f.pingGrantCount(); n != 0 {
		t.Fatalf("%d grants", n)
	}
}

func TestSamePCGetsNoGrant(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.pingCheckUser("mira")
	sam := f.addLocal("mira", "sam")
	f.drain("mira")

	f.refusedWith("mira", "sam", protocol.ChallengeUnavailable)
	if n := f.pingGrantCount(); n != 0 {
		t.Fatalf("%d grants", n)
	}
	if f.h.pingChecks.open(pingSide{pc: mira, alias: mira.ID}, pingSide{pc: mira, alias: sam.ID}, 0, time.Minute) {
		t.Fatal("a PC got a grant with itself")
	}
	f.probe("mira", sam.ID)
	f.heardNoProbe("mira")

	// Nor with the room it's in.
	room := f.createRoom("mira", 4)
	f.pingCheckRefused("mira", room, "", protocol.ReasonAlreadyInRoom)
}

func TestPingCheckRoutingKeepsLocalPlayersPrivate(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.pingCheckUser("mira")
	sam := f.addLocal("mira", "sam")
	kart := f.pingCheckUser("kart")
	f.drain("mira")
	f.drain("kart")

	// kart knows the other PC as sam only.
	f.send("kart", protocol.TypeChallengeSend, protocol.ChallengeSendRequest{ToUserID: sam.ID, Rom: testRom})
	sent := payloadOf[protocol.ChallengeSentPayload](t, f.drain("kart"), protocol.TypeChallengeSent)
	in := payloadOf[protocol.ChallengeIncomingPayload](t, f.drain("mira"), protocol.TypeChallengeIncoming)
	if sent.PingPeerUserID != sam.ID || in.PingPeerUserID != kart.ID {
		t.Fatalf("ping peers: sent %d (sam %d), incoming %d", sent.PingPeerUserID, sam.ID, in.PingPeerUserID)
	}

	f.probe("kart", sam.ID)
	f.heardOneProbeFrom("mira", kart.ID)
	// mira's answers reach kart as sam's, never as mira's.
	f.probe("mira", kart.ID)
	f.heardOneProbeFrom("kart", sam.ID)
	// The PC's other name gets kart nowhere, and says nothing.
	f.probe("kart", mira.ID)
	f.heardNoProbe("mira")

	// A second grant under other names would show both reach the same
	// addresses, so a ping check for mira's room is refused like any other.
	room := f.createRoom("mira", 4)
	f.drain("mira")
	refused := f.requestPingCheck("kart", room, "")
	if refused.OK || refused.Reason != protocol.ReasonPingCheckUnavailable {
		t.Fatalf("second grant: %+v", refused)
	}
	if envs := f.drain("mira"); len(envs) != 0 {
		t.Fatalf("mira heard about it: %+v", envs)
	}
	f.send("mira", protocol.TypeRoomLeave, struct{}{})
	f.drain("mira")
	f.drain("kart")

	// sam leaving the roster ends the check at once, still under sam's name.
	f.send("mira", protocol.TypeLocalPlayerRemove, protocol.LocalPlayerRemoveRequest{UserID: sam.ID})
	kartSaw := f.drain("kart")
	if got := payloadOf[protocol.ChallengeResultPayload](t, kartSaw, protocol.TypeChallengeResult); got.ToUserID != sam.ID {
		t.Fatalf("result %+v", got)
	}
	var ends []protocol.PingCheckEndPayload
	for _, env := range kartSaw {
		if env.Type == protocol.TypePingCheckEnd {
			var end protocol.PingCheckEndPayload
			if err := json.Unmarshal(env.Data, &end); err != nil {
				t.Fatal(err)
			}
			ends = append(ends, end)
		}
	}
	if len(ends) != 1 || ends[0].PeerUserID != sam.ID {
		t.Fatalf("kart's PING_CHECK_ENDs %+v, want one naming sam (%d)", ends, sam.ID)
	}
	f.endedWith("mira", kart.ID)
	if n := f.pingGrantCount(); n != 0 {
		t.Fatalf("%d grants left", n)
	}
}

func TestDisconnectEndsPingChecksAtOnce(t *testing.T) {
	saved := pingCheckJoinLife
	pingCheckJoinLife = 40 * time.Millisecond
	defer func() { pingCheckJoinLife = saved }()

	f := newLateJoinFixture(t, 4)
	host := f.pingCheckUser("host")
	joiner := f.pingCheckUser("joiner")
	room := f.createRoom("host", 4)
	if reply := f.requestPingCheck("joiner", room, ""); !reply.OK {
		t.Fatalf("reply %+v", reply)
	}
	f.drain("host")

	f.h.cleanupClient(f.client("joiner"))
	f.endedWith("host", joiner.ID)
	if n := f.pingGrantCount(); n != 0 {
		t.Fatalf("%d grants left", n)
	}
	// No timer is left to fire for the dead connection.
	time.Sleep(120 * time.Millisecond)
	if ends := f.pingCheckEnds("host"); len(ends) != 0 {
		t.Fatalf("a timer fired after the disconnect: %+v", ends)
	}
	if ends := f.pingCheckEnds("joiner"); len(ends) != 0 {
		t.Fatalf("the dead connection was sent %+v", ends)
	}
	_ = host
}

func TestJoinGrantExpires(t *testing.T) {
	saved := pingCheckJoinLife
	pingCheckJoinLife = 30 * time.Millisecond
	defer func() { pingCheckJoinLife = saved }()

	f := newLateJoinFixture(t, 4)
	host := f.pingCheckUser("host")
	joiner := f.pingCheckUser("joiner")
	room := f.createRoom("host", 4)
	if reply := f.requestPingCheck("joiner", room, ""); !reply.OK {
		t.Fatalf("reply %+v", reply)
	}
	f.drain("host")

	time.Sleep(150 * time.Millisecond)
	f.endedWith("host", joiner.ID)
	f.endedWith("joiner", host.ID)
	f.probe("joiner", host.ID)
	f.heardNoProbe("host")
}

func TestProbeSignalsPerGrantAreCapped(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	host := f.pingCheckUser("host")
	f.pingCheckUser("joiner")
	room := f.createRoom("host", 4)
	if reply := f.requestPingCheck("joiner", room, ""); !reply.OK {
		t.Fatalf("reply %+v", reply)
	}
	f.drain("host")
	heard := 0
	for i := 0; i < maxProbeSignals+10; i++ {
		f.probe("joiner", host.ID)
		heard += len(f.probesHeard("host"))
	}
	if heard != maxProbeSignals {
		t.Fatalf("host heard %d signals, want %d", heard, maxProbeSignals)
	}
}

func (f *lateJoinFixture) joinMeasured(name string, room *state.Room, pingMs int, measured bool) []protocol.Envelope {
	f.t.Helper()
	f.drain(name)
	f.send(name, protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID, PingMs: pingMs, PingMeasured: measured})
	return f.drain(name)
}

func TestPingMeasuredJoinRule(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.pingCheckUser("host")
	f.pingCheckUser("measured")
	f.pingCheckUser("estimated")
	f.socialUser("oldclient")
	f.socialUser("oldmeasured")
	f.send("host", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "room", Rom: testRom, MaxPlayers: 4, Prediction: 7, MaxPing: 100,
	})
	room := f.users["host"].CurrentRoom
	if room == nil {
		t.Fatal("room wasn't created")
	}

	// A measured ping over the limit is refused.
	if got := reasonOf(t, f.joinMeasured("measured", room, 140, true), protocol.TypeRoomJoinFail); got != protocol.ReasonPingTooHigh {
		t.Fatalf("measured: reason %q", got)
	}
	// A client with ping checks isn't turned away on an estimate alone.
	if _, ok := findEnvelope(f.joinMeasured("estimated", room, 140, false), protocol.TypeRoomJoinOK); !ok {
		t.Fatal("an estimate turned a ping check client away")
	}
	// An older client is held to its number as sent, whatever it says about it.
	if got := reasonOf(t, f.joinMeasured("oldclient", room, 140, false), protocol.TypeRoomJoinFail); got != protocol.ReasonPingTooHigh {
		t.Fatalf("oldclient: reason %q", got)
	}
	if got := reasonOf(t, f.joinMeasured("oldmeasured", room, 140, true), protocol.TypeRoomJoinFail); got != protocol.ReasonPingTooHigh {
		t.Fatalf("oldmeasured: reason %q", got)
	}
	// Within the limit, measured, gets in.
	if _, ok := findEnvelope(f.joinMeasured("measured", room, 100, true), protocol.TypeRoomJoinOK); !ok {
		t.Fatal("a measured ping at the limit was turned away")
	}
}
