package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fleetFile is a target-based fleet scenario: workload only.
const fleetFile = `kind: fleet
name: f
topology:
  gnbs: { count: 1 }
fleet: { count: 2 }
`

// lab2 is a second target, distinct from lab1 by its AMF.
var lab2 = strings.NewReplacer("name: lab1", "name: lab2", "192.0.2.10:38412", "192.0.2.99:38412").Replace(lab1NoUE) +
	"subscribers:\n  - name: main\n    first_supi: \"001010000000001\"\n    ki: 22222222222222222222222222222222\n    opc: 33333333333333333333333333333333\n"

func writeScenario(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// startFleet runs `runs start-fleet` against a stub server and returns what
// the server was sent.
func startFleet(t *testing.T, docs string, file string, args ...string) (*stubRunService, orbitRun) {
	t.Helper()
	s := &stubRunService{startRunID: "run-1"}
	url := stubRunServer(t, s)
	r := runOrbit(t, targetDir(t, docs), append([]string{"--server", *url, "runs", "start-fleet", file}, args...)...)
	return s, r
}

// The target a scenario uses: -t, then $ORBIT_TARGET, then the file's
// target: (a name or inline), with a notice when the file's is overridden.
// A self-contained file refuses -t and ignores $ORBIT_TARGET with a notice;
// a file with neither is an error.
func TestScenarioTargetPrecedence(t *testing.T) {
	both := "- " + strings.ReplaceAll(strings.TrimSpace(lab1), "\n", "\n  ") + "\n- " + strings.ReplaceAll(strings.TrimSpace(lab2), "\n", "\n  ") + "\n"
	inline := "target:\n  core:\n    amf: 192.0.2.77:38412\n    plmn: { mcc: \"001\", mnc: \"01\" }\n" +
		"  subscribers:\n    - name: main\n      first_supi: \"001010000000001\"\n      ki: " + testKi + "\n      opc: " + testOPc + "\n" +
		"  ran:\n    n2_source_ips: [192.0.2.70]\n"
	self := "kind: fleet\nname: f\ncore: { amf: 192.0.2.5:38412, plmn: { mcc: \"001\", mnc: \"01\" } }\ntopology:\n  gnbs: { count: 1, source_ips: [192.0.2.6] }\nfleet: { count: 2, supi_base: \"001010000000001\" }\n"
	tests := []struct {
		name     string
		doc      string
		env      string
		args     []string
		wantAMF  string // in the YAML the server was sent
		wantErr  string
		wantNote string
		noNote   bool
	}{
		{name: "file target: name", doc: "target: lab1\n" + fleetFile, wantAMF: "192.0.2.10:38412"},
		{name: "-t", doc: fleetFile, args: []string{"-t", "lab2"}, wantAMF: "192.0.2.99:38412"},
		{name: "$ORBIT_TARGET", doc: fleetFile, env: "lab2", wantAMF: "192.0.2.99:38412"},
		{name: "-t over $ORBIT_TARGET", doc: fleetFile, env: "lab1", args: []string{"-t", "lab2"}, wantAMF: "192.0.2.99:38412"},
		{name: "-t overrides the file's target", doc: "target: lab1\n" + fleetFile, args: []string{"-t", "lab2"}, wantAMF: "192.0.2.99:38412",
			wantNote: "target lab2 (flag -t) overrides the file's target: lab1"},
		{name: "$ORBIT_TARGET overrides the file's target", doc: "target: lab1\n" + fleetFile, env: "lab2", wantAMF: "192.0.2.99:38412",
			wantNote: "target lab2 ($ORBIT_TARGET) overrides the file's target: lab1"},
		{name: "inline target", doc: inline + fleetFile, wantAMF: "192.0.2.77:38412"},
		{name: "-t \"\" clears $ORBIT_TARGET, the file's target: applies", doc: "target: lab1\n" + fleetFile, env: "lab2", args: []string{"-t", ""}, wantAMF: "192.0.2.10:38412", noNote: true},
		{name: "-t \"\" with no file target: names no target", doc: fleetFile, env: "lab2", args: []string{"-t", ""}, wantErr: "names no target"},
		{name: "-t naming the file's own target is no override", doc: "target: lab1\n" + fleetFile, args: []string{"-t", "lab1"}, wantAMF: "192.0.2.10:38412", noNote: true},
		{name: "names no target", doc: fleetFile, wantErr: "x.yaml names no target: add target: <name> or pass -t"},
		{name: "self-contained", doc: self, wantAMF: "192.0.2.5:38412"},
		{name: "self-contained with -t", doc: self, args: []string{"-t", "lab1"}, wantErr: "-t lab1: x.yaml sets its own core.amf, core.plmn, topology.gnbs.source_ips, fleet.supi_base"},
		{name: "self-contained ignores $ORBIT_TARGET", doc: self, env: "lab1", wantAMF: "192.0.2.5:38412",
			wantNote: "$ORBIT_TARGET=lab1 ignored: x.yaml has its own core/credentials"},
		{name: "a target and own core", doc: "target: lab1\n" + self, wantErr: "the scenario sets core.amf, core.plmn, topology.gnbs.source_ips, … and uses target lab1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearOrbitEnv(t)
			if tc.env != "" {
				t.Setenv("ORBIT_TARGET", tc.env)
			}
			args := append([]string{"--ki", testKi, "--opc", testOPc}, tc.args...)
			s, r := startFleet(t, both, writeScenario(t, tc.doc), args...)
			if tc.wantErr != "" {
				if r.execErr == nil || !strings.Contains(r.execErr.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", r.execErr, tc.wantErr)
				}
				return
			}
			if r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			// Sent as a plan when one carries the file, else as the arm.
			y := s.startReq.GetFleet().GetScenarioYaml() //nolint:staticcheck // the deprecated arm, kept for API clients
			if amf := s.startReq.GetPlan().GetTarget().GetCore().GetAmf(); amf != "" {
				y = "amf: " + amf
			}
			if !strings.Contains(y, tc.wantAMF) {
				t.Errorf("sent YAML lacks AMF %s:\n%s", tc.wantAMF, y)
			}
			if tc.wantNote != "" && !strings.Contains(r.err, tc.wantNote) {
				t.Errorf("stderr lacks %q:\n%s", tc.wantNote, r.err)
			}
			if tc.noNote && strings.Contains(r.err, "overrides") {
				t.Errorf("stderr has an override notice:\n%s", r.err)
			}
		})
	}
}

// start-fleet with a target sends the scenario with the target applied, no
// target: key and no keys in the YAML: the keys travel in the request, the
// target's unless --ki/--opc is typed. The server parses what it is sent.
func TestStartFleetSendsAppliedScenario(t *testing.T) {
	clearOrbitEnv(t)
	s, r := startFleet(t, lab1, writeScenario(t, "target: lab1\n"+fleetFile))
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	// Sent as a one-workload plan (armplan.Fleet): the target applied
	// into the plan's own target, the keys in its subscriber pool, which
	// the server masks in what it records.
	plan := s.startReq.GetPlan()
	if plan == nil {
		t.Fatalf("start-fleet did not send a plan: %v", s.startReq)
	}
	tg := plan.GetTarget()
	if tg.GetCore().GetAmf() != "192.0.2.10:38412" || tg.GetRan().GetN2SourceIps()[0] != "192.0.2.50" ||
		tg.GetSubscribers()[0].GetFirstSupi() != "001010000000001" ||
		plan.GetWorkloads()[0].GetPopulation().GetTopology().GetGnbIds().GetFirst() != 100 {
		t.Errorf("plan target = %v", tg)
	}
	if sp := tg.GetSubscribers()[0]; sp.GetKi() != testKi || sp.GetOpc() != testOPc {
		t.Errorf("keys = %s/%s, want the target's", sp.GetKi(), sp.GetOpc())
	}
	// A typed --ki beats the target's.
	other := strings.Repeat("4", 32)
	s, r = startFleet(t, lab1, writeScenario(t, "target: lab1\n"+fleetFile), "--ki", other)
	if r.execErr != nil || s.startReq.GetPlan().GetTarget().GetSubscribers()[0].GetKi() != other {
		t.Errorf("--ki: %v %v", r.execErr, s.startReq.GetPlan().GetTarget().GetSubscribers())
	}
}

// --explain on a scenario command prints the chosen target, where it came
// from, and its fields with their sources, keys masked.
func TestScenarioExplain(t *testing.T) {
	clearOrbitEnv(t)
	t.Setenv("ORBIT_KI", strings.Repeat("5", 32))
	_, r := startFleet(t, lab1, writeScenario(t, "target: lab1\n"+fleetFile), "--explain")
	for _, want := range []string{"x.yaml: target lab1 (the file's target:)", "core.amf", "core.plmn.mcc", "env $ORBIT_KI", "subscribers[main].ki", "<redacted>"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("explain lacks %q:\n%s", want, r.err)
		}
	}
	if strings.Contains(r.err, testKi) || strings.Contains(r.err, strings.Repeat("5", 32)) {
		t.Error("explain shows a key")
	}
}

// A fleet and a step scenario, each with only its workload in the file,
// run with -t against the mock AMF over real SCTP: the target supplies the
// core, keys, gNB IDs and N2 sources.
func TestScenariosRunWithTarget(t *testing.T) {
	clearOrbitEnv(t)
	lab := newProbeLab(t, 85, false)
	doc := "version: 1\nkind: target\nname: lab1\ncore:\n  amf: " + lab.amf + "\n  plmn: { mcc: \"208\", mnc: \"93\" }\n" +
		"  slices:\n    - { sst: 1, sd: \"010203\" }\nsubscribers:\n  - name: main\n    first_supi: \"208930000000001\"\n    count: 10\n" +
		"    ki: " + testKi + "\n    opc: " + testOPc + "\nran:\n  n2_source_ips: [127.0.85.1-127.0.85.9]\n  gnb_ids: { first: 300 }\n"
	loc := targetDir(t, doc)

	fleet := writeScenario(t, "kind: fleet\nname: f\ntopology:\n  gnbs: { count: 2 }\nfleet: { count: 4, attach_rate: 50/s }\n")
	r := runOrbit(t, loc, "run", fleet, "-t", "lab1")
	if r.execErr != nil {
		t.Fatalf("fleet: %v\n%s", r.execErr, r.out)
	}
	for _, want := range []string{"core:     " + lab.amf + "  PLMN 208/93", "2 gNBs (ids 300–301)", "attach:       4/4"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("fleet output lacks %q:\n%s", want, r.out)
		}
	}

	steps := writeScenario(t, `gnbs:
  - { name: g1 }
  - { name: g2 }
ues:
  - { name: first, gnb: g1 }
  - { gnb: g2, range: { count: 2 } }
  - { name: last, gnb: g1 }
steps:
  - register: all
  - deregister: last
`)
	r = runOrbit(t, loc, "--server", lab.server, "run", steps, "-t", "lab1")
	if r.execErr != nil || !strings.Contains(r.out, "4 UE(s) registered") || !strings.Contains(r.out, "1 UE(s) deregistered") {
		t.Errorf("steps: %v\n%s", r.execErr, r.out)
	}
	// Each gNB registers from its own address of the pool: g1's UE from the
	// pool's first address, g2's two from its second.
	for supi, src := range map[string]string{"208930000000001": "127.0.85.1", "208930000000002": "127.0.85.2", "208930000000003": "127.0.85.2"} {
		r = runOrbit(t, loc, "--server", lab.server, "ue", "status", "--supi", supi)
		if r.execErr != nil || !strings.Contains(r.out, "n2-source="+src+":") {
			t.Errorf("ue status %s, want n2-source %s (its gNB's pool address):\n%s (err %v)", supi, src, r.out, r.execErr)
		}
	}
}
