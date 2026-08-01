package cli

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestResponderRequiresBind is the load-bearing security assertion of ADR-0007:
// the responder is a remotely-aimable traffic generator, so it must refuse to
// start until the operator states where it listens. A default — loopback or
// otherwise — would make reachability implicit.
func TestResponderRequiresBind(t *testing.T) {
	cmd := newResponderCmd("test")
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("responder started with no --bind; it must be required")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("error should name the missing flag, got %q", err)
	}
}

func TestResponderRejectsMalformedBind(t *testing.T) {
	for _, bind := range []string{"not-a-host-port", "127.0.0.1"} {
		t.Run(bind, func(t *testing.T) {
			cmd := newResponderCmd("test")
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--bind", bind})

			err := cmd.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("accepted malformed --bind %q", bind)
			}
			if !strings.Contains(err.Error(), "--bind") {
				t.Errorf("error should mention --bind, got %q", err)
			}
		})
	}
}

// TestResponderWarnsOnRoutableBindWithoutToken mirrors loomd's guard: an
// unauthenticated control plane on a reachable address is a real exposure, so
// it must be called out on the way up.
func TestResponderWarnsOnRoutableBindWithoutToken(t *testing.T) {
	routable := routableAddr(t)

	tests := []struct {
		name     string
		args     []string
		wantWarn bool
	}{
		{"routable without token", []string{"--bind", routable}, true},
		{"routable with token", []string{"--bind", routable, "--token", "s3cret"}, false},
		{"loopback without token", []string{"--bind", "127.0.0.1:0"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			cmd := newResponderCmd("test")
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&stderr)
			cmd.SetArgs(tc.args)

			// Serve blocks; cancelling drains it via GracefulStop.
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatalf("responder returned an error: %v", err)
			}

			gotWarn := strings.Contains(stderr.String(), "WARNING")
			if gotWarn != tc.wantWarn {
				t.Errorf("warning present = %v, want %v (stderr: %q)", gotWarn, tc.wantWarn, stderr.String())
			}
		})
	}
}

// TestResponderRejectsNegativeTuning guards the duration/count conversions
// before they reach loom.
func TestResponderRejectsNegativeTuning(t *testing.T) {
	for _, args := range [][]string{
		{"--bind", "127.0.0.1:0", "--telemetry-interval", "-1s"},
		{"--bind", "127.0.0.1:0", "--max-flows", "-1"},
	} {
		cmd := newResponderCmd("test")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

// routableAddr returns a bindable non-loopback address on this host, so the
// routable-vs-loopback branch is exercised for real rather than by string
// inspection. Skips when the host has only loopback.
func routableAddr(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return net.JoinHostPort(ipnet.IP.String(), "0")
	}
	t.Skip("no non-loopback IPv4 address on this host")
	return ""
}
