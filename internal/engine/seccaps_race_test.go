package engine

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// A caller reads Session.UESecurityCapabilities while Xn handovers replace
// the held copy: each Path Switch Request carries capabilities other than
// the AMF's, so each Acknowledge returns the AMF's and the handover stores
// them. Run under -race, it fails if the read is not guarded.
func TestUESecurityCapabilitiesReadDuringHandover(t *testing.T) {
	const supi = "208930100007500"
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: quietUPF{}, UPFAddress: "127.0.0.9"},
		mockamf.WithStoredCapabilitiesInPathSwitchAck())
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpFront(t, amf)
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := gnb.Config{ID: 1, Name: "orbit-gnb-1", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: addr}, UEConfig{
		Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = m.Deregister(dctx, supi) // test teardown
	})
	sess, err := m.Status(supi)
	if err != nil {
		t.Fatal(err)
	}
	fromAMF := gnb.NewUESecurityCapabilities(0, 0x4000, 0, 0)
	if got := sess.UESecurityCapabilities(); !gnb.SameUESecurityCapabilities(got, fromAMF) {
		t.Fatalf("held after the attach: %+v, want %+v", got, fromAMF)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = sess.UESecurityCapabilities()
			}
		}
	}()
	const rounds = 4
	for i := 0; i < rounds; i++ {
		// What a gNB with a wrong copy would hold; the AMF corrects it.
		sess.setUESecCaps(gnb.NewUESecurityCapabilities(0xE000, 0xE000, 0, 0))
		tgt := cfg
		tgt.ID, tgt.Name = uint32(2+i), "orbit-gnb-next"
		if _, err := m.XnHandover(ctx, supi, GNBEndpoint{Config: tgt, AMFAddr: addr, N3Addr: "127.0.0.2"}); err != nil {
			t.Fatalf("Xn handover %d: %v", i, err)
		}
		if got := sess.UESecurityCapabilities(); !gnb.SameUESecurityCapabilities(got, fromAMF) {
			t.Errorf("held after handover %d: %+v, want the AMF's %+v", i, got, fromAMF)
		}
	}
	close(stop)
	wg.Wait()
	if n := amf.PathSwitchCapabilityMismatches(); n != rounds {
		t.Errorf("mock counted %d capability mismatches, want %d", n, rounds)
	}
}
