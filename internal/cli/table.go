package cli

import (
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

// tableWriter renders tab-separated rows as an aligned lipgloss table. The
// first line written is the header. Rows are buffered until Flush.
type tableWriter struct {
	w   io.Writer
	buf strings.Builder
}

// newTable returns a writer for tab-separated lines (header first) that
// prints a borderless table with a bold header to w on Flush.
func newTable(w io.Writer) *tableWriter { return &tableWriter{w: w} }

func (t *tableWriter) Write(p []byte) (int, error) { return t.buf.Write(p) }

// Flush renders the buffered rows.
func (t *tableWriter) Flush() error {
	text := strings.TrimRight(t.buf.String(), "\n")
	t.buf.Reset()
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	header := strings.Split(lines[0], "\t")
	tb := table.New().
		Border(lipgloss.HiddenBorder()).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderHeader(false).
		StyleFunc(func(row, _ int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingRight(2)
			if row == table.HeaderRow {
				s = s.Bold(true)
			}
			return s
		}).
		Headers(header...)
	for _, l := range lines[1:] {
		tb.Row(strings.Split(l, "\t")...)
	}
	_, err := io.WriteString(t.w, tb.String()+"\n")
	return err
}
