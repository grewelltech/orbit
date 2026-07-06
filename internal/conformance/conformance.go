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
type Env struct {
	AMFAddr string
	GNB     gnb.Config
}

// Dial opens a raw SCTP association to the AMF (no NG Setup).
func (e Env) Dial() (*sctp.Conn, error) {
	return sctp.Dial("", e.AMFAddr)
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
func (e Env) Alive(ctx context.Context) bool {
	probe := e.GNB
	probe.ID = e.GNB.ID + 0x9000
	probe.Name = "orbit-conf-probe"
	conn, err := e.Dial()
	if err != nil {
		return false
	}
	defer conn.Close()
	ng, err := gnb.NGSetup(ctx, conn, probe)
	return err == nil && ng.Accepted
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
	if perTest <= 0 {
		perTest = 15 * time.Second
	}
	want := map[Category]bool{}
	for _, c := range cats {
		want[c] = true
	}
	var out []Result
	for i, t := range r.tests {
		if len(want) > 0 && !want[t.Category()] {
			continue
		}
		// Give each test a distinct gNB ID — the AMF does not cleanly re-key a
		// reused gNB ID from a new association (docs/interop/sdcore.md).
		e := env
		e.GNB.ID = env.GNB.ID + uint32(i)
		tctx, cancel := context.WithTimeout(ctx, perTest)
		res := t.Run(tctx, e)
		cancel()
		res.ID, res.Category, res.SpecRef = t.ID(), t.Category(), t.SpecRef()
		out = append(out, res)
	}
	return out
}

// Summary tallies a suite run for reporting and CI gating.
type Summary struct {
	Total, Pass, Fail, Error, Skip int
	Results                        []Result
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
		}
	}
	return s
}

// OK reports a clean run — no failed assertions and no harness errors. This is
// the integration-CI gate.
func (s Summary) OK() bool { return s.Fail == 0 && s.Error == 0 }

// builtins is the registered test set (populated in the tests_*.go files).
var builtins []Test
