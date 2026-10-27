package server

import (
	"strings"
	"testing"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/report"
	"github.com/bgrewell/orbit/internal/secrets"
)

// A report states the configuration a run had: an old arm's spec, a plan
// run's plan (masked as the archive holds it), or that none was recorded.
func TestReportConfig(t *testing.T) {
	plan := &orbitv1.RunPlan{Target: &orbitv1.ResolvedTarget{Core: &orbitv1.TargetCore{Amf: "192.0.2.1:38412"},
		Subscribers: []*orbitv1.SubscriberPool{{Name: "main", Ki: secrets.Masked}}},
		Workloads: []*orbitv1.Workload{{Name: "pop"}}}
	tests := []struct {
		name      string
		a         *orbitv1.RunArchive
		wantLabel string
		want      []string
	}{
		{name: "a load arm", a: &orbitv1.RunArchive{Spec: &orbitv1.RunArchive_LoadSpec{LoadSpec: &orbitv1.LoadRunSpec{AmfAddress: "192.0.2.1:38412"}}}, //nolint:staticcheck // the deprecated arm, kept for API clients
			wantLabel: "Load specification", want: []string{"amf: 192.0.2.1:38412"}},
		{name: "a plan", a: &orbitv1.RunArchive{Plan: plan}, wantLabel: "Plan", want: []string{`"amf": "192.0.2.1:38412"`, secrets.Masked, `"name": "pop"`}},
		{name: "nothing recorded", a: &orbitv1.RunArchive{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d report.Data
			addConfig(&d, tc.a)
			if d.ConfigLabel != tc.wantLabel {
				t.Errorf("label = %q, want %q", d.ConfigLabel, tc.wantLabel)
			}
			// protojson varies its whitespace on purpose: compare without it.
			flat := strings.Join(strings.Fields(d.Config), "")
			for _, w := range tc.want {
				if !strings.Contains(flat, strings.Join(strings.Fields(w), "")) {
					t.Errorf("config lacks %q:\n%s", w, d.Config)
				}
			}
			if tc.wantLabel == "" && (len(d.Notes) == 0 || !strings.Contains(d.Notes[0], "not recorded")) {
				t.Errorf("notes = %v, want the not-recorded note", d.Notes)
			}
		})
	}
}
