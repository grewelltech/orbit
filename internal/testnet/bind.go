package testnet

import (
	"errors"
	"syscall"
	"testing"
)

// RequireBind ends the test when binding what failed (err non-nil). Only a
// missing address skips: EADDRNOTAVAIL means this platform has no
// 127.0.N.x loopback alias (macOS without the aliases added), which is a
// capability the host lacks. Anything else fails, EADDRINUSE above all: that
// is two tests on one address, a collision to fix rather than a skip that
// hides lost coverage.
func RequireBind(t testing.TB, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Skipf("cannot bind %s: %v (this host has no such loopback address)", what, err)
	}
	t.Fatalf("cannot bind %s: %v", what, err)
}
