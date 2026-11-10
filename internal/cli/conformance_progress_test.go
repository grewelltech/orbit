package cli

import (
	"strings"
	"testing"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// A live (terminal) renderer prints a per-check line as each verdict lands and
// no line for the final done/no-result update; a non-terminal renderer stays
// silent.
func TestStreamProgressUpdate(t *testing.T) {
	updates := []*orbitv1.ConformanceUpdate{
		{Total: 3, Result: &orbitv1.ConformanceResult{Id: "AUTH-1", Verdict: "PASS"}},
		{Total: 3, Result: &orbitv1.ConformanceResult{Id: "SEC-2", Verdict: "FAIL"}},
		{Total: 3, Result: &orbitv1.ConformanceResult{Id: "NAS-3", Verdict: "SKIP"}},
		{Total: 3, Done: true, Tally: &orbitv1.ConformanceTally{Passed: 1, Failed: 1, Skipped: 1}},
	}

	tests := []struct {
		name string
		live bool
		want []string // substrings that must appear on stderr
	}{
		{
			name: "terminal prints a line per verdict",
			live: true,
			want: []string{"[1/3]", "AUTH-1", "PASS", "[2/3]", "SEC-2", "FAIL", "[3/3]", "NAS-3", "SKIP"},
		},
		{
			name: "non-terminal stays quiet",
			live: false,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var w syncBuffer
			p := newStreamProgress(&w, tc.live)
			p.start()
			for _, u := range updates {
				p.update(u)
			}
			p.stop()
			out := w.String()
			if !tc.live {
				if out != "" {
					t.Fatalf("non-terminal progress must be silent, got:\n%q", out)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("progress output lacks %q:\n%q", want, out)
				}
			}
			// Exactly three per-check lines (each newline-terminated); the
			// final done/no-result update must not render as a fourth.
			if n := strings.Count(out, "\n"); n != 3 {
				t.Errorf("want exactly 3 per-check lines, got %d:\n%q", n, out)
			}
		})
	}
}

// statusLine is the spinner text and is deterministic given the renderer's
// counters, so its two forms (total known / unknown) can be asserted without
// the ticker.
func TestStreamProgressStatusLine(t *testing.T) {
	tests := []struct {
		name  string
		total uint32
		count int
		want  string
	}{
		{name: "total known", total: 48, count: 12, want: "running… 12/48 checks"},
		{name: "total unknown", total: 0, count: 5, want: "running… 5 checks done"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newStreamProgress(&syncBuffer{}, true)
			p.total = tc.total
			p.count = tc.count
			if got := p.statusLine(); !strings.Contains(got, tc.want) {
				t.Errorf("statusLine() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// stop clears the spinner line (carriage return + erase) so the final report
// starts clean, and is safe when start was a no-op (non-terminal).
func TestStreamProgressStopClears(t *testing.T) {
	t.Run("terminal clears the spinner", func(t *testing.T) {
		var w syncBuffer
		p := newStreamProgress(&w, true)
		p.start()
		p.update(&orbitv1.ConformanceUpdate{Total: 1, Result: &orbitv1.ConformanceResult{Id: "X-1", Verdict: "PASS"}})
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
