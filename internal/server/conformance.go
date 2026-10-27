package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

var (
	errAMFRequired  = errors.New("amf_address is required (host:port of the AMF N2 endpoint)")
	errPLMNRequired = errors.New("mcc and mnc are required (the core's PLMN; the server has no default)")
)

// conformanceService runs the conformance suite server-side and streams results
// to a client — the same suite the CLI runs, driven over the API so the
// dashboard can present it live.
type conformanceService struct {
	log *slog.Logger
	// expected is the server's expected-failures list (orbit serve
	// --conformance-expected-failures), loaded at startup; nil without one.
	// A request never names a path.
	expected *conformance.ExpectedFailures
	// runs starts the suite as a run (ADR-0008 §2): RunConformance is a
	// thin wrapper over a one-workload plan.
	runs *runService
}

// RunConformance streams one update per completed test, then a final summary.
func (s *conformanceService) RunConformance(
	ctx context.Context,
	req *connect.Request[orbitv1.RunConformanceRequest],
	stream *connect.ServerStream[orbitv1.ConformanceUpdate],
) error {
	m := req.Msg
	if m.GetAmfAddress() == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errAMFRequired)
	}
	if err := conformance.ValidateBind(m.GetBindAddress()); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	// The PLMN is the caller's to supply: a wrong-PLMN NG Setup Failure reads
	// like a core finding, so the server never guesses one.
	if m.GetMcc() == "" || m.GetMnc() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errPLMNRequired)
	}
	if err := gnb.ValidateSNSSAI(m.GetSst(), m.GetSd()); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	// The suite's own identity rules, checked here as before so a client
	// sees the same refusals; the plan's build checks the rest.
	g := gnb.Config{ID: 1, MCC: m.GetMcc(), MNC: m.GetMnc(), TAC: orDefaultU32(m.GetTac(), 1),
		Slices: []gnb.SNSSAI{{SST: uint8(orDefaultU32(m.GetSst(), 1)), SD: m.GetSd()}}}
	if err := g.ValidateIdentity(); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if _, err := conformanceUE(m); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	run := &orbitv1.StartRunRequest{Name: "conformance", Spec: &orbitv1.StartRunRequest_Plan{Plan: conformancePlan(m)}}
	info, pr, err := s.runs.startPlan(run, engine.RunOptions{Name: run.GetName(), Verbosity: engine.EventsNormal})
	if err != nil {
		return err
	}
	return s.stream(ctx, stream, info.ID, pr.suiteLive)
}

// stream sends a conformance run's results as they come, one update per
// check, then the summary. A client that goes away stops the run, as it
// always stopped the suite: the run ends CANCELLED, with the results so
// far archived.
func (s *conformanceService) stream(ctx context.Context, stream *connect.ServerStream[orbitv1.ConformanceUpdate], id string, live *suiteLive) error {
	spec := live.spec
	specTargets := map[string]uint32{}
	for name, rel := range spec.env.NarrowingSpecTargets() {
		specTargets[name] = uint32(rel)
	}
	core := spec.expected.Label()
	var tally orbitv1.ConformanceTally
	sent := 0
	for {
		rs, done, wake := live.since(sent)
		for _, r := range rs {
			addToTally(&tally, r)
			if err := stream.Send(&orbitv1.ConformanceUpdate{
				Result: resultProto(r), Tally: cloneTally(&tally),
				Index: uint32(sent), Total: uint32(spec.total),
				TargetRelease: uint32(spec.env.TargetRelease), SpecTargetReleases: specTargets,
				ExpectedFailuresCore: core, RunId: id,
			}); err != nil {
				_, _ = s.runs.reg.Stop(id) // the client is gone; so is its run
				return err
			}
			sent++
		}
		if done {
			break
		}
		select {
		case <-wake:
		case <-ctx.Done():
			_, _ = s.runs.reg.Stop(id)
			return nil
		}
	}
	if ctx.Err() != nil {
		// Cancelled mid-run: the tallies so far are honest, but do not
		// claim the run finished.
		_, _ = s.runs.reg.Stop(id)
		return nil
	}
	results, _ := live.snapshot()
	return stream.Send(&orbitv1.ConformanceUpdate{
		Done: true, Tally: cloneTally(&tally),
		TargetRelease: uint32(spec.env.TargetRelease), SpecTargetReleases: specTargets,
		ExpectedFailuresCore: core, RunId: id,
		Stale: staleProto(spec.expected.Stale(results)),
	})
}

// conformancePlan is a RunConformance request as a one-workload plan, every
// field where the plan keeps it: the core's identity and N3 endpoint in
// the target, the N2 source and the UE's gNB N3 in its RAN pools, the UE
// as a one-subscriber pool, and the suite's own knobs in the workload.
// Defaults stay where the request leaves them unset: gNB base 700, TAC 1,
// SST 1, the server's target release.
func conformancePlan(m *orbitv1.RunConformanceRequest) *orbitv1.RunPlan {
	core := &orbitv1.TargetCore{
		Amf: m.GetAmfAddress(), Plmn: &orbitv1.Plmn{Mcc: m.GetMcc(), Mnc: m.GetMnc()}, Tac: orDefaultU32(m.GetTac(), 1),
		Slices: []*orbitv1.Snssai{{Sst: orDefaultU32(m.GetSst(), 1), Sd: m.GetSd()}}, Dnn: m.GetDnn(), UpfN3: m.GetUpfN3(),
	}
	ran := &orbitv1.TargetRAN{GnbIds: &orbitv1.GNBIDRange{Bits: 24}}
	if b := m.GetBindAddress(); b != "" {
		ran.N2SourceIps = []string{b}
	}
	if n3 := m.GetGnbN3(); n3 != "" {
		ran.N3Ips = []string{n3}
	}
	t := &orbitv1.ResolvedTarget{Core: core, Ran: ran}
	if supi := m.GetSupi(); supi != "" {
		t.Subscribers = []*orbitv1.SubscriberPool{{Name: "ue", FirstSupi: supi, Count: 1,
			Ki: m.GetCredentials().GetKi(), Opc: m.GetCredentials().GetOpc()}}
	}
	cw := &orbitv1.ConformanceWorkload{
		Categories: m.GetCategories(), PerTestTimeoutMs: m.GetPerTestSeconds() * 1000,
		Traffic: m.GetTraffic(), N3Bind: m.GetN3Bind(),
		GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: orDefaultU32(m.GetGnbIdBase(), defaultSuiteGNBBase)}},
	}
	if r := m.GetTargetRelease(); r != 0 {
		cw.TargetRelease = strconv.FormatUint(uint64(r), 10)
	}
	return &orbitv1.RunPlan{
		Target:    t,
		Workloads: []*orbitv1.Workload{{Name: "conformance", Kind: &orbitv1.Workload_Conformance{Conformance: cw}}},
		Until:     &orbitv1.Until{Stop: &orbitv1.Until_Complete{Complete: true}},
	}
}

// ListConformanceTests reports the registered tests without running them.
func (s *conformanceService) ListConformanceTests(
	ctx context.Context,
	req *connect.Request[orbitv1.ListConformanceTestsRequest],
) (*connect.Response[orbitv1.ListConformanceTestsResponse], error) {
	var out []*orbitv1.ConformanceTestInfo
	for _, t := range conformance.NewRegistry().Tests() {
		refs := t.SpecRefs()
		out = append(out, &orbitv1.ConformanceTestInfo{
			Id:         t.ID(),
			Category:   string(t.Category()),
			SpecRef:    conformance.RenderRefs(refs),
			SpecRefs:   specRefsProto(refs),
			MinRelease: uint32(conformance.MinRelease(t)),
		})
	}
	return connect.NewResponse(&orbitv1.ListConformanceTestsResponse{Tests: out}), nil
}

func resultProto(r conformance.Result) *orbitv1.ConformanceResult {
	return &orbitv1.ConformanceResult{
		Id:              r.ID,
		Category:        string(r.Category),
		SpecRef:         r.SpecRef,
		Verdict:         string(r.Verdict),
		Expected:        r.Expected,
		Observed:        r.Observed,
		Detail:          r.Detail,
		SpecRefs:        specRefsProto(r.SpecRefs),
		MinRelease:      uint32(r.MinRelease),
		ExpectedFailure: expectedFailureProto(r.ExpectedFailure),
	}
}

// expectedFailureProto carries a tolerated FAIL's entry, nil for none.
func expectedFailureProto(e *conformance.ExpectedFailure) *orbitv1.ExpectedFailure {
	if e == nil {
		return nil
	}
	return &orbitv1.ExpectedFailure{Finding: e.Finding, Added: e.Added, Reason: e.Reason}
}

// staleProto carries the stale expected-failures entries on the wire.
func staleProto(stale []conformance.StaleExpectation) []*orbitv1.StaleExpectation {
	out := make([]*orbitv1.StaleExpectation, 0, len(stale))
	for _, st := range stale {
		out = append(out, &orbitv1.StaleExpectation{Check: st.Check, Finding: st.Finding,
			ExpectedObserved: st.ExpectedObserved, Verdict: string(st.Verdict), Observed: st.Observed})
	}
	return out
}

// specRefsProto carries a check's structured references on the wire.
func specRefsProto(refs []conformance.SpecRef) []*orbitv1.SpecRef {
	out := make([]*orbitv1.SpecRef, 0, len(refs))
	for _, r := range refs {
		p := &orbitv1.SpecRef{
			Spec: r.Spec, Clause: r.Clause, Strength: string(r.Strength),
			Since: uint32(r.Since), WordingUnchanged: r.WordingUnchanged, Rendered: r.String(),
		}
		for _, v := range r.Verified {
			p.Verified = append(p.Verified, &orbitv1.SpecVersion{Release: uint32(v.Release), Version: v.Version, Archive: v.Archive})
		}
		for _, c := range r.Changed {
			p.Changed = append(p.Changed, &orbitv1.SpecChange{Release: uint32(c.Release), Note: c.Note})
		}
		out = append(out, p)
	}
	return out
}

func addToTally(t *orbitv1.ConformanceTally, r conformance.Result) {
	switch r.Verdict {
	case conformance.Pass:
		t.Passed++
	case conformance.Fail:
		t.Failed++
		if r.ExpectedFailure != nil {
			t.ExpectedFailures++
		}
	case conformance.Error:
		t.Errored++
	case conformance.Skip:
		t.Skipped++
	case conformance.Deviation:
		t.Deviations++
	}
}

// cloneTally copies the running totals so each streamed update carries its own
// snapshot rather than a pointer that keeps changing under the client.
func cloneTally(t *orbitv1.ConformanceTally) *orbitv1.ConformanceTally {
	return &orbitv1.ConformanceTally{
		Passed: t.Passed, Failed: t.Failed, Errored: t.Errored,
		Skipped: t.Skipped, Deviations: t.Deviations, ExpectedFailures: t.ExpectedFailures,
	}
}

func orDefaultU32(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}

// errUENeedsSUPI refuses UE fields without a SUPI: they would be silently
// ignored, and the mobility checks would SKIP.
var errUENeedsSUPI = errors.New("credentials, gnb_n3 and traffic describe the mobility checks' UE and need supi")

// conformanceUE builds the suite's UE section from a request. Without a
// SUPI there is none (the mobility checks SKIP). With one, the keys must
// parse, the SUPI must be in the request's PLMN, gnb_n3 must be an IPv4
// address (canonicalised; see gnb.CanonicalN3) and
// traffic, when given, a host:port. Errors never echo the keys.
func conformanceUE(m *orbitv1.RunConformanceRequest) (conformance.UESpec, error) {
	if m.GetSupi() == "" {
		if m.GetGnbN3() != "" || m.GetTraffic() != "" || m.GetCredentials().GetKi() != "" || m.GetCredentials().GetOpc() != "" {
			return conformance.UESpec{}, errUENeedsSUPI
		}
		return conformance.UESpec{}, nil
	}
	if _, err := ue.ParseIdentity(m.GetSupi(), m.GetMcc(), m.GetMnc(), "0"); err != nil {
		return conformance.UESpec{}, err
	}
	if _, err := auth.ParseHexKey("Ki", m.GetCredentials().GetKi()); err != nil {
		return conformance.UESpec{}, err
	}
	if _, err := auth.ParseHexKey("OPc", m.GetCredentials().GetOpc()); err != nil {
		return conformance.UESpec{}, err
	}
	if m.GetGnbN3() == "" {
		return conformance.UESpec{}, errors.New("gnb_n3 (the gNB N3 IP) is required with supi")
	}
	n3, err := gnb.CanonicalN3(m.GetGnbN3())
	if err != nil {
		return conformance.UESpec{}, fmt.Errorf("gnb_n3: %w", err)
	}
	if t := m.GetTraffic(); t != "" {
		if _, _, err := net.SplitHostPort(t); err != nil {
			return conformance.UESpec{}, fmt.Errorf("traffic: want host:port: %w", err)
		}
	}
	dnn := m.GetDnn()
	if dnn == "" {
		dnn = "internet"
	}
	return conformance.UESpec{
		SUPI: m.GetSupi(), Ki: m.GetCredentials().GetKi(), OPc: m.GetCredentials().GetOpc(),
		DNN: dnn, GNBN3: n3, Traffic: m.GetTraffic(),
	}, nil
}
