package engine

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestCloseDataPathUnderLiveTCPStream is the lock-order regression for the
// netstack teardown path: closeDataPath (and every other dpMu holder that
// reaches GNBStack.Detach/Close — deregistration, ReleaseGNB, the inter-gNB
// move) must complete while a live TCP stream is pumping segments through
// the bridged stacks.
//
// The hazard it pins: gVisor's TCP dispatcher processes inbound segments
// while HOLDING the endpoint lock and writes the responses synchronously
// through the link endpoint — stackTx.TxCommit → Session.SendUplink. If
// SendUplink took s.dpMu, a teardown holding dpMu across Detach (whose
// RemoveAddress → abortConns → Endpoint.Close blocks on that same endpoint
// lock) would be a permanent AB-BA deadlock, wedging SendUplink, DataStats,
// and ReleaseGNB for every consumer of the session. SendUplink is therefore
// lock-free w.r.t. dpMu (atomic tunnel-view pointer — see Session.SendUplink).
func TestCloseDataPathUnderLiveTCPStream(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	sess := newDataSession(m, "001010000000045", upf.LocalAddr().String(), "0", 0x11, 0x100, 1)
	ueIP := net.ParseIP(sess.Result.PDUAddress)
	var dlTEID atomic.Uint32
	dlTEID.Store(0x100)
	hairpinUPF(upf, &dlTEID)

	nw, err := sess.appNetwork("http", ueIP)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := nw.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	// Server side: accept and pump data downlink as fast as TCP allows, so
	// the dispatcher is continuously processing segments (and emitting ACKs
	// and data through SendUplink) when the teardown lands.
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64<<10)
		for {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	}()
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := nw.DialContext(dctx, "tcp", ln.Addr().String())
	dcancel()
	if err != nil {
		t.Fatalf("dial through the tunnel: %v", err)
	}
	defer conn.Close()
	// Client side: read continuously so the window keeps opening and the
	// stream never quiesces.
	streaming := make(chan struct{})
	go func() {
		buf := make([]byte, 64<<10)
		total := 0
		for {
			n, err := conn.Read(buf)
			total += n
			if total > 256<<10 {
				select {
				case <-streaming:
				default:
					close(streaming)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-streaming:
	case <-time.After(15 * time.Second):
		t.Fatal("TCP stream never got going over the hairpin UPF")
	}

	// Tear the data path down mid-stream. Pre-fix this deadlocked
	// deterministically (teardown held dpMu and blocked in Endpoint.Close;
	// the dispatcher held the endpoint lock and blocked in SendUplink→dpMu).
	done := make(chan struct{})
	go func() {
		sess.closeDataPath()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("closeDataPath deadlocked under a live TCP stream (dpMu ↔ gVisor endpoint lock AB-BA)")
	}
}
