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

var errAMFRequired = errors.New("amf_address is required (host:port of the AMF N2 endpoint)")

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

	env := conformance.Env{
		AMFAddr: m.GetAmfAddress(),
		UPFN3:   m.GetUpfN3(),
		N3Bind:  m.GetN3Bind(),
		GNB: gnb.Config{
			ID:     orDefaultU32(m.GetGnbIdBase(), 700),
			IDBits: 24,
			Name:   "orbit-conformance",
			MCC:    orDefaultStr(m.GetMcc(), "001"),
			MNC:    orDefaultStr(m.GetMnc(), "01"),
			TAC:    orDefaultU32(m.GetTac(), 1),
			// NG Setup fails without a supported slice, so one is always
			// broadcast — this was the whole suite ERRORing "at least one
			// supported S-NSSAI is required" until it was set.
			Slices: []gnb.SNSSAI{{
				SST: uint8(orDefaultU32(m.GetSst(), 1)),
				SD:  orDefaultStr(m.GetSd(), "010203"),
			}},
		},
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
				Result: resultProto(res),
				Tally:  cloneTally(&tally),
				Index:  uint32(index),
				Total:  uint32(total),
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
		Done:  true,
		Tally: cloneTally(&tally),
	})
}

// ListConformanceTests reports the registered tests without running them.
func (s *conformanceService) ListConformanceTests(
	ctx context.Context,
	req *connect.Request[orbitv1.ListConformanceTestsRequest],
) (*connect.Response[orbitv1.ListConformanceTestsResponse], error) {
	var out []*orbitv1.ConformanceTestInfo
	for _, t := range conformance.NewRegistry().Tests() {
		out = append(out, &orbitv1.ConformanceTestInfo{
			Id:       t.ID(),
			Category: string(t.Category()),
			SpecRef:  t.SpecRef(),
		})
	}
	return connect.NewResponse(&orbitv1.ListConformanceTestsResponse{Tests: out}), nil
}

func resultProto(r conformance.Result) *orbitv1.ConformanceResult {
	return &orbitv1.ConformanceResult{
		Id:       r.ID,
		Category: string(r.Category),
		SpecRef:  r.SpecRef,
		Verdict:  string(r.Verdict),
		Expected: r.Expected,
		Observed: r.Observed,
		Detail:   r.Detail,
	}
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

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orDefaultU32(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}
