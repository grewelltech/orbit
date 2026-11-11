package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

// phased is a check with a declared phase that records the Env it ran with
// and its place in the run.
type phased struct {
	envProbe
	phase Phase
	order *[]string
}

func (p phased) Phase() Phase { return p.phase }
func (p phased) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}}
}
func (p phased) Run(ctx context.Context, e Env) Result {
	*p.order = append(*p.order, p.id)
	return p.envProbe.Run(ctx, e)
}

func idsOf(tests []Test) string {
	out := make([]string, len(tests))
	for i, t := range tests {
		out[i] = t.ID()
	}
	return strings.Join(out, ",")
}

// A check without a declared phase runs in the main phase, and the phases
// have the names --list prints.
func TestPhaseOf(t *testing.T) {
	tests := []struct {
		name     string
		check    Test
		want     Phase
		wantName string
	}{
		{name: "undeclared", check: fakeTest{id: "A"}, want: PhaseMain, wantName: "main"},
		{name: "stranding", check: phased{phase: PhaseStranding}, want: PhaseStranding, wantName: "stranding"},
		{name: "disruptive", check: phased{phase: PhaseDisruptive}, want: PhaseDisruptive, wantName: "disruptive"},
		{name: "out of range", check: phased{phase: Phase(9)}, want: Phase(9), wantName: "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseOf(tc.check); got != tc.want || got.String() != tc.wantName {
				t.Errorf("PhaseOf = %d (%s), want %d (%s)", got, got, tc.want, tc.wantName)
			}
		})
	}
}

// Select and a run take the checks by phase, in registration order within a
// phase, whatever the category and --check filters; each check keeps the
// gNB IDs and the subscriber of its place in the registry.
func TestRunOrderFollowsPhases(t *testing.T) {
	var order []string
	seen := map[string]Env{}
	r := &Registry{}
	for _, c := range []struct {
		id    string
		phase Phase
	}{{"D1", PhaseDisruptive}, {"M1", PhaseMain}, {"S1", PhaseStranding}, {"M2", PhaseMain}, {"S2", PhaseStranding}, {"D2", PhaseDisruptive}} {
		r.Register(phased{envProbe: envProbe{id: c.id, seen: seen}, phase: c.phase, order: &order})
	}
	const want = "M1,M2,S1,S2,D1,D2"
	if got := idsOf(r.Select()); got != want {
		t.Errorf("Select = %s, want %s", got, want)
	}
	if got := idsOf(r.Tests()); got != "D1,M1,S1,M2,S2,D2" {
		t.Errorf("Tests = %s, want registration order", got)
	}
	env := Env{GNB: gnb.Config{ID: 5000}, PackedGNBIDs: true, UE: UESpec{SUPI: "001010100007500", Subscribers: 10}}
	var indexes []int
	r.RunStream(context.Background(), env, time.Second, func(_ Result, i, _ int) { indexes = append(indexes, i) })
	if got := strings.Join(order, ","); got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
	for i, idx := range indexes {
		if idx != i {
			t.Errorf("result %d emitted with index %d", i, idx)
		}
	}
	for i, id := range []string{"D1", "M1", "S1", "M2", "S2", "D2"} {
		e := seen[id]
		wantSUPI, err := supiAt("001010100007500", i)
		if err != nil {
			t.Fatal(err)
		}
		if e.GNB.ID != uint32(5000+i) || e.TargetGNBID != uint32(5006+i) || e.LivenessGNBID != uint32(5012+i) || e.UE.SUPI != wantSUPI {
			t.Errorf("%s ran with gNB IDs %d/%d/%d and SUPI %s, want those of registry place %d", id, e.GNB.ID, e.TargetGNBID, e.LivenessGNBID, e.UE.SUPI, i)
		}
	}

	order = nil
	if err := r.Only("d1", "s2", "m2"); err != nil {
		t.Fatal(err)
	}
	r.RunStream(context.Background(), env, time.Second, func(Result, int, int) {})
	if got := strings.Join(order, ","); got != "M2,S2,D1" {
		t.Errorf("narrowed run took %s, want M2,S2,D1", got)
	}
	if e := seen["D1"]; e.GNB.ID != 5000 {
		t.Errorf("D1 narrowed ran with gNB ID %d, want 5000", e.GNB.ID)
	}
}

// The built-in suite ends with the checks that leave a UE context in the
// core, in registration order, and then the one a core may not survive;
// every other check runs before them, in registration order. The same
// order comes out of every Select.
func TestBuiltinRunOrder(t *testing.T) {
	r := NewRegistry()
	sel := r.Select()
	const tail = "NAS-RETRANSMIT-NEW-COUNT,NAS-T3550-ACCEPT-RETRANSMIT,NAS-T3560-AUTH-RETRANSMIT,NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL,NGAP-PATH-SWITCH-SECCAP-MISMATCH"
	if len(sel) != len(r.Tests()) || len(sel) < 5 {
		t.Fatalf("Select has %d checks, the registry %d", len(sel), len(r.Tests()))
	}
	if got := idsOf(sel[len(sel)-5:]); got != tail {
		t.Errorf("the suite ends with %s, want %s", got, tail)
	}
	wantPhase := map[string]Phase{}
	for i, id := range strings.Split(tail, ",") {
		wantPhase[id] = PhaseStranding
		if i == 4 {
			wantPhase[id] = PhaseDisruptive
		}
	}
	var main, registered []string
	for _, c := range sel[:len(sel)-5] {
		main = append(main, c.ID())
	}
	for _, c := range r.Tests() {
		if PhaseOf(c) != wantPhase[c.ID()] {
			t.Errorf("%s runs in phase %s, want %s", c.ID(), PhaseOf(c), wantPhase[c.ID()])
		}
		if PhaseOf(c) == PhaseMain {
			registered = append(registered, c.ID())
		}
	}
	if strings.Join(main, ",") != strings.Join(registered, ",") {
		t.Errorf("main phase runs %v, want registration order %v", main, registered)
	}
	if again := idsOf(NewRegistry().Select()); again != idsOf(sel) {
		t.Errorf("a second registry selects %s, the first %s", again, idsOf(sel))
	}
	// A group filter keeps the phase order.
	if err := r.Groups(0, "NAS / NAS security", "TS 33.501"); err != nil {
		t.Fatal(err)
	}
	if got := r.Select(); got[len(got)-1].ID() != "NGAP-PATH-SWITCH-SECCAP-MISMATCH" || got[len(got)-2].ID() != "NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL" {
		t.Errorf("the NAS security and TS 33.501 groups end with %s", idsOf(got))
	}
}
