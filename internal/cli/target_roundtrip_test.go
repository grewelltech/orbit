package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/bgrewell/orbit/internal/target"
)

var updateRoundTrip = flag.Bool("update-roundtrip", false, "rewrite the target files the dashboard's round-trip tests read")

// roundTripDir holds the target files `orbit target init` writes, which the
// dashboard imports and exports byte for byte (web/src/data/targets), and
// the files the dashboard writes, which internal/target loads.
var roundTripDir = filepath.Join("..", "target", "testdata", "roundtrip", "cli")

// The CLI's own target files, as init writes them: the dashboard's
// round-trip tests import exactly these. A change to init's output fails
// here until they are rewritten (-update-roundtrip), and the dashboard's
// tests then run against the new output.
func TestRoundTripFixtures(t *testing.T) {
	clearOrbitEnv(t)
	base := []string{"--no-probe", "--bind", "10.0.0.5", "--amf", "10.0.0.1:38412", "--mcc", "001", "--mnc", "01"}
	tests := []struct {
		name string
		args []string
	}{
		{name: "minimal", args: base},
		{name: "keys-as-vars", args: append(append([]string(nil), base...), "--sd", "010203", "--first-supi", "001010000000001", "--count", "5",
			"--responder", "n6=203.0.113.20:9551,203.0.113.21")},
		{name: "keys-written", args: append(append([]string(nil), base...), "--sst", "2", "--tac", "7", "--dnn", "ims", "--gnb-n3", "10.0.0.6",
			"--first-supi", "001010000000001", "--write-keys", "--ki", testKi, "--opc", testOPc)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			loc := target.Locations{Dir: dir, ConfigDir: filepath.Join(dir, "config")}
			r := runOrbit(t, loc, append([]string{"target", "init", tc.name}, tc.args...)...)
			if r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			got, err := os.ReadFile(filepath.Join(loc.ConfigDir, "targets", tc.name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(roundTripDir, tc.name+".yaml")
			if *updateRoundTrip {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("init writes a different %s now (go test ./internal/cli -run TestRoundTripFixtures -update-roundtrip):\n%s", path, got)
			}
		})
	}
}
