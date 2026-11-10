package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"golang.org/x/term"

	"github.com/bgrewell/orbit/internal/conformance"
)

// writeConformanceReport renders a conformance run for the terminal. On an
// interactive terminal it draws a colored, box-drawn table mirroring the web
// dashboard's SuiteResults (verdict · test · category · spec · observed) with
// a tally footer; otherwise — a pipe, a file, a CI log, or a test buffer — it
// falls back to the plain borderless text of printConformanceText, so scripts
// and golden tests see unchanged output. --json never reaches here.
func writeConformanceReport(out io.Writer, network string, sum conformance.Summary, expectedPath string) {
	if !isTerminal(out) {
		printConformanceText(out, network, sum, expectedPath)
		return
	}
	printConformanceTable(out, network, sum, expectedPath)
}

// isTerminal reports whether w is an interactive terminal. A bytes.Buffer or a
// redirected file is not, which is what keeps non-interactive output plain.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// verdictStyle maps a verdict (and whether a FAIL is tolerated by the
// expected-failures list) to its cell color, using ANSI-16 codes so the
// result degrades cleanly on a basic terminal.
func verdictStyle(re *lipgloss.Renderer, v conformance.Verdict, tolerated bool) lipgloss.Style {
	s := re.NewStyle().Bold(true)
	switch v {
	case conformance.Pass:
		return s.Foreground(lipgloss.Color("2")) // green
	case conformance.Fail:
		if tolerated {
			return re.NewStyle().Foreground(lipgloss.Color("8")) // dim: a tolerated FAIL
		}
		return s.Foreground(lipgloss.Color("1")) // red
	case conformance.Error:
		return s.Foreground(lipgloss.Color("9")) // bright red
	case conformance.Deviation:
		return s.Foreground(lipgloss.Color("3")) // yellow
	case conformance.Skip:
		return re.NewStyle().Foreground(lipgloss.Color("8")) // dim
	default:
		return re.NewStyle()
	}
}

func printConformanceTable(out io.Writer, network string, sum conformance.Summary, expectedPath string) {
	re := lipgloss.NewRenderer(out)
	label := re.NewStyle().Foreground(lipgloss.Color("6")).Bold(true) // cyan labels
	faint := re.NewStyle().Foreground(lipgloss.Color("8"))

	// Header: the same facts as the plain output, laid out as labelled lines.
	line := func(k, v string) { _, _ = fmt.Fprintln(out, label.Render(k+":"), v) }
	line("network", network)
	if sum.N2Source != "" {
		_, _ = fmt.Fprintln(out, label.Render("n2-source:"), sum.N2Source)
	}
	line("target-release", sum.TargetLine())
	if sum.ExpectedFailures != "" {
		_, _ = fmt.Fprintln(out, label.Render("expected-failures:"), sum.ExpectedFailures+faint.Render(" ("+expectedPath+")"))
	}

	// Rows, keeping the verdict and tolerated flag per row so the verdict cell
	// can be colored in the StyleFunc (which only gets row/col indexes).
	type row struct {
		verdict   conformance.Verdict
		tolerated bool
		cells     []string
	}
	rows := make([]row, 0, len(sum.Results))
	for _, r := range sum.Results {
		verdict := string(r.Verdict)
		tolerated := r.ExpectedFailure != nil
		if tolerated {
			verdict += "*"
		}
		rows = append(rows, row{
			verdict:   r.Verdict,
			tolerated: tolerated,
			cells:     []string{verdict, r.ID, string(r.Category), r.SpecRef, r.Observed},
		})
	}

	border := re.NewStyle().Foreground(lipgloss.Color("8"))
	// Fit the table to the terminal so long SpecRef/Observed cells wrap inside
	// it rather than overflowing and breaking the borders and alignment.
	// isTerminal already guaranteed out is a terminal file.
	width := 100
	if f, ok := out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 20 {
			width = w
		}
	}
	tbl := table.New().
		Width(width).
		Border(lipgloss.RoundedBorder()).
		BorderStyle(border).
		Headers("VERDICT", "TEST", "CATEGORY", "SPEC", "OBSERVED").
		StyleFunc(func(r, c int) lipgloss.Style {
			if r == table.HeaderRow {
				return re.NewStyle().Bold(true).Padding(0, 1)
			}
			base := re.NewStyle().Padding(0, 1)
			if c == 0 && r >= 0 && r < len(rows) {
				return verdictStyle(re, rows[r].verdict, rows[r].tolerated).Padding(0, 1)
			}
			return base
		})
	for _, rw := range rows {
		tbl.Row(rw.cells...)
	}
	_, _ = fmt.Fprintln(out, tbl)

	// Per-check detail for anything that needs explaining: an ERROR's Detail is
	// often the only account of why (Observed can be empty), and the plain
	// output always carries it. PASS/SKIP rows are self-explanatory, so they
	// stay out of the way.
	for _, r := range sum.Results {
		if r.Verdict == conformance.Pass || r.Verdict == conformance.Skip || r.Detail == "" {
			continue
		}
		_, _ = fmt.Fprintf(out, "%s %s\n",
			verdictStyle(re, r.Verdict, r.ExpectedFailure != nil).Render(r.ID),
			faint.Render(r.Detail))
	}

	// Stale expected-failures warn (they do not gate), as in the plain output.
	for _, st := range sum.Stale {
		_, _ = fmt.Fprintln(out, faint.Render(fmt.Sprintf("stale expected failure: %s %s: now %s %q", st.Check, st.Finding, st.Verdict, st.Observed)))
	}

	_, _ = fmt.Fprintln(out, conformanceTally(re, sum))
	if hasTolerated(sum) {
		_, _ = fmt.Fprintln(out, faint.Render("* FAIL tolerated by the expected-failures list"))
	}
}

// conformanceTally renders the colored one-line summary.
func conformanceTally(re *lipgloss.Renderer, sum conformance.Summary) string {
	col := func(code, s string) string { return re.NewStyle().Foreground(lipgloss.Color(code)).Render(s) }
	passFail := fmt.Sprintf("%s, %s",
		col("2", fmt.Sprintf("%d pass", sum.Pass)),
		col("1", fmt.Sprintf("%d fail", sum.Fail)),
	)
	if sum.ExpectedFail > 0 {
		passFail += re.NewStyle().Foreground(lipgloss.Color("8")).Render(fmt.Sprintf(" (%d expected)", sum.ExpectedFail))
	}
	return fmt.Sprintf("%d checks: %s, %s, %s, %s",
		sum.Total, passFail,
		col("3", fmt.Sprintf("%d deviation", sum.Deviation)),
		col("9", fmt.Sprintf("%d error", sum.Error)),
		col("8", fmt.Sprintf("%d skip", sum.Skip)),
	)
}

func hasTolerated(sum conformance.Summary) bool {
	for _, r := range sum.Results {
		if r.ExpectedFailure != nil {
			return true
		}
	}
	return false
}
