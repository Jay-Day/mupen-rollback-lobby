package udp

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	magic0 byte = 'R'
	magic1 byte = 'M'
	magic2 byte = 'G'
	magic3 byte = 'K'

	opRegister  byte = 0x01
	opKeepalive byte = 0x02

	// Header: 4 magic + 1 op + 8 user id = 13 bytes minimum.
	// Updated clients may append their LAN-side UDP socket port as a u16-be:
	//
	//     [RMGK][op][user id:u64-be][local port:u16-be]
	//
	// Keeping the original 13-byte packet valid preserves compatibility with
	// older clients while allowing the server to advertise a usable LAN
	// endpoint to peers behind the same router.
	minPacketLen      = 13
	extendedPacketLen = minPacketLen + 2

	readBufSize = 64
)

// Anchor is a small UDP service used for two things:
//  1. STUN-like reflection so a client can learn its own NAT-mapped UDP endpoint.
//  2. Keepalive packets to hold the NAT mapping open while a user sits in the lobby.
//
// The same observed endpoint is used as the publicIp:publicPort that the server
// shares with peers in MATCH_BEGIN.
type Anchor struct {
	conn  *net.UDPConn
	state *state.State

	mu       sync.Mutex
	lastSeen map[uint64]time.Time
}

func NewAnchor(conn *net.UDPConn, s *state.State) *Anchor {
	return &Anchor{
		conn:     conn,
		state:    s,
		lastSeen: make(map[uint64]time.Time),
	}
}

func endpointFromAddr(addr *net.UDPAddr) (string, uint16) {
	// Prefer the IPv4 form when this is a v4-mapped v6 address so the
	// string we hand to clients (which they then feed to GekkoNet) is
	// a clean dotted-quad — IPv6 strings have colons that confuse the
	// pipe-delimited address format downstream.
	ip := addr.IP.String()
	if ip4 := addr.IP.To4(); ip4 != nil {
		ip = ip4.String()
	}
	return ip, uint16(addr.Port)
}

// Run consumes UDP packets until ctx is cancelled or the conn is closed.
func (a *Anchor) Run(ctx context.Context) {
	buf := make([]byte, readBufSize)
	go func() {
		<-ctx.Done()
		_ = a.conn.SetReadDeadline(time.Now())
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		n, addr, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("udp read error", "err", err)
			continue
		}
		if n < minPacketLen {
			continue
		}
		if buf[0] != magic0 || buf[1] != magic1 || buf[2] != magic2 || buf[3] != magic3 {
			continue
		}

		op := buf[4]
		userID := binary.BigEndian.Uint64(buf[5:13])

		user := a.state.GetUser(userID)
		if user == nil {
			continue
		}
		if localPort, ok := localPortFromPacket(buf[:n]); ok {
			user.SetLocalPort(localPort)
		}

		a.mu.Lock()
		a.lastSeen[userID] = time.Now()
		a.mu.Unlock()

		switch op {
		case opRegister:
			// Set up our UDP Endpoint for the given user
			ip, port := endpointFromAddr(addr)
			user.SetUDPEndpoint(ip, port)

			reply := buildRegisterReply(addr)
			_, _ = a.conn.WriteToUDP(reply, addr)
		case opKeepalive:
			// Refresh our connection AND update UDPEndpoint in case it changed
			// Seems to resolve a bug where certain users could not ping each
			//     other sometimes due to ports changing unexpectedly
			ip, port := endpointFromAddr(addr)
			user.SetUDPEndpoint(ip, port)

			ack := []byte{magic0, magic1, magic2, magic3, opKeepalive}
			_, _ = a.conn.WriteToUDP(ack, addr)
		default:
			slog.Debug("unknown udp op", "op", op, "user", userID)
		}
	}
}

// localPortFromPacket reads the optional backward-compatible LAN-port suffix
// used by updated REGISTER and KEEPALIVE packets. Port zero is treated as not
// supplied, so it cannot erase a previously reported endpoint.
func localPortFromPacket(packet []byte) (uint16, bool) {
	if len(packet) < extendedPacketLen {
		return 0, false
	}
	port := binary.BigEndian.Uint16(packet[minPacketLen:extendedPacketLen])
	return port, port != 0
}

// LastSeen returns the most recent time the user sent a UDP packet to the anchor.
// Returns zero time if they have never been seen.
func (a *Anchor) LastSeen(userID uint64) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastSeen[userID]
}

// buildRegisterReply formats the reflection response:
//
//	[magic][op][ipLen][ip bytes][port:u16-be]
func buildRegisterReply(addr *net.UDPAddr) []byte {
	out := make([]byte, 0, 32)
	out = append(out, magic0, magic1, magic2, magic3, opRegister)
	ip4 := addr.IP.To4()
	if ip4 != nil {
		out = append(out, 4)
		out = append(out, ip4...)
	} else {
		out = append(out, 16)
		out = append(out, addr.IP.To16()...)
	}
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], uint16(addr.Port))
	out = append(out, portBuf[:]...)
	return out
}
