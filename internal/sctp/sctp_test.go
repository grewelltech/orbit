package sctp

import (
	"errors"
	"net"
	"strings"
	"testing"

	isctp "github.com/ishidawataru/sctp"
)

func TestNormalizeLocal(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "empty means the route-selected source", in: "", want: ""},
		{name: "bare ipv4 gets ephemeral port", in: "10.0.0.1", want: "10.0.0.1:0"},
		{name: "ipv4 with zero port unchanged", in: "10.0.0.1:0", want: "10.0.0.1:0"},
		{name: "ipv4 with explicit port unchanged", in: "10.0.0.1:38412", want: "10.0.0.1:38412"},
		{name: "bare ipv6 is bracketed", in: "::1", want: "[::1]:0"},
		{name: "bracketed ipv6 with port unchanged", in: "[::1]:0", want: "[::1]:0"},
		{name: "bracketed ipv6 with explicit port unchanged", in: "[fd00::21]:38412", want: "[fd00::21]:38412"},
		{name: "zoned link-local ipv6 is bracketed", in: "fe80::1%eth0", want: "[fe80::1%eth0]:0"},
		{name: "zoned link-local ipv6 with port unchanged", in: "[fe80::1%eth0]:0", want: "[fe80::1%eth0]:0"},
		{name: "bare hostname gets ephemeral port", in: "localhost", want: "localhost:0"},
		{name: "hostname with port unchanged", in: "localhost:0", want: "localhost:0"},
		{name: "unresolvable hostname is still a hostname here", in: "not-an-ip", want: "not-an-ip:0"},
		{name: "out-of-range port passes normalisation (resolve rejects it)", in: "10.0.0.1:70000", want: "10.0.0.1:70000"},
		{name: "trailing colon rejected", in: "10.0.0.1:", wantErr: true},
		{name: "port only rejected", in: ":38412", wantErr: true},
		{name: "bracketed ipv6 without port rejected", in: "[::1]", wantErr: true},
		{name: "too many colons rejected", in: "10.0.0.1:0:0", wantErr: true},
		{name: "unbracketed ipv6 with port rejected", in: "::1:38412:0", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeLocal(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeLocal(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeLocal(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeLocal(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A malformed local address fails before any socket is touched, with the
// normalisation error rather than a resolver or bind error — and before the
// remote is looked at, so a bad --bind is reported even alongside a bad AMF.
func TestDialRejectsBadLocal(t *testing.T) {
	_, err := Dial("10.0.0.1:0:0", "not-an-amf")
	if err == nil {
		t.Fatal("Dial with a bad local address succeeded")
	}
	if want := `invalid local address "10.0.0.1:0:0"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}
}

// The port range is enforced by resolve, as it always was: 70000 passes
// normalisation and is rejected before any socket is opened.
func TestDialRejectsOutOfRangeLocalPort(t *testing.T) {
	_, err := Dial("10.0.0.1:70000", "127.0.0.1:38412")
	if err == nil {
		t.Fatal("Dial with port 70000 succeeded")
	}
	if !strings.Contains(err.Error(), `resolve local "10.0.0.1:70000"`) {
		t.Errorf("error %q does not name the local address", err)
	}
}

// A local address of one IP family paired with an AMF of the other is
// refused before any socket is opened, naming both, instead of sitting
// through the kernel's INIT retransmits for minutes.
func TestDialRejectsFamilyMismatch(t *testing.T) {
	tests := []struct {
		name, laddr, raddr string
	}{
		{name: "ipv6 local to ipv4 amf", laddr: "::1", raddr: "127.0.0.1:38412"},
		{name: "bracketed ipv6 local to ipv4 amf", laddr: "[::1]:0", raddr: "127.0.0.1:38412"},
		{name: "ipv4 local to ipv6 amf", laddr: "127.0.0.1", raddr: "[::1]:38412"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Dial(tc.laddr, tc.raddr)
			if err == nil {
				t.Fatal("mismatched families dialled")
			}
			if !strings.Contains(err.Error(), "address family mismatch") {
				t.Errorf("err = %v, want a family-mismatch error", err)
			}
		})
	}
	// Matching families are not refused here (the dial itself may still fail).
	if _, err := Dial("::1", "[::1]:1"); err != nil && strings.Contains(err.Error(), "address family mismatch") {
		t.Errorf("same-family dial refused as a mismatch: %v", err)
	}
}

// A dial that fails after the local address was given names that address,
// so "cannot assign requested address" points at the bind, not the AMF.
// Whatever this host's SCTP support (refused, or no kernel module), the
// error carries the normalised source.
func TestDialErrorNamesLocal(t *testing.T) {
	_, err := Dial("127.0.0.1", "127.0.0.1:1")
	if err == nil {
		t.Skip("an SCTP listener answered on 127.0.0.1:1")
	}
	if !strings.Contains(err.Error(), `from "127.0.0.1:0"`) {
		t.Errorf("error %q does not name the local address", err)
	}
}

// routeSource asks the kernel which address it routes to the remote from.
// On loopback the answer is the loopback address of the remote's family.
func TestRouteSource(t *testing.T) {
	tests := []struct {
		name   string
		remote *net.UDPAddr
		want   string
	}{
		{name: "ipv4 loopback", remote: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 38412}, want: "127.0.0.1"},
		{name: "ipv6 loopback", remote: &net.UDPAddr{IP: net.ParseIP("::1"), Port: 38412}, want: "::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := routeSource(tc.remote)
			if err != nil {
				if tc.remote.IP.To4() == nil {
					t.Skipf("no IPv6 loopback on this host: %v", err)
				}
				t.Fatalf("routeSource(%s): %v", tc.remote, err)
			}
			if got.String() != tc.want {
				t.Errorf("routeSource(%s) = %s, want %s", tc.remote, got, tc.want)
			}
		})
	}
}

// An unbound dial whose source probe fails is refused with a message naming
// the AMF address and --bind, before any connect is attempted: the remote is
// a TEST-NET address a real SCTP connect would sit on for minutes.
func TestDialUnboundProbeErrorNamesBind(t *testing.T) {
	var seen *net.UDPAddr
	_, err := Dial("", "192.0.2.1:38412", WithSourceResolver(func(remote *net.UDPAddr) (net.IP, error) {
		seen = remote
		return nil, errors.New("no route to host")
	}))
	if err == nil {
		t.Fatal("Dial succeeded with a failing source probe")
	}
	for _, want := range []string{`cannot determine N2 source for "192.0.2.1:38412"`, "no route to host", "pass --bind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if seen == nil || seen.IP.String() != "192.0.2.1" || seen.Port != 38412 {
		t.Errorf("probe saw remote %v, want 192.0.2.1:38412", seen)
	}
}

// Dial, not the resolver, guarantees that no unbound dial ever binds an
// unspecified address: nil, 0.0.0.0 and :: from an injected resolver are all
// refused before any connect (the remote is a TEST-NET address a real connect
// would sit on), so no resolver, present or future, can put the socket back
// on INADDR_ANY.
func TestDialUnboundRejectsUnspecifiedSource(t *testing.T) {
	tests := []struct {
		name string
		ip   net.IP
	}{
		{name: "nil", ip: nil},
		{name: "ipv4 any", ip: net.IPv4zero},
		{name: "ipv6 any", ip: net.IPv6unspecified},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Dial("", "192.0.2.1:38412", WithSourceResolver(func(*net.UDPAddr) (net.IP, error) {
				return tc.ip, nil
			}))
			if err == nil {
				t.Fatal("Dial bound an unspecified source")
			}
			for _, want := range []string{`cannot determine N2 source for "192.0.2.1:38412"`, "resolver returned", "pass --bind"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

// An IPv6 zone on the AMF address reaches the probe, and a link-local source
// is bound with that zone, so a link-local AMF stays dialable without --bind.
// The dial itself fails here (the source is not a host address, or there is
// no kernel SCTP), and the error names the zoned route-selected source.
func TestDialUnboundCarriesZone(t *testing.T) {
	var seen *net.UDPAddr
	_, err := Dial("", "[fe80::1%lo]:1", WithSourceResolver(func(remote *net.UDPAddr) (net.IP, error) {
		seen = remote
		return net.ParseIP("fe80::2"), nil
	}))
	if seen == nil {
		t.Fatal("resolver was not called")
	}
	if seen.Zone != "lo" || seen.IP.String() != "fe80::1" || seen.Port != 1 {
		t.Errorf("probe saw %v, want [fe80::1%%lo]:1", seen)
	}
	if err == nil {
		t.Fatal("dial from a source the host does not own succeeded")
	}
	if !strings.Contains(err.Error(), `from "[fe80::2%lo]:0 (route-selected)"`) {
		t.Errorf("error %q does not name the zoned route-selected source", err)
	}
}

// SourceFor reports the address an unbound Dial would bind, without opening
// an association: the route-selected IP for a resolvable AMF, Dial's own
// error shape for one that does not resolve.
func TestSourceFor(t *testing.T) {
	tests := []struct {
		name    string
		raddr   string
		want    string
		wantErr string
	}{
		{name: "loopback amf", raddr: "127.0.0.1:38412", want: "127.0.0.1"},
		{name: "unresolvable amf", raddr: "no-such-amf.invalid:38412", wantErr: `resolve remote "no-such-amf.invalid:38412"`},
		{name: "not host:port", raddr: "127.0.0.1", wantErr: `resolve remote "127.0.0.1"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SourceFor(tc.raddr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SourceFor(%q): %v", tc.raddr, err)
			}
			if got.String() != tc.want {
				t.Errorf("SourceFor(%q) = %s, want %s", tc.raddr, got, tc.want)
			}
		})
	}
}

// linkLocalAddr returns one of this host's IPv6 link-local addresses with its
// interface as the zone, or skips the test when there is none.
func linkLocalAddr(t *testing.T) *net.IPAddr {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("listing interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsLinkLocalUnicast() && n.IP.To4() == nil {
				return &net.IPAddr{IP: n.IP, Zone: ifc.Name}
			}
		}
	}
	t.Skip("no IPv6 link-local address on this host")
	return nil
}

// The kernel probe honours the zone: routing to one of the host's own
// link-local addresses picks that address as the source.
func TestRouteSourceLinkLocalZone(t *testing.T) {
	ll := linkLocalAddr(t)
	got, err := routeSource(&net.UDPAddr{IP: ll.IP, Port: 38412, Zone: ll.Zone})
	if err != nil {
		t.Fatalf("routeSource(%v): %v", ll, err)
	}
	if !got.Equal(ll.IP) {
		t.Errorf("routeSource(%v) = %s, want %s", ll, got, ll.IP)
	}
}

// The family check still runs on a probed source: a resolver handing back an
// IPv6 address for an IPv4 AMF is refused before any socket is opened.
func TestDialUnboundProbedSourceFamilyChecked(t *testing.T) {
	_, err := Dial("", "127.0.0.1:38412", WithSourceResolver(func(*net.UDPAddr) (net.IP, error) {
		return net.ParseIP("::1"), nil
	}))
	if err == nil {
		t.Fatal("mismatched probed family dialled")
	}
	if !strings.Contains(err.Error(), "address family mismatch") {
		t.Errorf("err = %v, want a family-mismatch error", err)
	}
}

// The unbound twin of TestDialErrorNamesLocal: a failed dial names the
// route-selected source, marked as such, so the operator can tell the probe
// picked it rather than a --bind.
func TestDialErrorNamesRouteSelectedSource(t *testing.T) {
	_, err := Dial("", "127.0.0.1:1")
	if err == nil {
		t.Skip("an SCTP listener answered on 127.0.0.1:1")
	}
	if !strings.Contains(err.Error(), `from "127.0.0.1:0 (route-selected)"`) {
		t.Errorf("error %q does not name the route-selected source", err)
	}
}

// listenLoopbackSCTP opens a real SCTP listener on 127.0.0.1 with an
// ephemeral port and accepts (and holds) every association until the test
// ends. It skips the test when the host has no kernel SCTP.
func listenLoopbackSCTP(t *testing.T) string {
	t.Helper()
	ln, err := isctp.ListenSCTPExt("sctp", &isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}, isctp.InitMsg{})
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

// Over a real association, an unbound dial binds exactly one address and
// reports it as ip:port, and that address is the one an explicit --bind to
// the same destination would use.
func TestDialUnboundReportsOneAddress(t *testing.T) {
	addr := listenLoopbackSCTP(t)

	unbound, err := Dial("", addr)
	if err != nil {
		t.Fatalf("unbound Dial(%s): %v", addr, err)
	}
	defer func() { _ = unbound.Close() }()
	bound, err := Dial("127.0.0.1", addr)
	if err != nil {
		t.Fatalf("bound Dial(%s): %v", addr, err)
	}
	defer func() { _ = bound.Close() }()

	src := unbound.LocalAddr()
	if src == nil {
		t.Fatal("unbound association reports no local address")
	}
	if strings.Contains(src.String(), "/") {
		t.Fatalf("unbound source %q lists several addresses", src)
	}
	host, port, err := net.SplitHostPort(src.String())
	if err != nil {
		t.Fatalf("unbound source %q is not host:port: %v", src, err)
	}
	if port == "0" || port == "" {
		t.Errorf("unbound source %q has no real port", src)
	}
	bhost, _, err := net.SplitHostPort(bound.LocalAddr().String())
	if err != nil {
		t.Fatalf("bound source %q is not host:port: %v", bound.LocalAddr(), err)
	}
	if host != bhost {
		t.Errorf("unbound source IP %s differs from the bound one %s", host, bhost)
	}
}
