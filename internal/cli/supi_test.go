package cli

import (
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
