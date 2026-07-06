package mockamf_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// simAttach returns a load.AttachFunc that attaches one UE against the
// in-process mock AMF, capturing total and registration latency. This is the
// sim-capacity path — the number that reflects ORBIT's own engine, not a core.
func simAttach(t *testing.T, amf *mockamf.AMF, gnbCfg gnb.Config, ki, opc []byte) load.AttachFunc {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return func(ctx context.Context, i int) load.Sample {
		supi := fmt.Sprintf("%015d", int64(208930100000000)+int64(i))
		id, err := ue.ParseIdentity(supi, "208", "93", "0")
		if err != nil {
			return load.Sample{Err: err}
		}
		start := time.Now()
		var regDur time.Duration
		emit := func(ev engine.StateEvent) {
			if ev.State == engine.StateRegistered && regDur == 0 {
				regDur = time.Since(start)
			}
		}
		conn := amf.Connect(ctx)
		defer conn.Close()
		if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
			return load.Sample{Err: fmt.Errorf("ng setup: %w", err)}
		}
		sess, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
			Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		}, log, emit)
		if err != nil {
			return load.Sample{Err: err}
		}
		if !sess.Result.Registered {
			return load.Sample{Err: fmt.Errorf("not registered")}
		}
		return load.Sample{Metrics: map[string]time.Duration{
			"attach":       time.Since(start),
			"registration": regDur,
		}}
	}
}

func newLoadAMF(t *testing.T) (*mockamf.AMF, gnb.Config, []byte, []byte) {
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
	gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	return amf, gnbCfg, ki, opc
}

// TestSimLoad drives a modest rate-controlled attach storm against the mock
// AMF through the load engine and checks the report is sane — proving the
// end-to-end load path (rate scheduler → bounded pool → real attaches →
// per-procedure KPIs) headless, no core.
func TestSimLoad(t *testing.T) {
	amf, gnbCfg, ki, opc := newLoadAMF(t)
	rep := load.Run(context.Background(),
		load.Config{Total: 300, Concurrency: 64},
		simAttach(t, amf, gnbCfg, ki, opc))

	if rep.Succeeded != 300 || rep.Failed != 0 {
		t.Fatalf("succeeded/failed = %d/%d, want 300/0", rep.Succeeded, rep.Failed)
	}
	reg, ok := rep.Latencies["registration"]
	if !ok || reg.Count != 300 {
		t.Fatalf("registration stats missing: %+v", reg)
	}
	t.Logf("sim capacity: %.0f attach/s; registration P50 %v P99 %v P99.9 %v",
		rep.AchievedRate, rep.Latencies["registration"].P50,
		rep.Latencies["registration"].P99, rep.Latencies["registration"].P999)
}

// TestSimLoadRamp exercises a linear ramp so the ramp scheduler is covered end
// to end against real attaches.
func TestSimLoadRamp(t *testing.T) {
	amf, gnbCfg, ki, opc := newLoadAMF(t)
	rep := load.Run(context.Background(),
		load.Config{Total: 200, Concurrency: 64, Rate: load.LinearRamp{Start: 100, End: 800, Over: time.Second}},
		simAttach(t, amf, gnbCfg, ki, opc))
	if rep.Succeeded != 200 {
		t.Fatalf("succeeded = %d, want 200", rep.Succeeded)
	}
	t.Logf("ramp: %.0f attach/s achieved; attach P99 %v", rep.AchievedRate, rep.Latencies["attach"].P99)
}
