package cli

import (
	"regexp"
	"strings"
	"testing"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// ansi matches the escape sequences the renderer writes: SGR colors, the
// erase-line sequence and OSC 8 hyperlink brackets.
var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]|\x1b\\]8;;[^\x1b]*\x1b\\\\")

// plain is the renderer's output without escape sequences, with carriage
// returns as line breaks, so each painted frame is its own line.
func plain(s string) string { return strings.ReplaceAll(ansi.ReplaceAllString(s, ""), "\r", "\n") }

// A live (terminal) renderer prints the preflight once, the checks'
// heading once, and one line per check with its number, category, summary
// and verdict; a result that is not PASS, INFO or SKIP gets its expected,
// observed and outcome link; a non-terminal renderer stays silent.
func TestStreamProgressUpdate(t *testing.T) {
	pre := []*orbitv1.PreflightStep{{Step: "NG Setup", Status: "ok"}}
	updates := []*orbitv1.ConformanceUpdate{
		{Total: 3, Preflight: pre},
		{Total: 3, Preflight: pre, Started: &orbitv1.ConformanceCheckStart{Id: "NGAP-NG-SETUP", Group: "NGAP / Interface Management Procedures / NG Setup", Summary: "NG Setup", Index: 0, Total: 3}},
		{Total: 3, Preflight: pre, ElapsedUs: 1500000, Result: &orbitv1.ConformanceResult{Id: "NGAP-NG-SETUP", Group: "NGAP / Interface Management Procedures / NG Setup", Verdict: "PASS"}},
		{Total: 3, Preflight: pre, Result: &orbitv1.ConformanceResult{Id: "GTPU-ECHO-RESPONSE", Group: "GTPv1-U / Path Management Messages / Echo Request",
			Verdict: "FAIL", Outcome: "no-echo-response", Expected: "an Echo Response", Observed: "none"}},
		{Total: 3, Result: &orbitv1.ConformanceResult{Id: "NAS-3", Verdict: "SKIP", Summary: "a check from a newer server"}},
		{Total: 3, Done: true, Tally: &orbitv1.ConformanceTally{Passed: 1, Failed: 1, Skipped: 1}},
	}

	t.Run("terminal", func(t *testing.T) {
		var w syncBuffer
		p := newStreamProgress(&w, true)
		p.start()
		for _, u := range updates {
			p.update(u)
		}
		p.stop()
		raw := w.String()
		out := plain(raw)
		for _, want := range []string{
			"[+] Preflight\n", "  NG Setup ...", " ok\n",
			"[+] Running 3 checks\n",
			"  1/3: [ NGAP      ] NG Setup ...", " PASS 1.5s\n",
			"  2/3: [ GTPv1-U   ] GTP-U Echo Request gets an Echo Response ...", " FAIL\n",
			"check:     GTPU-ECHO-RESPONSE\n",
			"expected:  an Echo Response\n",
			"observed:  none\n",
			"details:   https://orbit.grewelltech.com/conformance/checks/gtpu-echo-response/#no-echo-response\n",
			"  3/3: [           ] a check from a newer server ...", " SKIP\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("progress output lacks %q:\n%s", want, out)
			}
		}
		for heading, n := range map[string]int{"[+] Preflight": 1, "[+] Running": 1, "check: ": 1} {
			if got := strings.Count(out, heading); got != n {
				t.Errorf("%q appears %d times, want %d:\n%s", heading, got, n, out)
			}
		}
		if !strings.Contains(raw, "\x1b]8;;https://orbit.grewelltech.com/conformance/checks/ngap-ng-setup/\x1b\\NG Setup\x1b]8;;\x1b\\") {
			t.Errorf("the summary is not a link to the check's page:\n%q", raw)
		}
	})
	t.Run("non-terminal stays quiet", func(t *testing.T) {
		var w syncBuffer
		p := newStreamProgress(&w, false)
		p.start()
		for _, u := range updates {
			p.update(u)
		}
		p.stop()
		if w.String() != "" {
			t.Fatalf("non-terminal progress must be silent, got:\n%q", w.String())
		}
	})
}

// Only PASS, INFO and SKIP go without the detail lines; every other
// verdict gets them.
func TestStreamProgressDetailLines(t *testing.T) {
	p := newStreamProgress(&syncBuffer{}, true)
	for _, tc := range []struct {
		verdict string
		want    bool
	}{
		{"PASS", false}, {"INFO", false}, {"SKIP", false},
		{"FAIL", true}, {"DEVIATION", true}, {"WARN", true}, {"BLOCKED", true}, {"CRASH", true}, {"ERROR", true},
	} {
		t.Run(tc.verdict, func(t *testing.T) {
			lines := p.detailLines(&orbitv1.ConformanceResult{Id: "NGAP-NG-SETUP", Verdict: tc.verdict})
			if got := len(lines) > 0; got != tc.want {
				t.Fatalf("detail lines for %s: %q", tc.verdict, lines)
			}
			if tc.want && plain(lines[len(lines)-1]) != "       details:   https://orbit.grewelltech.com/conformance/checks/ngap-ng-setup/" {
				t.Errorf("without an outcome the link is the check's page: %q", lines)
			}
		})
	}
}

// statusLine is the spinner text: the running check with its elapsed time,
// and how long it can take when that is long; before any check, what the
// run is doing.
func TestStreamProgressStatusLine(t *testing.T) {
	began := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		running   *runningCheck
		preflight bool
		want      string
		notWant   string
	}{
		{name: "before the preflight", want: "preparing the run"},
		{name: "after the preflight", preflight: true, want: "starting"},
		{name: "short check", running: &runningCheck{id: "NGAP-NG-SETUP", category: "NGAP", summary: "NG Setup", index: 4, total: 41, expected: 15 * time.Second, began: began},
			want: "05/41: [ NGAP      ] NG Setup ...", notWant: "up to"},
		{name: "long check", running: &runningCheck{id: "NAS-T3550-ACCEPT-RETRANSMIT", category: "NAS", summary: "s", index: 0, total: 2, expected: 145 * time.Second, began: began},
			want: "12.0s of up to 2m25s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newStreamProgress(&syncBuffer{}, true)
			p.now = func() time.Time { return began.Add(12 * time.Second) }
			p.total = 41
			p.running = tc.running
			p.preflightShown = tc.preflight
			got := plain(p.statusLine())
			if !strings.Contains(got, tc.want) {
				t.Errorf("statusLine() = %q, want it to contain %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("statusLine() = %q, must not contain %q", got, tc.notWant)
			}
		})
	}
}

// A narrow terminal narrows the description column so the spinner line
// does not wrap; it never goes below the minimum or above the maximum.
func TestStreamProgressDescWidth(t *testing.T) {
	for _, tc := range []struct{ cols, want int }{
		{0, progressDescWidth}, {300, progressDescWidth}, {100, 100 - (2 + 4 + 3 + progressCategoryWidth + 5 + progressTailWidth)}, {40, progressDescMin},
	} {
		p := newStreamProgress(&syncBuffer{}, true)
		p.width, p.total = tc.cols, 41
		if got := p.descWidth(); got != tc.want {
			t.Errorf("descWidth at %d columns = %d, want %d", tc.cols, got, tc.want)
		}
	}
}

// truncate cuts on runes and marks the cut; fmtElapsed shows milliseconds
// under a second, tenths under a minute and whole seconds above.
func TestProgressHelpers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{{"abcdef", 4, "abc…"}, {"abc", 3, "abc"}, {"héllo", 2, "h…"}, {"x", 0, "x"}} {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{0, "0ms"}, {12 * time.Millisecond, "12ms"}, {1540 * time.Millisecond, "1.5s"}, {176 * time.Second, "2m56s"}} {
		if got := fmtElapsed(tc.d); got != tc.want {
			t.Errorf("fmtElapsed(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
	if got := describe("NGAP-NG-SETUP", ""); got != "NG Setup" {
		t.Errorf("describe falls back to the local summary: %q", got)
	}
	if got := describe("NO-SUCH-CHECK", ""); got != "NO-SUCH-CHECK" {
		t.Errorf("describe falls back to the ID: %q", got)
	}
}

// stop clears the spinner line (carriage return + erase) so the final report
// starts clean, and is safe when start was a no-op (non-terminal).
func TestStreamProgressStopClears(t *testing.T) {
	t.Run("terminal clears the spinner", func(t *testing.T) {
		var w syncBuffer
		p := newStreamProgress(&w, true)
		p.start()
		p.update(&orbitv1.ConformanceUpdate{Total: 1, Started: &orbitv1.ConformanceCheckStart{Id: "X-1", Total: 1}})
		p.stop()
		if !strings.HasSuffix(w.String(), "\r\033[K") {
			t.Errorf("stop must erase the spinner line, got tail:\n%q", w.String())
		}
	})
	t.Run("non-terminal stop is a no-op", func(t *testing.T) {
		var w syncBuffer
		p := newStreamProgress(&w, false)
		p.start()
		p.stop() // must not panic or write anything
		if w.String() != "" {
			t.Errorf("non-terminal stop wrote %q", w.String())
		}
	})
}

// A live renderer animates the spinner on its own ticker even when no updates
// arrive, and writes nothing more once stopped.
func TestStreamProgressSpinnerTicks(t *testing.T) {
	var w syncBuffer
	p := newStreamProgress(&w, true)
	p.every = 2 * time.Millisecond // tick fast so the test is quick
	p.start()

	// Bounded wait for the ticker to paint at least one spinner frame.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.ContainsAny(w.String(), string(conformanceSpinnerFrames)) {
		if time.Now().After(deadline) {
			p.stop()
			t.Fatal("spinner did not paint a frame within the timeout")
		}
		time.Sleep(2 * time.Millisecond)
	}

	p.stop()
	// The ticker goroutine has exited: no further bytes are written.
	settled := len(w.String())
	time.Sleep(20 * time.Millisecond)
	if n := len(w.String()); n != settled {
		t.Errorf("writes continued after stop(): %d -> %d bytes", settled, n)
	}
}
