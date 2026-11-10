package server

import (
	"testing"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

func conformanceWorkload(pass, fail, err, skip, dev, expFail, total uint32, gatesPass, gatesFail int) *orbitv1.RunReport {
	rr := &orbitv1.RunReport{
		Kind: orbitv1.RunKind_RUN_KIND_CONFORMANCE,
		Report: &orbitv1.RunReport_Conformance{Conformance: &orbitv1.ConformanceSummary{
			Total: total,
			Tally: &orbitv1.ConformanceTally{
				Passed: pass, Failed: fail, Errored: err, Skipped: skip,
				Deviations: dev, ExpectedFailures: expFail,
			},
		}},
	}
	rr.Gates = gateResults(gatesPass, gatesFail)
	return rr
}

func gateResults(pass, fail int) []*orbitv1.GateResult {
	var gs []*orbitv1.GateResult
	for i := 0; i < pass; i++ {
		gs = append(gs, &orbitv1.GateResult{Passed: true})
	}
	for i := 0; i < fail; i++ {
		gs = append(gs, &orbitv1.GateResult{Passed: false})
	}
	return gs
}

func TestRunSummary(t *testing.T) {
	tests := []struct {
		name string
		arc  *orbitv1.RunArchive
		want *orbitv1.RunSummary
	}{
		{
			name: "nil archive",
			arc:  nil,
			want: nil,
		},
		{
			name: "no report (failed/cancelled) yields nil",
			arc: &orbitv1.RunArchive{
				Run: &orbitv1.Run{RunId: "r1", Kind: orbitv1.RunKind_RUN_KIND_LOAD, State: orbitv1.RunState_RUN_STATE_FAILED},
			},
			want: nil,
		},
		{
			name: "legacy top-level load report (no plan reports)",
			arc: &orbitv1.RunArchive{
				Run:    &orbitv1.Run{RunId: "r2", Kind: orbitv1.RunKind_RUN_KIND_LOAD},
				Report: &orbitv1.RunArchive_LoadReport{LoadReport: &orbitv1.LoadReport{Attempted: 100, Succeeded: 97, Failed: 3}},
			},
			want: &orbitv1.RunSummary{
				Kinds:           []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_LOAD},
				AttachAttempted: 100, AttachSucceeded: 97, AttachFailed: 3,
			},
		},
		{
			name: "legacy top-level fleet report",
			arc: &orbitv1.RunArchive{
				Run:    &orbitv1.Run{RunId: "r3", Kind: orbitv1.RunKind_RUN_KIND_FLEET},
				Report: &orbitv1.RunArchive_FleetReport{FleetReport: &orbitv1.FleetReport{Attached: 40, AttachFailed: 2}},
			},
			want: &orbitv1.RunSummary{
				Kinds:           []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_FLEET},
				AttachAttempted: 42, AttachSucceeded: 40, AttachFailed: 2,
			},
		},
		{
			name: "conformance workload with all gates passing",
			arc: &orbitv1.RunArchive{
				Run:     &orbitv1.Run{RunId: "r4", Kind: orbitv1.RunKind_RUN_KIND_CONFORMANCE},
				Reports: []*orbitv1.RunReport{conformanceWorkload(18, 2, 0, 1, 1, 2, 21, 1, 0)},
			},
			want: &orbitv1.RunSummary{
				Kinds:                       []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_CONFORMANCE},
				ConformanceTotal:            21,
				ConformancePassed:           18,
				ConformanceFailed:           2,
				ConformanceSkipped:          1,
				ConformanceDeviations:       1,
				ConformanceExpectedFailures: 2,
				GatesTotal:                  1,
				GatesPassed:                 1,
				GatesOk:                     true,
			},
		},
		{
			name: "mixed run sums load + conformance, one gate fails",
			arc: &orbitv1.RunArchive{
				Run: &orbitv1.Run{
					RunId:         "r5",
					Kind:          orbitv1.RunKind_RUN_KIND_MIXED,
					WorkloadKinds: []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_LOAD, orbitv1.RunKind_RUN_KIND_CONFORMANCE},
				},
				Reports: []*orbitv1.RunReport{
					{
						Kind:   orbitv1.RunKind_RUN_KIND_LOAD,
						Report: &orbitv1.RunReport_Load{Load: &orbitv1.LoadReport{Attempted: 50, Succeeded: 50, Failed: 0}},
						Gates:  gateResults(1, 1),
					},
					conformanceWorkload(10, 0, 0, 0, 0, 0, 10, 1, 0),
				},
			},
			want: &orbitv1.RunSummary{
				Kinds:             []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_LOAD, orbitv1.RunKind_RUN_KIND_CONFORMANCE},
				AttachAttempted:   50,
				AttachSucceeded:   50,
				ConformanceTotal:  10,
				ConformancePassed: 10,
				GatesTotal:        3,
				GatesPassed:       2,
				GatesOk:           false, // one gate failed
			},
		},
		{
			name: "reports preferred over top-level report (no double count)",
			arc: &orbitv1.RunArchive{
				Run:    &orbitv1.Run{RunId: "r6", Kind: orbitv1.RunKind_RUN_KIND_LOAD},
				Report: &orbitv1.RunArchive_LoadReport{LoadReport: &orbitv1.LoadReport{Attempted: 999, Succeeded: 999}},
				Reports: []*orbitv1.RunReport{{
					Kind:   orbitv1.RunKind_RUN_KIND_LOAD,
					Report: &orbitv1.RunReport_Load{Load: &orbitv1.LoadReport{Attempted: 10, Succeeded: 9, Failed: 1}},
				}},
			},
			want: &orbitv1.RunSummary{
				Kinds:           []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_LOAD},
				AttachAttempted: 10, AttachSucceeded: 9, AttachFailed: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runSummary(tt.arc)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("want nil summary, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want %+v, got nil", tt.want)
			}
			assertUint(t, "conformance_total", got.GetConformanceTotal(), tt.want.GetConformanceTotal())
			assertUint(t, "conformance_passed", got.GetConformancePassed(), tt.want.GetConformancePassed())
			assertUint(t, "conformance_failed", got.GetConformanceFailed(), tt.want.GetConformanceFailed())
			assertUint(t, "conformance_errored", got.GetConformanceErrored(), tt.want.GetConformanceErrored())
			assertUint(t, "conformance_skipped", got.GetConformanceSkipped(), tt.want.GetConformanceSkipped())
			assertUint(t, "conformance_deviations", got.GetConformanceDeviations(), tt.want.GetConformanceDeviations())
			assertUint(t, "conformance_expected_failures", got.GetConformanceExpectedFailures(), tt.want.GetConformanceExpectedFailures())
			assertUint(t, "attach_attempted", got.GetAttachAttempted(), tt.want.GetAttachAttempted())
			assertUint(t, "attach_succeeded", got.GetAttachSucceeded(), tt.want.GetAttachSucceeded())
			assertUint(t, "attach_failed", got.GetAttachFailed(), tt.want.GetAttachFailed())
			assertUint(t, "gates_total", got.GetGatesTotal(), tt.want.GetGatesTotal())
			assertUint(t, "gates_passed", got.GetGatesPassed(), tt.want.GetGatesPassed())
			if got.GetGatesOk() != tt.want.GetGatesOk() {
				t.Errorf("gates_ok: got %v want %v", got.GetGatesOk(), tt.want.GetGatesOk())
			}
			if len(got.GetKinds()) != len(tt.want.GetKinds()) {
				t.Fatalf("kinds: got %v want %v", got.GetKinds(), tt.want.GetKinds())
			}
			for i, k := range tt.want.GetKinds() {
				if got.GetKinds()[i] != k {
					t.Errorf("kinds[%d]: got %v want %v", i, got.GetKinds()[i], k)
				}
			}
		})
	}
}

func assertUint(t *testing.T, field string, got, want uint32) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %d want %d", field, got, want)
	}
}
