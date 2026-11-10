package mockamf_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The mock answers a RAN CONFIGURATION UPDATE with an ACKNOWLEDGE by
// default, a FAILURE with a cause or nothing when configured, and records
// the IEs the update carried either way.
func TestMockRANConfigUpdate(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 1, Name: "g", MCC: "208", MNC: "93", TAC: 7, Slices: []gnb.SNSSAI{{SST: 1}}}
	tests := []struct {
		name      string
		opts      []mockamf.Option
		update    []gnb.RANConfigUpdateOption
		wantAck   bool
		wantCause string
		wantNone  bool
		wantIEs   []int64
		wantName  string
	}{
		{name: "acknowledged by default", update: []gnb.RANConfigUpdateOption{gnb.WithRANNodeName("renamed")},
			wantAck: true, wantIEs: []int64{ngapType.ProtocolIEIDRANNodeName}, wantName: "renamed"},
		{name: "acknowledged explicitly, with a TA list", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.AcknowledgeRANConfigUpdate)},
			update:  []gnb.RANConfigUpdateOption{gnb.WithSupportedTAList(cfg), gnb.WithRANNodeName("renamed")},
			wantAck: true, wantIEs: []int64{ngapType.ProtocolIEIDSupportedTAList, ngapType.ProtocolIEIDRANNodeName}, wantName: "renamed"},
		{name: "failure", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.FailRANConfigUpdate)},
			wantCause: "misc(5)"},
		{name: "ignored", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.IgnoreRANConfigUpdate)},
			update: []gnb.RANConfigUpdateOption{gnb.WithRANNodeName("renamed")}, wantNone: true,
			wantIEs: []int64{ngapType.ProtocolIEIDRANNodeName}, wantName: "renamed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NGSetup: %+v, %v", ng, err)
			}
			pdu, err := gnb.BuildRANConfigurationUpdate(tc.update...)
			if err != nil {
				t.Fatal(err)
			}
			rctx, rcancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer rcancel()
			res, err := gnb.RANConfigurationUpdate(rctx, conn, pdu)
			switch {
			case tc.wantNone:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("result %+v, err %v; want no answer within the bound", res, err)
				}
			case err != nil:
				t.Fatal(err)
			case res.Acknowledged != tc.wantAck || res.Cause != tc.wantCause || res.Skipped != 0:
				t.Fatalf("result = %+v, want acknowledged %v, cause %q", res, tc.wantAck, tc.wantCause)
			}
			got := amf.RANConfigUpdates()
			if len(got) != 1 {
				t.Fatalf("recorded updates = %+v, want one", got)
			}
			if len(got[0].IEs) != len(tc.wantIEs) || got[0].RANNodeName != tc.wantName {
				t.Fatalf("recorded update = %+v, want IEs %v and name %q", got[0], tc.wantIEs, tc.wantName)
			}
			for i, id := range tc.wantIEs {
				if got[0].IEs[i] != id || !got[0].HasIE(id) {
					t.Errorf("recorded IEs = %v, want %v", got[0].IEs, tc.wantIEs)
				}
			}
			if got[0].HasIE(ngapType.ProtocolIEIDGlobalRANNodeID) {
				t.Error("HasIE reports an IE the update did not carry")
			}
			if n := amf.EncodeErrors(); n != 0 {
				t.Errorf("mock dropped %d answers that failed to encode", n)
			}
		})
	}
}

// With WithRANConfigUpdateDroppingTAs, an acknowledged update without a
// Supported TA List clears the association's tracking areas: a registration
// on it is rejected, in the clear, with the configured 5GMM cause. An update
// that carries the list, an update that is not acknowledged, another
// association, and a mock without the option all keep serving the UE.
func TestMockRANConfigUpdateDroppingTAs(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 1, Name: "g", MCC: "208", MNC: "93", TAC: 7, Slices: []gnb.SNSSAI{{SST: 1}}}
	name := []gnb.RANConfigUpdateOption{gnb.WithRANNodeName("g")}
	withList := []gnb.RANConfigUpdateOption{gnb.WithRANNodeName("g"), gnb.WithSupportedTAList(cfg)}
	tests := []struct {
		name       string
		opts       []mockamf.Option
		update     []gnb.RANConfigUpdateOption
		noUpdate   bool // register on an association that sent no update
		wantReject uint8
	}{
		{name: "list absent, TAs dropped", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(12)}, update: name, wantReject: 12},
		{name: "another cause", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(11)}, update: name, wantReject: 11},
		{name: "list present, TAs kept", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(12)}, update: withList},
		{name: "failure, TAs kept", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(12), mockamf.WithRANConfigUpdate(mockamf.FailRANConfigUpdate)}, update: name},
		{name: "another association keeps its TAs", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(12)}, update: name, noUpdate: true},
		{name: "default keeps the TAs", update: name},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NGSetup: %+v, %v", ng, err)
			}
			pdu, err := gnb.BuildRANConfigurationUpdate(tc.update...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := gnb.RANConfigurationUpdate(ctx, conn, pdu); err != nil {
				t.Fatal(err)
			}
			regConn := conn
			if tc.noUpdate {
				regConn = amf.Connect(ctx)
				defer func() { _ = regConn.Close() }()
				other := cfg
				other.ID = 2
				if ng, err := gnb.NGSetup(ctx, regConn, other); err != nil || !ng.Accepted {
					t.Fatalf("second NGSetup: %+v, %v", ng, err)
				}
			}

			supi := "20893010000770" + string(rune('0'+i))
			id, err := ue.ParseIdentity(supi, "208", "93", "0")
			if err != nil {
				t.Fatal(err)
			}
			var cause uint8
			var plain bool
			sess, err := engine.Attach(ctx, regConn, cfg, engine.UEConfig{
				Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
				Probe: &engine.AttachProbe{OnNAS: func(ev engine.NASEvent) {
					if ev.MessageType() == f5nas.MsgTypeRegistrationReject && ev.Msg != nil {
						cause = ev.Msg.RegistrationReject.GetCauseValue()
						plain = f5nas.GetSecurityHeaderType(ev.Raw) == f5nas.SecurityHeaderTypePlainNas
					}
				}},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if tc.wantReject == 0 {
				if err != nil || !sess.Result.Registered {
					t.Fatalf("attach: %v; want REGISTERED", err)
				}
				return
			}
			if !errors.Is(err, engine.ErrRegistrationRejected) || cause != tc.wantReject || !plain {
				t.Fatalf("attach: err %v, reject cause %d, plain %v; want a plain Registration Reject #%d", err, cause, plain, tc.wantReject)
			}
		})
	}
}

// With CrashOnRANConfigUpdate the update's association ends unanswered and
// the mock serves no later association.
func TestMockRANConfigUpdateCrash(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 1, Name: "g", MCC: "208", MNC: "93", TAC: 7, Slices: []gnb.SNSSAI{{SST: 1}}}
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1}, mockamf.WithRANConfigUpdate(mockamf.CrashOnRANConfigUpdate))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := amf.Connect(ctx)
	defer func() { _ = conn.Close() }()
	if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
		t.Fatalf("NGSetup: %+v, %v", ng, err)
	}
	pdu, err := gnb.BuildRANConfigurationUpdate(gnb.WithRANNodeName("g"))
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithTimeout(ctx, time.Second)
	defer rcancel()
	if res, err := gnb.RANConfigurationUpdate(rctx, conn, pdu); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("result %+v, err %v; want the association closed", res, err)
	}
	second := amf.Connect(ctx)
	defer func() { _ = second.Close() }()
	sctx, scancel := context.WithTimeout(ctx, time.Second)
	defer scancel()
	if ng, err := gnb.NGSetup(sctx, second, cfg); err == nil {
		t.Errorf("a crashed mock answered a later NG Setup: %+v", ng)
	}
}
