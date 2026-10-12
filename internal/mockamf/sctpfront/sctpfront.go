// Package sctpfront puts the pipe-only mock AMF behind a real loopback SCTP
// listener, so tests exercise ORBIT's genuine dial path (sctp.Dial, source
// selection, kernel port assignment) against it. It is test support: only
// _test files import it.
package sctpfront

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	isctp "github.com/ishidawataru/sctp"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Front is a running loopback SCTP listener in front of a mock AMF.
type Front struct {
	// Addr is the listener's address (host:port), for the gNB to dial.
	Addr   string
	active atomic.Int64
}

// Active is the number of associations currently bridged: accepted and not
// yet closed by either side.
func (f *Front) Active() int { return int(f.active.Load()) }

// Listen starts a loopback SCTP listener in front of amf and returns its
// address (see Start).
func Listen(t testing.TB, amf *mockamf.AMF) string {
	t.Helper()
	return Start(t, amf).Addr
}

// Start starts a loopback SCTP listener bridging each accepted association
// to its own mock AMF connection. The listener closes when the test ends.
// The test is skipped when the host has no kernel SCTP.
func Start(t testing.TB, amf *mockamf.AMF) *Front {
	t.Helper()
	f := &Front{}
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
			f.active.Add(1)
			go func() {
				defer f.active.Add(-1)
				bridge(c, amf.ConnectShared(ctx))
			}()
		}
	}()
	// The pinned library reads the address with a stack buffer passed to
	// getsockopt as a uintptr through a helper, so a stack move between the
	// conversion and the syscall can make the read fail (seen under -race,
	// #131). A retry runs on the already-grown stack.
	for i := 0; i < 5; i++ {
		if addr := ln.Addr(); addr != nil {
			f.Addr = addr.String()
			return f
		}
	}
	t.Fatalf("SCTP listener reports no local address")
	return nil
}

// bridge copies NGAP messages between one accepted SCTP association and one
// mock AMF transport, preserving the stream and tagging downlink with the
// NGAP PPID. It returns when either side closes.
func bridge(c *isctp.SCTPConn, tr gnb.Transport) {
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
