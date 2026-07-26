package protocol

import (
	"encoding/json"
	"testing"
)

func TestHelloRequestLocalPortRoundTrip(t *testing.T) {
	want := HelloRequest{
		Username:      "alice",
		ClientVersion: "test",
		RomHashes:     []string{"abc"},
		LocalIP:       "192.168.1.20",
		LocalPort:     45000,
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal HELLO: %v", err)
	}

	var got HelloRequest
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal HELLO: %v", err)
	}
	if got.LocalIP != want.LocalIP || got.LocalPort != want.LocalPort {
		t.Fatalf("local endpoint mismatch: got %s:%d, want %s:%d",
			got.LocalIP, got.LocalPort, want.LocalIP, want.LocalPort)
	}
}

func TestMatchPeerIncludesLocalPort(t *testing.T) {
	peer := MatchPeer{
		UserID:     7,
		Username:   "bob",
		PublicIP:   "203.0.113.8",
		PublicPort: 52000,
		LocalIP:    "192.168.1.21",
		LocalPort:  45001,
		Slot:       2,
	}

	encoded, err := json.Marshal(peer)
	if err != nil {
		t.Fatalf("marshal MATCH_BEGIN peer: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode MATCH_BEGIN peer: %v", err)
	}
	if got := uint16(fields["localPort"].(float64)); got != peer.LocalPort {
		t.Fatalf("localPort = %d, want %d", got, peer.LocalPort)
	}
}
