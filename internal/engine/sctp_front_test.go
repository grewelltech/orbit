package engine

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/sctp"
)

// sctpFront puts the pipe-only mock AMF behind a real loopback SCTP listener,
// so the engine's genuine dial path (sctp.Dial, source selection, kernel
// port assignment) is exercised against it. Each accepted association is
// bridged to its own mock AMF connection. The test is skipped when the host
// has no kernel SCTP.
func sctpFront(t *testing.T, amf *mockamf.AMF) string {
	t.Helper()
	ln, err := isctp.ListenSCTPExt("sctp",
		&isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		isctp.InitMsg{NumOstreams: sctp.DefaultNGAPStreams, MaxInstreams: sctp.DefaultNGAPStreams})
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
	})
	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			go bridgeSCTP(c, amf.ConnectShared(ctx))
		}
	}()
	return ln.Addr().String()
}

// bridgeSCTP copies NGAP messages between one accepted SCTP association and
// one mock AMF transport, preserving the stream and tagging downlink with
// the NGAP PPID. It returns when either side closes.
func bridgeSCTP(c *isctp.SCTPConn, tr gnb.Transport) {
	defer func() { _ = c.Close() }()
	defer func() { _ = tr.Close() }()
	if err := c.SubscribeEvents(isctp.SCTP_EVENT_DATA_IO); err != nil {
		return
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			payload, stream, _, err := tr.ReadMsg(buf)
			if err != nil {
				_ = c.Close()
				return
			}
			if _, err := c.SCTPWrite(payload, &isctp.SndRcvInfo{Stream: stream, PPID: sctp.PPIDNGAP}); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 65536)
	for {
		n, info, err := c.SCTPRead(buf)
		if err != nil {
			return
		}
		var stream uint16
		if info != nil {
			stream = info.Stream
		}
		if err := tr.WriteNGAP(stream, append([]byte(nil), buf[:n]...)); err != nil {
			return
		}
	}
}

// With no bind addresses, every gNB in a fleet dials from the route-selected
// source: one address per gNB, the same IP for all (they route to the same
// AMF), and a distinct ephemeral port each, as the kernel bound them.
func TestNewFleetUnboundOneSourcePerGNB(t *testing.T) {
	amf, cfg, _, _ := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	cfg2 := cfg
	cfg2.ID, cfg2.Name = 2, "orbit-gnb-2"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := NewFleet(ctx, []GNBSpec{{Config: cfg, AMFAddr: addr}, {Config: cfg2, AMFAddr: addr}}, log)
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	defer f.Close()

	srcs := f.N2Sources()
	if len(srcs) != 2 {
		t.Fatalf("N2Sources = %v, want one per gNB", srcs)
	}
	hosts := map[string]int{}
	ports := map[string]int{}
	for _, s := range srcs {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			t.Fatalf("N2 source %q is not a single host:port: %v", s, err)
		}
		if host != "127.0.0.1" {
			t.Errorf("N2 source %q is not the route-selected loopback address", s)
		}
		if port == "0" {
			t.Errorf("N2 source %q has no real port", s)
		}
		hosts[host]++
		ports[port]++
	}
	if len(hosts) != 1 {
		t.Errorf("sources use %d different IPs, want 1: %v", len(hosts), srcs)
	}
	if len(ports) != 2 {
		t.Errorf("sources share a port, want distinct ephemeral ports: %v", srcs)
	}
}
