package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
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
		gnbCfg := f.gnbConfigFor(i)
		gnbLabel := gnbAttributionLabel(gnbCfg)
		cfg, err := makeUE(i)
		if err != nil {
			return load.Sample{Err: err, GNB: gnbLabel}
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
		supi := cfg.Sub.SUPI
		res, err := Attach(ctx, uet, gnbCfg, cfg, f.log, emit)
		if err != nil {
			return load.Sample{Err: err, SUPI: supi, GNB: gnbLabel}
		}
		if !res.Result.Registered {
			return load.Sample{Err: fmt.Errorf("UE %s not registered", supi), SUPI: supi, GNB: gnbLabel}
		}
		m := attachProcedureDurations(time.Since(start), regDur, res.Result.SessionActive)
		return load.Sample{Metrics: m, SUPI: supi, GNB: gnbLabel}
	}
}

// gnbAttributionLabel names a gNB for per-gNB telemetry attribution. The gNB
// Name (RANNodeName) is an optional IE, so it falls back to the required, unique
// gNB ID when unset: a fleet of unnamed gNBs still reports a distinct bucket per
// gNB rather than collapsing every sample under the empty string.
func gnbAttributionLabel(cfg gnb.Config) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return fmt.Sprintf("gnb-%d", cfg.ID)
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
	// Observer, if set, receives each attempt as it completes, so a caller can
	// show progress or export metrics while the run is still going. The
	// returned Report stays authoritative.
	Observer load.Observer
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
		// A soak keeps offering attaches for its whole duration, so the
		// dispatch index climbs without bound. Left alone it walks straight
		// past the provisioned subscribers and every attach beyond the last
		// one fails on an unknown SUPI — a 10s soak against 100 subscribers
		// reports "100/205 attached", where the 105 failures say nothing about
		// the core. Cycle the population instead: Count is the pool, and a
		// soak re-attaches the same UEs, which is what a soak is for.
		if spec.Duration > 0 && spec.Count > 0 {
			i %= spec.Count
		}
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

	rep := load.Run(ctx, loadConfig(spec), f.LoadFunc(makeUE))
	return rep, nil
}

// loadConfig maps a LoadSpec onto the load engine's Config.
//
// Split out from RunLoad so the mapping is testable without a live core:
// RunLoad brings up real SCTP associations, and a field silently missing here
// is exactly how the Observer hook sat unwired.
func loadConfig(spec LoadSpec) load.Config {
	return load.Config{
		Total:          spec.Count,
		Concurrency:    spec.Concurrency,
		Rate:           spec.Rate,
		Duration:       spec.Duration,
		SampleInterval: spec.SampleEvery,
		Observer:       spec.Observer,
	}
}

// incIMSI returns the base IMSI incremented by i, zero-padded to 15 digits.
func incIMSI(base string, i int) (string, error) {
	n, err := strconv.ParseInt(base, 10, 64)
	if err != nil {
		return "", fmt.Errorf("base IMSI %q: %w", base, err)
	}
	return fmt.Sprintf("%015d", n+int64(i)), nil
}

// attachProcedureDurations splits one attach into the procedures reported as
// control-plane latency: the whole attach, the registration half, and the PDU
// session establishment that follows it.
//
// pdu_session is the REMAINDER after registration, not the whole attach over
// again. Recording total for both made the two report an identical
// distribution, which looks like agreement between two measurements while
// actually being one measurement written down twice — and it hides how the
// attach budget divides between the control-plane and session halves.
//
// regDur == 0 means REGISTERED was never observed, so there is no split to
// report: only the total is meaningful, and inventing a pdu_session value from
// it would be the same conflation in another form.
func attachProcedureDurations(total, regDur time.Duration, sessionActive bool) map[string]time.Duration {
	m := map[string]time.Duration{ProcedureAttach: total}
	if regDur <= 0 {
		return m
	}
	m[ProcedureRegistration] = regDur
	if sessionActive && total > regDur {
		m[ProcedurePDUSession] = total - regDur
	}
	return m
}
