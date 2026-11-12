package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// conformanceSpinnerEvery is how often the live spinner line repaints.
const conformanceSpinnerEvery = 100 * time.Millisecond

// conformanceSpinnerFrames are the frames the live progress line cycles
// through. They render on any UTF-8 terminal and are never emitted off one.
var conformanceSpinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// streamProgress renders a server conformance run's progress to w (stderr) as
// each ConformanceUpdate arrives. On a terminal (live) it prints a line as each
// check's verdict lands and animates a one-line spinner carrying the running
// count; off a terminal it stays quiet, leaving piped output unchanged.
//
// Writes to w are serialised under mu, shared by the ticker goroutine and the
// receive loop that calls update.
type streamProgress struct {
	w     io.Writer
	live  bool
	every time.Duration

	mu      sync.Mutex
	frame   int
	count   int    // checks completed so far
	total   uint32 // checks selected; 0 until the stream reports it
	painted bool   // a spinner line is currently on screen
	// preflightShown is whether the run's preflight has been printed.
	preflightShown bool
	quit           chan struct{}
	wg             sync.WaitGroup
}

// newStreamProgress builds a progress renderer writing to w. live is whether
// to animate the spinner and print per-check lines (true only on a terminal).
func newStreamProgress(w io.Writer, live bool) *streamProgress {
	return &streamProgress{w: w, live: live, every: conformanceSpinnerEvery}
}

// start begins animating the spinner line. It is a no-op off a terminal.
func (p *streamProgress) start() {
	if !p.live {
		return
	}
	p.quit = make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(p.every)
		defer t.Stop()
		for {
			select {
			case <-p.quit:
				return
			case <-t.C:
				p.mu.Lock()
				p.frame++
				p.paintLocked()
				p.mu.Unlock()
			}
		}
	}()
}

// update renders one streamed update. The run's preflight is printed once,
// a line per step, when an update first carries it. When it carries a check result, the
// check's verdict is printed as its own line and the running count advances;
// the spinner line is cleared first and repainted after, so a completed-check
// line never lands in the middle of the spinner. The suite total is learned
// from the stream the first time it is reported.
func (p *streamProgress) update(u *orbitv1.ConformanceUpdate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := u.GetTotal(); t != 0 {
		p.total = t
	}
	if steps := u.GetPreflight(); len(steps) > 0 && !p.preflightShown {
		p.preflightShown = true
		if p.live {
			p.clearLocked()
			for _, step := range steps {
				_, _ = fmt.Fprintf(p.w, "preflight: %s %s\n", step.GetStep(), step.GetStatus())
			}
			p.paintLocked()
		}
	}
	r := u.GetResult()
	if r == nil {
		return
	}
	p.count++
	if !p.live {
		return
	}
	p.clearLocked()
	_, _ = fmt.Fprintf(p.w, "%s\n", p.checkLine(r))
	p.paintLocked()
}

// stop ends the spinner animation and clears the spinner line, so the final
// report starts on a clean line. It is safe to call when start did nothing.
func (p *streamProgress) stop() {
	if p.quit != nil {
		close(p.quit)
		p.wg.Wait()
		p.quit = nil
	}
	p.mu.Lock()
	p.clearLocked()
	p.mu.Unlock()
}

// checkLine is the one-line record of a completed check: its position, its
// verdict and its id.
func (p *streamProgress) checkLine(r *orbitv1.ConformanceResult) string {
	pos := fmt.Sprintf("%d", p.count)
	if p.total != 0 {
		pos = fmt.Sprintf("%d/%d", p.count, p.total)
	}
	return fmt.Sprintf("[%s] %-5s %s", pos, r.GetVerdict(), r.GetId())
}

// statusLine is the spinner line's text: the current frame and the running
// count.
func (p *streamProgress) statusLine() string {
	frame := conformanceSpinnerFrames[p.frame%len(conformanceSpinnerFrames)]
	if p.total != 0 {
		return fmt.Sprintf("%c running… %d/%d checks", frame, p.count, p.total)
	}
	return fmt.Sprintf("%c running… %d checks done", frame, p.count)
}

// paintLocked writes the spinner line in place (carriage return, clear to end
// of line, no newline) and records that a line is on screen. The caller holds
// mu.
func (p *streamProgress) paintLocked() {
	if !p.live {
		return
	}
	_, _ = fmt.Fprintf(p.w, "\r%s\033[K", p.statusLine())
	p.painted = true
}

// clearLocked erases the spinner line if one is on screen, leaving the cursor
// at the start of a clean line for the next write. The caller holds mu.
func (p *streamProgress) clearLocked() {
	if !p.painted {
		return
	}
	_, _ = fmt.Fprint(p.w, "\r\033[K")
	p.painted = false
}
