package conformance

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"

	"github.com/bgrewell/orbit/internal/gnb"
)

// infoTest is provTest declaring itself informational.
type infoTest struct{ provTest }

func (infoTest) Informational() bool { return true }

// notInfoTest implements Informer and answers false: it is graded.
type notInfoTest struct{ provTest }

func (notInfoTest) Informational() bool { return false }

func refOf(s Strength) SpecRef {
	r := goodRef()
	r.Strength = s
	return r
}

// A check is informational only when it says so.
func TestIsInformational(t *testing.T) {
	tests := []struct {
		name string
		test Test
		want bool
	}{
		{name: "declares it", test: infoTest{provTest{id: "X"}}, want: true},
		{name: "declares false", test: notInfoTest{provTest{id: "X"}}},
		{name: "does not implement Informer", test: provTest{id: "X"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInformational(tc.test); got != tc.want {
				t.Errorf("IsInformational = %v, want %v", got, tc.want)
			}
		})
	}
}

// ValidateTest ties the informational class to descriptive references: a
// descriptive-only check must declare itself informational, and an
// informational check cites no shall and no should, alone or beside
// descriptive text.
func TestValidateInformational(t *testing.T) {
	tests := []struct {
		name    string
		test    Test
		wantErr string
	}{
		{name: "informational, descriptive only", test: infoTest{provTest{id: "OK", refs: []SpecRef{refOf(Descriptive)}}}},
		{name: "informational, two descriptive refs", test: infoTest{provTest{id: "OK", refs: []SpecRef{refOf(Descriptive), refOf(Descriptive)}}}},
		{name: "descriptive only, not informational", test: provTest{id: "X", refs: []SpecRef{refOf(Descriptive)}},
			wantErr: "X: cites no shall or should; descriptive references alone carry no verdict, so the check must declare itself informational"},
		{name: "descriptive only, Informational() false", test: notInfoTest{provTest{id: "X", refs: []SpecRef{refOf(Descriptive)}}},
			wantErr: "must declare itself informational"},
		{name: "informational citing a shall", test: infoTest{provTest{id: "X", refs: []SpecRef{refOf(Shall)}}},
			wantErr: "X: informational, but cites a shall or should"},
		{name: "informational citing a should", test: infoTest{provTest{id: "X", refs: []SpecRef{refOf(Should)}}},
			wantErr: "informational, but cites a shall or should"},
		{name: "informational citing a shall beside descriptive text", test: infoTest{provTest{id: "X", refs: []SpecRef{refOf(Descriptive), refOf(Shall)}}},
			wantErr: "informational, but cites a shall or should"},
		{name: "informational with no refs", test: infoTest{provTest{id: "X"}}, wantErr: "no spec references"},
		{name: "informational with an unverified ref", test: infoTest{provTest{id: "X", refs: []SpecRef{{Spec: "TS 38.413", Clause: "8.7.2.2", Strength: Descriptive, Since: 15, WordingUnchanged: true}}}},
			wantErr: "no verified version"},
		{name: "graded check with Informational() false", test: notInfoTest{provTest{id: "OK", refs: []SpecRef{refOf(Shall)}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTest(tc.test)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateTest: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// The class of a check: INFO for an informational one; the worst verdict
// for a graded one.
func TestVerdictClass(t *testing.T) {
	tests := []struct {
		name      string
		test      Test
		wantClass Verdict
		wantWorst Verdict
	}{
		{name: "informational", test: infoTest{provTest{id: "X", refs: []SpecRef{refOf(Descriptive)}}}, wantClass: Info, wantWorst: Info},
		{name: "shall", test: provTest{id: "X", refs: []SpecRef{refOf(Shall)}}, wantClass: Fail, wantWorst: Fail},
		{name: "should only", test: provTest{id: "X", refs: []SpecRef{refOf(Should)}}, wantClass: Deviation, wantWorst: Deviation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if c, w := VerdictClass(tc.test), WorstVerdict(tc.test); c != tc.wantClass || w != tc.wantWorst {
				t.Errorf("class %s worst %s, want %s and %s", c, w, tc.wantClass, tc.wantWorst)
			}
		})
	}
}

// runOne never lets an informational check grade a core: a PASS, FAIL or
// DEVIATION that Run returns is reported as INFO, every other verdict keeps
// its meaning, and the detail says the check is informational. A graded
// check that returns INFO is an ERROR.
func TestRunOneInformational(t *testing.T) {
	desc, shall := refOf(Descriptive), refOf(Shall)
	tests := []struct {
		name          string
		informational bool
		refs          []SpecRef
		run           Verdict
		observed      string
		want          Verdict
		wantDetail    string
	}{
		{name: "INFO", informational: true, refs: []SpecRef{desc}, run: Info, want: Info},
		{name: "PASS is reported as INFO", informational: true, refs: []SpecRef{desc}, run: Pass, want: Info},
		{name: "FAIL is reported as INFO", informational: true, refs: []SpecRef{desc}, run: Fail, want: Info},
		{name: "DEVIATION is reported as INFO", informational: true, refs: []SpecRef{desc}, run: Deviation, want: Info},
		{name: "a crash stays CRASH", informational: true, refs: []SpecRef{desc}, run: Crash,
			observed: ObservedAMFUnresponsive + " (x)", want: Crash},
		{name: "a dead UPF stays CRASH", informational: true, refs: []SpecRef{desc}, run: Crash,
			observed: ObservedUPFUnresponsive, want: Crash},
		{name: "WARN stays WARN", informational: true, refs: []SpecRef{desc}, run: Warn, want: Warn},
		{name: "BLOCKED stays BLOCKED", informational: true, refs: []SpecRef{desc}, run: Blocked, want: Blocked},
		{name: "ERROR stays ERROR", informational: true, refs: []SpecRef{desc}, run: Error, want: Error},
		{name: "SKIP stays SKIP", informational: true, refs: []SpecRef{desc}, run: Skip, want: Skip},
		{name: "INFO from a shall check is an ERROR", refs: []SpecRef{shall}, run: Info, want: Error,
			wantDetail: "scored ERROR: INFO is reserved for an informational check"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := provTest{id: "X", refs: tc.refs, result: Result{Verdict: tc.run, Observed: tc.observed, Detail: "seen"}}
			var c Test = p
			if tc.informational {
				c = infoTest{p}
			}
			res := runOne(context.Background(), c, Env{}, time.Second)
			if res.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s", res.Verdict, tc.want)
			}
			if tc.wantDetail != "" && !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("detail %q lacks %q", res.Detail, tc.wantDetail)
			}
			if got := strings.Contains(res.Detail, informationalNote); got != tc.informational {
				t.Errorf("detail %q: informational note present = %v, want %v", res.Detail, got, tc.informational)
			}
			if want := "requires Rel-15 or later; " + RenderRefs(tc.refs); !strings.HasSuffix(res.Detail, want) {
				t.Errorf("detail %q does not end with the requirement line %q", res.Detail, want)
			}
			if tc.observed != "" && res.Observed != tc.observed {
				t.Errorf("observed = %q, want it kept as %q", res.Observed, tc.observed)
			}
		})
	}
}

// An informational check SKIPs on the target release like any other, on its
// descriptive references' Since, and says it is informational.
func TestRunOneInformationalSkipsOnRelease(t *testing.T) {
	ref17 := SpecRef{Spec: "TS 38.413", Clause: "8.4.5.2", Strength: Descriptive, Since: 17, WordingUnchanged: true}
	ran := false
	c := infoTest{provTest{id: "X", refs: []SpecRef{ref17}, ran: &ran, result: Result{Verdict: Info}}}
	res := runOne(context.Background(), c, Env{TargetRelease: 16}, time.Second)
	if ran || res.Verdict != Skip || !strings.Contains(res.Detail, informationalNote) {
		t.Errorf("ran=%v verdict=%s detail=%q, want a SKIP that says the check is informational", ran, res.Verdict, res.Detail)
	}
	if res := runOne(context.Background(), c, Env{TargetRelease: 17}, time.Second); !ran || res.Verdict != Info {
		t.Errorf("at the Since release: ran=%v verdict=%s, want INFO", ran, res.Verdict)
	}
}

// INFO is counted on its own and never gates; an ERROR from an
// informational check gates as any ERROR does, a FAIL beside informational
// results still gates, and so does an informational check's own CRASH for a
// core that did not survive.
func TestInformationalDoesNotGate(t *testing.T) {
	desc := []SpecRef{refOf(Descriptive)}
	info := func(id string, v Verdict) Test {
		return infoTest{provTest{id: id, refs: desc, result: Result{Verdict: v}}}
	}
	crashed := infoTest{provTest{id: "D", refs: desc, result: Result{Verdict: Crash, Observed: ObservedAMFUnresponsive + " (association reset)"}}}
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
		{name: "a FAIL returned by an informational check", tests: []Test{info("A", Fail), fakeTest{"B", Procedural, Pass, ""}},
			wantInfo: 1, wantPass: 1, wantOK: true},
		{name: "a PASS returned by an informational check is not a pass", tests: []Test{info("A", Pass)}, wantInfo: 1, wantOK: true},
		{name: "beside a graded FAIL", tests: []Test{info("A", Info), fakeTest{"B", NegativeIE, Fail, ""}},
			wantInfo: 1, wantFail: 1},
		{name: "an ERROR from an informational check gates", tests: []Test{info("A", Error)}, wantError: 1},
		{name: "a core that did not survive an informational check gates", tests: []Test{info("A", Info), crashed},
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
			if sum.OK() != tc.wantOK {
				t.Errorf("OK = %v, want %v", sum.OK(), tc.wantOK)
			}
		})
	}
}

// The JSON summary carries the INFO count and each result's verdict as the
// string INFO.
func TestSummaryJSONInformational(t *testing.T) {
	desc := []SpecRef{refOf(Descriptive)}
	r := &Registry{}
	r.Register(infoTest{provTest{id: "A", refs: desc, result: Result{Verdict: Info, Expected: "an acknowledge", Observed: "an acknowledge"}}})
	r.Register(infoTest{provTest{id: "B", refs: desc, result: Result{Verdict: Info, Expected: "an acknowledge", Observed: "no answer"}}})
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
			!strings.Contains(g.Detail, informationalNote) || len(g.SpecRefs) != 1 || g.SpecRefs[0].Strength != "descriptive" {
			t.Errorf("result %d = %+v, want %+v", i, g, w)
		}
	}
	for _, key := range []string{`"Info":2`, `"Warn":0`, `"Blocked":0`, `"Crash":0`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("summary JSON lacks %s: %s", key, b)
		}
	}
}

// The expected-failures list cannot name an informational check, and no
// entry ever marks an informational verdict or reports it stale by
// matching.
func TestExpectedFailuresRefuseInformational(t *testing.T) {
	tests := []Test{
		provTest{id: "GRADED", refs: []SpecRef{refOf(Shall)}},
		infoTest{provTest{id: "INFORMATIONAL", refs: []SpecRef{refOf(Descriptive)}}},
	}
	known := listableChecks(tests)
	if !known["GRADED"] || known["INFORMATIONAL"] || len(known) != 2 {
		t.Fatalf("listableChecks = %v", known)
	}
	entry := func(check string) []byte {
		return []byte("core: c\nversion: v\nentries:\n  - check: " + check + "\n    observed: o\n    finding: EX-1\n    added: 2026-11-10\n    reason: r\n")
	}
	cases := []struct {
		name, check, wantErr string
	}{
		{name: "graded", check: "GRADED"},
		{name: "informational", check: "INFORMATIONAL", wantErr: `f.yaml: entry 1: check "INFORMATIONAL" is informational and never FAILs; it cannot be listed`},
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
	for _, v := range []Verdict{Info, Info} {
		results := []Result{{ID: "A", Verdict: v, Observed: "o"}}
		x.Apply(results)
		sum := Summarize(results)
		if results[0].ExpectedFailure != nil || sum.ExpectedFail != 0 || !sum.OK() {
			t.Errorf("%s with an entry's observed text: marked %v, summary %+v", v, results[0].ExpectedFailure != nil, sum)
		}
	}
}

// checks.md names an informational check's class INFO and explains the
// class; a graded check keeps its worst verdict.
func TestChecksMarkdownVerdictClass(t *testing.T) {
	doc := ChecksMarkdown([]Test{
		provTest{id: "GRADED-SHALL", refs: []SpecRef{refOf(Shall)}},
		provTest{id: "GRADED-SHOULD", refs: []SpecRef{refOf(Should)}},
		infoTest{provTest{id: "INFORMATIONAL", refs: []SpecRef{refOf(Descriptive)}}},
	})
	for _, want := range []string{
		"| [GRADED-SHALL](provenance/GRADED-SHALL.md) | procedural | FAIL | Rel-15 |",
		"| [GRADED-SHOULD](provenance/GRADED-SHOULD.md) | procedural | DEVIATION | Rel-15 |",
		"| [INFORMATIONAL](provenance/INFORMATIONAL.md) | procedural | INFO | Rel-15 |",
		"A check of class `INFO` is informational: it cites descriptive text only, reports `INFO` with what the core did, and does not gate. In every class a core that does not survive a check's stimulus is `CRASH` and gates.",
		"- TS 38.413 10.3.5 (descriptive, since Rel-15):",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("checks.md lacks %q:\n%s", want, doc)
		}
	}
}

// observeTest is an informational check whose Run is fn.
type observeTest struct {
	infoTest
	fn func(Env) Result
}

func (o observeTest) Run(_ context.Context, env Env) Result { return o.fn(env) }

// observeAlive, the liveness probe of an informational check: a core that
// no longer completes a fresh NG Setup did not survive and is FAIL with the
// crash text, which runOne keeps and the gate counts; the same silence under
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
		in           Verdict
		want         Verdict
		wantObserved string
		wantDetail   string
		wantGate     bool
	}{
		{name: "dead after an INFO-DIFF", ctx: context.Background(), env: Env{AMFAddr: dead, GNB: g}, in: Info,
			want: Crash, wantObserved: ObservedAMFUnresponsive + " (no answer)",
			wantDetail: "seen; core did not survive the stimulus: the AMF no longer completes a fresh NG Setup", wantGate: true},
		{name: "dead after an INFO", ctx: context.Background(), env: Env{AMFAddr: dead, GNB: g}, in: Info,
			want: Crash, wantObserved: ObservedAMFUnresponsive + " (no answer)", wantDetail: "core did not survive the stimulus", wantGate: true},
		{name: "dead under load", ctx: context.Background(), env: Env{AMFAddr: dead, GNB: g, Load: func() string { return "fleet" }}, in: Info,
			want: Error, wantObserved: ObservedAMFUnresponsiveUnderLoad + " (no answer)",
			wantDetail: "AMF unresponsive while population fleet was active; not attributable to this check", wantGate: true},
		{name: "probe cannot be made", ctx: context.Background(), env: Env{AMFAddr: "127.0.0.1:38412", Bind: "10.0.0.1:0:0", GNB: g}, in: Info,
			want: Info, wantObserved: "no answer", wantDetail: "a fresh NG Setup afterwards could not be tried"},
		{name: "per-test timeout already expired", ctx: expired, env: Env{AMFAddr: dead, GNB: g}, in: Info,
			want: Info, wantObserved: "no answer", wantDetail: "a fresh NG Setup afterwards was not tried"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := observeTest{infoTest{provTest{id: "X", refs: []SpecRef{refOf(Descriptive)}}}, func(env Env) Result {
				return observeAlive(tc.ctx, env, Result{Verdict: tc.in, Observed: "no answer", Detail: "seen"}, "the stimulus")
			}}
			res := runOne(context.Background(), c, tc.env, 5*time.Second)
			if res.Verdict != tc.want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want %s %q with detail containing %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			if !strings.Contains(res.Detail, informationalNote) {
				t.Errorf("detail %q lacks the informational note", res.Detail)
			}
			if gated := !Summarize([]Result{res}).OK(); gated != tc.wantGate {
				t.Errorf("gated = %v, want %v", gated, tc.wantGate)
			}
		})
	}
}
