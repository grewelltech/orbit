package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
)

func TestIsTerminalBuffer(t *testing.T) {
	// A buffer (how the command tests capture output) must read as
	// non-interactive, which is what keeps their output plain.
	var b bytes.Buffer
	if isTerminal(&b) {
		t.Fatal("a bytes.Buffer must not be reported as a terminal")
	}
}

func TestPrintConformanceTable(t *testing.T) {
	sum := conformance.Summary{
		Total: 4, Pass: 1, Fail: 1, Deviation: 1, Error: 0, Skip: 1, ExpectedFail: 1,
		N2Source:         "10.0.0.1:0",
		ExpectedFailures: "Rel-18",
		Results: []conformance.Result{
			{ID: "AUTH-1", Group: "NGAP / Interface Management Procedures", SpecRef: "TS 24.501", Verdict: conformance.Pass, Observed: "registration completed"},
			{ID: "SEC-2", Group: "TS 33.501 / NAS security mechanisms / NAS integrity activation", SpecRef: "TS 33.501", Verdict: conformance.Fail, Observed: "accepted replayed NAS", ExpectedFailure: &conformance.ExpectedFailure{Finding: "known"}},
			{ID: "GTPU-3", Group: "GTPv1-U / Tunnel Management Messages / Error Indication", SpecRef: "TS 29.281", Verdict: conformance.Deviation, Observed: strings.Repeat("x", 90), Detail: "requires Rel-18; TS 29.281 clause 5"},
			{ID: "NAS-4", Group: "NAS / 5GMM specific procedures / Registration procedure", SpecRef: "TS 24.501", Verdict: conformance.Skip, Observed: "no UE configured", Detail: "set --supi, --ki/--opc and --gnb-n3 to exercise this check"},
		},
	}

	var b bytes.Buffer
	printConformanceTable(&b, "001-01", sum, "/etc/orbit/ef.yaml")
	out := b.String()

	for _, want := range []string{
		"VERDICT", "TEST", "GROUP", "SPEC", "OBSERVED",
		"AUTH-1", "SEC-2", "GTPU-3",
		"4 checks", "1 pass", "1 fail", "1 deviation", "1 skip",
		"001-01", "10.0.0.1:0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
	// A tolerated FAIL is marked, with the footnote and the expected count.
	if !strings.Contains(out, "FAIL*") {
		t.Errorf("expected tolerated FAIL to be marked with *\n%s", out)
	}
	if !strings.Contains(out, "(1 expected)") {
		t.Errorf("expected tally to note the tolerated FAIL\n%s", out)
	}
	// A long Observed value wraps inside its column rather than overflowing,
	// so its content is still present (not cut away).
	if !strings.Contains(out, "xxxxxxxxxx") {
		t.Errorf("expected the long observed value to wrap into the table\n%s", out)
	}
	// A non-pass check's Detail is preserved below the table so a terminal
	// user can still diagnose it.
	if !strings.Contains(out, "requires Rel-18; TS 29.281 clause 5") {
		t.Errorf("expected the deviation's Detail to be shown\n%s", out)
	}
	// A SKIP's Detail names how to exercise the check, so it must be shown too.
	if !strings.Contains(out, "set --supi, --ki/--opc and --gnb-n3") {
		t.Errorf("expected the skip's Detail to be shown\n%s", out)
	}
}

// The preflight is its own section in both renderings, ahead of the
// checks, a line per step with its status and detail; a run without one
// has no section.
func TestConformanceReportPreflight(t *testing.T) {
	pre := conformance.Preflight{
		{Step: conformance.StepNGSetup, Status: conformance.PreflightOK, Detail: "gNB ID 700, PLMN 208/93, TAC 1"},
		{Step: conformance.StepRegistration, Status: conformance.PreflightFailed, Detail: "registration on gNB 0x2bc: rejected"},
		{Step: conformance.StepPDUSession, Status: conformance.PreflightNotRun},
	}
	want := []string{"preflight:", "  ok      NG Setup: gNB ID 700, PLMN 208/93, TAC 1", "  failed  registration: registration on gNB 0x2bc: rejected", "  not run PDU session establishment"}
	if got := preflightLines(pre); !slices.Equal(got, want) {
		t.Errorf("preflightLines = %q, want %q", got, want)
	}
	if got := preflightLines(nil); got != nil {
		t.Errorf("no preflight: %q, want no lines", got)
	}
	sum := conformance.Summary{Total: 1, Error: 1, Preflight: pre, Results: []conformance.Result{
		{ID: "NAS-1", Group: "NAS / 5GMM specific procedures / Registration procedure", Verdict: conformance.Error, Observed: conformance.ObservedPreflightFailed + " (registration failed)"}}}
	render := map[string]func(*bytes.Buffer){
		"text":  func(b *bytes.Buffer) { printConformanceText(b, "208-93", sum, "") },
		"table": func(b *bytes.Buffer) { printConformanceTable(b, "208-93", sum, "") },
	}
	for name, r := range render {
		var b bytes.Buffer
		r(&b)
		out := b.String()
		at := strings.Index(out, "preflight:")
		for _, line := range want[1:] {
			if !strings.Contains(out, line) {
				t.Errorf("%s: lacks %q:\n%s", name, line, out)
			}
		}
		if at < 0 || at > strings.Index(out, "NAS-1") {
			t.Errorf("%s: the preflight section is not ahead of the checks:\n%s", name, out)
		}
	}
}

// INFO and WARN print as their own lines and tally columns in both
// renderings; the WARN fails the default gate and not the spec one, and
// the gate line says so.
func TestConformanceReportInformational(t *testing.T) {
	results := []conformance.Result{
		{ID: "NGAP-A", Group: "NGAP / Interface Management Procedures", SpecRef: "TS 38.413", Verdict: conformance.Pass, Observed: "ok", Detail: "d1"},
		{ID: "NGAP-B", Group: "NGAP / Interface Management Procedures", SpecRef: "TS 38.413", Verdict: conformance.Info, Observed: "acknowledged", Detail: "as described"},
		{ID: "NGAP-C", Group: "NGAP / Interface Management Procedures", SpecRef: "TS 38.413", Verdict: conformance.Warn, Observed: "no answer", Detail: "not as described"},
	}
	sum := conformance.Summarize(results)
	if sum.OK() {
		t.Fatalf("a WARN passed the default gate: %+v", sum)
	}
	if spec := sum; func() bool { spec.Gate = conformance.GateSpec; return !spec.OK() }() {
		t.Fatalf("a WARN failed the spec gate: %+v", sum)
	}
	tests := []struct {
		name    string
		render  func(*bytes.Buffer)
		want    []string
		wantNot []string
	}{
		{name: "text", render: func(b *bytes.Buffer) { printConformanceText(b, "plmn 001/01", sum, "") },
			want: []string{
				"INFO  NGAP-B", "WARN  NGAP-C", "      not as described\n",
				"3 checks: 1 pass, 0 fail (0 expected), 0 deviation, 1 warn, 1 info, 0 blocked, 0 crash, 0 skip, 0 error\n",
				"gate default: failed: 0 spec failures, 1 warning\n",
			}},
		{name: "table", render: func(b *bytes.Buffer) { printConformanceTable(b, "plmn 001/01", sum, "") },
			want:    []string{"INFO", "WARN", "NGAP-B", "NGAP-C", "not as described", "1 info", "1 warn", "0 blocked", "0 crash", "3 checks", "gate default: failed: 0 spec failures, 1 warning"},
			wantNot: []string{"NGAP-B as described"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tc.render(&b)
			for _, w := range tc.want {
				if !strings.Contains(b.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, b.String())
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(b.String(), w) {
					t.Errorf("output has %q:\n%s", w, b.String())
				}
			}
		})
	}
}

// A streamed result keeps what its declared outcome says of it: the outcome
// ID, a WARN's criterion, a BLOCKED's procedure and owner, and the
// references the verdict rests on.
func TestResultFromProtoOutcome(t *testing.T) {
	r := resultFromProto(&orbitv1.ConformanceResult{Id: "X", Verdict: "BLOCKED", Outcome: "blocked-registration",
		BlockedOn: "registration", Owner: "NAS-X", Criterion: "availability", NotCompleted: "NG Setup",
		OutcomeRefs: []string{"TS 38.413 10.6 (shall)"}})
	if r.Verdict != conformance.Blocked || r.Outcome != "blocked-registration" || r.BlockedOn != conformance.ProcRegistration || r.Owner != "NAS-X" ||
		r.Criterion != conformance.CriterionAvailability || r.NotCompleted != conformance.ProcNGSetup || len(r.Refs) != 1 {
		t.Errorf("result = %+v", r)
	}
}
