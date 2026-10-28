package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// StartRun's refusals quote what the plan named (a workload, a gate, a
// subscriber pool); a key typed as a name is masked in what the client is
// sent, over the real handler (the Connect error's own message is what
// goes on the wire), its code kept.
func TestStartRunMasksKeyText(t *testing.T) {
	const key = "465b5ce8b199b49faa5f0a2ee238a6bc"
	srv := httptest.NewServer(func() http.Handler {
		mux := http.NewServeMux()
		mux.Handle(orbitv1connect.NewRunServiceHandler(planService(t)))
		return mux
	}())
	t.Cleanup(srv.Close)
	client := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
	slo := func(name, workload string) *orbitv1.Gate {
		return &orbitv1.Gate{Name: name, Workload: workload, Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{Metric: "attach.success_ratio", Op: orbitv1.CompareOp_COMPARE_OP_GE, Threshold: 0.9}}}
	}
	for _, tc := range []struct {
		name string
		plan *orbitv1.RunPlan
	}{
		{"a gate on a workload named by a key", loadPlan(func(p *orbitv1.RunPlan) { p.Gates = []*orbitv1.Gate{slo("g1", key)} })},
		{"two gates named by a key", loadPlan(func(p *orbitv1.RunPlan) { p.Gates = []*orbitv1.Gate{slo(key, ""), slo(key, "")} })},
		{"a population's pool named by a key", loadPlan(func(p *orbitv1.RunPlan) { pop(p).SubscriberPool = key })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: tc.plan}}))
			if err == nil {
				t.Fatal("started")
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "<redacted>") {
				t.Errorf("err %v: want InvalidArgument with the key masked", err)
			}
		})
	}
}
