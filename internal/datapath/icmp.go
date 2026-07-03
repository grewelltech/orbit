package datapath

import (
	"encoding/binary"
	"fmt"
	"net"
)

// Native ICMP-over-N3 smoke test: craft an IPv4 ICMP echo request sourced
// from the UE's allocated IP, send it through the GTP-U tunnel, and match
// the echo reply that returns on the downlink. This is gnbsim's N3 health
// check and proves the user plane end to end without a TUN device.

// BuildICMPEchoRequest builds a complete IPv4 packet (header + ICMP echo
// request) sourced from src to dst. The returned bytes are the inner IP
// packet to hand to Tunnel.SendUplink.
func BuildICMPEchoRequest(src, dst net.IP, id, seq uint16, payload []byte) ([]byte, error) {
	s4, d4 := src.To4(), dst.To4()
	if s4 == nil || d4 == nil {
		return nil, fmt.Errorf("ICMP-over-N3 requires IPv4 addresses (src %v dst %v)", src, dst)
	}
	icmp := make([]byte, 8+len(payload))
	icmp[0] = 8 // echo request
	icmp[1] = 0
	binary.BigEndian.PutUint16(icmp[4:6], id)
	binary.BigEndian.PutUint16(icmp[6:8], seq)
	copy(icmp[8:], payload)
	binary.BigEndian.PutUint16(icmp[2:4], checksum(icmp))

	total := 20 + len(icmp)
	ip := make([]byte, total)
	ip[0] = 0x45 // IPv4, IHL 5
	binary.BigEndian.PutUint16(ip[2:4], uint16(total))
	binary.BigEndian.PutUint16(ip[4:6], id) // identification
	ip[6] = 0x40                            // don't fragment
	ip[8] = 64                              // TTL
	ip[9] = 1                               // protocol ICMP
	copy(ip[12:16], s4)
	copy(ip[16:20], d4)
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip[:20]))
	copy(ip[20:], icmp)
	return ip, nil
}

// EchoReply is the matched result of a returned ICMP echo.
type EchoReply struct {
	From net.IP
	ID   uint16
	Seq  uint16
}

// MatchICMPEchoReply checks whether inner (an IPv4 packet from the downlink)
// is an ICMP echo reply for id/seq, returning its details.
func MatchICMPEchoReply(inner []byte, id, seq uint16) (*EchoReply, bool) {
	if len(inner) < 20 || inner[0]>>4 != 4 || inner[9] != 1 {
		return nil, false
	}
	ihl := int(inner[0]&0x0F) * 4
	if len(inner) < ihl+8 {
		return nil, false
	}
	icmp := inner[ihl:]
	if icmp[0] != 0 { // echo reply
		return nil, false
	}
	if binary.BigEndian.Uint16(icmp[4:6]) != id || binary.BigEndian.Uint16(icmp[6:8]) != seq {
		return nil, false
	}
	return &EchoReply{From: net.IP(inner[12:16]), ID: id, Seq: seq}, true
}

// checksum is the 16-bit one's-complement sum used by IPv4 and ICMP.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	return ^uint16(sum)
}
