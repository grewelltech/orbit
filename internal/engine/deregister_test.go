package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// DeregisterWith reports the core's answer to each kind of deregistration:
// a switch-off draws the release alone, and a normal one the Deregistration
// Accept and then the release. A core that sends a normal one no Accept is
// reported, and one that answers it with nothing at all is sent a
// switch-off before the association closes.
func TestDeregisterWith(t *testing.T) {
	normal := []DeregisterOption{WithNormalDeregistration(300 * time.Millisecond)}
	tests := []struct {
		name          string
		amfOpts       []mockamf.Option
		opts          []DeregisterOption
		wantSwitchOff bool
		wantAccepted  bool
		wantAcceptErr string
		wantRelease   bool
		wantRelErr    string
		wantFallback  bool
		wantDeregs    int64
		wantAccepts   int64
		wantDetail    string
	}{
		{name: "switch-off: the release", wantSwitchOff: true, wantRelease: true, wantDeregs: 1,
			wantDetail: "UE deregistered (switch-off)"},
		{name: "switch-off: no release", amfOpts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			wantSwitchOff: true, wantRelErr: "the core sent no UE Context Release within 200ms", wantDeregs: 1,
			wantDetail: "UE deregistered (switch-off); the core sent no UE Context Release within 200ms"},
		{name: "switch-off: no release within the call's own wait", amfOpts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			opts:          []DeregisterOption{WithReleaseWait(50 * time.Millisecond)},
			wantSwitchOff: true, wantRelErr: "the core sent no UE Context Release within 50ms", wantDeregs: 1,
			wantDetail: "UE deregistered (switch-off); the core sent no UE Context Release within 50ms"},
		{name: "normal: the Accept, no release within the call's own wait", amfOpts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			opts:         []DeregisterOption{WithNormalDeregistration(300 * time.Millisecond), WithReleaseWait(50 * time.Millisecond), WithReleaseWait(0)},
			wantAccepted: true, wantRelErr: "the core sent no UE Context Release within 200ms", wantDeregs: 1, wantAccepts: 1,
			wantDetail: "UE deregistered; the core sent no UE Context Release within 200ms"},
		{name: "normal: the Accept, then the release", opts: normal, wantAccepted: true, wantRelease: true,
			wantDeregs: 1, wantAccepts: 1, wantDetail: "UE deregistered"},
		{name: "normal: the Accept, no release", opts: normal, amfOpts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			wantAccepted: true, wantRelErr: "the core sent no UE Context Release within 200ms", wantDeregs: 1, wantAccepts: 1,
			wantDetail: "UE deregistered; the core sent no UE Context Release within 200ms"},
		{name: "normal: a release without the Accept", opts: normal, amfOpts: []mockamf.Option{mockamf.WithNoDeregistrationAccept()},
			wantAcceptErr: "the core sent no Deregistration Accept: a UE Context Release Command came instead", wantRelease: true, wantDeregs: 1,
			wantDetail: "UE deregistered; the core sent no Deregistration Accept: a UE Context Release Command came instead"},
		{name: "normal: no answer, so a switch-off follows", opts: normal,
			amfOpts:       []mockamf.Option{mockamf.WithNoDeregistrationAccept(), mockamf.WithNoReleaseOnDeregistration()},
			wantAcceptErr: "the core sent no Deregistration Accept within 300ms", wantRelErr: "the core sent no UE Context Release within 200ms",
			wantFallback: true, wantDeregs: 2,
			wantDetail: "UE deregistered; the core sent no Deregistration Accept within 300ms; the core sent no UE Context Release within 200ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, append([]mockamf.Option{mockamf.WithGUTI()}, tc.amfOpts...)...)
			if err != nil {
				t.Fatal(err)
			}
			_, cfg, _, _ := newBindTestAMF(t)
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.dial = (&fakeN2Dialer{amf: amf}).dial
			m.releaseLinger = 200 * time.Millisecond
			events, stop := m.Subscribe()
			defer stop()

			const supi = "208930100000001"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			reg, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supi, ki, opc))
			if err != nil || !reg.Registered {
				t.Fatalf("Register: %v", err)
			}
			res, err := m.DeregisterWith(ctx, supi, tc.opts...)
			if err != nil {
				t.Fatalf("DeregisterWith: %v", err)
			}
			if res.SwitchOff != tc.wantSwitchOff || res.Accepted != tc.wantAccepted || (res.Release != nil) != tc.wantRelease || res.SwitchOffFallback != tc.wantFallback {
				t.Errorf("result %+v, want switch-off %v, accepted %v, release %v, fallback %v", res, tc.wantSwitchOff, tc.wantAccepted, tc.wantRelease, tc.wantFallback)
			}
			if res.AMFUENGAPID != reg.AMFUENGAPID || res.RANUENGAPID != reg.RANUENGAPID || res.SentAt.IsZero() {
				t.Errorf("result names connection (%d, %d) sent at %v, want (%d, %d)", res.AMFUENGAPID, res.RANUENGAPID, res.SentAt, reg.AMFUENGAPID, reg.RANUENGAPID)
			}
			if res.Accepted != !res.AcceptAt.IsZero() {
				t.Errorf("accepted %v with AcceptAt %v", res.Accepted, res.AcceptAt)
			}
			if got := errText(res.AcceptErr); got != tc.wantAcceptErr {
				t.Errorf("AcceptErr %q, want %q", got, tc.wantAcceptErr)
			}
			if got := errText(res.ReleaseErr); got != tc.wantRelErr {
				t.Errorf("ReleaseErr %q, want %q", got, tc.wantRelErr)
			}
			if tc.wantAcceptErr != "" && !errors.Is(res.AcceptErr, errNoDeregistrationAccept) {
				t.Errorf("AcceptErr %v is not errNoDeregistrationAccept", res.AcceptErr)
			}
			if tc.wantRelErr != "" && !errors.Is(res.ReleaseErr, errNoUEContextRelease) {
				t.Errorf("ReleaseErr %v is not errNoUEContextRelease", res.ReleaseErr)
			}
			if r := res.Release; r != nil && (!r.HasAMFUENGAPID || !r.HasRANUENGAPID || r.AMFUENGAPID != reg.AMFUENGAPID || r.RANUENGAPID != reg.RANUENGAPID || r.At.IsZero()) {
				t.Errorf("release %+v, want the pair (%d, %d)", r, reg.AMFUENGAPID, reg.RANUENGAPID)
			}
			if got := amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("the mock got %d Deregistration Requests, want %d", got, tc.wantDeregs)
			}
			if got := amf.DeregistrationAccepts(); got != tc.wantAccepts {
				t.Errorf("the mock sent %d Deregistration Accepts, want %d", got, tc.wantAccepts)
			}
			wantCompletes := int64(0)
			if tc.wantRelease {
				wantCompletes = 1
			}
			deadline := time.Now().Add(time.Second)
			for amf.ReleaseCompletes() < wantCompletes && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := amf.ReleaseCompletes(); got != wantCompletes {
				t.Errorf("the mock got %d UE Context Release Completes, want %d", got, wantCompletes)
			}
			if _, err := m.Status(supi); err == nil {
				t.Error("the UE is still in the manager")
			}
			var detail string
			for detail == "" {
				select {
				case ev := <-events:
					if ev.SUPI == supi && ev.State == StateDeregistered {
						detail = ev.Detail
					}
				case <-time.After(time.Second):
					t.Fatal("no DEREGISTERED event")
				}
			}
			if detail != tc.wantDetail {
				t.Errorf("DEREGISTERED detail %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// A normal deregistration's waits end with the caller's context, and a UE
// that is not registered is refused before anything is sent.
func TestDeregisterWithBounds(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, mockamf.WithGUTI(), mockamf.WithNoDeregistrationAccept(), mockamf.WithNoReleaseOnDeregistration())
	if err != nil {
		t.Fatal(err)
	}
	_, cfg, _, _ := newBindTestAMF(t)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = (&fakeN2Dialer{amf: amf}).dial

	if res, err := m.DeregisterWith(context.Background(), "208930100000009", WithNormalDeregistration(0)); err == nil || res != nil {
		t.Errorf("DeregisterWith for an unknown UE = %+v, %v; want an error", res, err)
	}

	const supi = "208930100000001"
	rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := m.Register(rctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supi, ki, opc)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer dcancel()
	start := time.Now()
	res, err := m.DeregisterWith(dctx, supi, WithNormalDeregistration(0))
	if err != nil {
		t.Fatalf("DeregisterWith: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s with a 300ms context", took)
	}
	if res.Accepted || res.Release != nil || res.SwitchOffFallback {
		t.Errorf("result %+v, want no Accept, no release and no fallback after the context ended", res)
	}
	for _, e := range []error{res.AcceptErr, res.ReleaseErr} {
		if e == nil || !strings.Contains(e.Error(), "context deadline exceeded") {
			t.Errorf("error %v, want the context's end", e)
		}
	}
	if amf.Deregistrations() != 1 {
		t.Errorf("the mock got %d Deregistration Requests, want 1", amf.Deregistrations())
	}
}

// releaseCommandOf reads the UE NGAP IDs a UE CONTEXT RELEASE COMMAND
// names, and refuses every other PDU.
func TestReleaseCommandOf(t *testing.T) {
	command := func(ids *ngapType.UENGAPIDs) *ngapType.NGAPPDU {
		pdu := &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentInitiatingMessage, InitiatingMessage: new(ngapType.InitiatingMessage)}
		pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
		pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseCommand
		cmd := new(ngapType.UEContextReleaseCommand)
		pdu.InitiatingMessage.Value.UEContextReleaseCommand = cmd
		if ids != nil {
			ie := ngapType.UEContextReleaseCommandIEs{}
			ie.Id.Value = ngapType.ProtocolIEIDUENGAPIDs
			ie.Value.Present = ngapType.UEContextReleaseCommandIEsPresentUENGAPIDs
			ie.Value.UENGAPIDs = ids
			cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ie)
		}
		return pdu
	}
	outcome := &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentSuccessfulOutcome, SuccessfulOutcome: new(ngapType.SuccessfulOutcome)}
	outcome.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
	nasTransport := &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentInitiatingMessage, InitiatingMessage: new(ngapType.InitiatingMessage)}
	nasTransport.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeDownlinkNASTransport
	bare := command(nil)
	bare.InitiatingMessage.Value.UEContextReleaseCommand = nil

	tests := []struct {
		name             string
		pdu              *ngapType.NGAPPDU
		wantOK           bool
		wantAMF, wantRAN bool
		amf, ran         int64
	}{
		{name: "the ID pair", pdu: command(&ngapType.UENGAPIDs{Present: ngapType.UENGAPIDsPresentUENGAPIDPair,
			UENGAPIDPair: &ngapType.UENGAPIDPair{AMFUENGAPID: ngapType.AMFUENGAPID{Value: 7}, RANUENGAPID: ngapType.RANUENGAPID{Value: 9}}}),
			wantOK: true, wantAMF: true, wantRAN: true, amf: 7, ran: 9},
		{name: "the AMF ID alone", pdu: command(&ngapType.UENGAPIDs{Present: ngapType.UENGAPIDsPresentAMFUENGAPID,
			AMFUENGAPID: &ngapType.AMFUENGAPID{Value: 7}}), wantOK: true, wantAMF: true, amf: 7},
		{name: "no UE NGAP IDs IE", pdu: command(nil), wantOK: true},
		{name: "a command without its value", pdu: bare, wantOK: true},
		{name: "a successful outcome of the procedure", pdu: outcome},
		{name: "a Downlink NAS Transport", pdu: nasTransport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rel, ok := releaseCommandOf(tc.pdu)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if rel.HasAMFUENGAPID != tc.wantAMF || rel.HasRANUENGAPID != tc.wantRAN || rel.AMFUENGAPID != tc.amf || rel.RANUENGAPID != tc.ran || rel.At.IsZero() {
				t.Errorf("release %+v, want AMF %v %d, RAN %v %d", rel, tc.wantAMF, tc.amf, tc.wantRAN, tc.ran)
			}
		})
	}
}
