package server

import (
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
)

// The handover response carries the target the engine presented, inherited
// fields included, and the elapsed time; gnbConfigToProto is the inverse of
// gnbTargetFromProto (#108).
func TestHandoverResponseCarriesTarget(t *testing.T) {
	tests := []struct {
		name   string
		target gnb.Config
	}{
		{name: "one slice", target: gnb.Config{ID: 2, IDBits: 24, Name: "tgt", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}},
		{name: "two slices, no SD", target: gnb.Config{ID: 0x43, IDBits: 22, Name: "orbit-gnb-tgt", MCC: "001", MNC: "001", TAC: 7,
			Slices: []gnb.SNSSAI{{SST: 1}, {SST: 2, SD: "abcdef"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := handoverResponse("208930100007500", engine.HandoverResult{Elapsed: 1500 * time.Microsecond, Target: tc.target})
			if r.GetGnbId() != tc.target.ID || r.GetElapsedMs() != 1.5 || r.GetState() != engine.StateHandoverComplete {
				t.Errorf("response = %+v", r)
			}
			back, err := gnbTargetFromProto(r.GetTarget())
			if err != nil {
				t.Fatal(err)
			}
			if back.ID != tc.target.ID || back.IDBits != tc.target.IDBits || back.Name != tc.target.Name || back.MCC != tc.target.MCC ||
				back.MNC != tc.target.MNC || back.TAC != tc.target.TAC || len(back.Slices) != len(tc.target.Slices) {
				t.Fatalf("round trip = %+v, want %+v", back, tc.target)
			}
			for i := range back.Slices {
				if back.Slices[i] != tc.target.Slices[i] {
					t.Errorf("slice %d = %+v, want %+v", i, back.Slices[i], tc.target.Slices[i])
				}
			}
		})
	}
}
