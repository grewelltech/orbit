package conformance

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
)

// outcomeKey identifies a declared outcome for the coverage guard: its ID
// and summary, which together are particular to one check.
func outcomeKey(o Outcome) string { return o.ID + "\x00" + o.Summary }

// TestMain runs the package's tests with every outcome a result is built
// with recorded, and then fails a full run in which a declared outcome of a
// registered check was never built: every declared outcome has a test. A
// filtered run (-run, -skip, -short, -list) is not held to it.
func TestMain(m *testing.M) {
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	record := func(o Outcome) {
		mu.Lock()
		seen[outcomeKey(o)] = true
		mu.Unlock()
	}
	outcomeObserver.Store(&record)
	code := m.Run()
	if code == 0 && fullRun() {
		if missing := unexercised(seen); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: %d declared outcomes have no test that reports them:\n", len(missing))
			for _, line := range missing {
				fmt.Fprintln(os.Stderr, "  "+line)
			}
			code = 1
		}
	}
	os.Exit(code)
}

// covers records o as exercised, for a test that judges an outcome a
// classifier returned without building a Result from it.
func covers(o Outcome) Outcome {
	noteExercised(o)
	return o
}

// fullRun reports whether the test binary runs every test.
func fullRun() bool {
	for _, name := range []string{"test.run", "test.skip", "test.list"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return false
		}
	}
	return !testing.Short()
}

// unexercised lists the declared outcomes of the registered checks that
// seen lacks, as "CHECK-ID outcome-id (VERDICT)".
func unexercised(seen map[string]bool) []string {
	var out []string
	for _, c := range NewRegistry().Tests() {
		d, ok := c.(Declarer)
		if !ok {
			continue
		}
		for _, o := range d.Outcomes() {
			if !seen[outcomeKey(o)] {
				out = append(out, fmt.Sprintf("%s %s (%s)", c.ID(), o.ID, o.Verdict))
			}
		}
	}
	sort.Strings(out)
	return out
}
