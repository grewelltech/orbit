package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/free5gc/ngap/ngapType"
	isctp "github.com/ishidawataru/sctp"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// amfFront puts the pipe-only mock AMF behind a real loopback SCTP listener
// and records, per accepted association, when it was accepted and when the
// gNB side closed it. Tests steer it: a reply delay, an NGAP PDU to inject
// during a hold, a rejected NG Setup, or closing the association early.
type amfFront struct {
	t    *testing.T
	amf  *mockamf.AMF
	addr string

	// replyDelay (nanoseconds) holds the AMF's downlink back, so NG Setup
	// answers late. Atomic: the test sets it after the accept loop started.
	replyDelay atomic.Int64
	// reject answers the NG Setup Request with an NG Setup Failure instead
	// of letting the mock answer. Atomic for the same reason.
	reject atomic.Bool

	mu       sync.Mutex
	accepted []time.Time
	closed   []time.Time
	conns    []*isctp.SCTPConn
}

func newAMFFront(t *testing.T) *amfFront {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := isctp.ListenSCTPExt("sctp",
		&isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		isctp.InitMsg{NumOstreams: sctp.DefaultNGAPStreams, MaxInstreams: sctp.DefaultNGAPStreams})
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	f := &amfFront{t: t, amf: amf, addr: ln.Addr().String()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			_ = c.Close()
		}
		f.mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.accepted = append(f.accepted, time.Now())
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go f.serve(ctx, c)
		}
	}()
	return f
}

// serve bridges one association to the mock AMF (or rejects its NG Setup)
// and records when the gNB side closed it.
func (f *amfFront) serve(ctx context.Context, c *isctp.SCTPConn) {
	defer func() {
		f.mu.Lock()
		f.closed = append(f.closed, time.Now())
		f.mu.Unlock()
		_ = c.Close()
	}()
	if err := c.SubscribeEvents(isctp.SCTP_EVENT_DATA_IO); err != nil {
		return
	}
	tr := f.amf.ConnectShared(ctx)
	defer func() { _ = tr.Close() }()
	go func() {
		buf := make([]byte, 65536)
		for {
			payload, stream, _, err := tr.ReadMsg(buf)
			if err != nil {
				return
			}
			if d := time.Duration(f.replyDelay.Load()); d > 0 {
				time.Sleep(d)
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
		if f.reject.Load() {
			raw, err := ngap.Encode(buildNGSetupFailure())
			if err != nil {
				f.t.Errorf("encode NG Setup Failure: %v", err)
				return
			}
			if _, err := c.SCTPWrite(raw, &isctp.SndRcvInfo{Stream: stream, PPID: sctp.PPIDNGAP}); err != nil {
				return
			}
			continue
		}
		if err := tr.WriteNGAP(stream, append([]byte(nil), buf[:n]...)); err != nil {
			return
		}
	}
}

// snapshot returns the accept and close times seen so far.
func (f *amfFront) snapshot() (accepted, closed []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.accepted...), append([]time.Time(nil), f.closed...)
}

// waitClosed waits up to d for n associations to have been closed by the gNB
// side and returns the close times.
func (f *amfFront) waitClosed(n int, d time.Duration) []time.Time {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, closed := f.snapshot(); len(closed) >= n {
			return closed
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, closed := f.snapshot()
	return closed
}

// inject writes one NGAP PDU from the AMF side on the latest association.
func (f *amfFront) inject(pdu ngapType.NGAPPDU) error {
	raw, err := ngap.Encode(pdu)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conns) == 0 {
		return errors.New("no association to inject on")
	}
	_, err = f.conns[len(f.conns)-1].SCTPWrite(raw, &isctp.SndRcvInfo{Stream: 0, PPID: sctp.PPIDNGAP})
	return err
}

// closeLatest shuts the latest association from the AMF side.
func (f *amfFront) closeLatest() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conns) > 0 {
		_ = f.conns[len(f.conns)-1].Close()
	}
}

// buildNGSetupFailure is the smallest NG Setup Failure the gNB side decodes:
// an unsuccessful outcome for procedure 21 with a Cause IE.
func buildNGSetupFailure() ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentUnsuccessfulOutcome
	pdu.UnsuccessfulOutcome = new(ngapType.UnsuccessfulOutcome)
	pdu.UnsuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeNGSetup
	pdu.UnsuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.UnsuccessfulOutcome.Value.Present = ngapType.UnsuccessfulOutcomePresentNGSetupFailure
	pdu.UnsuccessfulOutcome.Value.NGSetupFailure = new(ngapType.NGSetupFailure)
	var ie ngapType.NGSetupFailureIEs
	ie.Id.Value = ngapType.ProtocolIEIDCause
	ie.Criticality.Value = ngapType.CriticalityPresentIgnore
	ie.Value.Present = ngapType.NGSetupFailureIEsPresentCause
	ie.Value.Cause = &ngapType.Cause{Present: ngapType.CausePresentMisc,
		Misc: &ngapType.CauseMisc{Value: ngapType.CauseMiscPresentUnknownPLMN}}
	pdu.UnsuccessfulOutcome.Value.NGSetupFailure.ProtocolIEs.List = append(
		pdu.UnsuccessfulOutcome.Value.NGSetupFailure.ProtocolIEs.List, ie)
	return pdu
}

// newCellClient serves a cellService whose log is captured and returns a
// Connect client for it plus the log buffer.
func newCellClient(t *testing.T) (orbitv1connect.CellServiceClient, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&syncWriter{w: &logBuf}, nil))
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewCellServiceHandler(&cellService{log: log}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orbitv1connect.NewCellServiceClient(http.DefaultClient, srv.URL), &logBuf
}

// syncWriter serialises the handler's log writes with the test's reads.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func ngsetupReq(amf string, timeoutMs, holdMs uint32) *orbitv1.RunNGSetupRequest {
	return &orbitv1.RunNGSetupRequest{
		AmfAddress: amf, TimeoutMs: timeoutMs, HoldMs: holdMs,
		Gnb: &orbitv1.GnbConfig{Id: 1, IdBits: 24, Name: "orbit-gnb", Mcc: "208", Mnc: "93", Tac: 1,
			Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}},
	}
}

// A hold keeps the association open for about the requested time after the
// NG Setup Response and then closes it; held_ms says so. With no hold the
// association closes at once, as before.
func TestRunNGSetupHold(t *testing.T) {
	tests := []struct {
		name             string
		holdMs           uint32
		minHeld, maxHeld time.Duration
	}{
		{name: "hold 300 ms", holdMs: 300, minHeld: 250 * time.Millisecond, maxHeld: 700 * time.Millisecond},
		{name: "no hold closes at once", holdMs: 0, minHeld: 0, maxHeld: 150 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			front := newAMFFront(t)
			c, _ := newCellClient(t)
			start := time.Now()
			res, err := c.RunNGSetup(context.Background(), connect.NewRequest(ngsetupReq(front.addr, 0, tc.holdMs)))
			if err != nil {
				t.Fatalf("RunNGSetup: %v", err)
			}
			if !res.Msg.GetAccepted() {
				t.Fatalf("NG Setup not accepted: %s", res.Msg.GetCause())
			}
			closed := front.waitClosed(1, 2*time.Second)
			if len(closed) != 1 {
				t.Fatal("the association was not closed")
			}
			accepted, _ := front.snapshot()
			open := closed[0].Sub(accepted[0])
			if open < tc.minHeld || open > tc.maxHeld {
				t.Errorf("association open for %s, want within [%s, %s]", open.Round(time.Millisecond), tc.minHeld, tc.maxHeld)
			}
			held := time.Duration(res.Msg.GetHeldMs()) * time.Millisecond
			if tc.holdMs == 0 && held != 0 {
				t.Errorf("held_ms = %s with no hold, want 0", held)
			}
			if tc.holdMs > 0 && (held < tc.minHeld || held > tc.maxHeld) {
				t.Errorf("held_ms = %s, want within [%s, %s]", held, tc.minHeld, tc.maxHeld)
			}
			if total := time.Since(start); total > tc.maxHeld+2*time.Second {
				t.Errorf("RPC took %s", total)
			}
		})
	}
}

// A hold above the cap is refused as InvalidArgument before anything is
// dialled; the cap itself is accepted by validation.
func TestRunNGSetupHoldCap(t *testing.T) {
	c, _ := newCellClient(t)
	_, err := c.RunNGSetup(context.Background(), connect.NewRequest(ngsetupReq("127.0.0.1:1", 0, uint32(maxNGSetupHold/time.Millisecond)+1)))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("hold above the cap: err = %v, want InvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "exceeds the cap") || !strings.Contains(err.Error(), "10m0s") {
		t.Errorf("error %q does not name the cap", err)
	}
	// Exactly the cap passes validation and fails only on the refused dial.
	_, err = c.RunNGSetup(context.Background(), connect.NewRequest(ngsetupReq("127.0.0.1:1", 500, uint32(maxNGSetupHold/time.Millisecond))))
	if connect.CodeOf(err) == connect.CodeInvalidArgument {
		t.Errorf("hold at the cap rejected: %v", err)
	}
}

// Cancelling the call during a hold closes the association promptly: the
// server never keeps a held association after its client is gone. The
// handler's own measurement is checked through holdAssociation directly.
func TestRunNGSetupHoldClientCancel(t *testing.T) {
	front := newAMFFront(t)
	c, _ := newCellClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := c.RunNGSetup(ctx, connect.NewRequest(ngsetupReq(front.addr, 0, 5000)))
	cancelledAt := time.Now()
	if err == nil {
		t.Fatal("cancelled RunNGSetup returned a response")
	}
	closed := front.waitClosed(1, time.Second)
	if len(closed) != 1 {
		t.Fatal("the association was not closed after the client cancelled")
	}
	if lag := closed[0].Sub(cancelledAt); lag > 200*time.Millisecond {
		t.Errorf("association closed %s after the cancel, want promptly", lag.Round(time.Millisecond))
	}
}

// holdAssociation ends on context cancellation and reports the time actually
// held, closing the connection exactly once (the reader exits).
func TestHoldAssociationCancelReportsHeld(t *testing.T) {
	front := newAMFFront(t)
	conn, err := sctp.Dial(context.Background(), "", front.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	svc := &cellService{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	held := svc.holdAssociation(ctx, conn, 5*time.Second)
	if held < 100*time.Millisecond || held > 500*time.Millisecond {
		t.Errorf("held = %s, want about 120 ms", held.Round(time.Millisecond))
	}
	if closed := front.waitClosed(1, time.Second); len(closed) != 1 {
		t.Error("the association was not closed")
	}
	// A second close is the caller's mistake, not ours; the connection is
	// already closed and ReadMsg must fail at once rather than block.
	if _, _, _, err := conn.ReadMsg(make([]byte, 16)); err == nil {
		t.Error("ReadMsg succeeded on a closed connection")
	}
}

// An NGAP PDU the AMF sends during the hold is drained and logged by kind,
// procedure code and stream, and does not end the hold.
func TestRunNGSetupHoldLogsInboundNGAP(t *testing.T) {
	front := newAMFFront(t)
	c, logBuf := newCellClient(t)
	go func() {
		time.Sleep(150 * time.Millisecond)
		if err := front.inject(gnb.BuildNGReset()); err != nil {
			t.Errorf("inject: %v", err)
		}
	}()
	res, err := c.RunNGSetup(context.Background(), connect.NewRequest(ngsetupReq(front.addr, 0, 500)))
	if err != nil {
		t.Fatalf("RunNGSetup: %v", err)
	}
	if held := time.Duration(res.Msg.GetHeldMs()) * time.Millisecond; held < 450*time.Millisecond {
		t.Errorf("held_ms = %s; the inbound PDU must not end the hold", held)
	}
	logs := logBuf.String()
	for _, want := range []string{"NGAP during hold", "kind=initiating", "proc=20", "stream=0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("hold log lacks %q:\n%s", want, logs)
		}
	}
}

// The AMF closing the association during a hold ends it early: the response
// is still a success and held_ms reflects the shorter time.
func TestRunNGSetupHoldEndsWhenPeerCloses(t *testing.T) {
	front := newAMFFront(t)
	c, logBuf := newCellClient(t)
	go func() {
		time.Sleep(150 * time.Millisecond)
		front.closeLatest()
	}()
	start := time.Now()
	res, err := c.RunNGSetup(context.Background(), connect.NewRequest(ngsetupReq(front.addr, 0, 5000)))
	if err != nil {
		t.Fatalf("RunNGSetup: %v", err)
	}
	if total := time.Since(start); total > 2*time.Second {
		t.Errorf("RPC took %s after the peer closed at 150 ms", total.Round(time.Millisecond))
	}
	if !res.Msg.GetAccepted() {
		t.Error("NG Setup result lost: accepted=false")
	}
	if held := time.Duration(res.Msg.GetHeldMs()) * time.Millisecond; held < 100*time.Millisecond || held > time.Second {
		t.Errorf("held_ms = %s, want about 150 ms", held)
	}
	if !strings.Contains(logBuf.String(), "why=\"peer closed\"") {
		t.Errorf("hold log does not say the peer closed:\n%s", logBuf.String())
	}
}

// An NG Setup Failure is not held: the response returns at once with
// held_ms 0, whatever hold_ms asked for.
func TestRunNGSetupFailureIsNotHeld(t *testing.T) {
	front := newAMFFront(t)
	front.reject.Store(true)
	c, _ := newCellClient(t)
	start := time.Now()
	res, err := c.RunNGSetup(context.Background(), connect.NewRequest(ngsetupReq(front.addr, 0, 3000)))
	if err != nil {
		t.Fatalf("RunNGSetup: %v", err)
	}
	if res.Msg.GetAccepted() {
		t.Fatal("rejected NG Setup reported as accepted")
	}
	if !strings.Contains(res.Msg.GetCause(), "misc") {
		t.Errorf("cause = %q, want the misc cause", res.Msg.GetCause())
	}
	if res.Msg.GetHeldMs() != 0 {
		t.Errorf("held_ms = %d on a failure, want 0", res.Msg.GetHeldMs())
	}
	if total := time.Since(start); total > time.Second {
		t.Errorf("RPC took %s; a failure must not hold", total.Round(time.Millisecond))
	}
	if closed := front.waitClosed(1, time.Second); len(closed) != 1 {
		t.Error("the association was not closed after the failure")
	}
}

// timeout_ms bounds the dial plus the NG Setup exchange on its own; a hold
// never extends that budget, so an AMF that answers after the timeout fails
// with DeadlineExceeded even with a long hold requested.
func TestRunNGSetupHoldDoesNotExtendSetupTimeout(t *testing.T) {
	front := newAMFFront(t)
	front.replyDelay.Store(int64(400 * time.Millisecond))
	c, _ := newCellClient(t)
	start := time.Now()
	_, err := c.RunNGSetup(context.Background(), connect.NewRequest(ngsetupReq(front.addr, 200, 5000)))
	if err == nil {
		t.Fatal("NG Setup answered after the timeout succeeded")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("err = %v, want the setup deadline", err)
	}
	if total := time.Since(start); total > 2*time.Second {
		t.Errorf("RPC took %s; the hold extended the setup budget", total.Round(time.Millisecond))
	}
}
