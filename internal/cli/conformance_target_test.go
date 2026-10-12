package cli

import (
	"bytes"
	"encoding/json"
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

// --json records the target release, "unset" by default, and each result
// carries spec_refs and min_release.
func TestConformanceJSONRecordsTargetRelease(t *testing.T) {
	out, _ := runConformance(t, "--json", "--category", "procedural")
	var sum struct {
		TargetRelease string
		Results       []struct {
			SpecRefs   []map[string]any `json:"spec_refs"`
			MinRelease string           `json:"min_release"`
			Detail     string           `json:"detail"`
		}
	}
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out)
	}
	if sum.TargetRelease != "unset" {
		t.Errorf("TargetRelease = %q, want unset", sum.TargetRelease)
	}
	if len(sum.Results) == 0 {
		t.Fatal("no results")
	}
	for _, r := range sum.Results {
		if len(r.SpecRefs) == 0 || r.MinRelease != "Rel-15" || !strings.Contains(r.Detail, "requires Rel-15 or later") {
			t.Errorf("result lacks provenance: %+v", r)
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
