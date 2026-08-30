package ws

import (
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

func TestValidICESignalRequest(t *testing.T) {
	tests := []struct {
		name string
		req  protocol.ICESignalRequest
		want bool
	}{
		{"description", protocol.ICESignalRequest{Kind: "description", Value: "ufrag"}, true},
		{"empty description", protocol.ICESignalRequest{Kind: "description"}, false},
		{"oversized description", protocol.ICESignalRequest{Kind: "description", Value: strings.Repeat("x", 4097)}, false},
		{"candidate", protocol.ICESignalRequest{Kind: "candidate", Value: "candidate"}, true},
		{"empty candidate", protocol.ICESignalRequest{Kind: "candidate"}, false},
		{"gathering done", protocol.ICESignalRequest{Kind: "gathering_done"}, true},
		{"gathering done value", protocol.ICESignalRequest{Kind: "gathering_done", Value: "unexpected"}, false},
		{"restart", protocol.ICESignalRequest{Kind: "restart", Generation: 1}, true},
		{"restart generation zero", protocol.ICESignalRequest{Kind: "restart"}, false},
		{"restart value", protocol.ICESignalRequest{Kind: "restart", Value: "unexpected", Generation: 1}, false},
		{"unknown", protocol.ICESignalRequest{Kind: "unknown"}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validICESignalRequest(test.req); got != test.want {
				t.Fatalf("validICESignalRequest(%+v) = %v, want %v", test.req, got, test.want)
			}
		})
	}
}
