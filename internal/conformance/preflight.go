package conformance

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"
)

// A run starts with a preflight (ADR-0011 rule 6): the setup the selected
// checks declare they need, done once, before any check. It decides
// attribution for the whole run. A step that fails is the tool's, the
// testbed's or the configuration's to fix, and the checks that depend on it
// report ERROR without running, except the owner of the step's procedure,
// which runs and judges it. A step that passes has shown that this kind
// of setup works against this core, so the same setup failing later, inside
// a check, is the core's doing and is reported BLOCKED (Result.SetupFailed).

// PreflightStep names a step of the preflight.
type PreflightStep string

const (
	// StepNGSetup is an NG Setup with the run's identity. Every run with a
	// check that signals on N2 has it.
	StepNGSetup PreflightStep = "NG Setup"
	// StepRegistration registers a UE and deregisters it. A run has it when
	// a selected check needs a UE and one is configured.
	StepRegistration PreflightStep = "registration"
	// StepPDUSession establishes a PDU session for that UE. A run has it
	// when a selected check needs one.
	StepPDUSession PreflightStep = "PDU session establishment"
	// StepUPFProbe is a GTP-U Echo to the UPF's N3 endpoint. A run has it
	// when a selected check needs the UPF to answer one.
	StepUPFProbe PreflightStep = "UPF probe"
)

// PreflightStatus is how a preflight step ended.
type PreflightStatus string

const (
	// PreflightOK: the step completed.
	PreflightOK PreflightStatus = "ok"
	// PreflightFailed: the step did not complete; the checks that depend on
	// it report ERROR.
	PreflightFailed PreflightStatus = "failed"
	// PreflightNotRun: an earlier step it builds on failed.
	PreflightNotRun PreflightStatus = "not run"
)

// PreflightResult is one step of a run's preflight, as it ended.
type PreflightResult struct {
	Step   PreflightStep   `json:"step"`
	Status PreflightStatus `json:"status"`
	// Detail says what was done, or why the step failed.
	Detail string `json:"detail,omitempty"`
}

// Preflight is a run's preflight: its steps, in the order they ran. A run
// that selects no check with a need has none.
type Preflight []PreflightResult

// OK reports whether every step completed.
func (p Preflight) OK() bool {
	for _, s := range p {
		if s.Status != PreflightOK {
			return false
		}
	}
	return true
}

// step returns the result of step s, and whether the preflight had it.
func (p Preflight) step(s PreflightStep) (PreflightResult, bool) {
	for _, r := range p {
		if r.Step == s {
			return r, true
		}
	}
	return PreflightResult{}, false
}

// usesN2 reports whether a check with these needs signals on N2: every
// check but one that addresses the UPF's N3 endpoint alone.
func (n CheckNeeds) usesN2() bool { return !slices.Contains(n.Inputs, InputUPFN3) }

// usesPDUSession reports whether t has a PDU session established: it needs
// the procedure or owns it.
func usesPDUSession(t Test) bool {
	return NeedsOf(t).needs(ProcPDUSession) || owns(t, ProcPDUSession)
}

// usesGTPUEcho reports whether t needs the UPF to answer a GTP-U Echo.
func usesGTPUEcho(t Test) bool { return NeedsOf(t).needs(ProcGTPUEcho) }

// stepsFor are the preflight steps t depends on under e: the NG Setup when
// it signals on N2; the registration when it needs a UE and one is
// configured; the PDU session when it also has one established; the UPF
// probe when it needs the UPF to answer Echo and the N3 endpoint is
// configured. A check whose input is missing depends on no step for it: it
// SKIPs.
func stepsFor(t Test, e Env) []PreflightStep {
	n := NeedsOf(t)
	var out []PreflightStep
	if n.usesN2() {
		out = append(out, StepNGSetup)
	}
	if n.UEs > 0 && e.UE.Configured() {
		out = append(out, StepRegistration)
		if usesPDUSession(t) {
			out = append(out, StepPDUSession)
		}
	}
	if usesGTPUEcho(t) && e.UPFN3 != "" && e.N3Bind != "" {
		out = append(out, StepUPFProbe)
	}
	return out
}

// stepProcedure is the procedure each preflight step completes.
var stepProcedure = map[PreflightStep]Procedure{
	StepNGSetup:      ProcNGSetup,
	StepRegistration: ProcRegistration,
	StepPDUSession:   ProcPDUSession,
	StepUPFProbe:     ProcGTPUEcho,
}

// blockedBy returns the first step t depends on that did not complete, and
// whether there is one. A check that lacks an input depends on no step: it
// SKIPs for the input, whatever the preflight found. A check is never held
// to the step for a procedure it owns: it runs and judges that procedure
// itself, so a core that does not complete it gets the owner's verdict,
// not an ERROR.
func (p Preflight) blockedBy(t Test, e Env) (PreflightResult, bool) {
	for _, in := range NeedsOf(t).inputs() {
		if e.missing(in) {
			return PreflightResult{}, false
		}
	}
	for _, s := range stepsFor(t, e) {
		if owns(t, stepProcedure[s]) {
			continue
		}
		if r, ok := p.step(s); ok && r.Status != PreflightOK {
			return r, true
		}
	}
	return PreflightResult{}, false
}

// missing reports whether the run lacks input in, for the inputs a run is
// configured with before it starts; an input that only shows while a check
// runs (InputIntegrity) is never reported missing.
func (e Env) missing(in Input) bool {
	switch in {
	case InputUE:
		return !e.UE.Configured()
	case InputUPFN3:
		return e.UPFN3 == "" || e.N3Bind == ""
	case InputSecondN2:
		return !e.HasSecondSource()
	case InputDownlink:
		return e.Downlink == nil
	case InputTraffic:
		return e.UE.Traffic == ""
	case InputIdleCore:
		return e.loadedBy() != ""
	}
	return false
}

// ObservedPreflightFailed prefixes the ERROR of a check that did not run
// because a preflight step it depends on failed.
const ObservedPreflightFailed = "preflight failed"

// preflightError is the ERROR of a check that depends on step, which did
// not complete.
func preflightError(step PreflightResult) Result {
	what := string(step.Step) + " " + string(step.Status)
	detail := fmt.Sprintf("the run's preflight did not complete the %s this check depends on, so the check was not run and says nothing about the core", step.Step)
	if step.Detail != "" {
		detail += ": " + step.Detail
	}
	return Result{}.Errored(ObservedPreflightFailed+" ("+what+")", detail)
}

// preflightEchoAttempts is how many GTP-U Echo Requests the UPF probe
// sends before it reports the UPF as not answering.
const preflightEchoAttempts = 3

// runPreflight does the steps the selected checks depend on, once, with the
// preflight's own gNB ID and subscriber (env as RunSuite prepared it), each
// bounded by perTest.
func runPreflight(ctx context.Context, env Env, selected []Test, perTest time.Duration) Preflight {
	want := map[PreflightStep]bool{}
	for _, t := range selected {
		for _, s := range stepsFor(t, env) {
			want[s] = true
		}
	}
	var out Preflight
	add := func(s PreflightStep, status PreflightStatus, detail string) {
		out = append(out, PreflightResult{Step: s, Status: status, Detail: detail})
	}
	switch {
	case want[StepRegistration]:
		out = append(out, preflightUE(ctx, env, want[StepPDUSession], perTest)...)
	case want[StepNGSetup]:
		sctx, cancel := context.WithTimeout(ctx, perTest)
		conn, err := env.DialSetup(sctx)
		cancel()
		if err != nil {
			add(StepNGSetup, PreflightFailed, err.Error())
			break
		}
		_ = conn.Close()
		add(StepNGSetup, PreflightOK, fmt.Sprintf("gNB ID %d, PLMN %s/%s, TAC %d", env.GNB.ID, env.GNB.MCC, env.GNB.MNC, env.GNB.TAC))
	}
	if want[StepUPFProbe] {
		sctx, cancel := context.WithTimeout(ctx, perTest)
		status, detail := preflightEcho(sctx, env)
		cancel()
		add(StepUPFProbe, status, detail)
	}
	return out
}

// preflightUE registers the preflight's UE (with a PDU session when
// session is set), deregisters it, and reports the NG Setup, registration
// and PDU session steps from how far the registration got.
func preflightUE(ctx context.Context, env Env, session bool, perTest time.Duration) Preflight {
	steps := []PreflightStep{StepNGSetup, StepRegistration}
	if session {
		steps = append(steps, StepPDUSession)
	}
	// failedAt marks step as failed with detail, the steps before it as
	// completed and the ones after it as not run.
	failedAt := func(step PreflightStep, detail string) Preflight {
		var out Preflight
		status := PreflightOK
		for _, s := range steps {
			if s == step {
				out = append(out, PreflightResult{Step: s, Status: PreflightFailed, Detail: detail})
				status = PreflightNotRun
				continue
			}
			out = append(out, PreflightResult{Step: s, Status: status})
		}
		return out
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return failedAt(StepRegistration, "the UE could not be set up: "+err.Error())
	}
	defer h.Close()
	sctx, cancel := context.WithTimeout(ctx, perTest)
	defer cancel()
	if session {
		_, err = h.Register(sctx)
	} else {
		attach, aerr := h.RegisterWithProbe(sctx, nil)
		err = aerr
		if err == nil && !attach.Registered {
			err = fmt.Errorf("registration on gNB %#x: not registered", env.GNB.ID)
		}
	}
	if err != nil {
		step := map[Procedure]PreflightStep{ProcNGSetup: StepNGSetup, ProcRegistration: StepRegistration, ProcPDUSession: StepPDUSession}[setupProcedure(err)]
		if localFault(err) && step == StepPDUSession {
			step = StepRegistration
		}
		return failedAt(step, err.Error())
	}
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	derr := h.Deregister(dctx)
	dcancel()
	// The registration completed, which is what the step shows; a
	// deregistration that could not be sent is recorded with it.
	registered := "one UE registered and deregistered"
	if derr != nil {
		registered = "one UE registered; its deregistration could not be sent: " + derr.Error()
	}
	out := Preflight{
		{Step: StepNGSetup, Status: PreflightOK, Detail: fmt.Sprintf("gNB ID %d, PLMN %s/%s, TAC %d", env.GNB.ID, env.GNB.MCC, env.GNB.MNC, env.GNB.TAC)},
		{Step: StepRegistration, Status: PreflightOK, Detail: registered},
	}
	if session {
		out = append(out, PreflightResult{Step: StepPDUSession, Status: PreflightOK, Detail: "one PDU session established"})
	}
	return out
}

// preflightEcho sends up to preflightEchoAttempts GTP-U Echo Requests from
// Env.N3Bind to Env.UPFN3 and reports whether the UPF answered one.
func preflightEcho(ctx context.Context, env Env) (PreflightStatus, string) {
	laddr, err := net.ResolveUDPAddr("udp", env.N3Bind)
	if err != nil {
		return PreflightFailed, "bad N3 bind: " + err.Error()
	}
	raddr, err := net.ResolveUDPAddr("udp", env.UPFN3)
	if err != nil {
		return PreflightFailed, "bad UPF N3: " + err.Error()
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return PreflightFailed, "bind N3 socket: " + err.Error()
	}
	defer func() { _ = conn.Close() }()
	for i := 0; i < preflightEchoAttempts; i++ {
		alive, err := gtpuEcho(ctx, conn, raddr, uint16(0x7000+i), echoWait)
		if err != nil {
			return PreflightFailed, "GTP-U Echo Request: " + err.Error()
		}
		if alive {
			return PreflightOK, fmt.Sprintf("%s answered a GTP-U Echo Request", env.UPFN3)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return PreflightFailed, fmt.Sprintf("no GTP-U Echo Response from %s to %d Echo Requests, each waited %s: --upf-n3 must be the UPF's primary N3 address "+
		"(a UPF may answer Echo only there), or the UPF may not support Echo (TS 29.281 7.2)", env.UPFN3, preflightEchoAttempts, echoWait)
}
