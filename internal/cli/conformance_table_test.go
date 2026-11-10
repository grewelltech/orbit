package cli

import (
	"bytes"
	"strings"
	"testing"

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

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hell…"},
		{"", 3, ""},
		{"abc", 1, "a"},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestPrintConformanceTable(t *testing.T) {
	sum := conformance.Summary{
		Total: 3, Pass: 1, Fail: 1, Deviation: 1, Error: 0, Skip: 0, ExpectedFail: 1,
		N2Source:         "10.0.0.1:0",
		ExpectedFailures: "Rel-18",
		Results: []conformance.Result{
			{ID: "AUTH-1", Category: conformance.Procedural, SpecRef: "TS 24.501", Verdict: conformance.Pass, Observed: "registration completed"},
			{ID: "SEC-2", Category: conformance.Security, SpecRef: "TS 33.501", Verdict: conformance.Fail, Observed: "accepted replayed NAS", ExpectedFailure: &conformance.ExpectedFailure{Finding: "known"}},
			{ID: "GTPU-3", Category: conformance.GTPU, SpecRef: "TS 29.281", Verdict: conformance.Deviation, Observed: strings.Repeat("x", 90)},
		},
	}

	var b bytes.Buffer
	printConformanceTable(&b, "001-01", sum, "/etc/orbit/ef.yaml")
	out := b.String()

	for _, want := range []string{
		"VERDICT", "TEST", "CATEGORY", "SPEC", "OBSERVED",
		"AUTH-1", "SEC-2", "GTPU-3",
		"3 checks", "1 pass", "1 fail", "1 deviation",
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
	// A long Observed value is truncated with an ellipsis.
	if !strings.Contains(out, "…") {
		t.Errorf("expected a long observed value to be truncated\n%s", out)
	}
}
