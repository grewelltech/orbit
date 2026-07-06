package datapath

import (
	"encoding/binary"
	"fmt"
	"net"
)

// Native inner UDP for the user plane: craft an IPv4+UDP packet sourced from
// the UE's allocated IP, to hand to Tunnel.SendUplink (GTP-U). This lets a
// traffic generator (loom) drive real UDP flows over the N3 tunnel without a
// TUN device or kernel sockets.

// BuildUDPPacket builds a complete IPv4+UDP packet from src:srcPort to
// dst:dstPort carrying payload. The returned bytes are the inner IP packet for
// Tunnel.SendUplink.
func BuildUDPPacket(src, dst net.IP, srcPort, dstPort uint16, payload []byte) ([]byte, error) {
	s4, d4 := src.To4(), dst.To4()
	if s4 == nil || d4 == nil {
		return nil, fmt.Errorf("UDP-over-N3 requires IPv4 addresses (src %v dst %v)", src, dst)
	}
	udpLen := 8 + len(payload)
	total := 20 + udpLen
	if total > 0xFFFF {
		return nil, fmt.Errorf("UDP payload too large: %d bytes", len(payload))
	}
	ip := make([]byte, total)
	// IPv4 header.
	ip[0] = 0x45 // IPv4, IHL 5
	binary.BigEndian.PutUint16(ip[2:4], uint16(total))
	ip[6] = 0x40 // don't fragment
	ip[8] = 64   // TTL
	ip[9] = 17   // protocol UDP
	copy(ip[12:16], s4)
	copy(ip[16:20], d4)
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip[:20]))
	// UDP header + payload.
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[8:], payload)
	binary.BigEndian.PutUint16(udp[6:8], udpChecksum(s4, d4, udp))
	return ip, nil
}

// ExtractUDPPayload returns the UDP payload of a downlink inner IPv4 packet
// addressed to dstPort (0 = any), plus the source socket. ok is false if the
// packet is not a matching IPv4/UDP datagram.
func ExtractUDPPayload(inner []byte, dstPort uint16) (payload []byte, from *net.UDPAddr, ok bool) {
	if len(inner) < 20 || inner[0]>>4 != 4 || inner[9] != 17 {
		return nil, nil, false
	}
	ihl := int(inner[0]&0x0F) * 4
	if len(inner) < ihl+8 {
		return nil, nil, false
	}
	udp := inner[ihl:]
	ulen := int(binary.BigEndian.Uint16(udp[4:6]))
	if ulen < 8 || len(udp) < ulen {
		return nil, nil, false
	}
	dp := binary.BigEndian.Uint16(udp[2:4])
	if dstPort != 0 && dp != dstPort {
		return nil, nil, false
	}
	from = &net.UDPAddr{IP: net.IP(inner[12:16]), Port: int(binary.BigEndian.Uint16(udp[0:2]))}
	return udp[8:ulen], from, true
}

// udpChecksum is the UDP checksum over the IPv4 pseudo-header + UDP datagram.
func udpChecksum(src, dst net.IP, udp []byte) uint16 {
	pseudo := make([]byte, 12+len(udp))
	copy(pseudo[0:4], src)
	copy(pseudo[4:8], dst)
	pseudo[9] = 17 // protocol UDP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(udp)))
	copy(pseudo[12:], udp)
	cs := checksum(pseudo)
	if cs == 0 {
		cs = 0xFFFF // 0 means "no checksum"; use all-ones instead
	}
	return cs
}
