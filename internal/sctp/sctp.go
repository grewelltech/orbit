// Package sctp adapts the pinned SCTP library (github.com/bgrewell/sctp, a
// fork of github.com/ishidawataru/sctp pinned at commit 19ddcbc) for ORBIT's
// N2 transport. All ORBIT code goes through this package rather than the
// library directly, so the fork can be swapped or patched with a one-file
// blast radius.
//
// Constraints: Linux-only (raw syscalls, no cgo) and requires the kernel
// sctp module (modprobe sctp). Non-Linux builds compile but Dial returns an
// error at runtime via the library's unsupported stubs.
//
// PPID byte order (the NGAP wire trap): the SCTP payload protocol identifier
// for NGAP is the integer 60 (IANA "3GPP NGAP"), rendered on the wire as the
// big-endian bytes 00 00 00 3c. Whether application code must pass 60 or the
// pre-swapped 0x3c000000 depends on the library version:
//
//   - This fork's per-message send path applies htonl itself
//     (sctp_linux.go:92-95) and ntohl on read (sctp_linux.go:120-121), so
//     callers pass host-order 60.
//   - free5gc's ngap.PPID constant is 0x3c000000 because free5gc pins an
//     older library WITHOUT that swap. Passing that constant through this
//     fork would double-swap and the AMF drops the association.
//   - SetDefaultSentParam does NOT swap (it hands the struct raw to
//     setsockopt, sctp.go:565-569), so a PPID set there needs the opposite
//     byte order from SCTPWrite. This package never uses it; every write
//     carries an explicit SndRcvInfo.
//
// The authoritative check is the wire: the Phase-0 NG Setup smoke test
// asserts bytes 00 00 00 3c in a live capture.
package sctp

import (
	"fmt"
	"net"

	isctp "github.com/ishidawataru/sctp"
)

// PPIDNGAP is the NGAP payload protocol identifier in host byte order, as
// required by the pinned library's SCTPWrite path. See the package comment
// before changing this value or how it is passed.
const PPIDNGAP uint32 = 60

// PPIDNGAPSwapped is the byte-reversed rendering of the NGAP PPID observed
// ON THE WIRE in downlink messages from the ATB-01 SD-Core (omec) AMF.
//
// Evidence (pcap on the core node, 2026-07-02): our uplink carries PPID
// wire bytes 00 00 00 3c (big-endian 60 — the conventional NGAP encoding);
// the AMF's replies carry 3c 00 00 00 (big-endian 1,006,632,960, i.e. 60
// byte-reversed). Both directions' payloads are valid NGAP.
//
// This is NOT strictly a protocol violation: RFC 4960 §3.3.1 states the
// PPID "is NOT touched by an SCTP implementation; therefore its byte order
// is NOT necessarily big endian. The upper layer is responsible for any
// byte order conversions" — an explicit exception to SCTP's network-byte-
// order rule, and SCTP never interprets the field. What the AMF does is
// diverge from the universal NGAP convention (send 60 in network order):
// the free5gc/omec code uses the precomputed constant 0x3c000000 (= 60
// already put in network order for a non-swapping SCTP lib) but now runs a
// library that byte-swaps again, so the wire gets 60 reversed. Benign
// because no peer demuxes on the PPID; a candidate Phase-6 finding because
// a strict peer checking PPID==60 in network order would reject it.
//
// ORBIT's receive path accepts both encodings and reports which was seen.
const PPIDNGAPSwapped uint32 = 0x3c000000

// NGAP non-UE-associated signalling (NG Setup, NG Reset, ...) uses SCTP
// stream 0; UE-associated signalling uses a nonzero stream chosen per UE
// (TS 38.412 §7).
const StreamNonUEAssociated uint16 = 0

// DefaultNGAPStreams is the number of streams requested at association
// setup: stream 0 for non-UE-associated signalling plus room for UE muxing.
const DefaultNGAPStreams uint16 = 8

// Conn is an established SCTP association carrying NGAP.
type Conn struct {
	c *isctp.SCTPConn
}

// Dial opens an SCTP association to raddr (host:port). If laddr is non-empty
// the association binds that local address first — one distinct bind IP per
// gNB is the multi-gNB plan (DESIGN §3). Data-IO events are subscribed so
// reads return per-message stream/PPID info.
func Dial(laddr, raddr string) (*Conn, error) {
	ra, err := resolve(raddr)
	if err != nil {
		return nil, fmt.Errorf("resolve remote %q: %w", raddr, err)
	}
	var la *isctp.SCTPAddr
	if laddr != "" {
		la, err = resolve(laddr)
		if err != nil {
			return nil, fmt.Errorf("resolve local %q: %w", laddr, err)
		}
	}
	c, err := isctp.DialSCTPExt("sctp", la, ra, isctp.InitMsg{
		NumOstreams:  DefaultNGAPStreams,
		MaxInstreams: DefaultNGAPStreams,
	})
	if err != nil {
		return nil, fmt.Errorf("sctp dial %q: %w", raddr, err)
	}
	if err := c.SubscribeEvents(isctp.SCTP_EVENT_DATA_IO); err != nil {
		c.Close()
		return nil, fmt.Errorf("subscribe data-io events: %w", err)
	}
	return &Conn{c: c}, nil
}

// WriteNGAP sends one NGAP PDU on the given stream with PPID 60.
func (c *Conn) WriteNGAP(stream uint16, pdu []byte) error {
	info := &isctp.SndRcvInfo{Stream: stream, PPID: PPIDNGAP}
	n, err := c.c.SCTPWrite(pdu, info)
	if err != nil {
		return fmt.Errorf("sctp write (stream %d, %d bytes): %w", stream, len(pdu), err)
	}
	if n != len(pdu) {
		return fmt.Errorf("sctp short write: %d of %d bytes", n, len(pdu))
	}
	return nil
}

// ReadMsg receives one message, returning its payload, stream, and PPID
// (host order — the library ntohl's it). Callers should verify ppid ==
// PPIDNGAP before decoding.
func (c *Conn) ReadMsg(buf []byte) (payload []byte, stream uint16, ppid uint32, err error) {
	n, info, err := c.c.SCTPRead(buf)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("sctp read: %w", err)
	}
	if info != nil {
		stream = info.Stream
		ppid = info.PPID
	}
	return buf[:n], stream, ppid, nil
}

func (c *Conn) Close() error { return c.c.Close() }

// OutStreams returns the number of outbound SCTP streams negotiated for this
// association. NGAP spreads UE-associated signalling across streams to avoid
// head-of-line blocking, but the peer may negotiate fewer than requested, so
// callers must not send on a stream >= this value. Returns 1 on error (only
// stream 0 is always safe).
func (c *Conn) OutStreams() uint16 {
	st, err := c.c.GetStatus()
	if err != nil || st == nil || st.Ostreams == 0 {
		return 1
	}
	return st.Ostreams
}

func resolve(hostport string) (*isctp.SCTPAddr, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		return nil, err
	}
	p, err := net.LookupPort("sctp", port)
	if err != nil {
		return nil, err
	}
	return &isctp.SCTPAddr{IPAddrs: []net.IPAddr{*ip}, Port: p}, nil
}
