package ws

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

func waitViewerCount(t *testing.T, c *Client, matchID uint64, want int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("decode viewer-count envelope: %v", err)
			}
			if env.Type != protocol.TypeBroadcastViewerCount {
				continue
			}
			var payload protocol.BroadcastViewerCountPayload
			if err := json.Unmarshal(env.Data, &payload); err != nil {
				t.Fatalf("decode viewer-count payload: %v", err)
			}
			if payload.MatchID != matchID || payload.ViewerCount != want {
				t.Fatalf("viewer count = match %d/count %d, want match %d/count %d",
					payload.MatchID, payload.ViewerCount, matchID, want)
			}
			return
		case <-deadline:
			t.Fatalf("timed out waiting for viewer count %d", want)
		}
	}
}

func TestViewerCountTracksJoinStopAndDisconnect(t *testing.T) {
	const matchID uint64 = 77
	broadcaster := &Client{user: &state.User{ID: 10, Username: "host"}, send: make(chan []byte, 16)}
	player := &Client{user: &state.User{ID: 13, Username: "player"}, send: make(chan []byte, 16)}
	viewer1 := &Client{user: &state.User{ID: 11, Username: "viewer-one"}, send: make(chan []byte, 16)}
	viewer2 := &Client{user: &state.User{ID: 12, Username: "viewer-two"}, send: make(chan []byte, 16)}
	h := &Hub{clients: map[uint64]*Client{
		broadcaster.user.ID: broadcaster,
		player.user.ID:      player,
		viewer1.user.ID:     viewer1,
		viewer2.user.ID:     viewer2,
	}}
	m := &broadcastManager{
		hub:     h,
		dir:     t.TempDir(),
		byMatch: make(map[uint64]*broadcast),
	}
	h.broadcasts = m
	if !m.begin(matchID, 1, broadcaster.user.ID, []uint64{broadcaster.user.ID, player.user.ID}) {
		t.Fatal("begin broadcast")
	}
	t.Cleanup(func() { m.endByMatch(matchID) })
	m.notifyViewerCount(matchID)
	waitViewerCount(t, broadcaster, matchID, 0)
	waitViewerCount(t, player, matchID, 0)

	m.spectateStart(viewer1, matchID)
	waitViewerCount(t, broadcaster, matchID, 1)
	waitViewerCount(t, player, matchID, 1)
	waitViewerCount(t, viewer1, matchID, 1)

	m.spectateStart(viewer2, matchID)
	waitViewerCount(t, broadcaster, matchID, 2)
	waitViewerCount(t, player, matchID, 2)
	waitViewerCount(t, viewer1, matchID, 2)
	waitViewerCount(t, viewer2, matchID, 2)

	m.spectateStop(viewer1.user.ID, matchID)
	waitViewerCount(t, broadcaster, matchID, 1)
	waitViewerCount(t, player, matchID, 1)
	waitViewerCount(t, viewer2, matchID, 1)

	m.onUserGone(viewer2.user.ID)
	waitViewerCount(t, broadcaster, matchID, 0)
	waitViewerCount(t, player, matchID, 0)

	if !m.endByMatch(matchID) {
		t.Fatal("end broadcast")
	}
	waitViewerCount(t, broadcaster, matchID, 0)
	waitViewerCount(t, player, matchID, 0)
}

func TestKeyframeAssemblyAcceptsManyPacedChunks(t *testing.T) {
	m := &broadcastManager{
		dir:     t.TempDir(),
		byMatch: make(map[uint64]*broadcast),
	}
	const matchID uint64 = 88
	const broadcasterID uint64 = 99
	if !m.begin(matchID, 1, broadcasterID, nil) {
		t.Fatal("begin broadcast")
	}
	t.Cleanup(func() { m.endByMatch(matchID) })

	// A raw 16 MiB fallback uses about 257 of the client's new 64 KiB chunks,
	// well beyond the legacy 64-chunk shape limit.
	const chunks = 257
	var want []byte
	for i := 0; i < chunks; i++ {
		part := []byte{byte(i), byte(i >> 8)}
		want = append(want, part...)
		m.keyframe(broadcasterID, matchID, 123, i, chunks,
			base64.StdEncoding.EncodeToString(part))
	}

	b := m.byMatch[matchID]
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.keyframeFrame != 123 || !bytes.Equal(b.keyframeData, want) {
		t.Fatal("paced keyframe chunks were not reassembled")
	}
}

func TestSpectatePumpRetriesFullQueueWithoutGap(t *testing.T) {
	m := &broadcastManager{
		dir:     t.TempDir(),
		byMatch: make(map[uint64]*broadcast),
	}
	const matchID uint64 = 11
	const broadcasterID uint64 = 22
	if !m.begin(matchID, 1, broadcasterID, nil) {
		t.Fatal("begin broadcast")
	}
	b := m.byMatch[matchID]
	t.Cleanup(func() {
		m.spectateStop(33, matchID)
		b.mu.Lock()
		if b.file != nil {
			_ = b.file.Close()
		}
		_ = os.Remove(b.path)
		b.mu.Unlock()
	})

	history := make([]byte, spectateDataChunkBytes*4+123)
	for i := range history {
		history[i] = byte(i % 251)
	}
	m.data(broadcasterID, matchID, base64.StdEncoding.EncodeToString(history), 1000)

	// Capacity two forces the pump to stop after SPECTATE_BEGIN plus one data
	// message. It must retain its cursor and retry after the test drains a slot.
	c := &Client{
		user: &state.User{ID: 33, Username: "viewer"},
		send: make(chan []byte, 2),
	}
	m.spectateStart(c, matchID)
	time.Sleep(3 * spectateRetryInterval)
	b.mu.Lock()
	stalledOffset := b.spectators[33].streamOffset
	b.mu.Unlock()
	if stalledOffset >= int64(len(history)) {
		t.Fatal("history pump advanced through a full client queue")
	}

	var got []byte
	var expectedOffset int64
	beginSeen := false
	deadline := time.After(3 * time.Second)
	for len(got) < len(history) {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			switch env.Type {
			case protocol.TypeSpectateBegin:
				if beginSeen {
					t.Fatal("duplicate SPECTATE_BEGIN")
				}
				beginSeen = true
			case protocol.TypeSpectateData:
				if !beginSeen {
					t.Fatal("data arrived before SPECTATE_BEGIN")
				}
				var payload protocol.SpectateDataPayload
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					t.Fatalf("decode data payload: %v", err)
				}
				if payload.Offset != expectedOffset {
					t.Fatalf("offset = %d, want %d", payload.Offset, expectedOffset)
				}
				chunk, err := base64.StdEncoding.DecodeString(payload.Data)
				if err != nil {
					t.Fatalf("decode replay bytes: %v", err)
				}
				got = append(got, chunk...)
				expectedOffset += int64(len(chunk))
			default:
				t.Fatalf("unexpected envelope %q", env.Type)
			}
		case <-deadline:
			t.Fatalf("timed out after %d/%d replay bytes", len(got), len(history))
		}
	}
	if !bytes.Equal(got, history) {
		t.Fatal("reconstructed history differs from spool")
	}

	live := []byte("ordered-live-tail")
	m.data(broadcasterID, matchID, base64.StdEncoding.EncodeToString(live), 1001)
	select {
	case raw := <-c.send:
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode live envelope: %v", err)
		}
		var payload protocol.SpectateDataPayload
		if err := json.Unmarshal(env.Data, &payload); err != nil {
			t.Fatalf("decode live payload: %v", err)
		}
		if payload.Offset != expectedOffset {
			t.Fatalf("live offset = %d, want %d", payload.Offset, expectedOffset)
		}
		chunk, err := base64.StdEncoding.DecodeString(payload.Data)
		if err != nil || !bytes.Equal(chunk, live) {
			t.Fatalf("live data mismatch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for live replay tail")
	}
}

func TestKeyframeSpectateStreamHasContiguousLogicalOffsets(t *testing.T) {
	m := &broadcastManager{
		dir:     t.TempDir(),
		byMatch: make(map[uint64]*broadcast),
	}
	const matchID uint64 = 44
	const broadcasterID uint64 = 55
	if !m.begin(matchID, 1, broadcasterID, nil) {
		t.Fatal("begin broadcast")
	}
	b := m.byMatch[matchID]
	t.Cleanup(func() {
		m.spectateStop(66, matchID)
		b.mu.Lock()
		if b.file != nil {
			_ = b.file.Close()
		}
		_ = os.Remove(b.path)
		b.mu.Unlock()
	})

	krec := make([]byte, 400)
	copy(krec, "KRC1")
	for i := byte(0); i < 5; i++ {
		krec = append(krec, 0x12, 0x01, 0x00, i)
	}
	m.data(broadcasterID, matchID, base64.StdEncoding.EncodeToString(krec), 5)
	tailStart := krecFrameOffset(krec, 2)
	if tailStart < 0 {
		t.Fatal("locate keyframe input")
	}
	// A state captured after the latest complete input legitimately points to
	// the next record at EOF; live input appended later must continue from there.
	if got := krecFrameOffset(krec, 5); got != int64(len(krec)) {
		t.Fatalf("EOF keyframe offset = %d, want %d", got, len(krec))
	}
	keyframe := bytes.Repeat([]byte{0xA5}, spectateDataChunkBytes+17)
	b.mu.Lock()
	b.keyframeFrame = 2
	b.keyframeData = keyframe
	b.mu.Unlock()

	c := &Client{
		user: &state.User{ID: 66, Username: "keyframe-viewer"},
		send: make(chan []byte, 4),
	}
	m.spectateStart(c, matchID)

	wantKrec := append(append([]byte{}, krec[:400]...), krec[tailStart:]...)
	var gotKrec, gotKeyframe []byte
	var expectedOffset int64
	beginSeen := false
	dataSeen := false
	deadline := time.After(3 * time.Second)
	for len(gotKrec) < len(wantKrec) || len(gotKeyframe) < len(keyframe) {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			switch env.Type {
			case protocol.TypeSpectateBegin:
				beginSeen = true
			case protocol.TypeSpectateKeyframe:
				if !beginSeen || dataSeen {
					t.Fatal("keyframe envelope arrived outside the begin/data boundary")
				}
				var payload protocol.SpectateKeyframePayload
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					t.Fatalf("decode keyframe payload: %v", err)
				}
				chunk, err := base64.StdEncoding.DecodeString(payload.Data)
				if err != nil {
					t.Fatalf("decode keyframe data: %v", err)
				}
				gotKeyframe = append(gotKeyframe, chunk...)
			case protocol.TypeSpectateData:
				dataSeen = true
				var payload protocol.SpectateDataPayload
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					t.Fatalf("decode data payload: %v", err)
				}
				if payload.Offset != expectedOffset {
					t.Fatalf("offset = %d, want %d", payload.Offset, expectedOffset)
				}
				chunk, err := base64.StdEncoding.DecodeString(payload.Data)
				if err != nil {
					t.Fatalf("decode replay bytes: %v", err)
				}
				gotKrec = append(gotKrec, chunk...)
				expectedOffset += int64(len(chunk))
			default:
				t.Fatalf("unexpected envelope %q", env.Type)
			}
		case <-deadline:
			t.Fatal("timed out waiting for keyframe stream")
		}
	}
	if !bytes.Equal(gotKeyframe, keyframe) {
		t.Fatal("reconstructed keyframe differs")
	}
	if !bytes.Equal(gotKrec, wantKrec) {
		t.Fatal("keyframe krec header/tail differs")
	}
}
