package report

import (
	"math"
	"strings"
	"testing"
)

func TestChartOmitsItselfWithoutData(t *testing.T) {
	// An empty box reads as "measured, and flat at zero". Better to draw
	// nothing and let the template skip the figure.
	for name, c := range map[string]Chart{
		"no series": {Title: "x"},
		"one point": {Title: "x", Series: []Series{{Values: []float64{1}}}},
		"all empty": {Title: "x", Series: []Series{{Values: nil}}},
	} {
		if c.SVG() != "" || c.HasData() {
			t.Errorf("%s: expected no chart", name)
		}
	}
}

func TestChartDrawsSeries(t *testing.T) {
	c := Chart{
		Title:  "Throughput",
		Series: []Series{{Name: "dl", Color: "#123456", Values: []float64{1, 5, 3, 9}}},
	}
	svg := c.SVG()
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "</svg>") {
		t.Fatal("not an svg")
	}
	if !strings.Contains(svg, "#123456") {
		t.Error("series colour missing")
	}
	if !strings.Contains(svg, "<path") {
		t.Error("no line drawn")
	}
}

func TestChartSurvivesNonFiniteValues(t *testing.T) {
	// A rate derived from a zero interval can be NaN or Inf. Emitting those
	// into path coordinates produces an SVG the browser silently drops.
	inf := math.Inf(1)
	nan := math.NaN()
	c := Chart{Series: []Series{{Values: []float64{1, nan, inf, 4}}}}
	svg := c.SVG()
	for _, bad := range []string{"NaN", "Inf", "+Inf", "-Inf"} {
		if strings.Contains(svg, bad) {
			t.Errorf("svg contains %q, which makes the path invalid: %s", bad, svg)
		}
	}
}

func TestChartHandlesAllZeroSeries(t *testing.T) {
	// Scaling to a zero maximum would divide by zero.
	c := Chart{Series: []Series{{Values: []float64{0, 0, 0}}}}
	svg := c.SVG()
	if svg == "" {
		t.Fatal("a measured-but-zero series should still draw an axis")
	}
	if strings.Contains(svg, "NaN") {
		t.Error("zero-max scaling produced NaN coordinates")
	}
}

func TestNiceCeil(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0, 0},
		{0.4, 0.4}, // already a round number at its magnitude
		{0.41, 0.5},
		{7, 7.5}, {12, 15}, {99, 100}, {1001, 1500},
	} {
		if got := niceCeil(tc.in); got != tc.want {
			t.Errorf("niceCeil(%g) = %g, want %g", tc.in, got, tc.want)
		}
	}
}
