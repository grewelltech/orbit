package mockamf_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// nopUPF is a UPF that records nothing.
type nopUPF struct{}

func (nopUPF) SessionUp(mockamf.SessionInfo)   {}
func (nopUPF) PathSwitched(mockamf.PathSwitch) {}

// One UE's PDU session set up on its source association while a path
// switch for that session lands on another (each association its own
// goroutine in the mock): the two associations' handlers
// share the UE's session state, which must be guarded across them. Run
// under -race (-count=50), it fails if any of that state is read or written
// outside the lock.
func TestSessionStateAcrossAssociations(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: nopUPF{}, UPFAddress: "127.0.0.9"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srcCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	src := amf.Connect(ctx)
	defer func() { _ = src.Close() }()
	if ng, err := gnb.NGSetup(ctx, src, srcCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup: %v", err)
	}
	tgt := amf.Connect(ctx)
	defer func() { _ = tgt.Close() }()
	tgtCfg := srcCfg
	tgtCfg.ID, tgtCfg.Name = 2, "orbit-gnb-2"
	if ng, err := gnb.NGSetup(ctx, tgt, tgtCfg); err != nil || !ng.Accepted {
		t.Fatalf("target NG Setup: %v", err)
	}
	id, err := ue.ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	attached := make(chan struct{})
	go func() {
		// The source association: register and set up PDU session 1. The
		// path switch may move the UE before the setup completes, so the
		// attach's own outcome is not this test's concern.
		defer wg.Done()
		defer close(attached)
		_, _ = engine.Attach(ctx, src, srcCfg, engine.UEConfig{Identity: id,
			Sub:        auth.Subscription{SUPI: "208930100007500", Ki: ki, OPc: opc},
			PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"}},
			slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	}()
	go func() {
		// The target association: path switches for the first UE's
		// session 1 (AMF-UE-NGAP-ID 1, the mock's first) until one is
		// acknowledged, racing the setup above.
		defer wg.Done()
		ps, err := gnb.BuildPathSwitchRequest(tgtCfg, 1, 7, []gnb.AdmittedSession{
			{PDUSessionID: 1, GNBTunnel: gnb.GNBTunnel{Address: "127.0.0.2", TEID: 0x99}, QFIs: []int64{1}},
		})
		if err != nil {
			t.Error(err)
			return
		}
		for ctx.Err() == nil {
			if err := gnb.SendPDU(tgt, 1, ps); err != nil {
				t.Error(err)
				return
			}
			reply, err := gnb.ReadPDU(ctx, tgt)
			if err != nil {
				t.Errorf("no answer to the PathSwitchRequest: %v", err)
				return
			}
			if gnb.ClassifyPathSwitch(reply) == gnb.PathSwitchAcknowledged {
				return
			}
			select {
			case <-attached:
				return
			default:
			}
		}
	}()
	wg.Wait()
	if n := amf.EncodeErrors(); n != 0 {
		t.Errorf("mock dropped %d PDUs that did not encode", n)
	}
}

// A UE's RAN-UE-NGAP-ID raced across its associations: the source sends
// UE Context Release Requests for the UE (each answer reads the ID,
// AMF.ranIDOf) while path switches on the target write it. Run under
// -race (-count=50), it fails if the ID is read outside AMF.mu.
func TestRANIDAcrossAssociations(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: nopUPF{}, UPFAddress: "127.0.0.9"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srcCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	src := amf.Connect(ctx)
	defer func() { _ = src.Close() }()
	if ng, err := gnb.NGSetup(ctx, src, srcCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup: %v", err)
	}
	tgt := amf.Connect(ctx)
	defer func() { _ = tgt.Close() }()
	tgtCfg := srcCfg
	tgtCfg.ID, tgtCfg.Name = 2, "orbit-gnb-2"
	if ng, err := gnb.NGSetup(ctx, tgt, tgtCfg); err != nil || !ng.Accepted {
		t.Fatalf("target NG Setup: %v", err)
	}
	id, err := ue.ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := engine.Attach(ctx, src, srcCfg, engine.UEConfig{Identity: id,
		Sub:        auth.Subscription{SUPI: "208930100007500", Ki: ki, OPc: opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	amfID := sess.Result.AMFUENGAPID

	const rounds = 20
	ps, err := gnb.BuildPathSwitchRequest(tgtCfg, amfID, 7, []gnb.AdmittedSession{
		{PDUSessionID: 1, GNBTunnel: gnb.GNBTunnel{Address: "127.0.0.2", TEID: 0x99}, QFIs: []int64{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		// The source: release requests for the UE, under its first RAN ID.
		defer wg.Done()
		for i := 0; i < rounds && ctx.Err() == nil; i++ {
			if err := gnb.SendPDU(src, 1, gnb.BuildUEContextReleaseRequest(amfID, 1)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		// The source's answers, drained so the mock never blocks on them.
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, err := gnb.ReadPDU(ctx, src); err != nil {
				t.Errorf("answer %d to a release request: %v", i, err)
				return
			}
		}
	}()
	go func() {
		// The target: path switches for the UE's session, each rewriting
		// its RAN ID.
		defer wg.Done()
		for i := 0; i < rounds && ctx.Err() == nil; i++ {
			if err := gnb.SendPDU(tgt, 1, ps); err != nil {
				t.Error(err)
				return
			}
			reply, err := gnb.ReadPDU(ctx, tgt)
			if err != nil {
				t.Errorf("no answer to path switch %d: %v", i, err)
				return
			}
			if gnb.ClassifyPathSwitch(reply) != gnb.PathSwitchAcknowledged {
				t.Errorf("path switch %d was not acknowledged", i)
				return
			}
		}
	}()
	wg.Wait()
}
