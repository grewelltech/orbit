package conformance

import (
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is a valid list and uses only synthetic findings.
func TestLoadExpectedFailuresFixture(t *testing.T) {
	x, err := LoadExpectedFailures(filepath.Join("testdata", "expected-failures.yaml"))
	if err != nil {
		t.Fatalf("LoadExpectedFailures: %v", err)
	}
	if x.Core != "Example core (lab A)" || len(x.Entries) != 2 {
		t.Fatalf("loaded %+v", x)
	}
	for _, e := range x.Entries {
		if !strings.HasPrefix(e.Finding, "EX-") || !strings.HasPrefix(e.Reason, "example:") {
			t.Errorf("fixture entry %+v is not synthetic", e)
		}
	}
}

// Loading refuses anything that could match loosely or not at all, naming
// the file and the entry.
func TestParseExpectedFailures(t *testing.T) {
	known := map[string]bool{"NGAP-UNKNOWN-UE-SURVIVES": true, "GTPU-UNKNOWN-TEID-ERRIND": true}
	const head = "core: c\nversion: v\n"
	entry := func(fields ...string) string {
		return "entries:\n  - " + strings.Join(fields, "\n    ") + "\n"
	}
	full := []string{"check: NGAP-UNKNOWN-UE-SURVIVES", `observed: "silently dropped; association survived"`,
		"finding: EX-1", "added: 2026-10-12", `reason: "example"`}
	without := func(i int) []string { return append(append([]string{}, full[:i]...), full[i+1:]...) }
	tests := []struct {
		name    string
		yaml    string
		wantErr string
		entries int
	}{
		{name: "valid", yaml: head + entry(full...), entries: 1},
		{name: "quoted date", yaml: head + entry(full[0], full[1], full[2], `added: "2026-10-12"`, full[4]), entries: 1},
		{name: "empty entries", yaml: head + "entries: []\n"},
		{name: "no entries key", yaml: head},
		{name: "missing core", yaml: "version: v\n", wantErr: "f.yaml: core is required"},
		{name: "empty file", yaml: "", wantErr: "f.yaml: core is required"},
		{name: "missing version", yaml: "core: c\n", wantErr: "f.yaml: version is required"},
		{name: "missing check", yaml: head + entry(without(0)...), wantErr: "f.yaml: entry 1: check is required"},
		{name: "missing observed", yaml: head + entry(without(1)...), wantErr: "entry 1: observed is required"},
		{name: "missing finding", yaml: head + entry(without(2)...), wantErr: "entry 1: finding is required"},
		{name: "missing added", yaml: head + entry(without(3)...), wantErr: "entry 1: added is required"},
		{name: "missing reason", yaml: head + entry(without(4)...), wantErr: "entry 1: reason is required"},
		{name: "empty observed", yaml: head + entry(full[0], `observed: " "`, full[2], full[3], full[4]), wantErr: "observed is required"},
		{name: "bad date", yaml: head + entry(full[0], full[1], full[2], "added: 12/10/2026", full[4]), wantErr: `entry 1: added "12/10/2026" is not a YYYY-MM-DD date`},
		{name: "unknown check", yaml: head + entry("check: NGAP-NOPE", full[1], full[2], full[3], full[4]), wantErr: `entry 1: unknown check "NGAP-NOPE"`},
		{name: "duplicate", yaml: head + entry(full...) + "  - " + strings.Join(full, "\n    ") + "\n", wantErr: "entry 2: duplicates entry 1"},
		{name: "same check, other observed", yaml: head + entry(full...) + "  - " + strings.Join(append([]string{full[0], `observed: "association reset"`}, full[2:]...), "\n    ") + "\n", entries: 2},
		{name: "unknown key in entry", yaml: head + entry(append(full, "severity: low")...), wantErr: "field severity not found"},
		{name: "unknown top-level key", yaml: head + "owner: x\n", wantErr: "field owner not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, err := parseExpectedFailures("f.yaml", []byte(tc.yaml), known)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(x.Entries) != tc.entries {
				t.Errorf("entries = %d, want %d", len(x.Entries), tc.entries)
			}
		})
	}
}

// An entry tolerates exactly its (check, observed) FAIL. It never hides
// another failure mode of the check, an ERROR or a crash; stale entries are
// reported but do not gate; SKIP is neither matched nor stale.
func TestExpectedFailuresApplyAndGate(t *testing.T) {
	x := &ExpectedFailures{Core: "Example core", Version: "v", Entries: []ExpectedFailureEntry{
		{Check: "A", Observed: "silently dropped; association survived", Finding: "EX-1", Added: "2026-10-12", Reason: "example"},
	}}
	res := func(v Verdict, observed string) []Result { return []Result{{ID: "A", Verdict: v, Observed: observed}} }
	tests := []struct {
		name      string
		results   []Result
		wantMark  bool
		wantStale bool
		wantOK    bool
	}{
		{name: "exact match", results: res(Fail, "silently dropped; association survived"), wantMark: true, wantOK: true},
		{name: "other failure mode", results: res(Fail, "association reset"), wantStale: true},
		{name: "core died after the drop", results: res(Fail, "AMF unresponsive after the message (silently dropped; association survived)"), wantStale: true},
		{name: "ERROR with the same text", results: res(Error, "silently dropped; association survived")},
		{name: "now passes", results: res(Pass, "ERROR INDICATION"), wantStale: true, wantOK: true},
		{name: "now deviates", results: res(Deviation, "x"), wantStale: true, wantOK: true},
		{name: "skipped", results: res(Skip, "not applicable"), wantOK: true},
		{name: "not selected", results: []Result{{ID: "B", Verdict: Pass}}, wantOK: true},
		{name: "a second, unlisted FAIL still gates", results: append(res(Fail, "silently dropped; association survived"),
			Result{ID: "B", Verdict: Fail, Observed: "silently dropped; association survived"}), wantMark: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x.Apply(tc.results)
			r := tc.results[0]
			if (r.ExpectedFailure != nil) != tc.wantMark {
				t.Errorf("marked = %v, want %v", r.ExpectedFailure != nil, tc.wantMark)
			}
			if r.ExpectedFailure != nil && (r.Verdict != Fail || r.ExpectedFailure.Finding != "EX-1" || r.ExpectedFailure.Added != "2026-10-12") {
				t.Errorf("marked result %+v", r)
			}
			for _, o := range tc.results[1:] {
				if o.ExpectedFailure != nil {
					t.Errorf("result for another check marked: %+v", o)
				}
			}
			stale := x.Stale(tc.results)
			if (len(stale) == 1) != tc.wantStale || len(stale) > 1 {
				t.Errorf("stale = %+v, want stale %v", stale, tc.wantStale)
			}
			if len(stale) == 1 && (stale[0].Finding != "EX-1" || stale[0].Verdict != r.Verdict || stale[0].Observed != r.Observed) {
				t.Errorf("stale entry %+v", stale[0])
			}
			s := Summarize(tc.results)
			if s.OK() != tc.wantOK {
				t.Errorf("OK = %v (fail %d, expected %d, error %d), want %v", s.OK(), s.Fail, s.ExpectedFail, s.Error, tc.wantOK)
			}
			if s.ExpectedFail > s.Fail {
				t.Errorf("ExpectedFail %d exceeds Fail %d", s.ExpectedFail, s.Fail)
			}
		})
	}
}

// A nil list matches nothing and every FAIL gates.
func TestExpectedFailuresNil(t *testing.T) {
	var x *ExpectedFailures
	results := []Result{{ID: "A", Verdict: Fail, Observed: "o"}}
	x.Apply(results)
	if results[0].ExpectedFailure != nil || x.Stale(results) != nil || x.Label() != "" {
		t.Error("a nil list matched or reported something")
	}
	if Summarize(results).OK() {
		t.Error("an unlisted FAIL passed the gate")
	}
}
