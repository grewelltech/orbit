// Package conformance is ORBIT's core conformance / regression harness. Each
// test drives a specific procedure or a deliberately malformed message at a
// live core and asserts a spec-cited outcome — framed as graceful-rejection /
// regression assertions (does the core reject cleanly rather than crash?),
// not novel bug-hunting. Results are structured with a TS citation and the
// observed vs expected behaviour, so the suite can run headless in
// integration-CI and emit machine-readable output.
//
// D-11: the decode path uses free5gc's ngap/nas, which has decoded SD-Core's
// emitted NGAP across every ORBIT phase (NG Setup, NAS transport, PDU-session
// setup, handover, path switch). The one known omec/free5gc divergence is
// encode-side and type-specific (docs/interop/sdcore.md, N2 transfer) — not a
// decode incompatibility — so free5gc is adequate here.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Category groups tests for reporting and selective runs.
type Category string

const (
	Procedural Category = "procedural"  // well-formed procedures complete (one check needs Env.UE)
	NegativeIE Category = "negative-ie" // malformed / unexpected messages are rejected gracefully
	Security   Category = "security"    // NAS security-context violations
	GTPU       Category = "gtpu"        // user-plane error handling
	Timing     Category = "timing"      // concurrent-procedure timing (flaky-allowed)
	Mobility   Category = "mobility"    // UE-level procedures over two gNB associations (needs Env.UE)
	NAS        Category = "nas"         // NAS procedures and timers (needs Env.UE)
)

// Result is one test's structured outcome. The registry stamps the identity
// and provenance fields from the test, so a check sets only the verdict and
// what it saw. Detail always ends with the requirement line ("requires
// Rel-15 or later; <refs>"), so every verdict names its clauses and release.
type Result struct {
	ID       string   `json:"id"`
	Category Category `json:"category"`
	// SpecRef is the check's references rendered on one line (RenderRefs),
	// kept for consumers of the original string field.
	SpecRef string `json:"spec_ref"`
	// SpecRefs are the same references, structured.
	SpecRefs []SpecRef `json:"spec_refs"`
	// MinRelease is the oldest release the check applies to.
	MinRelease Release `json:"min_release"`
	Verdict    Verdict `json:"verdict"`
	// Outcome is the ID of the declared outcome the result reports
	// (Outcome.ID).
	Outcome string `json:"outcome,omitempty"`
	// Criterion is why a WARN result is one.
	Criterion Criterion `json:"criterion,omitempty"`
	// Refs are the references the outcome rests on, rendered with their
	// strength: the "shall" a FAIL violates, the "should" a DEVIATION does
	// not follow.
	Refs []string `json:"outcome_refs,omitempty"`
	// BlockedOn and Owner are set on a BLOCKED result: the procedure the
	// core did not complete and the check that owns it.
	BlockedOn Procedure `json:"blocked_on,omitempty"`
	Owner     string    `json:"owner,omitempty"`
	// NotCompleted is set on an owner check's result that reports a
	// procedure it owns as not completing.
	NotCompleted Procedure `json:"not_completed,omitempty"`
	Expected     string    `json:"expected"`
	Observed     string    `json:"observed"`
	Detail       string    `json:"detail,omitempty"`
	// ExpectedFailure is set on a FAIL the run's expected-failures list
	// tolerates (ExpectedFailures.Apply); the verdict stays FAIL. It is
	// unrelated to Expected, the behaviour the spec expects.
	ExpectedFailure *ExpectedFailure `json:"expected_failure,omitempty"`
}

// Test is a single conformance / regression check.
type Test interface {
	ID() string
	Category() Category
	// SpecRefs are the clauses the check asserts, with their release
	// provenance; ValidateTest states the rules they must meet.
	SpecRefs() []SpecRef
	// Run drives the check against env and returns its verdict.
	Run(ctx context.Context, env Env) Result
}

// Budgeter is implemented by a check that needs longer than the per-test
// timeout, such as one that waits out a NAS timer several times. runOne
// gives it max(per-test timeout, Budget()) and says so in Detail when that
// raised the timeout; --per-test-timeout stays the floor for every check.
type Budgeter interface{ Budget() time.Duration }

// DefaultPerTestTimeout is the per-check timeout when the caller sets none
// (RunStream) and the CLI's --per-test-timeout default.
const DefaultPerTestTimeout = 15 * time.Second

// ExpectedDuration is the longest t can take at the per-test timeout
// perTest: perTest, or its Budget when longer. It is what a check is
// announced with before it runs, and what --list and checks.md show.
func ExpectedDuration(t Test, perTest time.Duration) time.Duration {
	if perTest <= 0 {
		perTest = DefaultPerTestTimeout
	}
	d, _ := budget(t, perTest)
	return d
}

// budget is the time runOne gives t: perTest, or t's Budget when longer, with
// whether it was raised.
func budget(t Test, perTest time.Duration) (time.Duration, bool) {
	if b, ok := t.(Budgeter); ok && b.Budget() > perTest {
		return b.Budget(), true
	}
	return perTest, false
}

// Env is what a test needs to reach the core: the AMF N2 endpoint and a gNB
// identity to present. Tests dial their own association(s) via Dial, since
// most need a fresh or deliberately-misused one.
//
// Bind is the local N2 source address every check's association binds, as
// ip or ip:0 (see sctp.NormalizeLocal); empty binds the route-selected source. One
// address suffices because Registry.RunStream runs the checks one after
// another, each presenting its own gNB ID over its own association, so at
// any instant there is one gNB on that address. The port must be 0: a check
// may hold its association open while Alive probes the AMF over a second
// one from the same Bind, and only an ephemeral port gives the two distinct
// ports. ValidateBind enforces this; callers run it before a suite.
//
// SecondBind is an optional second local N2 source address, in Bind's form
// and on another IP, for a check that holds two associations from two
// addresses at once (DialSecond). Empty, such a check SKIPs.
//
// UPFN3/N3Bind are optional and used only by user-plane (GTP-U) checks: UPFN3
// is the UPF's N3 endpoint (host:port) and N3Bind the local N3 source
// (host:port) — both must be set, from a host on the UPF access network, or
// GTP-U checks SKIP. See docs/USAGE.md "Where things must run".
//
// TargetRelease is the 3GPP release the core claims; a check that requires a
// newer release SKIPs instead of running. The zero value (unset) runs every
// check. SpecTargets pins individual specifications ("TS 29.244") to an older
// release than TargetRelease, for a core whose interfaces are built against
// different releases; a pin only narrows (see Env.SpecTarget).
//
// UE is the subscriber the Mobility checks register (see UESpec and
// UEHarness); with no SUPI they SKIP, so a suite without one runs as before.
type Env struct {
	AMFAddr       string
	Bind          string
	SecondBind    string
	GNB           gnb.Config
	UPFN3         string
	N3Bind        string
	TargetRelease Release
	SpecTargets   map[string]Release
	UE            UESpec
	// TargetGNBID and LivenessGNBID are the check's handover target's and
	// liveness probe's gNB IDs, which RunStream assigns with its own ID
	// (GNB.ID): checks take them from here, never computing one.
	TargetGNBID, LivenessGNBID uint32
	// PackedGNBIDs lays a suite's IDs out compactly from the base, for a
	// target that bounds its gNB IDs (see Registry.gnbIDsOf); unset, the
	// legacy layout applies.
	PackedGNBIDs bool
	// Downlink, when set, is something on the N6 side that can send
	// downlink traffic to a UE's address unasked (a responder): the
	// stimulus of the checks that need a CM-IDLE UE paged, and the traffic
	// of the ones that need downlink on a UE's N3 tunnel. Nil, they SKIP.
	Downlink engine.DownlinkTrigger
	// Load, when set, names a population that is putting load on the core
	// right now ("" when none is active): a run with conformance under
	// load (ADR-0008 §2). A liveness miss under load is not attributable
	// to the check, and checks that need an idle core SKIP. It is asked as
	// each check starts and again at a liveness miss, so a population that
	// ends partway through the suite stops counting from the next check.
	Load func() string
	// sharedWith is the earlier check of this run that registered the
	// subscriber this check registers, "" when the check has its own
	// (RunWithStart sets it; see sharedSubscriberNote).
	sharedWith string
	// owners names the registered check that owns each procedure, for a
	// BLOCKED result (RunWithStart sets it).
	owners map[Procedure]string
	// preflight is the run's preflight (RunSuite sets it): a check that
	// depends on a step of it that did not complete reports ERROR.
	preflight Preflight
}

// loadedBy is the population loading the core now, "" for none.
func (e Env) loadedBy() string {
	if e.Load == nil {
		return ""
	}
	return e.Load()
}

// SpecTarget is the release a reference to spec is gated on: the pin for
// spec when it is older than TargetRelease, else TargetRelease. A pin never
// widens a run, and with TargetRelease unset (0) nothing is gated, pins
// included.
func (e Env) SpecTarget(spec string) Release {
	if e.TargetRelease == 0 {
		return 0
	}
	if pin, ok := e.SpecTargets[spec]; ok && pin < e.TargetRelease {
		return pin
	}
	return e.TargetRelease
}

// NarrowingSpecTargets returns the pins that narrow the run, each at its
// effective release: none when TargetRelease is unset, and none for a pin at
// or above TargetRelease. The map is fresh and never nil.
func (e Env) NarrowingSpecTargets() map[string]Release {
	out := map[string]Release{}
	for spec := range e.SpecTargets {
		if t := e.SpecTarget(spec); t != e.TargetRelease {
			out[spec] = t
		}
	}
	return out
}

// Dial opens a raw SCTP association to the AMF (no NG Setup) from Bind,
// bounded by ctx (see sctp.Dial).
func (e Env) Dial(ctx context.Context) (*sctp.Conn, error) {
	return sctp.Dial(ctx, e.Bind, e.AMFAddr)
}

// DialSecond opens a raw SCTP association to the AMF (no NG Setup) from
// SecondBind, bounded by ctx. It is an error when no second address is
// configured: a check asks HasSecondSource first.
func (e Env) DialSecond(ctx context.Context) (*sctp.Conn, error) {
	if e.SecondBind == "" {
		return nil, errors.New("no second N2 source address configured")
	}
	return sctp.Dial(ctx, e.SecondBind, e.AMFAddr)
}

// HasSecondSource reports whether SecondBind names an address on another
// IP than the one Bind's associations leave from (N2Source), so that two
// associations can come from two source addresses.
func (e Env) HasSecondSource() bool {
	if e.SecondBind == "" {
		return false
	}
	return bindHost(e.SecondBind) != bindHost(e.N2Source())
}

// bindHost is the host of a bind address written as host or host:port.
func bindHost(bind string) string {
	n, err := sctp.NormalizeLocal(bind)
	if err != nil {
		return bind
	}
	if host, _, err := net.SplitHostPort(n); err == nil {
		return host
	}
	return n
}

// ValidateBind checks a conformance bind address up front: it must
// normalise (sctp.NormalizeLocal) and must not fix a port, because the suite
// opens several associations per check from the same address. "" is valid.
func ValidateBind(bind string) error {
	n, err := sctp.NormalizeLocal(bind)
	if err != nil {
		return err
	}
	if n == "" {
		return nil
	}
	if _, port, err := net.SplitHostPort(n); err == nil && port != "0" {
		return fmt.Errorf("bind address %q fixes a port, but conformance opens several associations per check; give --bind as ip or ip:0", bind)
	}
	return nil
}

// N2Source is the address the checks' associations are taken from, for
// reporting. With Bind set it is Bind in the normalised ip:port form
// sctp.Dial uses (an unparseable Bind is returned as written, since every
// check will have ERRORed on it and the report should show what was asked
// for). Unbound, it is the route-selected IP for AMFAddr, as a bare IP: each
// check opens its own association on a fresh ephemeral port, so there is no
// one port to report. "" only when the probe fails, and then the first
// check's Dial surfaces the same failure.
func (e Env) N2Source() string {
	if e.Bind == "" {
		ip, err := sctp.SourceFor(e.AMFAddr)
		if err != nil {
			return ""
		}
		return ip.String()
	}
	n, err := sctp.NormalizeLocal(e.Bind)
	if err != nil {
		return e.Bind
	}
	return n
}

// DialSetup opens an association and performs NG Setup, returning it ready for
// UE-associated signalling. The caller closes it. An error names the NG
// Setup as the procedure that did not complete (Result.SetupFailed).
func (e Env) DialSetup(ctx context.Context) (*sctp.Conn, error) {
	conn, err := e.Dial(ctx)
	if err != nil {
		return nil, during(ProcNGSetup, err)
	}
	ng, err := gnb.NGSetup(ctx, conn, e.GNB)
	if err != nil || !ng.Accepted {
		conn.Close()
		if err == nil {
			err = &setupError{"NG Setup rejected: " + ng.Cause}
		}
		return nil, during(ProcNGSetup, err)
	}
	return conn, nil
}

// Alive reports whether the AMF still accepts a fresh NG Setup — a liveness
// probe used to confirm the core survived a malformed message. It uses a gNB
// ID well clear of the per-test IDs so it never collides. This is the
// crash-safety primitive: if a negative test leaves the AMF unable to complete
// a fresh NG Setup, the core did not survive.
//
// A non-nil error means the probe could not show anything about the core's
// survival: the local dial failed, or the AMF answered with NG SETUP FAILURE.
// An AMF that answers is alive; a rejection means the probe's identity
// (--mcc/--mnc/--tac/--sd) is not one the core serves, an operator problem.
// Callers report either as a harness ERROR, never as a FAIL. alive=false
// with a nil error is the core's doing: no answer to the probe.
func (e Env) Alive(ctx context.Context) (alive bool, err error) {
	probe := e.GNB
	probe.ID = e.livenessGNBID()
	probe.Name = "orbit-conf-probe"
	conn, err := e.Dial(ctx)
	if err != nil {
		return false, fmt.Errorf("liveness probe dial: %w", err)
	}
	defer conn.Close()
	ng, err := gnb.NGSetup(ctx, conn, probe)
	if err != nil {
		return false, nil
	}
	if !ng.Accepted {
		return false, fmt.Errorf("liveness probe rejected by the AMF: cause %s; check --mcc/--mnc/--tac/--sd", ng.Cause)
	}
	return true, nil
}

// Observed texts the liveness probes write. They are shared with
// LoadExpectedFailures, which refuses a list entry for any of them: a list
// can never tolerate a dead core or a check that could not confirm it was
// alive.
const (
	// ObservedAMFUnresponsive prefixes a CRASH: the fresh NG Setup no longer
	// completes after the stimulus (confirmAlive).
	ObservedAMFUnresponsive = "AMF unresponsive after the message"
	// ObservedAMFUnresponsiveUnderLoad prefixes the ERROR for the same
	// silence while a population loaded the core.
	ObservedAMFUnresponsiveUnderLoad = "AMF unresponsive under load"
	// ObservedProbeFailed prefixes an ERROR whose liveness probe could not
	// be made (confirmAlive).
	ObservedProbeFailed = "liveness probe could not run"
	// ObservedUPFUnresponsive is a CRASH: the UPF answered GTP-U Echo before
	// the unknown-TEID G-PDU and not after it.
	ObservedUPFUnresponsive = "UPF unresponsive after the G-PDU (no Echo Response)"
	// ObservedUPFNoEcho is an ERROR whose UPF did not answer GTP-U Echo
	// before the test, so its liveness cannot be confirmed.
	ObservedUPFNoEcho = "UPF did not answer a GTP-U Echo Request before the test"
	// ObservedProbeCutShort prefixes an ERROR whose liveness probe came
	// back negative because the per-test timeout expired during it: the
	// harness, not the core, ran out of time.
	ObservedProbeCutShort = "liveness probe cut short by the per-test timeout"
)

// cutShort reports whether ctx's deadline has passed (or ctx is done), so a
// negative liveness probe says nothing about the core.
func cutShort(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	d, ok := ctx.Deadline()
	return ok && !time.Now().Before(d)
}

// cutShortResult turns r into the ERROR for a liveness probe the per-test
// timeout cut short, keeping what was observed before the cut.
func cutShortResult(r Result, needs string) Result {
	return r.Errored(ObservedProbeCutShort+" ("+r.Observed+")",
		"the per-test timeout expired before the liveness probe completed, so the core's state is unknown; "+
			"raise --per-test-timeout (this check needs about "+needs+")")
}

// livenessTexts are the Observed prefixes no expected-failures entry may
// name.
var livenessTexts = []string{ObservedAMFUnresponsive, ObservedAMFUnresponsiveUnderLoad, ObservedProbeFailed, ObservedUPFUnresponsive, ObservedUPFNoEcho, ObservedProbeCutShort}

// hasPrefixAny reports whether s starts with any of prefixes.
func hasPrefixAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// confirmAlive applies the crash-safety confirmation to a result in which
// the association survived the message (survived), whatever the verdict: a
// PASS, or a FAIL for a missed shall such as a silent drop. A fresh NG Setup
// must still complete. If it does not, the core did not survive: CRASH with
// its own Observed text, so an expected-failures entry for the survivable
// outcome never hides a dead core. If the probe itself cannot be made, that
// says nothing about the core (ERROR), and neither does a probe the
// per-test timeout cut short (ERROR, ObservedProbeCutShort). A result whose
// association did not survive passes through unchanged.
func confirmAlive(ctx context.Context, env Env, r Result, survived bool, failDetail string) Result {
	if !survived {
		return r
	}
	alive, err := env.Alive(ctx)
	switch {
	case err != nil:
		r = r.Errored(ObservedProbeFailed+" ("+r.Observed+")", err.Error())
	case !alive && cutShort(ctx):
		// The probe ran out of the check's time: that is the harness, not
		// a dead core.
		r = cutShortResult(r, "its wait plus a fresh NG Setup")
	case !alive && env.loadedBy() != "":
		// A population loads the same AMF: its silence may be the
		// population's doing, so it is not this check's finding.
		r = r.Errored(ObservedAMFUnresponsiveUnderLoad+" ("+r.Observed+")",
			fmt.Sprintf("AMF unresponsive while population %s was active; not attributable to this check", env.loadedBy()))
	case !alive:
		r = r.Crashed(ObservedAMFUnresponsive+" ("+r.Observed+")", failDetail)
	}
	return r
}

// observeAlive is the liveness probe of an informational check. It tries a
// fresh NG Setup after the stimulus, which after names ("the RAN
// CONFIGURATION UPDATE"), and records the result in r's detail. A core that
// no longer completes it did not survive the stimulus: the result becomes
// CRASH with ObservedAMFUnresponsive, as confirmAlive gives a graded check,
// and it gates. The same
// silence while a population loads the core is not attributable to the
// check and is ERROR, as for a graded check. A probe that could not be
// made, or that the per-test timeout cut short, says nothing about the core
// and is recorded in the detail, the verdict left as it is.
func observeAlive(ctx context.Context, env Env, r Result, after string) Result {
	if cutShort(ctx) {
		r.Detail = joinDetail(r.Detail, "a fresh NG Setup afterwards was not tried: the per-test timeout had expired")
		return r
	}
	alive, err := env.Alive(ctx)
	switch {
	case err != nil:
		r.Detail = joinDetail(r.Detail, "a fresh NG Setup afterwards could not be tried: "+err.Error())
	case alive:
		r.Detail = joinDetail(r.Detail, "a fresh NG Setup afterwards completes")
	case cutShort(ctx):
		r.Detail = joinDetail(r.Detail, "a fresh NG Setup afterwards was cut short by the per-test timeout")
	case env.loadedBy() != "":
		r = r.Errored(ObservedAMFUnresponsiveUnderLoad+" ("+r.Observed+")",
			joinDetail(r.Detail, fmt.Sprintf("AMF unresponsive while population %s was active; not attributable to this check", env.loadedBy())))
	default:
		r = r.Crashed(ObservedAMFUnresponsive+" ("+r.Observed+")",
			joinDetail(r.Detail, "core did not survive "+after+": the AMF no longer completes a fresh NG Setup"))
	}
	return r
}

type setupError struct{ s string }

func (e *setupError) Error() string { return e.s }

// Registry holds the registered tests.
type Registry struct {
	tests []Test
	// only, when non-nil, narrows Select to the checks it names (Only).
	only map[string]bool
}

// NewRegistry returns a registry preloaded with the built-in tests.
func NewRegistry() *Registry {
	r := &Registry{}
	for _, t := range builtins {
		r.Register(t)
	}
	return r
}

// Register adds a test.
func (r *Registry) Register(t Test) { r.tests = append(r.tests, t) }

// Select returns the registered tests in cats (every test when cats is
// empty), narrowed to the checks Only named, in the order a suite runs
// them: by Phase, and in registration order within a phase.
func (r *Registry) Select(cats ...Category) []Test {
	want := map[Category]bool{}
	for _, c := range cats {
		want[c] = true
	}
	var out []Test
	for _, t := range r.tests {
		if r.only != nil && !r.only[t.ID()] {
			continue
		}
		if len(want) == 0 || want[t.Category()] {
			out = append(out, t)
		}
	}
	runOrder(out)
	return out
}

// Only narrows every later Select, and so every run and listing, to the
// checks with the given IDs (matched without regard to case), on top of any
// category filter. The checks keep their place in the registry, so each
// runs with the gNB IDs it has in a full run. An ID that names no
// registered check is an error and leaves the registry as it was; no IDs
// lifts the restriction.
func (r *Registry) Only(ids ...string) error {
	if len(ids) == 0 {
		r.only = nil
		return nil
	}
	known := map[string]string{}
	for _, t := range r.tests {
		known[strings.ToUpper(t.ID())] = t.ID()
	}
	only := map[string]bool{}
	for _, id := range ids {
		canon, ok := known[strings.ToUpper(strings.TrimSpace(id))]
		if !ok {
			return fmt.Errorf("unknown check %q", id)
		}
		only[canon] = true
	}
	r.only = only
	return nil
}

// Tests returns the registered tests in registration order, the order
// their gNB IDs and subscriber blocks follow; Select gives the run order.
func (r *Registry) Tests() []Test { return r.tests }

// Run executes every registered test against env (filtered to cats if given)
// and returns the results in run order (Select). Each test gets its own
// timeout so one hang does not stall the suite.
func (r *Registry) Run(ctx context.Context, env Env, perTest time.Duration, cats ...Category) []Result {
	var out []Result
	r.RunStream(ctx, env, perTest, func(res Result, _, _ int) { out = append(out, res) }, cats...)
	return out
}

// RunStream is Run with a callback per completed test, so a caller can report
// results as they arrive rather than after the whole suite — a suite takes
// tens of seconds, and a live view of it is exactly what the dashboard wants.
// emit receives (result, index, total): index is the 0-based position among
// the SELECTED tests and total their count, for progress display.
//
// Run is defined in terms of this so the two cannot drift: there is one
// per-test loop, with one place that assigns a distinct gNB ID and one place
// that stamps identity onto the result.
func (r *Registry) RunStream(ctx context.Context, env Env, perTest time.Duration,
	emit func(res Result, index, total int), cats ...Category) {
	r.RunWithStart(ctx, env, perTest, nil, emit, cats...)
}

// RunWithStart is RunStream with start, if non-nil, called just before each
// selected test runs, with the time it may take (ExpectedDuration), so a
// caller can announce a long check rather than appear to hang. It runs the
// checks without a preflight; RunSuite is a run as a user starts one.
func (r *Registry) RunWithStart(ctx context.Context, env Env, perTest time.Duration,
	start func(t Test, expected time.Duration, index, total int),
	emit func(res Result, index, total int), cats ...Category) {
	r.run(ctx, env, perTest, false, Hooks{Start: start, Result: emit}, cats...)
}

// Hooks are the callbacks of a suite run (RunSuite). Any may be nil.
type Hooks struct {
	// Preflight is called once, before any check, with the run's
	// preflight.
	Preflight func(Preflight)
	// Start is called just before each selected check runs, with the time
	// it may take (ExpectedDuration), its 0-based position among the
	// selected checks and their count.
	Start func(t Test, expected time.Duration, index, total int)
	// Result is called with each check's result, position and the count.
	Result func(res Result, index, total int)
}

// RunSuite is a conformance run: the preflight the selected checks' needs
// call for (ADR-0011 rule 6; see runPreflight), then the checks in cats
// (every check when empty) in run order, each with its own timeout. A check
// that depends on a preflight step that did not complete reports ERROR
// without running; the rest are judged. It returns the preflight, which is
// empty when no selected check needs anything from the core.
func (r *Registry) RunSuite(ctx context.Context, env Env, perTest time.Duration, hooks Hooks, cats ...Category) Preflight {
	return r.run(ctx, env, perTest, true, hooks, cats...)
}

// run is the one per-check loop: it assigns each selected check its gNB
// IDs and subscribers, runs it, and reports the result. With preflight set
// it first runs the preflight and holds the checks to it.
func (r *Registry) run(ctx context.Context, env Env, perTest time.Duration, preflight bool, hooks Hooks, cats ...Category) Preflight {
	if perTest <= 0 {
		perTest = DefaultPerTestTimeout
	}
	// The selected set, so `total` and `index` count only what will run rather
	// than every registered test.
	selected := r.Select(cats...)
	// registeredBy is the last check to register each subscriber.
	registeredBy := map[string]string{}
	owners := r.owners()
	var pre Preflight
	if preflight {
		pe := env
		pe.GNB.ID = r.preflightGNBID(env.GNB.ID, env.PackedGNBIDs)
		pe.GNB.Name = "orbit-conf-preflight"
		pe.UE = env.UE.forCheck(r.preflightSubscriber())
		pre = runPreflight(ctx, pe, selected, perTest)
		if _, registered := pre.step(StepRegistration); registered {
			registeredBy[pe.UE.SUPI] = "the preflight"
		}
		if hooks.Preflight != nil {
			hooks.Preflight(pre)
		}
	}
	for i, t := range selected {
		if err := ctx.Err(); err != nil {
			return pre // client went away; stop rather than keep dialling the core
		}
		if hooks.Start != nil {
			hooks.Start(t, ExpectedDuration(t, perTest), i, len(selected))
		}
		// Give each test a distinct gNB ID — the AMF does not cleanly re-key a
		// reused gNB ID from a new association (docs/interop/sdcore.md). Indexed
		// by position in the FULL registry so IDs are stable regardless of the
		// category filter.
		e := env
		e.GNB.ID, e.TargetGNBID, e.LivenessGNBID = r.gnbIDsOf(env.GNB.ID, r.indexOf(t), env.PackedGNBIDs)
		// And its own subscribers, when the suite has a pool of them.
		e.UE = env.UE.forCheck(r.subscriberBlock(r.indexOf(t)))
		e.owners = owners
		e.preflight = pre
		if e.UE.Configured() && subscribersOf(t) > 0 {
			e.sharedWith = registeredBy[e.UE.SUPI]
			registeredBy[e.UE.SUPI] = t.ID()
		}
		res := runOne(ctx, t, e, perTest)
		if hooks.Result != nil {
			hooks.Result(res, i, len(selected))
		}
	}
	return pre
}

// runOne runs one check, or SKIPs it when it requires a release newer than
// the target (see skipReason) or an idle core under load, or reports ERROR
// without running it when a step of the run's preflight it depends on did
// not complete, and stamps identity and provenance onto the result. A check that declares its
// outcomes (Declarer) is held to them (enforceDeclared). For one that does
// not, the verdict follows the check's references (WorstVerdict): a FAIL
// from a check that cites no shall becomes DEVIATION, and a DEVIATION from a
// check that cites no should becomes FAIL; an informational check is scored
// by scoreInformational, and INFO from any other is an ERROR. Every
// result's detail ends with the requirement line; when the target is newer
// than the releases the wording was verified through, the detail says so.
func runOne(ctx context.Context, t Test, e Env, perTest time.Duration) Result {
	min := MinRelease(t)
	var res Result
	n := NeedsOf(t)
	_, declares := declared(t)
	if why, skip := skipReason(t, e); skip {
		res = Result{}.As(Outcome{ID: OutcomeSkipRelease, Verdict: Skip, Skip: SkipRelease}, "requires a newer release than the target", why)
	} else if pop := e.loadedBy(); n.RequiresIdleCore && pop != "" {
		res = Result{}.Skipped(InputIdleCore, ObservedNeedsIdleCore, fmt.Sprintf("%s; population %s is active", n.IdleCoreReason, pop))
	} else if step, failed := e.preflight.blockedBy(t, e); failed {
		res = preflightError(step)
	} else {
		d, raised := budget(t, perTest)
		tctx, cancel := context.WithTimeout(ctx, d)
		res = t.Run(tctx, e)
		cancel()
		if raised {
			res.Detail = joinDetail(res.Detail, fmt.Sprintf("per-test timeout raised to %s for this check", d))
		}
		switch {
		case declares:
			res = enforceDeclared(t, res, e.owners)
		case IsInformational(t):
			res.Verdict = scoreInformational(res)
		case res.Verdict == Info || res.Verdict == infoDiffers:
			res.Detail = joinDetail(res.Detail, "scored ERROR: INFO is reserved for an informational check")
			res.Verdict = Error
		case res.Verdict == Fail && WorstVerdict(t) == Deviation:
			res.Verdict = Deviation
		case res.Verdict == Deviation && !cites(t, Should):
			res.Verdict = Fail
			res.Detail = joinDetail(res.Detail, "scored FAIL: DEVIATION is reserved for a missed should")
		}
		if e.TargetRelease > LatestRelease {
			res.Detail = joinDetail(res.Detail, fmt.Sprintf("wording verified through %s", LatestRelease))
		}
		if (res.Verdict == Error || res.Verdict == Blocked) && e.sharedWith != "" {
			res.Detail = joinDetail(res.Detail, sharedSubscriberNote(e))
		}
	}
	res.ID, res.Category = t.ID(), t.Category()
	res.SpecRefs = t.SpecRefs()
	res.SpecRef = RenderRefs(res.SpecRefs)
	res.MinRelease = min
	if !declares && IsInformational(t) {
		res.Detail = joinDetail(res.Detail, informationalNote)
	}
	res.Detail = joinDetail(res.Detail, requirementLine(t))
	return res
}

// enforceDeclared holds the result of a check's Run to the check's
// declaration. A result that names no outcome, an outcome the check neither
// declares nor derives from its needs, or a verdict other than the
// outcome's becomes an ERROR that says so: a check cannot report what it
// did not declare. The detail of a declared result then gains what the
// declaration says of it: the references a FAIL or DEVIATION rests on, a
// WARN's criterion, the bound of an absence, and for a BLOCKED the
// procedure and its owner (from owners).
func enforceDeclared(t Test, res Result, owners map[Procedure]string) Result {
	o, ok := outcomeOf(t, res.Outcome)
	switch {
	case res.Outcome == "":
		return res.Errored("the check reported no declared outcome",
			fmt.Sprintf("Run returned %s %q without one of the check's declared outcomes", res.Verdict, res.Observed))
	case !ok:
		return res.Errored("the check reported an undeclared outcome",
			fmt.Sprintf("Run returned outcome %q (%s %q), which the check neither declares nor has from its needs", res.Outcome, res.Verdict, res.Observed))
	case o.Verdict != res.Verdict:
		return res.Errored("the check reported an outcome with another verdict",
			fmt.Sprintf("Run returned outcome %q as %s; it is declared %s", res.Outcome, res.Verdict, o.Verdict))
	}
	res.Criterion, res.NotCompleted = o.Criterion, o.NotCompleted
	refs := map[string]SpecRef{}
	for _, r := range t.SpecRefs() {
		refs[r.Key()] = r
	}
	res.Refs = nil
	for _, k := range o.Refs {
		res.Refs = append(res.Refs, fmt.Sprintf("%s (%s)", k, refs[k].Strength))
	}
	switch res.Verdict {
	case Fail:
		res.Detail = joinDetail(res.Detail, "violates "+strings.Join(res.Refs, ", "))
	case Deviation:
		res.Detail = joinDetail(res.Detail, "does not follow "+strings.Join(res.Refs, ", "))
	case Warn:
		res.Detail = joinDetail(res.Detail, fmt.Sprintf("no requirement violated; WARN for %s impact", res.Criterion))
	case Blocked:
		res.Owner = owners[res.BlockedOn]
		owner := res.Owner
		if owner == "" {
			owner = "no check of this run's registry"
		}
		res.Detail = joinDetail(res.Detail, fmt.Sprintf("blocked: the core did not complete %s, which this check needs first; %s owns whether it completes",
			res.BlockedOn.article(), owner))
	}
	if o.Bound != nil && !strings.Contains(res.Detail, "absence inferred") {
		res.Detail = joinDetail(res.Detail, "absence inferred within "+o.Bound.String())
	}
	return res
}

// informationalNote is what every result of an informational check says
// ahead of its requirement line, whatever its verdict.
const informationalNote = "informational: reported, not graded"

// infoDiffers is what an informational check that has not declared its
// outcomes returns for behaviour other than the described one. It never
// leaves runOne, which reports it as INFO (scoreInformational).
const infoDiffers Verdict = "info-differs"

// scoreInformational is the verdict an informational check's result is
// reported with: INFO for whatever the core did (the check's INFO,
// infoDiffers, PASS, FAIL or DEVIATION), the observed text saying whether
// it is the described behaviour. Every other verdict keeps its meaning.
func scoreInformational(res Result) Verdict {
	switch res.Verdict {
	case Pass, Fail, Deviation, infoDiffers:
		return Info
	}
	return res.Verdict
}

// skipReason reports whether a check SKIPs under e's targets, and why. A
// shall or should reference whose Since is newer than its spec's target
// (Env.SpecTarget) skips it; a descriptive reference never does, unless the
// check cites nothing stronger (the same floor ValidateTest uses). A declared
// MinRelease newer than the run target skips it too, whatever the pins say:
// the declaration is about the whole check, not one specification (see
// minReleaser). When the run target binds, the reason is the run-target
// text; when only a pin binds, it names the reference and the pinned target.
func skipReason(t Test, e Env) (string, bool) {
	if e.TargetRelease == 0 {
		return "", false
	}
	min := MinRelease(t)
	runTarget := false
	if _, ok := t.(minReleaser); ok && min > e.TargetRelease {
		runTarget = true
	}
	refs := requirementRefs(t.SpecRefs())
	var pinned SpecRef
	hasPinned := false
	for _, r := range refs {
		switch {
		case r.Since > e.TargetRelease:
			runTarget = true
		case r.Since > e.SpecTarget(r.Spec) && !hasPinned:
			pinned, hasPinned = r, true
		}
	}
	switch {
	case runTarget:
		return fmt.Sprintf("requires %s (%s); target release is %s", min, refsSince(t, min), e.TargetRelease), true
	case hasPinned:
		return fmt.Sprintf("requires %s (%s %s); target for %s is %s",
			pinned.Since, pinned.Spec, pinned.Clause, pinned.Spec, e.SpecTarget(pinned.Spec)), true
	}
	return "", false
}

// requirementRefs returns the shall and should references, or every
// reference when there is none.
func requirementRefs(refs []SpecRef) []SpecRef {
	var out []SpecRef
	for _, r := range refs {
		if r.Strength == Shall || r.Strength == Should {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return refs
	}
	return out
}

// refsSince names, briefly, the references that set a check's minimum
// release ("TS 24.501 5.4.1.3.7"), for the SKIP detail.
func refsSince(t Test, min Release) string {
	refs := requirementRefs(t.SpecRefs())
	var out []string
	for _, r := range refs {
		if r.Since >= min {
			out = append(out, r.Spec+" "+r.Clause)
		}
	}
	if len(out) == 0 { // a check whose MinRelease is declared above every Since
		for _, r := range refs {
			out = append(out, r.Spec+" "+r.Clause)
		}
	}
	return strings.Join(out, "; ")
}

// joinDetail appends one sentence to a detail; an empty one adds nothing.
func joinDetail(detail, more string) string {
	if detail == "" {
		return more
	}
	if more == "" {
		return detail
	}
	return detail + "; " + more
}

// indexOf returns a test's position in the full registry, used for a stable
// per-test gNB ID.
func (r *Registry) indexOf(t Test) int {
	for i, x := range r.tests {
		if x.ID() == t.ID() {
			return i
		}
	}
	return 0
}

// Summary tallies a suite run for reporting and CI gating.
type Summary struct {
	Total int
	// One count per verdict (see Count).
	Pass, Fail, Deviation, Warn, Info, Blocked, Crash, Skip, Error int
	Results                                                        []Result
	// N2Source records which address the checks were taken from: the
	// configured Env.Bind in sctp.NormalizeLocal's ip:0 form, or, unbound,
	// the route-selected IP alone. It is never a bound ip:port: every check
	// dials its own association on its own ephemeral port, so there is no
	// single real port to report. Empty only when the source probe failed.
	N2Source string `json:",omitempty"`
	// Network is the identity the checks presented to the core (PLMN, TAC,
	// slice), so a verdict records which network it was taken under.
	Network *NetworkIdentity `json:",omitempty"`
	// TargetRelease is the release the run was gated on ("unset" when none
	// was given, in which case every check ran).
	TargetRelease Release
	// SpecTargets are the per-specification pins that narrowed the run
	// (Env.NarrowingSpecTargets), each at its effective release; empty when
	// TargetRelease is unset.
	SpecTargets map[string]Release
	// ExpectedFail counts the FAILs an expected-failures entry tolerates;
	// they are in Fail too, and only the rest gate (OK).
	ExpectedFail int
	// Stale lists the expected-failures entries whose check ran without the
	// FAIL they tolerate (ExpectedFailures.Stale); they warn, not gate.
	Stale []StaleExpectation `json:",omitempty"`
	// ExpectedFailures is the core line of the list the run used, "" when
	// none was given.
	ExpectedFailures string `json:",omitempty"`
	// Preflight is the run's preflight, the setup its checks need, done
	// once before any of them; empty when the run had none.
	Preflight Preflight `json:",omitempty"`
}

// TargetLine renders the run's targets for the text output: "Rel-18 (TS
// 29.244 at Rel-16)", "Rel-18" with no narrowing pins, or "unset".
func (s Summary) TargetLine() string {
	if s.TargetRelease == 0 || len(s.SpecTargets) == 0 {
		return s.TargetRelease.String()
	}
	specs := make([]string, 0, len(s.SpecTargets))
	for spec := range s.SpecTargets {
		specs = append(specs, spec)
	}
	sort.Strings(specs)
	pins := make([]string, len(specs))
	for i, spec := range specs {
		pins[i] = fmt.Sprintf("%s at %s", spec, s.SpecTargets[spec])
	}
	return fmt.Sprintf("%s (%s)", s.TargetRelease, strings.Join(pins, ", "))
}

// NetworkIdentity is the PLMN, tracking area and slice a conformance run
// presented, as recorded in the JSON summary.
type NetworkIdentity struct {
	MCC, MNC string
	TAC      uint32
	SST      uint32
	SD       string
}

// Network reports the identity Env.GNB presents, for the summary. The first
// supported slice is the one the checks register under.
func (e Env) Network() *NetworkIdentity {
	n := &NetworkIdentity{MCC: e.GNB.MCC, MNC: e.GNB.MNC, TAC: e.GNB.TAC}
	if len(e.GNB.Slices) > 0 {
		n.SST, n.SD = uint32(e.GNB.Slices[0].SST), e.GNB.Slices[0].SD
	}
	return n
}

// Summarize counts verdicts across results.
func Summarize(results []Result) Summary {
	s := Summary{Total: len(results), Results: results, SpecTargets: map[string]Release{}}
	for _, r := range results {
		if n := s.counter(r.Verdict); n != nil {
			*n++
		}
		if r.Verdict == Fail && r.ExpectedFailure != nil {
			s.ExpectedFail++
		}
	}
	return s
}

// counter is the tally field for v, nil for a verdict outside the nine.
func (s *Summary) counter(v Verdict) *int {
	switch v {
	case Pass:
		return &s.Pass
	case Fail:
		return &s.Fail
	case Deviation:
		return &s.Deviation
	case Warn:
		return &s.Warn
	case Info:
		return &s.Info
	case Blocked:
		return &s.Blocked
	case Crash:
		return &s.Crash
	case Skip:
		return &s.Skip
	case Error:
		return &s.Error
	}
	return nil
}

// Count is how many results have verdict v.
func (s Summary) Count(v Verdict) int {
	if n := s.counter(v); n != nil {
		return *n
	}
	return 0
}

// TallyLine renders the counts on one line, every verdict in the order of
// Verdicts: "4 checks: 1 pass, 2 fail (1 expected), 0 deviation, ...".
func (s Summary) TallyLine() string {
	parts := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		part := fmt.Sprintf("%d %s", s.Count(v), v.Label())
		if v == Fail {
			part += fmt.Sprintf(" (%d expected)", s.ExpectedFail)
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("%d checks: %s", s.Total, strings.Join(parts, ", "))
}

// OK reports a clean run: no FAIL beyond those the expected-failures list
// tolerates, and no CRASH, BLOCKED or ERROR. This is the integration-CI
// gate. DEVIATION, WARN, INFO, SKIP and stale entries do not fail it.
func (s Summary) OK() bool {
	return s.Fail-s.ExpectedFail == 0 && s.Crash == 0 && s.Blocked == 0 && s.Error == 0
}

// builtins is the registered test set (populated in the tests_*.go files).
var builtins []Test
