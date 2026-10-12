package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func runConformance(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := New("test")
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"conformance", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01", "--per-test-timeout", "2s"}, args...))
	err := root.Execute()
	return out.String(), err
}

// --target-release older than every check SKIPs the suite without dialling
// (exit 0: SKIP does not fail the gate), and the text output names the
// target and, for each check, the release it requires and its clauses.
func TestConformanceTargetReleaseSkipsAndPrintsRequirement(t *testing.T) {
	out, err := runConformance(t, "--target-release", "Rel-14")
	if err != nil {
		t.Fatalf("conformance: %v\n%s", err, out)
	}
	if !strings.Contains(out, "target-release: Rel-14") {
		t.Errorf("output lacks the target line:\n%s", out)
	}
	for _, id := range []string{"NGAP-UNKNOWN-UE-SURVIVES", "NGAP-NG-RESET-ACK", "NGAP-NGSETUP-MISSING-TALIST"} {
		if !strings.Contains(out, id) {
			t.Errorf("output lacks %s", id)
		}
	}
	if strings.Count(out, "requires Rel-15 or later; TS ") < 3 || strings.Contains(out, "FAIL ") {
		t.Errorf("each check must print its requirement line and none may FAIL:\n%s", out)
	}
	if !strings.Contains(out, "0 fail") {
		t.Errorf("summary line:\n%s", out)
	}
}

// --json records the targets: Rel-18 with the TS 29.244 pin by default,
// "unset" with no pins for --target-release unset, and an explicit target
// with the pin when it still narrows. Each result carries spec_refs and
// min_release.
func TestConformanceJSONRecordsTargetRelease(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantTarget string
		wantPins   map[string]string
	}{
		{name: "default", wantTarget: "Rel-18", wantPins: map[string]string{"TS 29.244": "Rel-16"}},
		{name: "unset", args: []string{"--target-release", "unset"}, wantTarget: "unset", wantPins: map[string]string{}},
		{name: "explicit", args: []string{"--target-release", "17"}, wantTarget: "Rel-17", wantPins: map[string]string{"TS 29.244": "Rel-16"}},
		{name: "explicit below the pin", args: []string{"--target-release", "Rel-15"}, wantTarget: "Rel-15", wantPins: map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runConformance(t, append([]string{"--json", "--category", "procedural"}, tc.args...)...)
			var sum struct {
				TargetRelease string
				SpecTargets   map[string]string `json:"spec_targets"`
				Results       []struct {
					SpecRefs   []map[string]any `json:"spec_refs"`
					MinRelease string           `json:"min_release"`
					Detail     string           `json:"detail"`
				}
			}
			if err := json.Unmarshal([]byte(out), &sum); err != nil {
				t.Fatalf("JSON: %v\n%s", err, out)
			}
			if sum.TargetRelease != tc.wantTarget {
				t.Errorf("TargetRelease = %q, want %q", sum.TargetRelease, tc.wantTarget)
			}
			if sum.SpecTargets == nil || !reflect.DeepEqual(sum.SpecTargets, tc.wantPins) {
				t.Errorf("spec_targets = %v, want %v", sum.SpecTargets, tc.wantPins)
			}
			if len(sum.Results) == 0 {
				t.Fatal("no results")
			}
			for _, r := range sum.Results {
				if len(r.SpecRefs) == 0 || r.MinRelease != "Rel-15" || !strings.Contains(r.Detail, "requires Rel-15 or later") {
					t.Errorf("result lacks provenance: %+v", r)
				}
			}
		})
	}
}

// The text output states the targets the run used.
func TestConformanceTextTargetLine(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "target-release: Rel-18 (TS 29.244 at Rel-16)\n"},
		{[]string{"--target-release", "unset"}, "target-release: unset\n"},
		{[]string{"--target-release", "17"}, "target-release: Rel-17 (TS 29.244 at Rel-16)\n"},
	} {
		out, _ := runConformance(t, append([]string{"--category", "procedural"}, tc.args...)...)
		if !strings.Contains(out, tc.want) {
			t.Errorf("%v: output lacks %q:\n%s", tc.args, tc.want, out)
		}
	}
}

// A malformed --target-release is refused before anything runs.
func TestConformanceTargetReleaseRejectsMalformed(t *testing.T) {
	_, err := runConformance(t, "--target-release", "fifteen")
	if err == nil || !strings.Contains(err.Error(), "--target-release: invalid release") {
		t.Errorf("err = %v", err)
	}
}
