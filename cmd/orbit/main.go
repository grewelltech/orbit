// Command orbit is the entry point for ORBIT — the Open Radio Benchmark and
// Integration Testbed. It is an API-first tool; the CLI here drives the engine
// through that API. See docs/DESIGN.md for the design, phased plan, and roadmap.
//
// Status: early — scaffolding only. The engine, API, and CLI land per the phased
// plan (Phase 0 onward).
package main

import "fmt"

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Printf("ORBIT %s — Open Radio Benchmark and Integration Testbed\n", version)
	fmt.Println("early development; see docs/DESIGN.md")
}
