package conformance

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// needTest is a check with the given needs whose Run reports a fixed PASS
// and counts its runs.
type needTest struct {
	id    string
	needs CheckNeeds
	owned []Procedure
	runs  *int
}

func (n needTest) ID() string          { return n.id }
func (n needTest) SpecRefs() []SpecRef { return []SpecRef{declShall} }
func (n needTest) Needs() CheckNeeds   { return n.needs }
func (n needTest) Owns() []Procedure   { return n.owned }
func (n needTest) Outcomes() []Outcome { return []Outcome{needTestMet} }
func (n needTest) Run(context.Context, Env) Result {
	if n.runs != nil {
		*n.runs++
	}
	return Result{}.As(needTestMet, "met", "")
}

var needTestMet = Outcome{ID: "met", Verdict: Pass, Summary: "did it", Refs: []string{declShall.Key()}}

// The steps a check depends on follow its declared needs and what the run
// is configured with: the NG Setup for every check that signals on N2, the
// registration only with a UE configured, the PDU session only for a check
// that has one established, the UPF probe only for one that needs Echo
// answered and with the N3 endpoint configured.
func TestStepsFor(t *testing.T) {
	ue := UESpec{SUPI: "208930100000011"}
	tests := []struct {
		name  string
		needs CheckNeeds
		owned []Procedure
		env   Env
		want  []PreflightStep
	}{
		{name: "an NG association only", needs: defaultNeeds, env: Env{UE: ue}, want: []PreflightStep{StepNGSetup}},
		{name: "the raw NG Setup checks", needs: rawNeeds, want: []PreflightStep{StepNGSetup}},
		{name: "a UE check with no UE configured", needs: ueNeeds, want: []PreflightStep{StepNGSetup}},
		{name: "a UE check", needs: ueNeeds, env: Env{UE: ue}, want: []PreflightStep{StepNGSetup, StepRegistration}},
		{name: "a UE check with a PDU session", needs: ueSessionNeeds, env: Env{UE: ue}, want: []PreflightStep{StepNGSetup, StepRegistration, StepPDUSession}},
		{name: "the owner of the PDU session", needs: ueNeeds, owned: []Procedure{ProcPDUSession}, env: Env{UE: ue},
			want: []PreflightStep{StepNGSetup, StepRegistration, StepPDUSession}},
		{name: "a GTP-U check that needs Echo", needs: NeedsOf(gtpuUnknownTEID{}), env: Env{UPFN3: "192.0.2.1:2152", N3Bind: "192.0.2.2:2152"},
			want: []PreflightStep{StepUPFProbe}},
		{name: "a GTP-U check that needs Echo, no N3 endpoint", needs: NeedsOf(gtpuUnknownTEID{})},
		{name: "the owner of GTP-U Echo", needs: NeedsOf(gtpuEchoResponse{}), owned: []Procedure{ProcGTPUEcho}, env: Env{UPFN3: "192.0.2.1:2152", N3Bind: "192.0.2.2:2152"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stepsFor(needTest{id: "X", needs: tc.needs, owned: tc.owned}, tc.env)
			if len(got) != len(tc.want) {
				t.Fatalf("steps %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("steps %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The preflight against the mock core: only the steps the selected checks
// need are done, each reports how it ended, a step after a failed one is
// not run, and nothing stays registered or open.
func TestRunPreflight(t *testing.T) {
	ngOnly := []Test{needTest{id: "N", needs: defaultNeeds}}
	withUE := []Test{needTest{id: "N", needs: defaultNeeds}, needTest{id: "U", needs: ueNeeds}}
	withSession := []Test{needTest{id: "U", needs: ueNeeds}, needTest{id: "S", needs: ueSessionNeeds}}
	echo := []Test{needTest{id: "E", needs: NeedsOf(gtpuUnknownTEID{})}}
	type want struct {
		step   PreflightStep
		status PreflightStatus
		detail string
	}
	tests := []struct {
		name     string
		net24    int
		selected []Test
		ki       string
		upf      mockupf.Options
		noGUTI   bool
		amfOpts  []mockamf.Option
		mut      func(*Env, *mockCore)
		want     []want
		ok       bool
		regs     int64
	}{
		{name: "NG Setup alone", net24: testnet.ConformancePreflightUE, selected: ngOnly, ki: testKi,
			want: []want{{StepNGSetup, PreflightOK, "gNB ID 256, PLMN 208/93, TAC 1"}}, ok: true},
		{name: "NG Setup alone with a UE configured and no UE check selected", net24: testnet.ConformancePreflightUE, selected: ngOnly, ki: testKi,
			want: []want{{StepNGSetup, PreflightOK, ""}}, ok: true},
		{name: "NG Setup rejected", net24: testnet.ConformancePreflightUE, selected: ngOnly, ki: testKi,
			mut:  func(e *Env, _ *mockCore) { e.GNB.MCC, e.GNB.MNC = "001", "01" },
			want: []want{{StepNGSetup, PreflightFailed, "NG Setup rejected"}}},
		{name: "no UE configured", net24: testnet.ConformancePreflightUE, selected: withUE, ki: testKi,
			mut:  func(e *Env, _ *mockCore) { e.UE = UESpec{} },
			want: []want{{StepNGSetup, PreflightOK, ""}}, ok: true},
		{name: "registers and deregisters", net24: testnet.ConformancePreflightUE, selected: withUE, ki: testKi,
			want: []want{{StepNGSetup, PreflightOK, ""}, {StepRegistration, PreflightOK, "one UE registered and deregistered"}}, ok: true, regs: 1},
		{name: "registers a UE it cannot deregister", net24: testnet.ConformancePreflightNoSession, selected: withUE, ki: testKi, noGUTI: true,
			want: []want{{StepNGSetup, PreflightOK, ""}, {StepRegistration, PreflightOK, "one UE registered; its deregistration could not be sent: no 5G-GUTI"}}, ok: true},
		{name: "establishes a PDU session", net24: testnet.ConformancePreflightSession, selected: withSession, ki: testKi,
			want: []want{{StepNGSetup, PreflightOK, ""}, {StepRegistration, PreflightOK, ""}, {StepPDUSession, PreflightOK, "one PDU session established"}}, ok: true, regs: 1},
		{name: "PDU session not answered", net24: testnet.ConformancePreflightNoPDU, selected: withSession, ki: testKi,
			amfOpts: []mockamf.Option{mockamf.WithPDUSessionAnswers(0)},
			want:    []want{{StepNGSetup, PreflightOK, ""}, {StepRegistration, PreflightOK, ""}, {StepPDUSession, PreflightFailed, "deadline exceeded"}}, regs: 1},
		{name: "registration refused", net24: testnet.ConformancePreflightRefused, selected: withSession, ki: "000102030405060708090a0b0c0d0e0f",
			want: []want{{StepNGSetup, PreflightOK, ""}, {StepRegistration, PreflightFailed, "registration on gNB 0x100"}, {StepPDUSession, PreflightNotRun, ""}}},
		{name: "NG Setup rejected, with a UE", net24: testnet.ConformancePreflightRefused, selected: withUE, ki: testKi,
			mut:  func(e *Env, _ *mockCore) { e.GNB.MCC, e.GNB.MNC, e.UE.SUPI = "001", "01", "001010100000011" },
			want: []want{{StepNGSetup, PreflightFailed, "NG Setup rejected"}, {StepRegistration, PreflightNotRun, ""}}},
		{name: "UPF answers Echo", net24: testnet.ConformancePreflightEcho, selected: echo, ki: testKi,
			mut: func(e *Env, c *mockCore) {
				e.UPFN3, e.N3Bind = c.upf.Addr().String(), net.JoinHostPort(testnet.Addr(testnet.ConformancePreflightEcho, 12), "0")
			},
			want: []want{{StepUPFProbe, PreflightOK, "answered a GTP-U Echo Request"}}, ok: true},
		{name: "UPF does not answer Echo", net24: testnet.ConformancePreflightNoEcho, selected: echo, ki: testKi, upf: mockupf.Options{NoEcho: true},
			mut: func(e *Env, c *mockCore) {
				e.UPFN3, e.N3Bind = c.upf.Addr().String(), net.JoinHostPort(testnet.Addr(testnet.ConformancePreflightNoEcho, 12), "0")
			},
			want: []want{{StepUPFProbe, PreflightFailed, "no GTP-U Echo Response from"}}},
		{name: "no N3 endpoint configured", net24: testnet.ConformancePreflightEcho, selected: echo, ki: testKi, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]mockamf.Option(nil), tc.amfOpts...)
			if !tc.noGUTI {
				opts = append(opts, mockamf.WithGUTI())
			}
			c := newMockCore(t, tc.net24, tc.upf, tc.ki, opts...)
			env := ueEnv(c.front.Addr, "", c.n3)
			if tc.mut != nil {
				tc.mut(&env, c)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			got := runPreflight(ctx, env, tc.selected, 5*time.Second)
			if len(got) != len(tc.want) {
				t.Fatalf("preflight %+v, want %d steps", got, len(tc.want))
			}
			for i, w := range tc.want {
				if got[i].Step != w.step || got[i].Status != w.status || !strings.Contains(got[i].Detail, w.detail) {
					t.Errorf("step %d = %+v, want %s %s with detail containing %q", i, got[i], w.step, w.status, w.detail)
				}
				if strings.Contains(got[i].Detail, testKi) || strings.Contains(got[i].Detail, testOPc) {
					t.Error("a step's detail carries key material")
				}
			}
			if got.OK() != tc.ok {
				t.Errorf("OK = %v, want %v", got.OK(), tc.ok)
			}
			deadline := time.Now().Add(3 * time.Second)
			for (c.front.Active() != 0 || c.amf.Deregistrations() < tc.regs) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := c.amf.Deregistrations(); n != tc.regs {
				t.Errorf("mock AMF saw %d deregistrations, want %d", n, tc.regs)
			}
			if n := c.front.Active(); n != 0 {
				t.Errorf("%d associations still open after the preflight", n)
			}
		})
	}
}

// A run: the preflight comes first and is reported once; a check that
// depends on a step that did not complete reports ERROR without running,
// the rest are judged; a check whose input is missing still SKIPs; the
// preflight presents its own gNB ID.
func TestRunSuitePreflight(t *testing.T) {
	type want struct {
		verdict Verdict
		ran     bool
		detail  string
	}
	tests := []struct {
		name      string
		net24     int
		ki        string
		noUE      bool
		amfOpts   []mockamf.Option
		wantSteps string
		want      map[string]want
	}{
		{name: "passes", net24: testnet.ConformancePreflightSuite, ki: testKi,
			wantSteps: "NG Setup=ok registration=ok PDU session establishment=ok",
			want:      map[string]want{"N": {Pass, true, ""}, "U": {Pass, true, ""}, "S": {Pass, true, ""}, "G": {Pass, true, ""}}},
		{name: "registration refused", net24: testnet.ConformancePreflightSuiteFail, ki: "000102030405060708090a0b0c0d0e0f",
			wantSteps: "NG Setup=ok registration=failed PDU session establishment=not run",
			want: map[string]want{"N": {Pass, true, ""}, "G": {Pass, true, ""},
				"U": {Error, false, "the run's preflight did not complete the registration this check depends on, so the check was not run and says nothing about the core: registration on gNB"},
				"S": {Error, false, "preflight did not complete the registration"}}},
		{name: "PDU session not answered", net24: testnet.ConformancePreflightSuiteNoPDU, ki: testKi, amfOpts: []mockamf.Option{mockamf.WithPDUSessionAnswers(0)},
			wantSteps: "NG Setup=ok registration=ok PDU session establishment=failed",
			want: map[string]want{"N": {Pass, true, ""}, "U": {Pass, true, ""}, "G": {Pass, true, ""},
				"S": {Error, false, "preflight did not complete the PDU session establishment"}}},
		{name: "no UE configured", net24: testnet.ConformancePreflightSuite, ki: testKi, noUE: true,
			wantSteps: "NG Setup=ok",
			want:      map[string]want{"N": {Pass, true, ""}, "U": {Pass, true, ""}, "S": {Pass, true, ""}, "G": {Pass, true, ""}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, mockupf.Options{}, tc.ki, append([]mockamf.Option{mockamf.WithGUTI()}, tc.amfOpts...)...)
			env := ueEnv(c.front.Addr, "", c.n3)
			if tc.noUE {
				env.UE = UESpec{}
			}
			runs := map[string]*int{"N": new(int), "U": new(int), "S": new(int), "G": new(int)}
			reg := &Registry{}
			reg.Register(needTest{id: "N", needs: defaultNeeds, runs: runs["N"]})
			reg.Register(needTest{id: "U", needs: ueNeeds, runs: runs["U"]})
			reg.Register(needTest{id: "S", needs: ueSessionNeeds, runs: runs["S"]})
			reg.Register(needTest{id: "G", needs: NeedsOf(gtpuEchoResponse{}), runs: runs["G"]})
			var (
				mu        sync.Mutex
				reported  []Preflight
				results   = map[string]Result{}
				preBefore = true
			)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			pre := reg.RunSuite(ctx, env, 5*time.Second, Hooks{
				Preflight: func(p Preflight) {
					mu.Lock()
					defer mu.Unlock()
					reported = append(reported, p)
					preBefore = preBefore && len(results) == 0
				},
				Result: func(r Result, _, _ int) {
					mu.Lock()
					defer mu.Unlock()
					results[r.ID] = r
				}})
			var steps []string
			for _, s := range pre {
				steps = append(steps, string(s.Step)+"="+string(s.Status))
			}
			if got := strings.Join(steps, " "); got != tc.wantSteps {
				t.Errorf("preflight %q, want %q", got, tc.wantSteps)
			}
			if len(reported) != 1 || len(reported[0]) != len(pre) || !preBefore {
				t.Errorf("the preflight hook saw %d reports (before any result: %v), want the one preflight first", len(reported), preBefore)
			}
			for id, w := range tc.want {
				r := results[id]
				if r.Verdict != w.verdict || (*runs[id] == 1) != w.ran || !strings.Contains(r.Detail, w.detail) {
					t.Errorf("%s = %s %q (%s), ran %d times; want %s, ran %v, detail containing %q", id, r.Verdict, r.Observed, r.Detail, *runs[id], w.verdict, w.ran, w.detail)
				}
				if w.verdict == Error && (r.Outcome != OutcomeError || !strings.HasPrefix(r.Observed, ObservedPreflightFailed+" (")) {
					t.Errorf("%s: outcome %q, observed %q; want the preflight ERROR naming the step", id, r.Outcome, r.Observed)
				}
			}
			// The preflight's gNB ID is none of the checks'.
			if id := reg.preflightGNBID(env.GNB.ID, false); id != env.GNB.ID+legacyLivenessOffset+4 {
				t.Errorf("preflight gNB ID %#x", id)
			}
		})
	}
}

// A check that lacks an input SKIPs for it, whatever the preflight found:
// the preflight's failure is reported only by checks that would have run.
func TestPreflightErrorYieldsToMissingInput(t *testing.T) {
	failed := Preflight{{Step: StepNGSetup, Status: PreflightFailed, Detail: "NG Setup rejected: misc/5"}}
	if _, blocked := failed.blockedBy(needTest{id: "U", needs: ueNeeds}, Env{}); blocked {
		t.Error("a UE check with no UE configured is held to the preflight; it SKIPs")
	}
	step, blocked := failed.blockedBy(needTest{id: "U", needs: ueNeeds}, Env{UE: UESpec{SUPI: "208930100000011"}})
	if !blocked || step.Step != StepNGSetup {
		t.Errorf("a UE check with a UE: blocked %v by %q, want the NG Setup", blocked, step.Step)
	}
	if _, blocked := failed.blockedBy(needTest{id: "G", needs: NeedsOf(gtpuEchoResponse{})}, Env{UPFN3: "192.0.2.1:2152", N3Bind: "192.0.2.2:2152"}); blocked {
		t.Error("a GTP-U check is held to the NG Setup step")
	}
	r := preflightError(failed[0])
	if r.Verdict != Error || r.Observed != "preflight failed (NG Setup failed)" || !strings.Contains(r.Detail, "NG Setup rejected: misc/5") {
		t.Errorf("preflight ERROR = %s %q (%s)", r.Verdict, r.Observed, r.Detail)
	}
	if Preflight(nil).OK() != true || failed.OK() {
		t.Error("OK: an empty preflight is one that completed; a failed step is not")
	}
}

// The preflight has its own gNB ID and subscriber in both layouts, leased
// with the suite's.
func TestPreflightIdentity(t *testing.T) {
	r := NewRegistry()
	n := uint32(len(r.Tests()))
	if got := r.preflightGNBID(700, true); got != 700+3*n || r.PackedSpan() != int(3*n)+1 {
		t.Errorf("packed: preflight ID %d, span %d; want %d and %d", got, r.PackedSpan(), 700+3*n, 3*n+1)
	}
	if got := r.preflightGNBID(700, false); got != 700+0x9000+n {
		t.Errorf("legacy: preflight ID %d", got)
	}
	for _, packed := range []bool{false, true} {
		ids := r.Needs(700, packed).GNBIDs
		found := false
		for _, id := range ids {
			found = found || id == r.preflightGNBID(700, packed)
		}
		if !found {
			t.Errorf("packed=%v: the suite's gNB IDs lack the preflight's", packed)
		}
	}
	off, span := r.preflightSubscriber()
	if span != 1 || r.SubscriberSpan() != off+1 {
		t.Errorf("preflight subscriber at %d+%d, span %d", off, span, r.SubscriberSpan())
	}
	for i := range r.Tests() {
		if o, s := r.subscriberBlock(i); s > 0 && o+s > off {
			t.Errorf("check %d's block %d+%d overlaps the preflight's subscriber %d", i, o, s, off)
		}
	}
}

// regAgain is a check that registers the run's UE again with a key the core
// does not hold, and reports its setup's failure the one way every check
// does (Result.SetupFailed).
type regAgain struct{}

func (regAgain) ID() string          { return "R" }
func (regAgain) SpecRefs() []SpecRef { return []SpecRef{declShall} }
func (regAgain) Needs() CheckNeeds   { return ueNeeds }
func (regAgain) Outcomes() []Outcome { return []Outcome{needTestMet} }
func (regAgain) Run(ctx context.Context, e Env) Result {
	e.UE.Ki = "000102030405060708090a0b0c0d0e0f"
	h, err := NewUEHarness(e, nil)
	if err != nil {
		return Result{}.Errored("UE harness", err.Error())
	}
	defer h.Close()
	if _, err := h.RegisterWithProbe(ctx, nil); err != nil {
		return Result{}.SetupFailed("UE registration failed", err)
	}
	return Result{}.As(needTestMet, "met", "")
}

// After a preflight that registered a UE, the same setup failing inside a
// check is the core's: BLOCKED on the registration, naming its owner.
func TestSetupFailsAfterPassedPreflight(t *testing.T) {
	c := newMockCore(t, testnet.ConformancePreflightBlocked, mockupf.Options{}, testKi, mockamf.WithGUTI())
	env := ueEnv(c.front.Addr, "", c.n3)
	reg := &Registry{}
	reg.Register(needTest{id: "O", needs: defaultNeeds, owned: []Procedure{ProcRegistration}})
	reg.Register(regAgain{})
	results := map[string]Result{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pre := reg.RunSuite(ctx, env, 5*time.Second, Hooks{Result: func(r Result, _, _ int) { results[r.ID] = r }})
	if !pre.OK() || len(pre) != 2 {
		t.Fatalf("preflight %+v, want the NG Setup and registration completed", pre)
	}
	r := results["R"]
	if r.Verdict != Blocked || r.BlockedOn != ProcRegistration || r.Owner != "O" || !strings.Contains(r.Detail, "O owns whether it completes") {
		t.Errorf("R = %s on %q owner %q (%s %q), want BLOCKED on the registration naming O", r.Verdict, r.BlockedOn, r.Owner, r.Observed, r.Detail)
	}
}
