package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// recordingUPF is a mockamf.UPF with no sockets that records the sessions
// it is told were released.
type recordingUPF struct {
	mu       sync.Mutex
	released []mockamf.SessionInfo
}

func (*recordingUPF) SessionUp(mockamf.SessionInfo)   {}
func (*recordingUPF) PathSwitched(mockamf.PathSwitch) {}
func (u *recordingUPF) SessionReleased(s mockamf.SessionInfo) {
	u.mu.Lock()
	u.released = append(u.released, s)
	u.mu.Unlock()
}

func (u *recordingUPF) releases() []mockamf.SessionInfo {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]mockamf.SessionInfo(nil), u.released...)
}

// releaseTestManager registers one UE with a PDU session against an
// in-process mock AMF adjusted by opts. The data path is never opened, so
// no N3 socket is bound.
func releaseTestManager(t *testing.T, supi string, opts ...mockamf.Option) (*Manager, *mockamf.AMF, *recordingUPF, *AttachResult) {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	upf := &recordingUPF{}
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: upf, UPFAddress: "127.0.0.1",
	}, append([]mockamf.Option{mockamf.WithGUTI()}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if n := amf.EncodeErrors(); n != 0 {
			t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
		}
	})
	_, cfg, _, _ := newBindTestAMF(t)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = (&fakeN2Dialer{amf: amf}).dial
	m.releaseLinger = time.Second
	ueCfg := bindTestUE(t, supi, ki, opc)
	ueCfg.PDUSession = &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"}
	ueCfg.GNBN3Addr = "127.0.0.1"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, ueCfg)
	if err != nil || !res.SessionActive {
		t.Fatalf("Register with a PDU session: %+v, %v", res, err)
	}
	return m, amf, upf, res
}

// ReleasePDUSession reports each answer the network can give: both release
// commands (the NAS one inside the NGAP one, or apart), one of them alone,
// a reject, and nothing. It answers what it gets, and the deregistration
// after it still gets its UE Context Release, a wait that ran out included.
func TestReleasePDUSession(t *testing.T) {
	answer := func(a mockamf.PDUReleaseAnswer) []mockamf.Option {
		return []mockamf.Option{mockamf.WithPDUReleaseAnswer(a)}
	}
	tests := []struct {
		name          string
		opts          []mockamf.Option
		wantNAS       bool
		wantInNGAP    bool
		wantResource  bool
		wantRejected  bool
		wantCompletes int64
		wantResponses int64
		wantUPFTold   bool
		minWait       time.Duration
	}{
		{name: "the NAS command in the NGAP command", wantNAS: true, wantInNGAP: true, wantResource: true,
			wantCompletes: 1, wantResponses: 1, wantUPFTold: true},
		{name: "the two commands apart", opts: answer(mockamf.ReleaseSeparately), wantNAS: true, wantResource: true,
			wantCompletes: 1, wantResponses: 1, wantUPFTold: true},
		{name: "the NAS command alone", opts: answer(mockamf.ReleaseNASOnly), wantNAS: true,
			wantCompletes: 1, wantUPFTold: true, minWait: 150 * time.Millisecond},
		{name: "the NGAP command alone", opts: answer(mockamf.ReleaseNGAPOnly), wantResource: true,
			wantResponses: 1, wantUPFTold: true, minWait: 150 * time.Millisecond},
		{name: "the user plane kept", opts: []mockamf.Option{mockamf.WithPDUReleaseKeepingUserPlane()}, wantNAS: true, wantInNGAP: true, wantResource: true,
			wantCompletes: 1, wantResponses: 1},
		{name: "a reject", opts: answer(mockamf.RejectPDURelease), wantRejected: true},
		{name: "no answer", opts: answer(mockamf.IgnorePDURelease), minWait: 400 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const supi = "208930100000001"
			m, amf, upf, reg := releaseTestManager(t, supi, tc.opts...)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			res, err := m.ReleasePDUSession(ctx, supi, WithPDUReleaseWait(400*time.Millisecond), WithPDUReleaseSettle(150*time.Millisecond))
			if err != nil {
				t.Fatalf("ReleasePDUSession: %v", err)
			}
			if took := time.Since(start); took < tc.minWait || took > tc.minWait+2*time.Second {
				t.Errorf("took %s, want about %s", took, tc.minWait)
			}
			if res.PDUSessionID != 1 || res.PTI != 1 || res.DLTEID != reg.DLTEID || res.SentAt.IsZero() {
				t.Errorf("result %+v, want session 1, PTI 1, DL TEID %#x", res, reg.DLTEID)
			}
			if res.NASCommand != tc.wantNAS || res.NASInResourceRelease != tc.wantInNGAP || res.ResourceRelease != tc.wantResource || res.Rejected != tc.wantRejected {
				t.Errorf("result %+v, want NAS %v (in NGAP %v), NGAP %v, rejected %v", res, tc.wantNAS, tc.wantInNGAP, tc.wantResource, tc.wantRejected)
			}
			if res.Released() != (tc.wantNAS && tc.wantResource) || res.CompleteSent != tc.wantNAS || res.DecodeErr != nil {
				t.Errorf("released %v, complete sent %v, decode error %v", res.Released(), res.CompleteSent, res.DecodeErr)
			}
			if tc.wantNAS && (res.CommandPTI != 1 || res.CommandCause != 36 || res.NASCommandAt.IsZero()) {
				t.Errorf("command PTI %d, cause %d at %v; want PTI 1, cause 36", res.CommandPTI, res.CommandCause, res.NASCommandAt)
			}
			if tc.wantResource == res.ResourceReleaseAt.IsZero() {
				t.Errorf("NGAP command %v at %v", res.ResourceRelease, res.ResourceReleaseAt)
			}
			if tc.wantRejected && res.RejectCause != 43 {
				t.Errorf("reject cause %d, want 43", res.RejectCause)
			}
			deadline := time.Now().Add(time.Second)
			for (amf.PDUReleaseCompletes() < tc.wantCompletes || amf.PDUResourceReleaseResponses() < tc.wantResponses) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if amf.PDUReleaseRequests() != 1 || amf.PDUReleaseCompletes() != tc.wantCompletes || amf.PDUResourceReleaseResponses() != tc.wantResponses {
				t.Errorf("the mock got %d requests, %d completes, %d NGAP responses; want 1, %d, %d",
					amf.PDUReleaseRequests(), amf.PDUReleaseCompletes(), amf.PDUResourceReleaseResponses(), tc.wantCompletes, tc.wantResponses)
			}
			if got := upf.releases(); (len(got) == 1) != tc.wantUPFTold || (tc.wantUPFTold && (got[0].SUPI != supi || got[0].PDUSessionID != 1 || got[0].DL.TEID != reg.DLTEID)) {
				t.Errorf("the UPF was told of %+v, want told %v", got, tc.wantUPFTold)
			}
			// The session is still the manager's, and its deregistration
			// reads the association after the release's last read.
			sess, err := m.Status(supi)
			if err != nil || !sess.Result.SessionActive {
				t.Fatalf("after the release: session %+v, %v", sess, err)
			}
			// A released session is no longer one the UE holds: an AN
			// release or a Service Request does not name it.
			if held := len(sess.heldSessions()); (held == 0) != res.Released() {
				t.Errorf("the UE holds %d sessions after a release that ended %v", held, res.Released())
			}
			dres, err := m.DeregisterWith(ctx, supi)
			if err != nil {
				t.Fatalf("DeregisterWith: %v", err)
			}
			if dres.Release == nil {
				t.Errorf("the deregistration after the release saw no UE Context Release: %v", dres.ReleaseErr)
			}
		})
	}
}

// ReleasePDUSession refuses a UE that is not registered or has no PDU
// session before sending anything, and ends with the caller's context.
func TestReleasePDUSessionRefusalsAndBounds(t *testing.T) {
	t.Run("unknown UE", func(t *testing.T) {
		m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
		if res, err := m.ReleasePDUSession(context.Background(), "208930100000009"); err == nil || res != nil {
			t.Errorf("= %+v, %v; want an error", res, err)
		}
	})
	t.Run("no PDU session", func(t *testing.T) {
		amf, cfg, ki, opc := newBindTestAMF(t)
		m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
		m.dial = (&fakeN2Dialer{amf: amf}).dial
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		const supi = "208930100000001"
		if _, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supi, ki, opc)); err != nil {
			t.Fatal(err)
		}
		if res, err := m.ReleasePDUSession(ctx, supi); err == nil || res != nil {
			t.Errorf("= %+v, %v; want an error", res, err)
		}
		if amf.PDUReleaseRequests() != 0 {
			t.Errorf("the mock got %d release requests", amf.PDUReleaseRequests())
		}
	})
	t.Run("the caller's context ends the wait", func(t *testing.T) {
		const supi = "208930100000001"
		m, _, _, _ := releaseTestManager(t, supi, mockamf.WithPDUReleaseAnswer(mockamf.IgnorePDURelease))
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		res, err := m.ReleasePDUSession(ctx, supi)
		if err != nil || res == nil || res.NASCommand || res.ResourceRelease {
			t.Errorf("= %+v, %v; want an empty result", res, err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("took %s with a 200ms context and the default 16s wait", took)
		}
	})
	t.Run("the association is lost during the wait", func(t *testing.T) {
		const supi = "208930100000001"
		m, _, _, _ := releaseTestManager(t, supi, mockamf.WithPDUReleaseAnswer(mockamf.DropOnPDURelease))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := m.ReleasePDUSession(ctx, supi, WithPDUReleaseWait(2*time.Second))
		if !errors.Is(err, ErrPDUReleaseAssociationLost) || res == nil || res.SentAt.IsZero() {
			t.Errorf("= %+v, %v; want the request sent and ErrPDUReleaseAssociationLost", res, err)
		}
		if ctx.Err() != nil {
			t.Error("the loss was only noticed when the context ended")
		}
	})
}

// ReleasePDUSession on a CM-IDLE UE is refused before anything is sent,
// like a handover, and the same UE releases once a Service Request has
// brought it back to CM-CONNECTED.
func TestReleasePDUSessionNeedsAConnectedUE(t *testing.T) {
	tests := []struct {
		name         string
		idle         bool
		serviceReq   bool
		wantErr      string
		wantRequests int64
	}{
		{name: "connected", wantRequests: 1},
		{name: "CM-IDLE", idle: true, wantErr: "is CM-IDLE"},
		{name: "CM-IDLE then a Service Request", idle: true, serviceReq: true, wantRequests: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const supi = "208930100000001"
			m, amf, _, _ := releaseTestManager(t, supi)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tc.idle {
				if _, err := m.Idle(ctx, supi); err != nil {
					t.Fatalf("Idle: %v", err)
				}
			}
			if tc.serviceReq {
				if _, err := m.ServiceRequest(ctx, supi); err != nil {
					t.Fatalf("ServiceRequest: %v", err)
				}
			}
			res, err := m.ReleasePDUSession(ctx, supi, WithPDUReleaseWait(2*time.Second), WithPDUReleaseSettle(150*time.Millisecond))
			if tc.wantErr != "" {
				if err == nil || res != nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("= %+v, %v; want nil and an error containing %q", res, err, tc.wantErr)
				}
				if sess, serr := m.Status(supi); serr != nil || !sess.Idle() {
					t.Errorf("after the refusal: %v, idle %v; want the UE still CM-IDLE", serr, serr == nil && sess.Idle())
				}
			} else if err != nil || res == nil || !res.Released() {
				t.Errorf("= %+v, %v; want the session released", res, err)
			}
			if got := amf.PDUReleaseRequests(); got != tc.wantRequests {
				t.Errorf("the mock got %d release requests, want %d", got, tc.wantRequests)
			}
		})
	}
}

// scriptedConn is a Transport whose reads return the PDUs fed to it.
type scriptedConn struct {
	in     chan []byte
	closed chan struct{}
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{in: make(chan []byte, 8), closed: make(chan struct{})}
}

func (c *scriptedConn) WriteNGAP(uint16, []byte) error { return nil }
func (c *scriptedConn) Close() error                   { close(c.closed); return nil }
func (c *scriptedConn) ReadMsg([]byte) ([]byte, uint16, uint32, error) {
	select {
	case b := <-c.in:
		return b, 1, sctp.PPIDNGAP, nil
	case <-c.closed:
		return nil, 0, 0, errors.New("closed")
	}
}

func (c *scriptedConn) feed(t *testing.T, pdu ngapType.NGAPPDU) {
	t.Helper()
	b, err := ngap.Encode(pdu)
	if err != nil {
		t.Fatal(err)
	}
	c.in <- b
}

// A read that timed out is resumed by the next one, so the PDU that
// arrives in between is not lost; dropHeld discards one that has already
// arrived; and a kept read of an association the session has left is not
// taken for the new one's.
func TestSessionReadPDUKeepsAnAbandonedRead(t *testing.T) {
	short := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 50*time.Millisecond)
	}
	long := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 5*time.Second)
	}
	isComplete := func(pdu *ngapType.NGAPPDU, ranID int64) bool {
		if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentSuccessfulOutcome || pdu.SuccessfulOutcome.Value.UEContextReleaseComplete == nil {
			return false
		}
		for _, ie := range pdu.SuccessfulOutcome.Value.UEContextReleaseComplete.ProtocolIEs.List {
			if ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID {
				return ie.Value.RANUENGAPID.Value == ranID
			}
		}
		return false
	}

	t.Run("resumed", func(t *testing.T) {
		conn := newScriptedConn()
		defer func() { _ = conn.Close() }()
		s := &Session{conn: conn}
		ctx, cancel := short()
		if _, err := s.readPDU(ctx); err == nil {
			t.Fatal("read of an idle association returned")
		}
		cancel()
		conn.feed(t, gnb.BuildUEContextReleaseComplete(1, 11))
		conn.feed(t, gnb.BuildUEContextReleaseComplete(1, 12))
		for _, want := range []int64{11, 12} {
			ctx, cancel := long()
			pdu, err := s.readPDU(ctx)
			cancel()
			if err != nil || !isComplete(pdu, want) {
				t.Fatalf("read = %+v, %v; want the PDU for RAN ID %d", pdu, err, want)
			}
		}
	})
	t.Run("dropped once it has arrived", func(t *testing.T) {
		conn := newScriptedConn()
		defer func() { _ = conn.Close() }()
		s := &Session{conn: conn}
		ctx, cancel := short()
		_, _ = s.readPDU(ctx)
		cancel()
		s.dropHeld() // still in flight: kept
		if s.held == nil {
			t.Fatal("a read in flight was dropped")
		}
		conn.feed(t, gnb.BuildUEContextReleaseComplete(1, 11))
		deadline := time.Now().Add(2 * time.Second)
		for len(s.held) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		s.dropHeld()
		if s.held != nil {
			t.Fatal("a finished read was kept")
		}
		conn.feed(t, gnb.BuildUEContextReleaseComplete(1, 12))
		ctx, cancel = long()
		defer cancel()
		if pdu, err := s.readPDU(ctx); err != nil || !isComplete(pdu, 12) {
			t.Fatalf("read = %+v, %v; want the PDU for RAN ID 12", pdu, err)
		}
		s.dropHeld() // nothing kept: a no-op
	})
	t.Run("not carried to another association", func(t *testing.T) {
		old, next := newScriptedConn(), newScriptedConn()
		defer func() { _ = next.Close() }()
		s := &Session{conn: old}
		ctx, cancel := short()
		_, _ = s.readPDU(ctx)
		cancel()
		s.conn = next
		_ = old.Close()
		next.feed(t, gnb.BuildUEContextReleaseComplete(1, 21))
		ctx, cancel = long()
		defer cancel()
		if pdu, err := s.readPDU(ctx); err != nil || !isComplete(pdu, 21) {
			t.Fatalf("read = %+v, %v; want the new association's PDU", pdu, err)
		}
	})
}
