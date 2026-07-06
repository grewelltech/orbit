//go:build integration

package conformance_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/gnb"
)

// TestLiveConformance runs the conformance/regression suite against the live
// core and reports structured results. It fails only on ERROR verdicts (the
// harness could not run); PASS/FAIL are the core's behaviour and are logged
// with citations for the report.
func TestLiveConformance(t *testing.T) {
	amf := os.Getenv("ORBIT_AMF_N2")
	if amf == "" {
		amf = "172.17.50.11:38412"
	}
	baseGNB := uint32(0x400)
	if v := os.Getenv("ORBIT_GNB_BASE"); v != "" {
		if n, err := strconv.ParseUint(v, 0, 32); err == nil {
			baseGNB = uint32(n)
		}
	}

	env := conformance.Env{
		AMFAddr: amf,
		GNB: gnb.Config{
			ID: baseGNB, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1,
			Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	results := conformance.NewRegistry().Run(ctx, env, 15*time.Second)

	errors := 0
	for _, r := range results {
		t.Logf("%-5s [%-11s] %-24s expected=%q observed=%q", r.Verdict, r.Category, r.ID, r.Expected, r.Observed)
		if r.Detail != "" {
			t.Logf("        %s — %s", r.SpecRef, r.Detail)
		}
		if r.Verdict == conformance.Error {
			errors++
		}
	}
	b, _ := json.MarshalIndent(results, "", "  ")
	t.Logf("results JSON:\n%s", string(b))
	if errors > 0 {
		t.Fatalf("%d conformance test(s) could not run (ERROR)", errors)
	}
}
