package ws

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

func declineReason(t *testing.T, c *Client) string {
	t.Helper()
	msgs := drain(c, protocol.TypeReplayDecline)
	if len(msgs) != 1 {
		t.Fatalf("%s got %d REPLAY_DECLINE", c.user.Username, len(msgs))
	}
	var p protocol.ReplayDeclinePayload
	json.Unmarshal(msgs[0].Data, &p)
	return p.Reason
}

// ratedSet plays a set to a rated result and returns its id.
func ratedSet(t *testing.T, f *rankedFixture) (*Client, *Client, int64) {
	t.Helper()
	host, guest, matchID := startRankedMatch(t, f)
	f.hub.handleMatchResult(host, envelope(t, result(matchID, 1, []int{2, 0})))
	f.hub.handleMatchResult(guest, envelope(t, result(matchID, 1, []int{2, 0})))
	res := rankedResultFor(t, host)
	drain(guest, protocol.TypeRankedResult)
	if res.SetID == 0 {
		t.Fatal("RANKED_RESULT has no set id")
	}
	return host, guest, res.SetID
}

func TestReplayUpload(t *testing.T) {
	f := newRankedFixture(t)
	dir := t.TempDir()
	if err := f.hub.SetReplayDir(dir); err != nil {
		t.Fatal(err)
	}
	host, guest, setID := ratedSet(t, f)

	data := bytes.Repeat([]byte("krec"), (replayChunkBytes+1000)/4) // two chunks
	sum := sha256.Sum256(data)
	offer := protocol.ReplayOfferRequest{SetID: setID, Size: len(data), SHA256: hex.EncodeToString(sum[:]),
		Bookmarks: []protocol.ReplayBookmark{{Record: 10, Label: "Game 1", Kind: "game_start", Game: 1}}}

	outsider := f.connect("lurker", true)
	f.hub.handleReplayOffer(outsider, envelope(t, offer))
	if r := declineReason(t, outsider); r != "not_participant" {
		t.Fatalf("outsider offer = %s", r)
	}

	f.hub.handleReplayOffer(host, envelope(t, offer))
	if len(drain(host, protocol.TypeReplayAccept)) != 1 {
		t.Fatal("host offer not accepted")
	}
	f.hub.handleReplayOffer(guest, envelope(t, offer))
	if r := declineReason(t, guest); r != "in_progress" {
		t.Fatalf("second offer = %s", r)
	}

	for i := 0; i < 2; i++ {
		end := min((i+1)*replayChunkBytes, len(data))
		f.hub.handleReplayChunk(host, envelope(t, protocol.ReplayChunkRequest{SetID: setID, Index: i, Count: 2,
			Data: base64.StdEncoding.EncodeToString(data[i*replayChunkBytes : end])}))
	}
	if len(drain(host, protocol.TypeReplayStored)) != 1 {
		t.Fatal("replay not stored")
	}
	stored, err := os.ReadFile(accounts.ReplayFile(dir, setID))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("stored file mismatch: %v", err)
	}
	info, err := f.store.Replay(setID)
	if err != nil || info.Size != int64(len(data)) || info.Bookmarks == "[]" {
		t.Fatalf("replay info = %+v, %v", info, err)
	}

	f.hub.handleReplayOffer(guest, envelope(t, offer))
	if r := declineReason(t, guest); r != "exists" {
		t.Fatalf("offer after store = %s", r)
	}
}

func TestReplayUploadRejectsBadHash(t *testing.T) {
	f := newRankedFixture(t)
	dir := t.TempDir()
	f.hub.SetReplayDir(dir)
	host, _, setID := ratedSet(t, f)

	data := []byte("not what was offered")
	f.hub.handleReplayOffer(host, envelope(t, protocol.ReplayOfferRequest{SetID: setID, Size: len(data),
		SHA256: hex.EncodeToString(make([]byte, 32))}))
	drain(host, protocol.TypeReplayAccept)
	f.hub.handleReplayChunk(host, envelope(t, protocol.ReplayChunkRequest{SetID: setID, Index: 0, Count: 1,
		Data: base64.StdEncoding.EncodeToString(data)}))
	if r := declineReason(t, host); r != "failed" {
		t.Fatalf("bad hash = %s", r)
	}
	if _, err := os.Stat(accounts.ReplayFile(dir, setID)); !os.IsNotExist(err) {
		t.Fatal("rejected replay was written")
	}
	if _, err := f.store.Replay(setID); err == nil {
		t.Fatal("rejected replay was recorded")
	}
}
