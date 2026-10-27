package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every --supi and --base-imsi flag names the two accepted spellings through
// the shared help text, so an operator sees "digits or imsi-<digits>" on
// every command that takes a SUPI.
func TestEverySUPIFlagNamesBothForms(t *testing.T) {
	var seen int
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		for _, name := range []string{"supi", "base-imsi"} {
			f := c.Flags().Lookup(name)
			if f == nil {
				continue
			}
			seen++
			if !strings.Contains(f.Usage, supiFlagHelp) {
				t.Errorf("%s --%s: help %q does not carry the shared SUPI text", path, name, f.Usage)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(New("test"), "orbit")
	if seen < 12 {
		t.Fatalf("found only %d SUPI flags; the walk is broken", seen)
	}
}

// The in-process `load` refuses a malformed --base-imsi with the shared
// message before any association is opened (the AMF here is a refused
// loopback port, so a dial would have shown as "sctp dial ..."), and accepts
// the imsi- spelling, which then fails only at that dial.
func TestLoadBaseIMSIValidatedBeforeDialing(t *testing.T) {
	tests := []struct {
		name     string
		baseIMSI string
		want     string
		wantNot  string
		wantCode int
	}{
		// Bad input is refused before the run: could not start (3).
		{name: "too short refused", baseIMSI: "2089", want: `base IMSI: invalid SUPI "2089": want 5-15 digits or imsi-<digits>`, wantNot: "sctp dial", wantCode: ExitCouldNotStart},
		{name: "upper-case prefix refused", baseIMSI: "IMSI-208930100000000", want: `invalid SUPI "IMSI-208930100000000"`, wantNot: "sctp dial", wantCode: ExitCouldNotStart},
		// A run that started and failed did not complete (2).
		{name: "nai accepted, fails at the dial", baseIMSI: "imsi-208930100000000", want: "sctp dial", wantNot: "invalid SUPI", wantCode: ExitDidNotComplete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := New("test")
			var out, errOut bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			root.SetArgs([]string{"load", "--amf", "127.0.0.1:1", "--base-imsi", tc.baseIMSI,
				"--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f",
				"--mcc", "208", "--mnc", "93", "--gnb-count", "1", "--count", "1", "--progress-every", "0"})
			err := root.Execute()
			if err == nil {
				t.Fatal("load against a refused loopback AMF succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.wantNot) {
				t.Errorf("err = %v, must not contain %q", err, tc.wantNot)
			}
			if got := ExitCode(err); got != tc.wantCode {
				t.Errorf("exit %d, want %d", got, tc.wantCode)
			}
		})
	}
}
