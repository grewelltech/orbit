package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/conformance"
)

const expectedFixture = "../conformance/testdata/expected-failures.yaml"

// A bad list fails the command before any check runs (no network line, no
// results), naming the flag and the file.
func TestConformanceExpectedFailuresBadFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	writeFile(t, bad, "core: c\nversion: v\nentries:\n  - check: NGAP-NOPE\n    observed: o\n    finding: EX-1\n    added: 2026-10-12\n    reason: r\n")
	for _, path := range []string{bad, filepath.Join(t.TempDir(), "missing.yaml")} {
		out, err := runConformance(t, "--expected-failures", path)
		if err == nil || !strings.Contains(err.Error(), "--expected-failures:") || !strings.Contains(err.Error(), filepath.Base(path)) {
			t.Errorf("%s: err = %v", path, err)
		}
		if out != "" {
			t.Errorf("%s: output before the list was refused:\n%s", path, out)
		}
	}
}

// With a list, the run states it; checks that ERROR are neither tolerated
// nor stale, so the gate still fails on them.
func TestConformanceExpectedFailuresHeader(t *testing.T) {
	out, err := runConformance(t, "--category", "negative-ie", "--expected-failures", expectedFixture)
	if !strings.Contains(out, "expected-failures: Example core (lab A) ("+expectedFixture+")\n") {
		t.Errorf("no expected-failures header:\n%s", out)
	}
	if strings.Contains(out, "(expected:") || strings.Contains(out, "stale expected failure") {
		t.Errorf("an ERROR was tolerated or reported stale:\n%s", out)
	}
	if err == nil || !strings.Contains(err.Error(), "conformance gate failed:") || !strings.Contains(err.Error(), "fail (0 expected)") {
		t.Errorf("err = %v, want the gate to fail on the ERRORs", err)
	}
}

// The text output marks a tolerated FAIL with its finding, lists stale
// entries, and the tally counts the expected FAILs; --json carries the same.
func TestPrintConformanceTextExpected(t *testing.T) {
	results := []conformance.Result{
		{ID: "NGAP-UNKNOWN-UE-SURVIVES", Category: conformance.NegativeIE, Verdict: conformance.Fail,
			Observed: "silently dropped; association survived", Detail: "d1"},
		{ID: "GTPU-UNKNOWN-TEID-ERRIND", Category: conformance.GTPU, Verdict: conformance.Pass,
			Observed: "Error Indication", Detail: "d2"},
		{ID: "NGAP-NG-RESET-ACK", Category: conformance.Procedural, Verdict: conformance.Fail, Observed: "no ack", Detail: "d3"},
	}
	x, err := conformance.LoadExpectedFailures(expectedFixture)
	if err != nil {
		t.Fatal(err)
	}
	x.Apply(results)
	sum := conformance.Summarize(results)
	sum.Stale = x.Stale(results)
	sum.ExpectedFailures = x.Label()

	var out bytes.Buffer
	printConformanceText(&out, "plmn 001/01", sum, "lab.yaml")
	for _, want := range []string{
		"expected-failures: Example core (lab A) (lab.yaml)\n",
		"FAIL (expected: EX-1) [negative-ie] NGAP-UNKNOWN-UE-SURVIVES",
		"FAIL  [procedural ] NGAP-NG-RESET-ACK",
		`stale expected failure: GTPU-UNKNOWN-TEID-ERRIND EX-2: now PASS "Error Indication"` + "\n",
		"3 checks: 1 pass, 2 fail (1 expected), 0 deviation, 0 error, 0 skip\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if sum.OK() {
		t.Error("the unlisted FAIL did not fail the gate")
	}

	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ExpectedFail":1`, `"ExpectedFailures":"Example core (lab A)"`,
		`"Stale":[{"Check":"GTPU-UNKNOWN-TEID-ERRIND","Finding":"EX-2"`,
		`"expected_failure":{"finding":"EX-1","added":"2026-10-12","reason":"example: the AMF drops the message without ERROR INDICATION"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s:\n%s", want, b)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// orbit serve refuses to start with a bad list, before it listens.
func TestServeRefusesBadExpectedFailures(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	writeFile(t, bad, "core: c\n") // no version
	root := New("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"serve", "--listen", "127.0.0.1:0", "--state-dir", t.TempDir(), "--conformance-expected-failures", bad})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--conformance-expected-failures:") || !strings.Contains(err.Error(), "version is required") {
		t.Errorf("err = %v", err)
	}
}
