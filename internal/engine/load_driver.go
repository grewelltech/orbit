package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// LoadFunc returns a load.AttachFunc that attaches one UE per call, muxed over
// the fleet's gNB associations (round-robin), using makeUE(index) for the
// per-UE config and capturing total, registration, and (if requested) PDU
// session latency. This bridges the actor-model fleet to the load engine.
func (f *Fleet) LoadFunc(makeUE func(i int) (UEConfig, error)) load.AttachFunc {
	return func(ctx context.Context, i int) load.Sample {
		cfg, err := makeUE(i)
		if err != nil {
			return load.Sample{Err: err}
		}
		sess := f.sessions[i%len(f.sessions)]
		uet, ranID := sess.NewUE()
		defer uet.Close()
		cfg.RANUENGAPID = ranID

		start := time.Now()
		var regDur time.Duration
		emit := func(ev StateEvent) {
			if ev.State == StateRegistered && regDur == 0 {
				regDur = time.Since(start)
			}
		}
		res, err := Attach(ctx, uet, f.gnbConfigFor(i), cfg, f.log, emit)
		if err != nil {
			return load.Sample{Err: err}
		}
		if !res.Result.Registered {
			return load.Sample{Err: fmt.Errorf("UE %s not registered", cfg.Sub.SUPI)}
		}
		m := map[string]time.Duration{"attach": time.Since(start)}
		if regDur > 0 {
			m["registration"] = regDur
		}
		if res.Result.SessionActive {
			m["pdu_session"] = time.Since(start)
		}
		return load.Sample{Metrics: m}
	}
}

// LoadSpec parameterises a rate-controlled attach storm against a core.
type LoadSpec struct {
	GNBs        []GNBSpec // one association per gNB; UEs are muxed across them
	BaseIMSI    string    // first SUPI; each UE increments it
	Count       int
	MCC, MNC    string
	Ki, OPc     []byte
	Concurrency int
	Rate        load.Rate            // offered-rate curve (nil = concurrency-bound)
	Duration    time.Duration        // soak: run for this long instead of Count
	SampleEvery time.Duration        // soak: resource-sample cadence
	PDUSession  *ue.PDUSessionParams // optional session per UE
	GNBN3Addr   string               // gNB N3 for the data path (with PDUSession)
}

// RunLoad brings up the fleet, generates Count UEs from BaseIMSI, and drives a
// rate-controlled attach storm, returning the per-procedure KPI report. It is
// the integration-capacity path — the numbers are bounded by the core under
// test (report them separately from mock-AMF sim capacity).
func RunLoad(ctx context.Context, log *slog.Logger, spec LoadSpec) (load.Report, error) {
	f, err := NewFleet(ctx, spec.GNBs, log)
	if err != nil {
		return load.Report{}, err
	}
	defer f.Close()

	makeUE := func(i int) (UEConfig, error) {
		supi, err := incIMSI(spec.BaseIMSI, i)
		if err != nil {
			return UEConfig{}, err
		}
		id, err := ue.ParseIdentity(supi, spec.MCC, spec.MNC, "0")
		if err != nil {
			return UEConfig{}, err
		}
		cfg := UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: spec.Ki, OPc: spec.OPc}}
		if spec.PDUSession != nil {
			p := *spec.PDUSession
			cfg.PDUSession = &p
			cfg.GNBN3Addr = spec.GNBN3Addr
		}
		return cfg, nil
	}

	rep := load.Run(ctx, load.Config{
		Total: spec.Count, Concurrency: spec.Concurrency, Rate: spec.Rate,
		Duration: spec.Duration, SampleInterval: spec.SampleEvery,
	}, f.LoadFunc(makeUE))
	return rep, nil
}

// incIMSI returns the base IMSI incremented by i, zero-padded to 15 digits.
func incIMSI(base string, i int) (string, error) {
	n, err := strconv.ParseInt(base, 10, 64)
	if err != nil {
		return "", fmt.Errorf("base IMSI %q: %w", base, err)
	}
	return fmt.Sprintf("%015d", n+int64(i)), nil
}
