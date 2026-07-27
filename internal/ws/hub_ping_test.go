package ws

import (
	"encoding/json"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

func TestPingProbePrefersLANForSamePublicIP(t *testing.T) {
	h, requester, target, replies := newPingProbeTest(t)
	requester.PublicIP = "203.0.113.9"
	target.PublicIP = requester.PublicIP

	h.handlePingProbeRequest(&Client{hub: h, user: requester, send: replies}, pingProbeEnvelope(target.ID))
	got := readPingProbeReply(t, replies)

	if got.TargetEndpoint != "192.168.1.21:45001" {
		t.Fatalf("preferred endpoint = %q, want LAN endpoint", got.TargetEndpoint)
	}
	if got.PublicEndpoint != "203.0.113.9:52001" {
		t.Fatalf("public endpoint = %q", got.PublicEndpoint)
	}
	if got.LocalEndpoint != "192.168.1.21:45001" {
		t.Fatalf("local endpoint = %q", got.LocalEndpoint)
	}
}

func TestPingProbePrefersPublicForDifferentPublicIP(t *testing.T) {
	h, requester, target, replies := newPingProbeTest(t)
	requester.PublicIP = "198.51.100.10"
	target.PublicIP = "203.0.113.9"

	h.handlePingProbeRequest(&Client{hub: h, user: requester, send: replies}, pingProbeEnvelope(target.ID))
	got := readPingProbeReply(t, replies)

	if got.TargetEndpoint != "203.0.113.9:52001" {
		t.Fatalf("preferred endpoint = %q, want public endpoint", got.TargetEndpoint)
	}
}

func newPingProbeTest(t *testing.T) (*Hub, *state.User, *state.User, chan []byte) {
	t.Helper()
	s := state.New()
	h := NewHub(s, "127.0.0.1:6364", nil)
	replies := make(chan []byte, 1)
	requester := &state.User{ID: 1, Username: "alice"}
	target := &state.User{
		ID:        2,
		Username:  "bob",
		LocalIP:   "192.168.1.21",
		LocalPort: 45001,
	}
	target.SetUDPEndpoint("203.0.113.9", 52001)
	if err := s.AddUser(requester); err != nil {
		t.Fatalf("add requester: %v", err)
	}
	if err := s.AddUser(target); err != nil {
		t.Fatalf("add target: %v", err)
	}
	return h, requester, target, replies
}

func pingProbeEnvelope(targetID uint64) protocol.Envelope {
	data, _ := json.Marshal(protocol.PingProbeRequestPayload{TargetUserID: targetID})
	return protocol.Envelope{Type: protocol.TypePingProbeRequest, Data: data}
}

func readPingProbeReply(t *testing.T, replies <-chan []byte) protocol.PingProbeReplyPayload {
	t.Helper()
	var env protocol.Envelope
	if err := json.Unmarshal(<-replies, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var payload protocol.PingProbeReplyPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}

func TestPingProbeStartsReciprocalProbe(t *testing.T) {
	h, requester, target, requesterReplies := newPingProbeTest(t)
	requester.PublicIP = "198.51.100.10"
	requester.LocalIP = "10.0.0.11"
	requester.LocalPort = 44000
	requester.SetUDPEndpoint("198.51.100.10", 51000)
	target.PublicIP = "203.0.113.9"

	targetReplies := make(chan []byte, 1)
	h.clients[target.ID] = &Client{hub: h, user: target, send: targetReplies}

	h.handlePingProbeRequest(&Client{hub: h, user: requester, send: requesterReplies}, pingProbeEnvelope(target.ID))
	_ = readPingProbeReply(t, requesterReplies)
	reciprocal := readPingProbeReply(t, targetReplies)

	if reciprocal.TargetUserID != requester.ID {
		t.Fatalf("reciprocal target user = %d, want %d", reciprocal.TargetUserID, requester.ID)
	}
	if reciprocal.TargetEndpoint != "198.51.100.10:51000" {
		t.Fatalf("reciprocal preferred endpoint = %q", reciprocal.TargetEndpoint)
	}
	if reciprocal.PublicEndpoint != "198.51.100.10:51000" {
		t.Fatalf("reciprocal public endpoint = %q", reciprocal.PublicEndpoint)
	}
}
