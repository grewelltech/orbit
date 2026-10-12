// Command gendocs writes docs/conformance/checks.md from the conformance
// registry. Run it through go generate ./internal/conformance.
package main

import (
	"fmt"
	"os"

	"github.com/bgrewell/orbit/internal/conformance"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gendocs <output.md>")
		os.Exit(2)
	}
	md := conformance.ChecksMarkdown(conformance.NewRegistry().Tests())
	if err := os.WriteFile(os.Args[1], []byte(md), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
