package conformance

import (
	"context"
	"strings"
	"testing"
	"time"
)

// packCheck is a test-only pack check: declared outcomes, a primary
// reference the clause-title table covers.
func packCheck(id string, outs ...Outcome) Test {
	if len(outs) == 0 {
		outs = []Outcome{{ID: "seen", Verdict: Info, Summary: "did what the core does"}}
	}
	return declTest{provTest: provTest{id: id, refs: []SpecRef{goodRef()}, result: Result{Verdict: outs[0].Verdict, Outcome: outs[0].ID}}, outs: outs}
}

// defaultChecks is a default run of two checks, one owning the NG Setup.
func defaultChecks() []Test {
	out := []Outcome{{ID: "seen", Verdict: Info, Summary: "did something"}}
	return []Test{
		declTest{provTest: provTest{id: "SETUP", refs: []SpecRef{goodRef()}, result: Result{Verdict: Info, Outcome: "seen"}}, outs: out, owned: []Procedure{ProcNGSetup}},
		declTest{provTest: provTest{id: "OTHER", refs: []SpecRef{goodRef()}, result: Result{Verdict: Info, Outcome: "seen"}}, outs: out},
	}
}

func TestBuiltinPacksNone(t *testing.T) {
	r := NewRegistry()
	if len(r.Packs()) != 0 {
		t.Errorf("packs %v, want none", r.Packs())
	}
	if err := r.UsePack("sdcore", "sdcore"); err == nil || err.Error() != `unknown pack "sdcore" (have none)` {
		t.Errorf("err = %v", err)
	}
}

// A pack is refused at registration when it breaks the pack layer's rules.
func TestNewRegistryPacks(t *testing.T) {
	cases := []struct {
		name    string
		packs   []Pack
		wantErr string
	}{
		{name: "valid", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-1")}}}},
		{name: "a WARN outcome", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-1",
			Outcome{ID: "risky", Verdict: Warn, Criterion: CriterionAvailability, Summary: "drops it"})}}}},
		{name: "no name", packs: []Pack{{Profile: "sdcore", Checks: []Test{packCheck("LAB-1")}}}, wantErr: "a pack has no name"},
		{name: "strict-3gpp", packs: []Pack{{Name: "lab", Profile: "strict-3gpp", Checks: []Test{packCheck("LAB-1")}}},
			wantErr: `pack lab: profile "strict-3gpp" has no pack`},
		{name: "no profile", packs: []Pack{{Name: "lab", Checks: []Test{packCheck("LAB-1")}}}, wantErr: `pack lab: profile "" has no pack`},
		{name: "an unknown profile", packs: []Pack{{Name: "lab", Profile: "acme", Checks: []Test{packCheck("LAB-1")}}}, wantErr: `unknown core profile "acme"`},
		{name: "no checks", packs: []Pack{{Name: "lab", Profile: "sdcore"}}, wantErr: "pack lab: no checks"},
		{name: "a PASS outcome", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-1",
			Outcome{ID: "ok", Verdict: Pass, Refs: []string{goodRef().Key()}, Summary: "did it"})}}},
			wantErr: `pack lab: LAB-1 declares outcome "ok" as PASS; a pack check reports no conformance verdict`},
		{name: "a FAIL outcome", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-1",
			Outcome{ID: "bad", Verdict: Fail, Refs: []string{goodRef().Key()}, Summary: "did not"})}}},
			wantErr: `declares outcome "bad" as FAIL`},
		{name: "an owner", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{
			declTest{provTest: provTest{id: "LAB-1", refs: []SpecRef{goodRef()}}, outs: []Outcome{{ID: "seen", Verdict: Info, Summary: "x"}}, owned: []Procedure{ProcRegistration}}}}},
			wantErr: "pack lab: LAB-1 owns registration"},
		{name: "a pack check that breaks its declaration rules", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-1",
			Outcome{ID: "odd", Verdict: Warn, Summary: "did something odd"})}}},
			wantErr: `LAB-1: outcome "odd": a WARN outcome names its criterion`},
		{name: "two packs, one name", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-1")}}, {Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-2")}}},
			wantErr: "two packs are named lab"},
		{name: "a pack check with a default check's ID", packs: []Pack{{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("other")}}},
			wantErr: "other: two checks have this ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newRegistry(defaultChecks(), tc.packs...)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// A pack's checks run only when the run names the pack against a target
// with its profile, after the default run's checks, and their results say
// which pack they are from.
func TestUsePack(t *testing.T) {
	newReg := func(t *testing.T) *Registry {
		r, err := newRegistry(defaultChecks(), Pack{Name: "lab", Profile: "sdcore", Checks: []Test{packCheck("LAB-1")}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ids := func(tests []Test) string {
		var out []string
		for _, c := range tests {
			out = append(out, c.ID())
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name, pack, profile string
		want                string
		wantErr             string
	}{
		{name: "no pack: the default run", profile: "sdcore", want: "SETUP,OTHER"},
		{name: "the pack, its profile", pack: "lab", profile: "sdcore", want: "SETUP,OTHER,LAB-1"},
		{name: "a target with no profile", pack: "lab", wantErr: `pack "lab" needs a target with a core profile; the target's is none, which has no pack`},
		{name: "a strict-3gpp target", pack: "lab", profile: "strict-3gpp", wantErr: `the target's is strict-3gpp, which has no pack`},
		{name: "another profile", pack: "lab", profile: "open5gs", wantErr: `pack "lab" is for core profile "sdcore"; the target's is "open5gs"`},
		{name: "an unknown pack", pack: "other", profile: "sdcore", wantErr: `unknown pack "other" (have lab)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReg(t)
			err := (Selection{Pack: tc.pack, Profile: tc.profile}).Apply(r)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if got := ids(r.Select()); got != "SETUP,OTHER" {
					t.Errorf("a refused pack left %s selected", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(r.Select()); got != tc.want {
				t.Errorf("selected %s, want %s", got, tc.want)
			}
		})
	}

	r := newReg(t)
	if got := ids(r.Tests()); got != "SETUP,OTHER" {
		t.Errorf("Tests() = %s: a pack's checks are not the default run's", got)
	}
	if err := r.Only("LAB-1"); err == nil || err.Error() != "check LAB-1 is in pack lab, which this run does not name" {
		t.Errorf("a pack check named without its pack: %v", err)
	}
	if err := (Selection{Pack: "lab", Profile: "sdcore", Checks: []string{"lab-1"}}).Apply(r); err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), Env{}, time.Second)
	if len(res) != 1 || res[0].ID != "LAB-1" || res[0].Pack != "lab" || res[0].Verdict != Info {
		t.Fatalf("results %+v", res)
	}
	if r.PackOf(r.Select()[0]) != "lab" {
		t.Errorf("PackOf = %q", r.PackOf(r.Select()[0]))
	}
	if err := r.UsePack("", ""); err != nil || r.pack != "" {
		t.Errorf("an empty name keeps pack %q (%v)", r.pack, err)
	}
}
