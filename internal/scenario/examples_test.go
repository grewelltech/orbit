package scenario

import (
	"os"
	"path/filepath"
	"testing"
)

// knownBrokenExamples are shipped examples that fail to parse for a reason
// tracked elsewhere; each entry names its issue so the skip is removed with
// the fix.
var knownBrokenExamples = map[string]string{
	"fleet-testbed-attach-rate.yaml": "#130: xn handover enabled without pdu_session (pre-existing)",
}

// Every scenario shipped under examples/ parses with the parser for its kind
// (step scenarios through Parse, kind: fleet through ParseFleet), with the
// ${ORBIT_*} credentials it references set, so a shipped example that drifts
// from the validation rules (for instance SUPIs outside its PLMN) fails CI
// instead of failing an operator at run time.
func TestEveryShippedExampleParses(t *testing.T) {
	t.Setenv("ORBIT_KI", "00112233445566778899aabbccddeeff")
	t.Setenv("ORBIT_OPC", "000102030405060708090a0b0c0d0e0f")
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("found %d examples; the path is wrong", len(files))
	}
	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			if why, ok := knownBrokenExamples[name]; ok {
				t.Skip(why)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			kind, err := PeekKind(data)
			if err != nil {
				t.Fatalf("kind: %v", err)
			}
			if kind == "fleet" {
				if _, err := ParseFleet(data); err != nil {
					t.Errorf("ParseFleet: %v", err)
				}
				return
			}
			if _, err := Parse(data); err != nil {
				t.Errorf("Parse: %v", err)
			}
		})
	}
}
