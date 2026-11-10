package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// ResponderDownlink makes a loom responder send UDP to the UE's address:
// datagrams arrive on the trigger's port at a socket standing in for the
// UE (an ephemeral port here; 5001 by default). An unreachable responder and a missing one are errors.
func TestResponderDownlink(t *testing.T) {
	agent := startLoomAgent(t, "test")
	ue, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ue.Close() }()
	port := ue.LocalAddr().(*net.UDPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var trig DownlinkTrigger = ResponderDownlink{Peer: agent, Port: port}
	if err := trig.SendDownlink(ctx, netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatalf("SendDownlink: %v", err)
	}
	buf := make([]byte, 2048)
	if err := ue.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _, err := ue.ReadFromUDP(buf)
	if err != nil || n == 0 {
		t.Fatalf("no downlink datagram reached the UE address: n=%d, %v", n, err)
	}

	if err := (ResponderDownlink{}).SendDownlink(ctx, netip.MustParseAddr("127.0.0.1")); err == nil || !strings.Contains(err.Error(), "no responder configured") {
		t.Errorf("SendDownlink without a responder: %v", err)
	}
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := dead.Addr().String()
	_ = dead.Close()
	sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	if err := (ResponderDownlink{Peer: addr}).SendDownlink(sctx, netip.MustParseAddr("127.0.0.1")); err == nil || !strings.Contains(err.Error(), addr) {
		t.Errorf("SendDownlink to a dead responder: %v", err)
	}
}

// DownlinkTriggerFunc passes its arguments through and returns the
// function's error.
func TestDownlinkTriggerFunc(t *testing.T) {
	want := errors.New("no route")
	var got netip.Addr
	var trig DownlinkTrigger = DownlinkTriggerFunc(func(_ context.Context, ip netip.Addr) error {
		got = ip
		return want
	})
	ip := netip.MustParseAddr("10.60.0.1")
	if err := trig.SendDownlink(context.Background(), ip); !errors.Is(err, want) || got != ip {
		t.Errorf("SendDownlink = %v with %s, want %v with %s", err, got, want, ip)
	}
}
