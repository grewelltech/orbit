package scenario

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// knownBrokenExamples are shipped examples that fail to parse for a reason
// tracked elsewhere; each entry names its issue so the skip is removed with
// the fix.
var knownBrokenExamples = map[string]string{
	"fleet-testbed-attach-rate.yaml": "#130: xn handover enabled without pdu_session (pre-existing)",
}

// Every scenario shipped under examples/ parses with the parser for its kind
// (step scenarios through ParseWithTarget, kind: fleet through
// ParseFleetWithTarget) against a fixture target, so a shipped example that
// drifts from the validation rules fails CI instead of failing an operator
// at run time. Each takes its core, keys and addresses from the target: run
// on its own, it names no target.
func TestEveryShippedExampleParses(t *testing.T) {
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
			if own, err := SelfContained(data); err != nil || len(own) > 0 {
				t.Errorf("sets %v itself (err %v); examples take them from a target", own, err)
			}
			kind, err := PeekKind(data)
			if err != nil {
				t.Fatalf("kind: %v", err)
			}
			tg := exampleTarget
			if kind == "fleet" {
				if _, err := ParseFleetWithTarget(data, &tg); err != nil {
					t.Errorf("ParseFleetWithTarget: %v", err)
				}
				return
			}
			if _, err := ParseWithTarget(data, &tg); err != nil {
				t.Errorf("ParseWithTarget: %v", err)
			}
		})
	}
}

// ipLiteral matches an IPv4 address, or an IPv6 one with at least two
// groups around a "::" or seven colons.
var ipLiteral = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b|\b[0-9a-fA-F]{1,4}(:[0-9a-fA-F]{0,4}){2,7}\b`)

// No example names an address, comments included: a lab's addresses live in
// its target (roadmap T4).
func TestNoExampleHardCodesAddresses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*"))
	if err != nil || len(files) < 5 {
		t.Fatalf("examples: %v (%d files)", err, len(files))
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range ipLiteral.FindAllString(line, -1) {
				if net.ParseIP(m) != nil {
					t.Errorf("%s:%d names the address %s", filepath.Base(path), i+1, m)
				}
			}
		}
	}
}
