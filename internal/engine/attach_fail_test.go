package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
)

// An attach that fails after registration (here a PDU session that never
// comes: the mock has no UPF) deregisters the UE before returning, over
// real SCTP, so the core is not left holding a registered UE nobody has a
// handle to. One that succeeds, or is probe-driven (a conformance check
// owns its cleanup), sends none.
func TestAttachFailureAfterRegistrationDeregisters(t *testing.T) {
	_, cfg, ki, opc := newBindTestAMF(t)
	// Deregistration identifies the UE by its 5G-GUTI, which the mock assigns
	// only on request.
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"}, mockamf.WithGUTI())
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpFront(t, amf)
	m := NewManager(slog.New(slog.DiscardHandler))

	ueCfg := bindTestUE(t, "208930000000001", ki, opc)
	ueCfg.PDUSession = &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: addr}, ueCfg)
	if err == nil {
		t.Fatal("the attach succeeded without a UPF")
	}
	deadline := time.Now().Add(3 * time.Second)
	for amf.Deregistrations() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := amf.Deregistrations(); n != 1 {
		t.Fatalf("the AMF saw %d Deregistration Requests after the failed PDU step, want 1", n)
	}

	// A probe-driven attach (a conformance check) owns its cleanup: the same
	// failure after registration sends nothing.
	pcfg := cfg
	pcfg.ID = 3
	pue := bindTestUE(t, "208930000000003", ki, opc)
	pue.PDUSession = &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"}
	pue.Probe = &AttachProbe{}
	pctx, pcancel := context.WithTimeout(context.Background(), time.Second)
	defer pcancel()
	if _, err := m.Register(pctx, GNBSpec{Config: pcfg, AMFAddr: addr}, pue); err == nil {
		t.Fatal("the probe-driven attach succeeded without a UPF")
	}
	time.Sleep(200 * time.Millisecond)
	if n := amf.Deregistrations(); n != 1 {
		t.Fatalf("a probe-driven attach was deregistered automatically (%d in all, want the first test's 1)", n)
	}

	// Registration only, succeeding: the session is kept, nothing is sent.
	cfg.ID = 2
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := m.Register(ctx2, GNBSpec{Config: cfg, AMFAddr: addr}, bindTestUE(t, "208930000000002", ki, opc)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := amf.Deregistrations(); n != 1 {
		t.Errorf("a successful attach sent a Deregistration (%d in all)", n)
	}
}
