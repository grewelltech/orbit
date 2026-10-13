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
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Verdict is a conformance test outcome.
type Verdict string

const (
	Pass  Verdict = "PASS"  // the core behaved as the spec requires
	Fail  Verdict = "FAIL"  // the core violated the asserted behaviour
	Error Verdict = "ERROR" // the test could not run (setup failure)
	Skip  Verdict = "SKIP"  // not applicable / not exercised
	// Deviation is a missed "should": a recommendation the core does not
	// follow. It does not fail the CI gate. A missed "shall" is always Fail;
	// a known, tolerated failure goes on the core's expected-failures list,
	// not into a softer verdict.
	Deviation Verdict = "DEVIATION"
)

// Category groups tests for reporting and selective runs.
type Category string

const (
	Procedural Category = "procedural"  // well-formed procedures complete
	NegativeIE Category = "negative-ie" // malformed / unexpected messages are rejected gracefully
	Security   Category = "security"    // NAS security-context violations
	GTPU       Category = "gtpu"        // user-plane error handling
	Timing     Category = "timing"      // concurrent-procedure timing (flaky-allowed)
	Mobility   Category = "mobility"    // UE-level procedures over two gNB associations (needs Env.UE)
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
	Expected   string  `json:"expected"`
	Observed   string  `json:"observed"`
	Detail     string  `json:"detail,omitempty"`
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
	GNB           gnb.Config
	UPFN3         string
	N3Bind        string
	TargetRelease Release
	SpecTargets   map[string]Release
	UE            UESpec
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
// UE-associated signalling. The caller closes it.
func (e Env) DialSetup(ctx context.Context) (*sctp.Conn, error) {
	conn, err := e.Dial(ctx)
	if err != nil {
		return nil, err
	}
	ng, err := gnb.NGSetup(ctx, conn, e.GNB)
	if err != nil || !ng.Accepted {
		conn.Close()
		if err == nil {
			err = &setupError{"NG Setup rejected: " + ng.Cause}
		}
		return nil, err
	}
	return conn, nil
}

// Alive reports whether the AMF still accepts a fresh NG Setup — a liveness
// probe used to confirm the core survived a malformed message. It uses a gNB
// ID well clear of the per-test IDs so it never collides. This is the
// crash-safety primitive: if a negative test leaves the AMF unable to complete
// a fresh NG Setup, the core did not survive.
//
// A non-nil error means the probe itself could not be made (the local dial
// failed), which says nothing about the core: callers report it as a harness
// ERROR, never as a FAIL. alive=false with a nil error is the core's doing.
func (e Env) Alive(ctx context.Context) (alive bool, err error) {
	probe := e.GNB
	probe.ID = e.GNB.ID + 0x9000
	probe.Name = "orbit-conf-probe"
	conn, err := e.Dial(ctx)
	if err != nil {
		return false, fmt.Errorf("liveness probe dial: %w", err)
	}
	defer conn.Close()
	ng, err := gnb.NGSetup(ctx, conn, probe)
	return err == nil && ng.Accepted, nil
}

// confirmAlive applies the crash-safety confirmation to a result in which
// the association survived the message (survived), whatever the verdict: a
// PASS, or a FAIL for a missed shall such as a silent drop. A fresh NG Setup
// must still complete. If it does not, the core did not survive: FAIL with
// its own Observed text, so an expected-failures entry for the survivable
// outcome never hides a dead core. If the probe itself cannot be made, that
// says nothing about the core (ERROR). A result whose association did not
// survive passes through unchanged.
func confirmAlive(ctx context.Context, env Env, r Result, survived bool, failDetail string) Result {
	if !survived {
		return r
	}
	alive, err := env.Alive(ctx)
	switch {
	case err != nil:
		r.Verdict = Error
		r.Observed = "liveness probe could not run (" + r.Observed + ")"
		r.Detail = err.Error()
	case !alive:
		r.Verdict = Fail
		r.Observed = "AMF unresponsive after the message (" + r.Observed + ")"
		r.Detail = failDetail
	}
	return r
}

type setupError struct{ s string }

func (e *setupError) Error() string { return e.s }

// Registry holds the registered tests.
type Registry struct {
	tests []Test
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

// Tests returns the registered tests.
func (r *Registry) Tests() []Test { return r.tests }

// Run executes every registered test against env (filtered to cats if given)
// and returns the results in registration order. Each test gets its own
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
	if perTest <= 0 {
		perTest = 15 * time.Second
	}
	want := map[Category]bool{}
	for _, c := range cats {
		want[c] = true
	}
	// The selected set, so `total` and `index` count only what will run rather
	// than every registered test.
	var selected []Test
	for _, t := range r.tests {
		if len(want) == 0 || want[t.Category()] {
			selected = append(selected, t)
		}
	}
	for i, t := range selected {
		if err := ctx.Err(); err != nil {
			return // client went away; stop rather than keep dialling the core
		}
		// Give each test a distinct gNB ID — the AMF does not cleanly re-key a
		// reused gNB ID from a new association (docs/interop/sdcore.md). Indexed
		// by position in the FULL registry so IDs are stable regardless of the
		// category filter.
		e := env
		e.GNB.ID = env.GNB.ID + uint32(r.indexOf(t))
		emit(runOne(ctx, t, e, perTest), i, len(selected))
	}
}

// runOne runs one check, or SKIPs it when it requires a release newer than
// the target (see skipReason), and stamps identity and provenance onto the
// result. The verdict follows the check's references (WorstVerdict): a FAIL
// from a check that cites no shall becomes DEVIATION, and a DEVIATION from a
// check that cites no should becomes FAIL, since DEVIATION is reserved for a
// missed should. Every
// result's detail ends with the requirement line; when the target is newer
// than the releases the wording was verified through, the detail says so.
func runOne(ctx context.Context, t Test, e Env, perTest time.Duration) Result {
	min := MinRelease(t)
	var res Result
	if why, skip := skipReason(t, e); skip {
		res = Result{Verdict: Skip, Observed: "requires a newer release than the target", Detail: why}
	} else {
		tctx, cancel := context.WithTimeout(ctx, perTest)
		res = t.Run(tctx, e)
		cancel()
		switch {
		case res.Verdict == Fail && WorstVerdict(t) == Deviation:
			res.Verdict = Deviation
		case res.Verdict == Deviation && !cites(t, Should):
			res.Verdict = Fail
			res.Detail = joinDetail(res.Detail, "scored FAIL: DEVIATION is reserved for a missed should")
		}
		if e.TargetRelease > LatestRelease {
			res.Detail = joinDetail(res.Detail, fmt.Sprintf("wording verified through %s", LatestRelease))
		}
	}
	res.ID, res.Category = t.ID(), t.Category()
	res.SpecRefs = t.SpecRefs()
	res.SpecRef = RenderRefs(res.SpecRefs)
	res.MinRelease = min
	res.Detail = joinDetail(res.Detail, requirementLine(t))
	return res
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

// joinDetail appends one sentence to a detail.
func joinDetail(detail, more string) string {
	if detail == "" {
		return more
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
	Total, Pass, Fail, Error, Skip, Deviation int
	Results                                   []Result
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
		switch r.Verdict {
		case Pass:
			s.Pass++
		case Fail:
			s.Fail++
		case Error:
			s.Error++
		case Skip:
			s.Skip++
		case Deviation:
			s.Deviation++
		}
	}
	return s
}

// OK reports a clean run — no failed assertions and no harness errors. This is
// the integration-CI gate. Deviations (missed shoulds) do not fail the gate.
func (s Summary) OK() bool { return s.Fail == 0 && s.Error == 0 }

// builtins is the registered test set (populated in the tests_*.go files).
var builtins []Test
