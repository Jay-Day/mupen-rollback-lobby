package udp

import (
	"encoding/binary"
	"testing"
)

func TestLocalPortFromPacket(t *testing.T) {
	base := make([]byte, minPacketLen)

	if port, ok := localPortFromPacket(base); ok || port != 0 {
		t.Fatalf("legacy packet returned port=%d ok=%v", port, ok)
	}

	shortExtension := append(append([]byte(nil), base...), 0x12)
	if port, ok := localPortFromPacket(shortExtension); ok || port != 0 {
		t.Fatalf("short extension returned port=%d ok=%v", port, ok)
	}

	extended := make([]byte, extendedPacketLen)
	binary.BigEndian.PutUint16(extended[minPacketLen:], 45000)
	if port, ok := localPortFromPacket(extended); !ok || port != 45000 {
		t.Fatalf("extended packet returned port=%d ok=%v, want 45000/true", port, ok)
	}

	binary.BigEndian.PutUint16(extended[minPacketLen:], 0)
	if port, ok := localPortFromPacket(extended); ok || port != 0 {
		t.Fatalf("zero port returned port=%d ok=%v", port, ok)
	}
}
