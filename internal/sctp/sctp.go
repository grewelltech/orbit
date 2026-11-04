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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"

	isctp "github.com/ishidawataru/sctp"
	"golang.org/x/sys/unix"
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
	c rawConn
}

// rawConn is the slice of *isctp.SCTPConn that Conn uses. It exists so the
// dial can be driven by a test double standing in for the kernel; production
// code only ever holds an *isctp.SCTPConn here.
type rawConn interface {
	SCTPWrite(b []byte, info *isctp.SndRcvInfo) (int, error)
	SCTPRead(b []byte) (int, *isctp.SndRcvInfo, error)
	SubscribeEvents(flags int) error
	GetStatus() (*isctp.Status, error)
	LocalAddr() net.Addr
	Close() error
}

// rawDialer performs the blocking SCTP connect: the one place the pinned
// library's DialSCTPExt is called, so a context bound or a test double wraps
// it without reshaping Dial.
type rawDialer func(la, ra *isctp.SCTPAddr, init isctp.InitMsg) (rawConn, error)

// kernelDial is the production rawDialer.
func kernelDial(la, ra *isctp.SCTPAddr, init isctp.InitMsg) (rawConn, error) {
	c, err := isctp.DialSCTPExt("sctp", la, ra, init)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// DefaultDialTimeout bounds an N2 dial whose context carries no deadline. A
// black-holed AMF otherwise costs the kernel's full INIT retransmit schedule
// (minutes); 20 s is long enough for any reachable core to answer the INIT.
const DefaultDialTimeout = 20 * time.Second

// maxInitTimeout is the top of the SCTP_INITMSG max-init-timeout range, in
// milliseconds (the field is a uint16).
const maxInitTimeout = 65535

// initAttempts is the SCTP_INITMSG max-attempts asked of the kernel: the
// number of INIT retransmits before it abandons the exchange. With each
// attempt's RTO capped at a quarter of the dial bound (initTimeout), the
// kernel gives up at about the bound, so a socket orphaned by a cancelled
// dial does not keep retransmitting for the sysctl default of ~2.4 min. A
// reachable AMF answers the first INIT and never sees the difference; only a
// lost INIT now gets three retransmits inside the bound instead of eight.
const initAttempts = 3

// maxConnectRetries is how many fresh sockets an ephemeral-port dial tries
// after the first connect fails with a retryable error, connectRetryGap
// apart. Two make a stale-association collision (about 1 in 2,400 dials on
// a busy loopback) vanishingly rare without delaying a real refusal by more
// than about 20 ms.
const (
	maxConnectRetries = 2
	connectRetryGap   = 10 * time.Millisecond
)

// DialOption configures Dial. Options exist for tests and for callers that
// need to bound or steer the dial; the defaults are right for every command.
type DialOption func(*dialConfig)

// dialConfig is the resolved set of Dial options.
type dialConfig struct {
	// source picks the local address for an unbound dial, given the
	// resolved remote. Defaults to routeSource.
	source func(remote *net.UDPAddr) (net.IP, error)
	// timeout bounds the dial when ctx carries no deadline.
	timeout time.Duration
	// raw performs the blocking connect. Defaults to kernelDial.
	raw rawDialer
	// noRetry turns off the fresh-socket retry (WithoutRetry).
	noRetry bool
	// retryGap is the pause before a retry. Defaults to connectRetryGap.
	retryGap time.Duration
	// log takes the debug line a retried dial leaves. Defaults to
	// slog.Default().
	log *slog.Logger
}

// WithSourceResolver replaces how an unbound dial picks its local address.
// The default asks the kernel which source it would route to remote from.
func WithSourceResolver(f func(remote *net.UDPAddr) (net.IP, error)) DialOption {
	return func(c *dialConfig) { c.source = f }
}

// WithDialTimeout sets the bound used when ctx has no deadline, in place of
// DefaultDialTimeout. It never extends a deadline ctx already carries. A
// non-positive d is ignored.
func WithDialTimeout(d time.Duration) DialOption {
	return func(c *dialConfig) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithoutRetry turns off the fresh-socket retry of an ephemeral-port dial
// (see Dial), so one Dial is exactly one connect: for a caller that counts
// INITs or associations, or that dials expecting the AMF to refuse.
func WithoutRetry() DialOption {
	return func(c *dialConfig) { c.noRetry = true }
}

// WithLogger sets the logger that takes the debug line a retried dial
// leaves, in place of slog.Default(). A nil l is ignored.
func WithLogger(l *slog.Logger) DialOption {
	return func(c *dialConfig) {
		if l != nil {
			c.log = l
		}
	}
}

// withRetryGap replaces the pause before a retry; tests use it to place a
// bound between attempts.
func withRetryGap(d time.Duration) DialOption {
	return func(c *dialConfig) { c.retryGap = d }
}

// withRawDialer replaces the blocking connect; tests use it to stand in for
// the kernel. Unexported so production code cannot bypass the real dial.
func withRawDialer(f rawDialer) DialOption {
	return func(c *dialConfig) { c.raw = f }
}

// initTimeout returns the SCTP max-init timeout to request, in milliseconds,
// for a dial bounded by bound. The field (sinit_max_init_timeo) caps the RTO
// of ONE INIT attempt, not the whole exchange: the kernel still retransmits
// initAttempts times, each waiting at most this long (and at least rto_min,
// 1 s by default). So the request is a quarter of the bound, and with three
// retransmits the exchange is abandoned at about the bound. Rounded up and
// clamped to the option's 1..65535 ms range: never 0, which the kernel reads
// as "use the sysctl default", and never above what the uint16 carries. A
// bound already spent still asks for the shortest cap.
func initTimeout(bound time.Duration) uint16 {
	ms := (bound/4 + time.Millisecond - 1) / time.Millisecond
	switch {
	case ms < 1:
		return 1
	case ms > maxInitTimeout:
		return maxInitTimeout
	default:
		return uint16(ms)
	}
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

// probeSource runs a source resolver for the resolved remote ra (named raddr
// in errors) and returns the address an unbound dial binds. Whatever the
// resolver, an unspecified or missing address never gets through: that is
// INADDR_ANY, the multi-homed socket this path exists to avoid, so Dial owns
// the invariant rather than trusting each resolver. A link-local source keeps
// the remote's zone, since a link-local pair shares an interface. An IPv6
// link-local remote without a zone cannot be routed at all, so it gets a
// hint naming the zone rather than the probe's "pass --bind" (#120).
func probeSource(source func(*net.UDPAddr) (net.IP, error), raddr string, ra *isctp.SCTPAddr) (net.IPAddr, error) {
	remote := &net.UDPAddr{IP: ra.IPAddrs[0].IP, Port: ra.Port, Zone: ra.IPAddrs[0].Zone}
	if remote.IP.To4() == nil && remote.IP.IsLinkLocalUnicast() && remote.Zone == "" {
		example := net.JoinHostPort(remote.IP.String()+"%eth0", strconv.Itoa(remote.Port))
		return net.IPAddr{}, fmt.Errorf("link-local AMF address %q needs a %%zone, e.g. %s", raddr, example)
	}
	ip, err := source(remote)
	if err != nil {
		return net.IPAddr{}, fmt.Errorf("cannot determine N2 source for %q: %w; pass --bind", raddr, err)
	}
	if ip == nil || ip.IsUnspecified() {
		return net.IPAddr{}, fmt.Errorf("cannot determine N2 source for %q: resolver returned %v; pass --bind", raddr, ip)
	}
	src := net.IPAddr{IP: ip}
	if ip.IsLinkLocalUnicast() {
		src.Zone = remote.Zone
	}
	return src, nil
}

// SourceFor reports the local address an unbound Dial to raddr would bind:
// the route-selected source, with the same guard against an unspecified
// result. It lets a caller that only needs to report the source (conformance
// records which address its checks were taken from) answer without opening
// an association. The error is the one Dial would return.
func SourceFor(raddr string) (net.IP, error) {
	ra, err := resolve(raddr)
	if err != nil {
		return nil, fmt.Errorf("resolve remote %q: %w", raddr, err)
	}
	src, err := probeSource(routeSource, raddr, ra)
	if err != nil {
		return nil, err
	}
	return src.IP, nil
}

// DialError is every failure Dial returns once the local address is
// well-formed: the AMF could not be reached from this host, or the attempt
// was cancelled. Its text is the cause's, unchanged; callers classify with
// errors.As rather than by message, and errors.Is still sees the context
// errors and errnos the cause wraps.
type DialError struct {
	Remote string // the AMF address as given
	// Source is the local address in the form errors name it: the
	// normalised --bind, the probed one marked "(route-selected)", or ""
	// while it is not yet known.
	Source string
	Err    error
}

// Error returns the cause's text.
func (e *DialError) Error() string { return e.Err.Error() }

// Unwrap returns the cause.
func (e *DialError) Unwrap() error { return e.Err }

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
// The dial is bounded by ctx's deadline, or by DefaultDialTimeout (or
// WithDialTimeout) when ctx has none: Dial derives that deadline itself, so
// the bound is always on. The blocking connect runs in a goroutine and Dial
// returns as soon as the bound passes, wrapping ctx.Err() so callers can
// errors.Is it. The kernel is asked to abandon the INIT exchange at about the
// same time (initTimeout, initAttempts), so an AMF that never answers fails
// with "no INIT-ACK after ..." and the connection the kernel may still
// produce after an early return is closed when it arrives. Nothing leaks, and
// nothing keeps retransmitting for the sysctl default of minutes.
//
// An ephemeral-port dial (laddr "", a bare IP, or ip:0) whose connect fails
// with EISCONN, EALREADY or ECONNREFUSED is retried on a fresh socket, up to
// maxConnectRetries times connectRetryGap apart, inside the same bound: the
// port the kernel drew may still belong to a closing association with the
// same AMF (see retryable). No retry starts once the bound leaves no time
// for it. A dial that fails after retries names every attempt's error; one
// that succeeds after a retry leaves one debug line. A pinned port is never
// retried, and its collision error says why; WithoutRetry turns the retry
// off.
//
// Every failure after the local address is normalised and resolved is a
// *DialError (see there); a malformed or unresolvable local address is a
// plain error, as is a failure to subscribe events on an open association.
//
// Data-IO events are subscribed so reads return per-message stream/PPID info.
func Dial(ctx context.Context, laddr, raddr string, opts ...DialOption) (*Conn, error) {
	cfg := dialConfig{source: routeSource, timeout: DefaultDialTimeout, raw: kernelDial, retryGap: connectRetryGap, log: slog.Default()}
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
		return nil, &DialError{Remote: raddr, Source: laddr, Err: fmt.Errorf("resolve remote %q: %w", raddr, err)}
	}
	// source names the local address in errors: the normalised --bind, or
	// the probed one marked as such so the operator can tell who chose it.
	source := laddr
	if la == nil {
		src, err := probeSource(cfg.source, raddr, ra)
		if err != nil {
			return nil, &DialError{Remote: raddr, Err: err}
		}
		la = &isctp.SCTPAddr{IPAddrs: []net.IPAddr{src}, Port: 0}
		source = la.String() + " (route-selected)"
	}
	dialErr := func(err error) error { return &DialError{Remote: raddr, Source: source, Err: err} }
	if err := sameFamily(la, ra); err != nil {
		return nil, dialErr(err)
	}

	// A context already done gets the same error it would get a moment later,
	// without starting a connect nobody will wait for.
	if err := ctx.Err(); err != nil {
		return nil, dialErr(fmt.Errorf("sctp dial %q from %q: %w", raddr, source, err))
	}

	// The bound: the caller's deadline, else one Dial sets itself, so it is
	// always on. The kernel is asked to abandon the exchange at about the
	// same time (initTimeout, initAttempts).
	start := time.Now()
	bound := cfg.timeout
	if deadline, ok := ctx.Deadline(); ok {
		bound = deadline.Sub(start)
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}
	init := isctp.InitMsg{
		NumOstreams:    DefaultNGAPStreams,
		MaxInstreams:   DefaultNGAPStreams,
		MaxAttempts:    initAttempts,
		MaxInitTimeout: initTimeout(bound),
	}
	elapsed := func() time.Duration { return time.Since(start).Round(100 * time.Millisecond) }

	// tried names each failed connect, in order, for the error and the
	// debug line; only an unpinned port is retried (see retryable).
	pinned := la.Port != 0
	retry := !cfg.noRetry && !pinned
	var tried []string
	fail := func(err error) error {
		if len(tried) > 1 {
			err = fmt.Errorf("%w (%d attempts: %s)", err, len(tried), strings.Join(tried, ", "))
		}
		return dialErr(err)
	}
	var c rawConn
	for {
		conn, cut, err := connect(ctx, cfg.raw, la, ra, init)
		if err == nil {
			c = conn
			break
		}
		if cut {
			if len(tried) > 0 {
				// A retry the bound cut short still counts in the list.
				tried = append(tried, "no answer in the bound")
			}
			if errors.Is(err, context.DeadlineExceeded) {
				// Same fact as the kernel branch, nothing answered the INIT,
				// so the operator reads the same hint whichever bound fired
				// first.
				return nil, fail(fmt.Errorf("sctp dial %q from %q: no INIT-ACK after %s (bound %s; AMF unreachable or black-holed): %w",
					raddr, source, elapsed(), printedBound(bound), err))
			}
			return nil, fail(fmt.Errorf("sctp dial %q from %q: cancelled after %s: %w", raddr, source, elapsed(), err))
		}
		tried = append(tried, errnoName(err))
		if errors.Is(err, syscall.ETIMEDOUT) {
			// The kernel abandoned the INIT exchange: nothing answered.
			return nil, fail(fmt.Errorf("sctp dial %q from %q: no INIT-ACK after %s (bound %s; AMF unreachable or black-holed): %w",
				raddr, source, elapsed(), printedBound(bound), err))
		}
		// Name the source too: "cannot assign requested address" is about
		// the bind, not the AMF, and the operator needs to see which
		// address the host does not own (or which one the route picked).
		final := fmt.Errorf("sctp dial %q from %q: %w", raddr, source, err)
		if !retryable(err) {
			return nil, fail(final)
		}
		if pinned && !errors.Is(err, syscall.ECONNREFUSED) {
			// A refusal is the AMF's answer either way; a local collision
			// on a port the caller fixed is the earlier dial's leftover.
			return nil, fail(fmt.Errorf("%w; source port %d may still hold an association to this AMF from an earlier dial: let it close, or bind port 0",
				final, la.Port))
		}
		if !retry || len(tried) > maxConnectRetries {
			return nil, fail(final)
		}
		// A retry never starts after the bound: one that would has no time
		// left to answer in, so the connect error stands.
		if deadline, _ := ctx.Deadline(); time.Until(deadline) <= cfg.retryGap {
			return nil, fail(fmt.Errorf("%w; no time left in the bound (%s) to retry", final, printedBound(bound)))
		}
		gap := time.NewTimer(cfg.retryGap)
		select {
		case <-gap.C:
		case <-ctx.Done():
			gap.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, fail(fmt.Errorf("%w; cancelled before a retry", final))
			}
			return nil, fail(fmt.Errorf("%w; no time left in the bound (%s) to retry", final, printedBound(bound)))
		}
	}
	if len(tried) > 0 {
		cfg.log.Debug("sctp dial retried on a fresh socket", "remote", raddr, "source", source,
			"attempts", len(tried)+1, "errors", strings.Join(tried, ","))
	}
	if err := c.SubscribeEvents(isctp.SCTP_EVENT_DATA_IO); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("subscribe data-io events: %w", err)
	}
	return &Conn{c: c}, nil
}

// connect runs one blocking connect on a fresh socket and waits for it or
// for ctx, whichever comes first. cut reports that ctx did, and err is then
// ctx's: the connection the kernel may still produce, for about the bound,
// belongs to nobody and is closed on arrival.
func connect(ctx context.Context, raw rawDialer, la, ra *isctp.SCTPAddr, init isctp.InitMsg) (c rawConn, cut bool, err error) {
	type dialResult struct {
		c   rawConn
		err error
	}
	done := make(chan dialResult, 1)
	go func() {
		c, err := raw(la, ra, init)
		done <- dialResult{c: c, err: err}
	}()
	select {
	case r := <-done:
		return r.c, false, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, true, ctx.Err()
	}
}

// retryable reports whether a connect error on an ephemeral port is worth a
// fresh socket. Linux frees a closed socket's port at once while its
// association lingers (SHUTDOWN-SENT, SHUTDOWN-ACK-SENT, COOKIE-ECHOED), so
// a new socket can draw the same port and collide with it at the same AMF:
// EISCONN or EALREADY locally, or the peer's ABORT for the stale association
// (RFC 4960 §9.2), read as ECONNREFUSED. A fresh socket draws another port.
// A real refusal fails every attempt and the error names them all.
func retryable(err error) bool {
	return errors.Is(err, syscall.EISCONN) || errors.Is(err, syscall.EALREADY) || errors.Is(err, syscall.ECONNREFUSED)
}

// errnoName names a connect error for the attempt list: the errno's symbol
// (EISCONN) when it is one, else its text.
func errnoName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return err.Error()
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

// printedBound is a dial bound as error text shows it: rounded to 10 ms, so a
// bound derived from a deadline a moment old (9.999s for --timeout 10s) reads
// as requested (#122). A bound under 5 ms keeps millisecond precision rather
// than reading 0s.
func printedBound(bound time.Duration) time.Duration {
	if r := bound.Round(10 * time.Millisecond); r > 0 {
		return r
	}
	return bound.Round(time.Millisecond)
}
