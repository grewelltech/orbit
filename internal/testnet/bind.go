package testnet

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
)

// RequireBind ends the test when binding addr (host:port, or a bare host)
// failed with err. Only a missing loopback alias skips: EADDRNOTAVAIL for an
// address in 127.0.0.0/8 means this platform has no such 127.0.N.x alias
// (macOS without the aliases added), a capability the host lacks. Anything
// else fails: EADDRINUSE above all, which is two tests on one address, a
// collision to fix rather than a skip that hides lost coverage; and
// EADDRNOTAVAIL for any other address, which is a wrong address, not a
// missing alias.
func RequireBind(t testing.TB, addr string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) && inLoopback8(addr) {
		t.Skipf("cannot bind %s: %v (this host has no such loopback address)", addr, err)
	}
	t.Fatalf("cannot bind %s: %v", addr, err)
}

// inLoopback8 reports whether addr's host is an IPv4 address in 127.0.0.0/8.
func inLoopback8(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.Is4() && ip.As4()[0] == 127
}
