// Command orbit is the entry point for ORBIT — the Open Radio Benchmark and
// Integration Testbed. The binary serves the API (`orbit serve`) and drives
// it as a client (every other command); see docs/DESIGN.md for the
// architecture and phased plan.
package main

import (
	"fmt"
	"os"

	"github.com/bgrewell/orbit/internal/cli"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := cli.New(version).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
