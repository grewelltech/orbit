package engine

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// quietUPF is a UPF for the mock AMF that records nothing.
type quietUPF struct{}

func (quietUPF) SessionUp(mockamf.SessionInfo)   {}
func (quietUPF) PathSwitched(mockamf.PathSwitch) {}

// A Configuration Update Command is accepted wherever it reaches the UE
// (TS 24.501 5.4.4.3): during the attach's wait for its PDU session, or
// from a registered UE's association while a procedure waits for another
// PDU. The Complete is sent only when acknowledgement is requested, and a
// new 5G-GUTI replaces the one the Deregistration Request then names. A
// requested registration update is not performed: the Complete still goes
// out when asked for, and the session and the attach result record the
// request.
func TestConfigurationUpdateCommand(t *testing.T) {
	const supi = "208930100007500"
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	updates := []struct {
		name string
		cu   mockamf.ConfigurationUpdate
	}{
		{name: "no indication", cu: mockamf.ConfigurationUpdate{}},
		{name: "acknowledgement requested", cu: mockamf.ConfigurationUpdate{AckRequested: true}},
		{name: "new 5G-GUTI, acknowledgement requested", cu: mockamf.ConfigurationUpdate{AckRequested: true, NewGUTI: true}},
		{name: "new 5G-GUTI, no acknowledgement", cu: mockamf.ConfigurationUpdate{NewGUTI: true}},
		{name: "registration requested", cu: mockamf.ConfigurationUpdate{RegistrationRequested: true}},
		{name: "registration and acknowledgement requested", cu: mockamf.ConfigurationUpdate{AckRequested: true, RegistrationRequested: true}},
	}
	for _, during := range []bool{true, false} {
		for _, tc := range updates {
			name := tc.name + ", after the attach"
			if during {
				name = tc.name + ", during the attach"
			}
			t.Run(name, func(t *testing.T) {
				amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
					MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: quietUPF{}, UPFAddress: "127.0.0.9"},
					mockamf.WithGUTI(), mockamf.WithConfigurationUpdate(tc.cu))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
				conn := amf.Connect(ctx)
				defer func() { _ = conn.Close() }() // test teardown
				if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
					t.Fatalf("NG Setup: %v", err)
				}
				ueCfg := UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}}
				if during {
					// The command leaves the mock before it answers the PDU
					// session request, so the attach reads it first.
					ueCfg.PDUSession = &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"}
				}
				sess, err := Attach(ctx, conn, cfg, ueCfg, log, nil)
				if err != nil {
					t.Fatalf("attach: %v", err)
				}
				if !during {
					// The command waits on the association; a procedure's
					// wait reads it and passes it on.
					pdu, err := gnb.ReadPDU(ctx, conn)
					if err != nil {
						t.Fatal(err)
					}
					if !sess.absorbDownlinkNAS(ctx, log, pdu) {
						t.Fatal("the Downlink NAS Transport was not absorbed")
					}
				}
				wantGUTI := sess.guti
				if tc.cu.NewGUTI {
					wantGUTI = amf.ConfigurationUpdateGUTI(sess.Result.AMFUENGAPID)
				}
				if !bytes.Equal(sess.guti, wantGUTI) || len(wantGUTI) != 11 {
					t.Errorf("5G-GUTI = %x, want %x", sess.guti, wantGUTI)
				}

				if got := sess.RegistrationUpdateRequested(); got != tc.cu.RegistrationRequested {
					t.Errorf("Session.RegistrationUpdateRequested = %t, want %t", got, tc.cu.RegistrationRequested)
				}
				if during && sess.Result.RegistrationUpdateRequested != tc.cu.RegistrationRequested {
					t.Errorf("AttachResult.RegistrationUpdateRequested = %t, want %t", sess.Result.RegistrationUpdateRequested, tc.cu.RegistrationRequested)
				}
				if !sess.Result.Registered {
					t.Error("the UE is no longer reported registered")
				}

				// The Deregistration Request follows any Complete on the
				// association, so the count is final once the mock has it.
				if err := sess.deregister(ctx); err != nil {
					t.Fatal(err)
				}
				for amf.Deregistrations() == 0 && ctx.Err() == nil {
					time.Sleep(5 * time.Millisecond)
				}
				wantCompletes := int64(0)
				if tc.cu.AckRequested {
					wantCompletes = 1
				}
				if n := amf.ConfigurationUpdateCompletes(); n != wantCompletes {
					t.Errorf("mock received %d Configuration Update Completes, want %d", n, wantCompletes)
				}
				if ids := amf.DeregistrationIdentities(); len(ids) != 1 || !bytes.Equal(ids[0], wantGUTI) {
					t.Errorf("Deregistration Request names %x, want %x", ids, wantGUTI)
				}
				if n := amf.EncodeErrors(); n != 0 {
					t.Errorf("mock dropped %d PDUs that did not encode", n)
				}
			})
		}
	}
}

// absorbDownlinkNAS leaves any PDU that is not a Downlink NAS Transport to
// the waiting procedure.
func TestAbsorbDownlinkNASLeavesOtherPDUs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Session{SUPI: "208930100007500"}
	release := gnb.BuildUEContextReleaseComplete(1, 1)
	if s.absorbDownlinkNAS(context.Background(), log, &release) {
		t.Error("absorbed a PDU that is not a Downlink NAS Transport")
	}
	if s.absorbDownlinkNAS(context.Background(), log, nil) {
		t.Error("absorbed a nil PDU")
	}
}
