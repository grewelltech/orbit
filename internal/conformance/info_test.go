package conformance

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"

	"github.com/bgrewell/orbit/internal/gnb"
)

// infoOnly is a check citing descriptive text whose one outcome is INFO,
// and whose Run reports it, or the result run returns when run is set.
type infoOnly struct {
	provTest
	run func(Env) Result
}

// infoSeen is infoOnly's one outcome.
var infoSeen = Outcome{ID: "seen", Verdict: Info, Summary: "did what the text describes"}

func (infoOnly) Outcomes() []Outcome { return []Outcome{infoSeen} }

func (i infoOnly) Run(ctx context.Context, env Env) Result {
	if i.run != nil {
		return i.run(env)
	}
	r := i.provTest.Run(ctx, env)
	if r.Verdict == Info {
		r.Outcome = infoSeen.ID
	}
	return r
}

func refOf(s Strength) SpecRef {
	r := goodRef()
	r.Strength = s
	return r
}

// A check citing descriptive text only SKIPs on the target release like any
// other, on its descriptive references' Since.
func TestRunOneInfoSkipsOnRelease(t *testing.T) {
	ref17 := SpecRef{Spec: "TS 38.413", Clause: "8.4.5.2", Strength: Descriptive, Since: 17, WordingUnchanged: true}
	ran := false
	c := infoOnly{provTest: provTest{id: "X", refs: []SpecRef{ref17}, ran: &ran, result: Result{Verdict: Info}}}
	res := runOne(context.Background(), c, Env{TargetRelease: 16}, time.Second)
	if ran || res.Verdict != Skip {
		t.Errorf("ran=%v verdict=%s detail=%q, want a SKIP", ran, res.Verdict, res.Detail)
	}
	if res := runOne(context.Background(), c, Env{TargetRelease: 17}, time.Second); !ran || res.Verdict != Info {
		t.Errorf("at the Since release: ran=%v verdict=%s, want INFO", ran, res.Verdict)
	}
}

// INFO is counted on its own and gates under no policy; an ERROR from a
// check whose outcomes are INFO gates as any ERROR does, a FAIL beside INFO
// results still gates, and so does a CRASH for a core that did not survive.
func TestInfoDoesNotGate(t *testing.T) {
	desc := []SpecRef{refOf(Descriptive)}
	info := func(id string, v Verdict) Test {
		return infoOnly{provTest: provTest{id: id, refs: desc, result: Result{Verdict: v}}}
	}
	crashed := infoOnly{provTest: provTest{id: "D", refs: desc, result: Result{Verdict: Crash, Observed: ObservedAMFUnresponsive + " (association reset)"}}}
	tests := []struct {
		name      string
		tests     []Test
		wantInfo  int
		wantPass  int
		wantFail  int
		wantCrash int
		wantError int
		wantOK    bool
	}{
		{name: "INFO only", tests: []Test{info("A", Info), info("B", Info), info("C", Info)},
			wantInfo: 3, wantOK: true},
		{name: "beside a PASS", tests: []Test{info("A", Info), fakeTest{"B", Pass, ""}},
			wantInfo: 1, wantPass: 1, wantOK: true},
		{name: "beside a graded FAIL", tests: []Test{info("A", Info), fakeTest{"B", Fail, ""}},
			wantInfo: 1, wantFail: 1},
		{name: "an ERROR gates", tests: []Test{info("A", Error)}, wantError: 1},
		{name: "a core that did not survive gates", tests: []Test{info("A", Info), crashed},
			wantInfo: 1, wantCrash: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Registry{}
			for _, c := range tc.tests {
				r.Register(c)
			}
			sum := Summarize(r.Run(context.Background(), Env{}, time.Second))
			if sum.Info != tc.wantInfo || sum.Crash != tc.wantCrash || sum.Pass != tc.wantPass || sum.Fail != tc.wantFail ||
				sum.Error != tc.wantError || sum.Deviation != 0 || sum.Total != len(tc.tests) {
				t.Errorf("summary = %+v", sum)
			}
			for _, p := range []GatePolicy{GateDefault, GateSpec} {
				if sum.Gate = p; sum.OK() != tc.wantOK {
					t.Errorf("%s gate: OK = %v, want %v", p, sum.OK(), tc.wantOK)
				}
			}
		})
	}
}

// The JSON summary carries the INFO count and each result's verdict as the
// string INFO.
func TestSummaryJSONInfo(t *testing.T) {
	desc := []SpecRef{refOf(Descriptive)}
	r := &Registry{}
	r.Register(infoOnly{provTest: provTest{id: "A", refs: desc, result: Result{Verdict: Info, Expected: "an acknowledge", Observed: "an acknowledge"}}})
	r.Register(infoOnly{provTest: provTest{id: "B", refs: desc, result: Result{Verdict: Info, Expected: "an acknowledge", Observed: "no answer"}}})
	b, err := json.Marshal(Summarize(r.Run(context.Background(), Env{}, time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Total, Pass, Fail, Deviation, Info int
		Results                            []struct {
			ID       string `json:"id"`
			Verdict  string `json:"verdict"`
			Expected string `json:"expected"`
			Observed string `json:"observed"`
			Detail   string `json:"detail"`
			SpecRefs []struct {
				Strength string `json:"strength"`
			} `json:"spec_refs"`
		}
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 2 || got.Info != 2 || got.Pass != 0 || got.Fail != 0 || got.Deviation != 0 {
		t.Errorf("counts in %s", b)
	}
	want := []struct{ id, verdict, observed string }{{"A", "INFO", "an acknowledge"}, {"B", "INFO", "no answer"}}
	if len(got.Results) != len(want) {
		t.Fatalf("%d results in %s", len(got.Results), b)
	}
	for i, w := range want {
		g := got.Results[i]
		if g.ID != w.id || g.Verdict != w.verdict || g.Observed != w.observed || g.Expected != "an acknowledge" ||
			len(g.SpecRefs) != 1 || g.SpecRefs[0].Strength != "descriptive" {
			t.Errorf("result %d = %+v, want %+v", i, g, w)
		}
	}
	for _, key := range []string{`"Info":2`, `"Warn":0`, `"Blocked":0`, `"Crash":0`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("summary JSON lacks %s: %s", key, b)
		}
	}
}

// The expected-failures list cannot name a check whose outcomes are INFO
// only, and no entry ever marks an INFO result or reports it stale by
// matching.
func TestExpectedFailuresRefuseInfo(t *testing.T) {
	tests := []Test{
		provTest{id: "GRADED", refs: []SpecRef{refOf(Shall)}},
		infoOnly{provTest: provTest{id: "INFO-ONLY", refs: []SpecRef{refOf(Descriptive)}}},
	}
	known := listableChecks(tests)
	if !slices.Contains(known["GRADED"], Fail) || len(known["INFO-ONLY"]) != 0 || len(known) != 2 {
		t.Fatalf("listableChecks = %v", known)
	}
	entry := func(check string) []byte {
		return []byte("core: c\nversion: v\nentries:\n  - check: " + check + "\n    observed: o\n    finding: EX-1\n    added: 2026-11-10\n    reason: r\n")
	}
	cases := []struct {
		name, check, wantErr string
	}{
		{name: "graded", check: "GRADED"},
		{name: "INFO only", check: "INFO-ONLY", wantErr: `f.yaml: entry 1: check "INFO-ONLY" never reports FAIL; it cannot be listed with that verdict`},
		{name: "unknown", check: "NOPE", wantErr: `unknown check "NOPE"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseExpectedFailures("f.yaml", entry(tc.check), known)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	x := &ExpectedFailures{Core: "c", Version: "v", Entries: []ExpectedFailureEntry{{Check: "A", Observed: "o", Finding: "EX-1", Added: "2026-11-10", Reason: "r"}}}
	for _, v := range []Verdict{Info} {
		results := []Result{{ID: "A", Verdict: v, Observed: "o"}}
		x.Apply(results)
		sum := Summarize(results)
		if results[0].ExpectedFailure != nil || sum.ExpectedFail != 0 || !sum.OK() {
			t.Errorf("%s with an entry's observed text: marked %v, summary %+v", v, results[0].ExpectedFailure != nil, sum)
		}
	}
}

// checks.md names a check's class by the worst of its declared outcomes,
// and explains the classes.
func TestChecksMarkdownVerdictClass(t *testing.T) {
	doc := ChecksMarkdown([]Test{
		provTest{id: "GRADED-SHALL", refs: []SpecRef{refOf(Shall)}},
		provTest{id: "GRADED-SHOULD", refs: []SpecRef{refOf(Should)}},
		provTest{id: "DESCRIPTIVE", refs: []SpecRef{refOf(Descriptive)}},
		infoOnly{provTest: provTest{id: "INFO-ONLY", refs: []SpecRef{refOf(Descriptive)}}},
	})
	for _, want := range []string{
		"| [GRADED-SHALL](provenance/GRADED-SHALL.md) | NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Missing IE or IE group |  | FAIL | Rel-15 |",
		"| [GRADED-SHOULD](provenance/GRADED-SHOULD.md) | NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Missing IE or IE group |  | DEVIATION | Rel-15 |",
		"| [DESCRIPTIVE](provenance/DESCRIPTIVE.md) | NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Missing IE or IE group |  | WARN | Rel-15 |",
		"| [INFO-ONLY](provenance/INFO-ONLY.md) | NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Missing IE or IE group |  | INFO | Rel-15 |",
		"**Verdict class** is the worst a check's declared outcomes can say of a core: `FAIL` for a missed shall, `DEVIATION` for a missed should, `WARN` for behaviour no requirement covers that meets a criterion (security, availability or interoperability), `INFO` for behaviour that is only recorded.",
		"- TS 38.413 10.3.5 (descriptive, since Rel-15):",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("checks.md lacks %q:\n%s", want, doc)
		}
	}
}

// observeAlive, the liveness probe of a check with no PASS or FAIL: a core
// that no longer completes a fresh NG Setup did not survive and is CRASH
// with the crash text, which runOne keeps and the gate counts; the same silence under
// load is an ERROR naming the population; a probe that cannot be made, or
// that the per-test timeout cut short, leaves the verdict and says so.
func TestObserveAlive(t *testing.T) {
	ln, err := isctp.ListenSCTP("sctp", &isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}})
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Addr reads the bound address from the kernel and can come back nil
	// straight after the listen.
	var dead string
	for deadline := time.Now().Add(2 * time.Second); dead == "" && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if a := ln.Addr(); a != nil {
			dead = a.String()
		}
	}
	if dead == "" {
		t.Fatal("the listener reports no address")
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close() // no NG Setup answer: the AMF is "dead"
		}
	}()
	g := gnb.Config{ID: 1, Name: "g", MCC: "001", MNC: "01", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1}}}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name         string
		ctx          context.Context
		env          Env
		want         Verdict
		wantObserved string
		wantDetail   string
		wantGate     bool
	}{
		{name: "dead after an INFO", ctx: context.Background(), env: Env{AMFAddr: dead, GNB: g},
			want: Crash, wantObserved: ObservedAMFUnresponsive + " (no answer)",
			wantDetail: "seen; core did not survive the stimulus: the AMF no longer completes a fresh NG Setup", wantGate: true},
		{name: "dead under load", ctx: context.Background(), env: Env{AMFAddr: dead, GNB: g, Load: func() string { return "fleet" }},
			want: Error, wantObserved: ObservedAMFUnresponsiveUnderLoad + " (no answer)",
			wantDetail: "AMF unresponsive while population fleet was active; not attributable to this check", wantGate: true},
		{name: "probe cannot be made", ctx: context.Background(), env: Env{AMFAddr: "127.0.0.1:38412", Bind: "10.0.0.1:0:0", GNB: g},
			want: Info, wantObserved: "no answer", wantDetail: "a fresh NG Setup afterwards could not be tried"},
		{name: "per-test timeout already expired", ctx: expired, env: Env{AMFAddr: dead, GNB: g},
			want: Info, wantObserved: "no answer", wantDetail: "a fresh NG Setup afterwards was not tried"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := infoOnly{provTest: provTest{id: "X", refs: []SpecRef{refOf(Descriptive)}}, run: func(env Env) Result {
				return observeAlive(tc.ctx, env, Result{}.As(infoSeen, "no answer", "seen"), "the stimulus")
			}}
			res := runOne(context.Background(), c, tc.env, 5*time.Second)
			if res.Verdict != tc.want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want %s %q with detail containing %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			if gated := !Summarize([]Result{res}).OK(); gated != tc.wantGate {
				t.Errorf("gated = %v, want %v", gated, tc.wantGate)
			}
		})
	}
}
