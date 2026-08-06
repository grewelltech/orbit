// Package report renders a finished run as a self-contained HTML document.
//
// Self-contained is the requirement that shapes everything: the file has to
// open offline, survive being emailed, and print to PDF without a toolchain.
// So there are no external requests, no CDN, and no JavaScript — charts are
// inline SVG generated here rather than drawn by a charting library at view
// time. SVG also prints at the printer's resolution instead of a canvas
// bitmap's, which is the difference between a legible PDF and a blurry one.
package report

import (
	"fmt"
	"math"
	"strings"
)

// Series is one line on a chart.
type Series struct {
	Name   string
	Color  string
	Values []float64
}

// Chart is a small multi-series line plot.
type Chart struct {
	Title  string
	Unit   string
	Series []Series
	// Height in user units; width is fixed and the SVG scales to its container.
	Height float64
}

const (
	chartWidth  = 720.0
	padLeft     = 52.0
	padRight    = 12.0
	padTop      = 10.0
	padBottom   = 22.0
	gridLines   = 4
	defaultTall = 150.0
)

// SVG renders the chart. It returns an empty string when there is nothing to
// draw, so a template can omit the whole figure rather than print an empty box
// that reads as "measured, and flat at zero".
func (c Chart) SVG() string {
	maxLen, maxVal := 0, 0.0
	for _, s := range c.Series {
		if len(s.Values) > maxLen {
			maxLen = len(s.Values)
		}
		for _, v := range s.Values {
			if !math.IsNaN(v) && !math.IsInf(v, 0) && v > maxVal {
				maxVal = v
			}
		}
	}
	if maxLen < 2 {
		return ""
	}

	h := c.Height
	if h <= 0 {
		h = defaultTall
	}
	plotW := chartWidth - padLeft - padRight
	plotH := h - padTop - padBottom

	// A flat-zero series still gets an axis, but scaling to zero would divide
	// by it; 1 keeps the geometry valid and the line on the floor.
	top := niceCeil(maxVal)
	if top <= 0 {
		top = 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %g %g" class="chart" preserveAspectRatio="none" role="img">`, chartWidth, h)

	// Horizontal grid and y labels.
	for i := 0; i <= gridLines; i++ {
		frac := float64(i) / gridLines
		y := padTop + plotH*frac
		val := top * (1 - frac)
		fmt.Fprintf(&b, `<line class="grid" x1="%g" y1="%g" x2="%g" y2="%g"/>`,
			padLeft, y, chartWidth-padRight, y)
		fmt.Fprintf(&b, `<text class="ylab" x="%g" y="%g">%s</text>`,
			padLeft-6, y+3, axisLabel(val))
	}

	for _, s := range c.Series {
		if len(s.Values) < 2 {
			continue
		}
		var path strings.Builder
		for i, v := range s.Values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				v = 0
			}
			x := padLeft + plotW*float64(i)/float64(len(s.Values)-1)
			y := padTop + plotH*(1-v/top)
			if i == 0 {
				fmt.Fprintf(&path, "M%.1f %.1f", x, y)
			} else {
				fmt.Fprintf(&path, "L%.1f %.1f", x, y)
			}
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="1.6"/>`,
			path.String(), s.Color)
	}

	fmt.Fprintf(&b, `<line class="axis" x1="%g" y1="%g" x2="%g" y2="%g"/>`,
		padLeft, padTop+plotH, chartWidth-padRight, padTop+plotH)
	b.WriteString(`</svg>`)
	return b.String()
}

// HasData reports whether the chart would draw anything.
func (c Chart) HasData() bool { return c.SVG() != "" }

// niceCeil rounds an axis maximum up to a round number, so the top gridline
// reads as a value someone would choose rather than as the sample that happened
// to be largest.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 0
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, step := range []float64{1, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10} {
		if v <= step*mag {
			return step * mag
		}
	}
	return 10 * mag
}

// axisLabel formats an axis value compactly — a report axis has no room for
// six significant figures.
func axisLabel(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v >= 1e9:
		return fmt.Sprintf("%.1fG", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case v >= 10:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.2g", v)
	}
}
