package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
)

// conformanceSpinnerEvery is how often the live spinner line repaints.
const conformanceSpinnerEvery = 100 * time.Millisecond

// conformanceSpinnerFrames are the frames the running check's spinner
// cycles through. They render on any UTF-8 terminal and are never emitted
// off one.
var conformanceSpinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

const (
	// progressCategoryWidth is the category column's width: the longest
	// category ("TS 23.501") fits.
	progressCategoryWidth = 9
	// progressDescWidth is the widest the description column and its
	// dotted leader get; a narrow terminal gets less.
	progressDescWidth = 60
	// progressDescMin is the narrowest the description column gets.
	progressDescMin = 20
	// progressTailWidth is the room kept after the leader for the status:
	// the spinner and elapsed time, or the verdict and duration.
	progressTailWidth = 28
	// progressLongCheck is the expected duration above which the running
	// line also shows how long the check can take.
	progressLongCheck = 30 * time.Second
	// progressPreflightWidth is the preflight steps' column and leader.
	progressPreflightWidth = 36
)

// runningCheck is the check on the spinner line.
type runningCheck struct {
	id       string
	category string
	summary  string
	index    int // 0-based
	total    int
	expected time.Duration
	began    time.Time
}

// streamProgress renders a server conformance run's progress to w (stderr)
// as each ConformanceUpdate arrives, one line per check in the style of a
// test runner: its number, category and description (a link to the check's
// page on the documentation site), a dotted leader, then a spinner while it
// runs and its verdict once it has one. A result that is neither PASS, INFO
// nor SKIP is followed by what was expected and observed and a link to its
// outcome's documentation. Off a terminal it stays quiet, leaving piped
// output unchanged.
//
// Writes to w are serialised under mu, shared by the ticker goroutine and
// the receive loop that calls update.
type streamProgress struct {
	w     io.Writer
	live  bool
	every time.Duration
	now   func() time.Time
	// width is the terminal's width in columns, 0 when unknown.
	width int
	re    *lipgloss.Renderer

	mu      sync.Mutex
	frame   int
	count   int    // checks completed so far
	total   uint32 // checks selected; 0 until the stream reports it
	painted bool   // a spinner line is currently on screen
	running *runningCheck
	// preflightShown and checksShown are whether the preflight and the
	// checks' headings have been printed.
	preflightShown bool
	checksShown    bool
	quit           chan struct{}
	wg             sync.WaitGroup
}

// newStreamProgress builds a progress renderer writing to w. live is whether
// to animate the spinner and print per-check lines (true only on a terminal).
func newStreamProgress(w io.Writer, live bool) *streamProgress {
	p := &streamProgress{w: w, live: live, every: conformanceSpinnerEvery, now: time.Now, re: lipgloss.NewRenderer(w)}
	if f, ok := w.(*os.File); ok {
		if cols, _, err := term.GetSize(int(f.Fd())); err == nil {
			p.width = cols
		}
	}
	return p
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
// a line per step, when an update first carries it. An update naming a
// started check puts that check on the spinner line; one carrying a result
// replaces the spinner line with the check's final line. A result whose
// start never arrived (an older server, or a check that finished before its
// start was sent) gets its final line all the same.
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
			p.heading("Preflight")
			for _, step := range steps {
				p.println("  " + p.leader(step.GetStep(), "", progressPreflightWidth) + " " + p.preflightStatus(step.GetStatus()))
			}
		}
	}
	if st := u.GetStarted(); st != nil {
		if st.GetTotal() != 0 {
			p.total = st.GetTotal()
		}
		p.running = &runningCheck{id: st.GetId(), category: conformance.Category(st.GetGroup()), summary: st.GetSummary(),
			index: int(st.GetIndex()), total: int(st.GetTotal()), expected: time.Duration(st.GetExpectedSeconds()) * time.Second,
			began: p.now()}
		if p.live && !p.checksShown {
			p.clearLocked()
			p.checksHeading()
		}
	}
	if r := u.GetResult(); r != nil {
		p.count++
		if p.live {
			p.clearLocked()
			p.checksHeading()
			p.println(p.resultLine(r, time.Duration(u.GetElapsedUs())*time.Microsecond))
			for _, l := range p.detailLines(r) {
				p.println(l)
			}
		}
		if p.running != nil && p.running.id == r.GetId() {
			p.running = nil
		}
	}
	if p.live {
		p.paintLocked()
	}
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

// heading prints a section heading ("[+] Running 41 checks").
func (p *streamProgress) heading(text string) {
	mark := p.re.NewStyle().Bold(true).Render("[+]")
	p.println(mark + " " + p.re.NewStyle().Foreground(lipgloss.Color("12")).Bold(true).Render(text))
}

// checksHeading prints the checks' heading once, before the first check
// line.
func (p *streamProgress) checksHeading() {
	if p.checksShown {
		return
	}
	p.checksShown = true
	if p.total != 0 {
		p.heading(fmt.Sprintf("Running %d checks", p.total))
		return
	}
	p.heading("Running checks")
}

// prefix is a check line's number and category: "  07/41: [ NGAP      ] ".
func (p *streamProgress) prefix(index, total int, category string) string {
	if total == 0 {
		total = int(p.total)
	}
	digits := len(fmt.Sprint(total))
	num := fmt.Sprintf("%0*d", digits, index+1)
	if total != 0 {
		num += fmt.Sprintf("/%d", total)
	}
	faint := p.re.NewStyle().Foreground(lipgloss.Color("8"))
	cat := p.re.NewStyle().Foreground(lipgloss.Color("10")).Render(padRight(truncate(category, progressCategoryWidth), progressCategoryWidth))
	return fmt.Sprintf("  %s: %s %s %s ", num, faint.Render("["), cat, faint.Render("]"))
}

// descWidth is the description column's width: progressDescWidth, less
// what a narrow terminal cannot fit, so the spinner line never wraps (a
// wrapped line cannot be repainted in place).
func (p *streamProgress) descWidth() int {
	if p.width == 0 {
		return progressDescWidth
	}
	used := 2 + 2*len(fmt.Sprint(p.total)) + 3 + progressCategoryWidth + 5 + progressTailWidth
	w := p.width - used
	switch {
	case w > progressDescWidth:
		return progressDescWidth
	case w < progressDescMin:
		return progressDescMin
	}
	return w
}

// leader is text, truncated to width and linked to url when url is set,
// followed by a dotted leader to width.
func (p *streamProgress) leader(text, url string, width int) string {
	text = truncate(text, width-2)
	dots := width - utf8.RuneCountInString(text) - 1
	shown := text
	if url != "" {
		shown = hyperlink(url, text)
	}
	return shown + " " + p.re.NewStyle().Foreground(lipgloss.Color("8")).Render(strings.Repeat(".", max(dots, 2)))
}

// describe is the description a check line shows: the check's summary, or
// its ID when it has none.
func describe(id, summary string) string {
	if summary == "" {
		summary = conformance.SummaryOf(id)
	}
	if summary == "" {
		return id
	}
	return summary
}

// resultLine is the final line of a completed check: its number, category,
// linked description, leader, verdict and how long it ran, as the server
// timed it (elapsed; 0 when it did not).
func (p *streamProgress) resultLine(r *orbitv1.ConformanceResult, elapsed time.Duration) string {
	index, total, took := p.count-1, int(p.total), ""
	if run := p.running; run != nil && run.id == r.GetId() {
		index, total = run.index, run.total
	}
	if elapsed > 0 {
		took = " " + p.re.NewStyle().Foreground(lipgloss.Color("8")).Render(fmtElapsed(elapsed))
	}
	v := conformance.Verdict(r.GetVerdict())
	tolerated := r.GetExpectedFailure() != nil
	verdict := verdictStyle(p.re, v, tolerated).Render(string(v))
	if tolerated {
		verdict += p.re.NewStyle().Foreground(lipgloss.Color("8")).Render(" (expected)")
	}
	desc := p.leader(describe(r.GetId(), r.GetSummary()), conformance.DocURL(r.GetId()), p.descWidth())
	return p.prefix(index, total, conformance.Category(r.GetGroup())) + desc + " " + verdict + took
}

// detailLines are the lines under a result that is not PASS, INFO or SKIP:
// the check's ID, what was expected and observed, and a link to its
// outcome's documentation.
func (p *streamProgress) detailLines(r *orbitv1.ConformanceResult) []string {
	switch conformance.Verdict(r.GetVerdict()) {
	case conformance.Pass, conformance.Info, conformance.Skip:
		return nil
	}
	label := p.re.NewStyle().Foreground(lipgloss.Color("6"))
	pad := strings.Repeat(" ", 7)
	var out []string
	add := func(k, v string) {
		if v != "" {
			out = append(out, pad+label.Render(k+":")+strings.Repeat(" ", 10-len(k))+v)
		}
	}
	add("check", r.GetId())
	add("expected", r.GetExpected())
	add("observed", r.GetObserved())
	url := conformance.OutcomeURL(r.GetId(), r.GetOutcome())
	add("details", hyperlink(url, url))
	return out
}

// preflightStatus renders a preflight step's status.
func (p *streamProgress) preflightStatus(status string) string {
	switch status {
	case "ok":
		return p.re.NewStyle().Foreground(lipgloss.Color("2")).Bold(true).Render(status)
	case "failed":
		return p.re.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render(status)
	}
	return p.re.NewStyle().Foreground(lipgloss.Color("8")).Render(status)
}

// statusLine is the spinner line's text: the running check with the
// spinner and its elapsed time, or, before any check has started, the
// spinner alone.
func (p *streamProgress) statusLine() string {
	frame := p.re.NewStyle().Foreground(lipgloss.Color("14")).Render(string(conformanceSpinnerFrames[p.frame%len(conformanceSpinnerFrames)]))
	run := p.running
	if run == nil {
		if !p.preflightShown {
			return "  " + frame + " preparing the run"
		}
		return "  " + frame + " starting"
	}
	elapsed := fmtElapsed(p.now().Sub(run.began))
	if run.expected > progressLongCheck {
		elapsed += " of up to " + run.expected.String()
	}
	desc := p.leader(describe(run.id, run.summary), conformance.DocURL(run.id), p.descWidth())
	return p.prefix(run.index, run.total, run.category) + desc + " " + frame + " " +
		p.re.NewStyle().Foreground(lipgloss.Color("8")).Render(elapsed)
}

// paintLocked writes the spinner line in place (carriage return, clear to end
// of line, no newline) and records that a line is on screen. Before any
// check has started and after the last has finished there is none to
// paint, unless nothing has finished yet. The caller holds mu.
func (p *streamProgress) paintLocked() {
	if !p.live || (p.running == nil && p.count > 0) {
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

// println writes one finished line. The caller holds mu and has cleared
// the spinner line.
func (p *streamProgress) println(s string) { _, _ = fmt.Fprintln(p.w, s) }

// hyperlink wraps text in an OSC 8 hyperlink to url; a terminal without
// OSC 8 shows text alone.
func hyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// truncate shortens s to at most n runes, ending it with "…" when cut.
func truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// padRight pads s with spaces to n runes.
func padRight(s string, n int) string {
	if c := utf8.RuneCountInString(s); c < n {
		return s + strings.Repeat(" ", n-c)
	}
	return s
}

// fmtElapsed renders a duration for a progress line: milliseconds under a
// second ("12ms"), tenths of a second under a minute ("4.2s"), whole
// seconds above ("2m56s").
func fmtElapsed(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}
