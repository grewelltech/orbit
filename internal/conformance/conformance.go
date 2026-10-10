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
	// Deviation is a spec "shall" that the core does not meet but which is
	// benign and commonly unimplemented (e.g. a courtesy Error Indication) — a
	// documented non-conformance that does NOT fail the CI gate, so a known,
	// steady-state gap is reported without crying wolf.
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
)

// Result is one test's structured outcome.
type Result struct {
	ID       string   `json:"id"`
	Category Category `json:"category"`
	SpecRef  string   `json:"spec_ref"`
	Verdict  Verdict  `json:"verdict"`
	Expected string   `json:"expected"`
	Observed string   `json:"observed"`
	Detail   string   `json:"detail,omitempty"`
}

// Test is a single conformance / regression check.
type Test interface {
	ID() string
	Category() Category
	SpecRef() string
	// Run drives the check against env and returns its verdict.
	Run(ctx context.Context, env Env) Result
}

// Env is what a test needs to reach the core: the AMF N2 endpoint and a gNB
// identity to present. Tests dial their own association(s) via Dial, since
// most need a fresh or deliberately-misused one.
//
// Bind is the local N2 source address every check's association binds, as
// ip or ip:0 (see sctp.NormalizeLocal); empty lets the kernel choose. One
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
type Env struct {
	AMFAddr string
	Bind    string
	GNB     gnb.Config
	UPFN3   string
	N3Bind  string
}

// Dial opens a raw SCTP association to the AMF (no NG Setup) from Bind.
func (e Env) Dial() (*sctp.Conn, error) {
	return sctp.Dial(e.Bind, e.AMFAddr)
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

// N2Source is Bind in the normalised ip:port form sctp.Dial uses, for
// reporting; "" when the kernel chooses. An unparseable Bind is returned as
// written, since every check will have ERRORed on it and the report should
// show what was asked for.
func (e Env) N2Source() string {
	n, err := sctp.NormalizeLocal(e.Bind)
	if err != nil {
		return e.Bind
	}
	return n
}

// DialSetup opens an association and performs NG Setup, returning it ready for
// UE-associated signalling. The caller closes it.
func (e Env) DialSetup(ctx context.Context) (*sctp.Conn, error) {
	conn, err := e.Dial()
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
	conn, err := e.Dial()
	if err != nil {
		return false, fmt.Errorf("liveness probe dial: %w", err)
	}
	defer conn.Close()
	ng, err := gnb.NGSetup(ctx, conn, probe)
	return err == nil && ng.Accepted, nil
}

// confirmAlive applies the crash-safety confirmation to a passing result: a
// core that no longer completes a fresh NG Setup turns the PASS into a FAIL,
// while a probe that could not even be dialled turns it into an ERROR.
func confirmAlive(ctx context.Context, env Env, r Result, failDetail string) Result {
	if r.Verdict != Pass {
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
		tctx, cancel := context.WithTimeout(ctx, perTest)
		res := t.Run(tctx, e)
		cancel()
		res.ID, res.Category, res.SpecRef = t.ID(), t.Category(), t.SpecRef()
		emit(res, i, len(selected))
	}
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
	// N2Source is the configured Env.Bind in sctp.NormalizeLocal's ip:0
	// form, so a verdict records which address the checks were taken from.
	// It is the requested bind, not a bound address: every check dials its
	// own association, each on its own ephemeral port, so there is no single
	// real port to report. Empty when the kernel chose.
	N2Source string `json:",omitempty"`
}

// Summarize counts verdicts across results.
func Summarize(results []Result) Summary {
	s := Summary{Total: len(results), Results: results}
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
// the integration-CI gate. Deviations are documented non-conformances that do
// not fail the gate.
func (s Summary) OK() bool { return s.Fail == 0 && s.Error == 0 }

// builtins is the registered test set (populated in the tests_*.go files).
var builtins []Test
