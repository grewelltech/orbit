package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/gnb"
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

	target := conformance.DefaultTargetRelease
	if m.GetTargetRelease() != 0 {
		target = conformance.Release(m.GetTargetRelease())
	}
	env := conformance.Env{
		AMFAddr:       m.GetAmfAddress(),
		Bind:          m.GetBindAddress(),
		UPFN3:         m.GetUpfN3(),
		N3Bind:        m.GetN3Bind(),
		TargetRelease: target,
		GNB: gnb.Config{
			ID:     orDefaultU32(m.GetGnbIdBase(), 700),
			IDBits: 24,
			Name:   "orbit-conformance",
			MCC:    m.GetMcc(),
			MNC:    m.GetMnc(),
			TAC:    orDefaultU32(m.GetTac(), 1),
			// NG Setup fails without a supported slice, so one is always
			// broadcast — this was the whole suite ERRORing "at least one
			// supported S-NSSAI is required" until it was set. SST defaults
			// to 1; SD has no default (the core's slice is the caller's to
			// name — SD-Core wants 010203).
			Slices: []gnb.SNSSAI{{SST: uint8(orDefaultU32(m.GetSst(), 1)), SD: m.GetSd()}},
		},
	}
	if err := env.GNB.ValidateIdentity(); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	var cats []conformance.Category
	for _, c := range m.GetCategories() {
		cats = append(cats, conformance.Category(c))
	}
	perTest := time.Duration(m.GetPerTestSeconds()) * time.Second

	var tally orbitv1.ConformanceTally
	var sendErr error
	conformance.NewRegistry().RunStream(ctx, env, perTest,
		func(res conformance.Result, index, total int) {
			if sendErr != nil {
				return // a prior Send failed; stop trying but let the loop unwind
			}
			addToTally(&tally, res.Verdict)
			upd := &orbitv1.ConformanceUpdate{
				Result:        resultProto(res),
				Tally:         cloneTally(&tally),
				Index:         uint32(index),
				Total:         uint32(total),
				TargetRelease: uint32(target),
			}
			if err := stream.Send(upd); err != nil {
				sendErr = err
			}
		}, cats...)
	if sendErr != nil {
		return sendErr
	}
	if err := ctx.Err(); err != nil {
		// Cancelled mid-run: the tallies so far are honest, but do not claim
		// the run finished.
		return nil
	}

	// Final summary update.
	return stream.Send(&orbitv1.ConformanceUpdate{
		Done:          true,
		Tally:         cloneTally(&tally),
		TargetRelease: uint32(target),
	})
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
		Id:         r.ID,
		Category:   string(r.Category),
		SpecRef:    r.SpecRef,
		Verdict:    string(r.Verdict),
		Expected:   r.Expected,
		Observed:   r.Observed,
		Detail:     r.Detail,
		SpecRefs:   specRefsProto(r.SpecRefs),
		MinRelease: uint32(r.MinRelease),
	}
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

func addToTally(t *orbitv1.ConformanceTally, v conformance.Verdict) {
	switch v {
	case conformance.Pass:
		t.Passed++
	case conformance.Fail:
		t.Failed++
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
		Skipped: t.Skipped, Deviations: t.Deviations,
	}
}

func orDefaultU32(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}
