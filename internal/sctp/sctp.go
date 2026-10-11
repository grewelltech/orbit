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
	"net/netip"
	"strings"

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

// DialOption configures Dial. Options exist for tests and for callers that
// need to bound or steer the dial; the defaults are right for every command.
type DialOption func(*dialConfig)

// dialConfig is the resolved set of Dial options.
type dialConfig struct {
	// source picks the local address for an unbound dial, given the
	// resolved remote. Defaults to routeSource.
	source func(remote *net.UDPAddr) (net.IP, error)
}

// WithSourceResolver replaces how an unbound dial picks its local address.
// The default asks the kernel which source it would route to remote from.
func WithSourceResolver(f func(remote *net.UDPAddr) (net.IP, error)) DialOption {
	return func(c *dialConfig) { c.source = f }
}

// routeSource is the default source resolver: it connects a UDP socket to
// remote, which makes the kernel run route selection and fill in the source
// address without sending a packet, then reads that address back and closes
// the socket. The result is the address the host would use for an unbound
// SCTP INIT to the same destination, as a single IP of remote's family.
func routeSource(remote *net.UDPAddr) (net.IP, error) {
	c, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return nil, err
	}
	la, ok := c.LocalAddr().(*net.UDPAddr)
	// The probe socket carried nothing, so a close error has nothing to say.
	_ = c.Close()
	if !ok || la.IP == nil || la.IP.IsUnspecified() {
		return nil, fmt.Errorf("kernel reported no source address for %s", remote)
	}
	if v4 := la.IP.To4(); v4 != nil {
		return v4, nil
	}
	return la.IP, nil
}

// Dial opens an SCTP association to raddr (host:port). If laddr is non-empty
// the association binds that local address first — one distinct bind IP per
// gNB is the multi-gNB plan (DESIGN §3). laddr may be "" (the single address
// the host routes to raddr from, ephemeral port), a bare IP (ephemeral port),
// or ip:port; see NormalizeLocal. Every N2 source address in ORBIT — CLI
// --bind flags, API bind_address fields, scenario source_ips — funnels
// through here, so the accepted forms are defined exactly once.
//
// An unbound dial never leaves the socket on INADDR_ANY: SCTP would then
// advertise every host address in the INIT, and the AMF would see loopback,
// management and data addresses alongside the N2 one. Instead the
// route-selected source is probed (routeSource, or the WithSourceResolver
// option) and bound with port 0, so the INIT carries one address exactly as
// --bind does. No route to the AMF is an error naming --bind, not a fallback.
//
// Data-IO events are subscribed so reads return per-message stream/PPID info.
func Dial(laddr, raddr string, opts ...DialOption) (*Conn, error) {
	cfg := dialConfig{source: routeSource}
	for _, o := range opts {
		o(&cfg)
	}
	// The local address is checked first so a bad --bind is reported even
	// when the AMF address does not resolve either.
	laddr, err := NormalizeLocal(laddr)
	if err != nil {
		return nil, err
	}
	var la *isctp.SCTPAddr
	if laddr != "" {
		la, err = resolve(laddr)
		if err != nil {
			return nil, fmt.Errorf("resolve local %q: %w", laddr, err)
		}
	}
	ra, err := resolve(raddr)
	if err != nil {
		return nil, fmt.Errorf("resolve remote %q: %w", raddr, err)
	}
	// source names the local address in errors: the normalised --bind, or
	// the probed one marked as such so the operator can tell who chose it.
	source := laddr
	if la == nil {
		ip, err := cfg.source(&net.UDPAddr{IP: ra.IPAddrs[0].IP, Port: ra.Port})
		if err != nil {
			return nil, fmt.Errorf("cannot determine N2 source for %q: %w; pass --bind", raddr, err)
		}
		la = &isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: ip}}, Port: 0}
		source = la.String() + " (route-selected)"
	}
	if err := sameFamily(la, ra); err != nil {
		return nil, err
	}
	c, err := isctp.DialSCTPExt("sctp", la, ra, isctp.InitMsg{
		NumOstreams:  DefaultNGAPStreams,
		MaxInstreams: DefaultNGAPStreams,
	})
	if err != nil {
		// Name the source too: "cannot assign requested address" is about
		// the bind, not the AMF, and the operator needs to see which
		// address the host does not own (or which one the route picked).
		return nil, fmt.Errorf("sctp dial %q from %q: %w", raddr, source, err)
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

// LocalAddr returns the association's local (source) address as the kernel
// bound it — with the real port when the caller asked for :0 — so a command
// can report the N2 source it actually used rather than the one it asked
// for. nil when the library cannot read it back.
func (c *Conn) LocalAddr() net.Addr { return c.c.LocalAddr() }

// NormalizeLocal canonicalises a local N2 source address for Dial. It
// accepts every host:port form resolve accepted before it existed and ADDS
// the port-less forms, returning host:port or "":
//
//   - ""                 → "" (Dial binds the route-selected address, ephemeral port)
//   - "10.0.0.1"         → "10.0.0.1:0" (ephemeral port)
//   - "10.0.0.1:0"       → unchanged; any explicit port is kept as given
//   - "::1"              → "[::1]:0" (bare IPv6 is bracketed, not mangled)
//   - "fe80::1%eth0"     → "[fe80::1%eth0]:0" (zoned link-local is kept)
//   - "[::1]:38412"      → unchanged
//   - "ran-21"           → "ran-21:0" (a hostname; resolved at dial time)
//   - "ran-21:38412"     → unchanged
//
// Anything else — an IPv6 literal with brackets but no port, a trailing colon
// with no port, too many colons — is an error naming the input, so a typo
// fails at the flag rather than as an opaque bind failure. Host resolution
// and port range are checked by resolve, as before.
func NormalizeLocal(laddr string) (string, error) {
	if laddr == "" {
		return "", nil
	}
	if host, port, err := net.SplitHostPort(laddr); err == nil {
		if host == "" || port == "" {
			return "", fmt.Errorf("invalid local address %q (want host, ip, host:port or ip:port)", laddr)
		}
		return laddr, nil
	}
	if _, err := netip.ParseAddr(laddr); err == nil {
		return net.JoinHostPort(laddr, "0"), nil
	}
	if !strings.Contains(laddr, ":") {
		return net.JoinHostPort(laddr, "0"), nil // bare hostname
	}
	return "", fmt.Errorf("invalid local address %q (want host, ip, host:port or ip:port)", laddr)
}

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

// sameFamily rejects an IPv4 local address paired with an IPv6 AMF (or the
// reverse) before any socket is opened. The kernel cannot say no to that
// pairing quickly: the INIT goes nowhere and the dial sits through every
// retransmit (minutes) before "connection timed out". It runs on the probed
// source of an unbound dial too; a nil local address always matches.
func sameFamily(la, ra *isctp.SCTPAddr) error {
	if la == nil || len(la.IPAddrs) == 0 || len(ra.IPAddrs) == 0 {
		return nil
	}
	lv4, rv4 := la.IPAddrs[0].IP.To4() != nil, ra.IPAddrs[0].IP.To4() != nil
	if lv4 == rv4 {
		return nil
	}
	return fmt.Errorf("address family mismatch: local %s is %s but AMF %s is %s",
		la.String(), family(lv4), ra.String(), family(rv4))
}

func family(v4 bool) string {
	if v4 {
		return "IPv4"
	}
	return "IPv6"
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
