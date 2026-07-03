package mockamf_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

func testAMF(t *testing.T) *mockamf.AMF {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	a, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc,
		SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAttachAgainstMockAMF runs the real engine.Attach against the in-process
// mock AMF over the pipe — no core, no sockets — and confirms the UE reaches
// REGISTERED. This is the sim-capacity harness the D-6 spike measures.
func TestAttachAgainstMockAMF(t *testing.T) {
	amf := testAMF(t)
	gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := amf.Connect(ctx)
	defer conn.Close()

	if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup against mock AMF: %v (accepted=%v)", err, ng)
	}

	supi := "208930100007500"
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
		Identity: id,
		Sub:      auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
	}, log, nil)
	if err != nil {
		t.Fatalf("attach against mock AMF: %v", err)
	}
	if !sess.Result.Registered {
		t.Fatal("UE did not reach REGISTERED against the mock AMF")
	}
	t.Logf("UE %s REGISTERED against mock AMF (AMF-UE-NGAP-ID %d)", supi, sess.Result.AMFUENGAPID)
}
