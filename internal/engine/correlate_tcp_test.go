package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/loom/core/metrics"
	"github.com/bgrewell/loom/core/rtp"
)

// mkVideoSample builds one interval video QoE sample.
func mkVideoSample(end string, at time.Time, v metrics.Video) AppSample {
	vc := v
	return AppSample{Time: at, TimeSource: "local", End: end, Video: &vc}
}

// mkHTTPSample builds one interval HTTP quality sample.
func mkHTTPSample(end string, at time.Time, h metrics.HTTP) AppSample {
	hc := h
	return AppSample{Time: at, TimeSource: "local", End: end, HTTP: &hc}
}

// TestCorrelateVideoStallJoin drives the design Phase-7 demo sequence: a
// stall whose outage spans the handover is attributed, the buffer drain and
// the ABR downshift (labeled coincident — correlation, not causation) join
// the same annotation, and the completed stall reads as recovered at its end.
func TestCorrelateVideoStallJoin(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(1500 * time.Millisecond)

	// Baseline interval before the handover: healthy buffer at 2500 kbps.
	c.observe(mkVideoSample(AppEndUE, corrBase, metrics.Video{
		BufferMs: 4200, AvgBitrateKbps: 2500,
	}))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))

	// The stall opens: one underrun started, buffer at zero.
	c.observe(mkVideoSample(AppEndUE, t0.Add(time.Second), metrics.Video{
		Stalls: 1, StallTimeMs: 800, BufferMs: 0, AvgBitrateKbps: 2500,
	}))
	if len(*got) != 0 {
		t.Fatalf("annotation before the stall resolved: %v", *got)
	}

	// The stall completes (event delivered in the interval where it ENDED)
	// and the player downshifts.
	stall := rtp.Gap{Start: t0.Add(200 * time.Millisecond), End: t0.Add(2 * time.Second)}
	c.observe(mkVideoSample(AppEndUE, t0.Add(2500*time.Millisecond), metrics.Video{
		StallTimeMs: 1000, BufferMs: 1500, AvgBitrateKbps: 1200,
		RepSwitchesDown: 1, StallEvents: []rtp.Gap{stall},
	}))

	// A FINAL whole-call sample replays cumulative counters and the event
	// list: the dedup and the Final guard must swallow both (no phantom
	// second stall, no "ongoing").
	fin := mkVideoSample(AppEndUE, t0.Add(3*time.Second), metrics.Video{
		Stalls: 1, StallTimeMs: 1800, StallEvents: []rtp.Gap{stall},
	})
	fin.Final = true
	c.observe(fin)

	c.flush(t0.Add(5 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("want exactly 1 annotation, got %d: %v", len(*got), *got)
	}
	text := (*got)[0].text
	wantParts := []string{
		"XnHandover @",
		"buffer drain 4.2s→0.00ms",
		"1.8s stall @+200ms",
		"ABR downshift 2500→1200 kbps @+2.5s (coincident)",
		"recovered @+2.0s",
	}
	pos := -1
	for _, p := range wantParts {
		i := strings.Index(text, p)
		if i < 0 {
			t.Errorf("annotation missing %q\n  got: %s", p, text)
			continue
		}
		if i < pos {
			t.Errorf("annotation ordering: %q appears before an earlier part\n  got: %s", p, text)
		}
		pos = i
	}
	if strings.Contains(text, "not recovered") || strings.Contains(text, "ongoing") {
		t.Errorf("completed stall misreported: %s", text)
	}
}

// TestCorrelateVideoStallBeforeHandover: a stall that both opened AND healed
// before HANDOVER_STARTED is pre-existing trouble — not attributed, and its
// started/closed counts net out so it is not misread as still open.
func TestCorrelateVideoStallBeforeHandover(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(2 * time.Second)

	c.observe(mkVideoSample(AppEndUE, corrBase, metrics.Video{
		BufferMs: 4000, AvgBitrateKbps: 2500,
	}))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))

	// The straddling interval [t0-0.5s, t0+0.5s] carries a stall that started
	// AND ended before t0 (inside the slack region): Stalls=1 plus its event.
	stale := rtp.Gap{Start: t0.Add(-450 * time.Millisecond), End: t0.Add(-50 * time.Millisecond)}
	c.observe(mkVideoSample(AppEndUE, t0.Add(500*time.Millisecond), metrics.Video{
		Stalls: 1, StallTimeMs: 400, BufferMs: 3500, AvgBitrateKbps: 2500,
		StallEvents: []rtp.Gap{stale},
	}))

	c.flush(t0.Add(5 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	if strings.Contains(text, "stall") {
		t.Errorf("pre-handover stall attributed to the handover: %s", text)
	}
	if !strings.Contains(text, "no media impact observed") {
		t.Errorf("expected the all-clear: %s", text)
	}
}

// TestCorrelateVideoOpenStallAtEnd: a stall that never closes by session end
// (vidstream emits the event only when the stall ENDS) must be reported as
// ongoing with the accumulated stall-time floor, and "not recovered".
func TestCorrelateVideoOpenStallAtEnd(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(time.Second)

	c.observe(mkVideoSample(AppEndUE, corrBase, metrics.Video{
		BufferMs: 3000, AvgBitrateKbps: 2000,
	}))
	c.observe(mkEvent(StateHandoverStarted, "N2 handover gnb1 → gnb2", t0))
	c.observe(mkVideoSample(AppEndUE, t0.Add(time.Second), metrics.Video{
		Stalls: 1, StallTimeMs: 700, BufferMs: 0, AvgBitrateKbps: 2000,
	}))
	c.observe(mkVideoSample(AppEndUE, t0.Add(2*time.Second), metrics.Video{
		StallTimeMs: 1000, BufferMs: 0, AvgBitrateKbps: 2000,
	}))

	c.flush(t0.Add(3 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	wantParts := []string{
		"N2Handover @",
		"buffer drain 3.0s→0.00ms",
		"stall ongoing (≥1.7s stalled)",
		"not recovered by session end",
	}
	for _, p := range wantParts {
		if !strings.Contains(text, p) {
			t.Errorf("annotation missing %q\n  got: %s", p, text)
		}
	}
	if strings.Contains(text, "no media impact") {
		t.Errorf("open stall presented as all-clear: %s", text)
	}
}

// TestCorrelateVideoOpenStallBeforeHandover: a stall that opened BEFORE the
// attribution window (earlier than t0−slack) and never closes is pre-existing
// trouble, exactly like the completed-stall rule — it must not be annotated
// as "stall ongoing … not recovered" (nor pull in a buffer-drain line) just
// because its close event never arrives. The interval Stalls counter carries
// no start time, so the join bounds the start from the interval's own stall
// time (a.Time − StallTimeMs = latest possible start).
func TestCorrelateVideoOpenStallBeforeHandover(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(time.Second)

	// Baseline interval, healthy buffer.
	c.observe(mkVideoSample(AppEndUE, corrBase, metrics.Video{
		BufferMs: 4000, AvgBitrateKbps: 2500,
	}))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))

	// Straddling interval (stamped t0+200ms): a stall opened at t0−800ms and
	// is still open — Stalls=1 with ~1s of accumulated stall time, so its
	// latest possible start (t0+200ms − 1000ms) precedes t0−slack.
	c.observe(mkVideoSample(AppEndUE, t0.Add(200*time.Millisecond), metrics.Video{
		Stalls: 1, StallTimeMs: 1000, BufferMs: 0, AvgBitrateKbps: 2500,
	}))
	// The stall keeps running, never closing.
	c.observe(mkVideoSample(AppEndUE, t0.Add(1200*time.Millisecond), metrics.Video{
		StallTimeMs: 1000, BufferMs: 0, AvgBitrateKbps: 2500,
	}))

	c.flush(t0.Add(5 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	if strings.Contains(text, "ongoing") || strings.Contains(text, "not recovered") ||
		strings.Contains(text, "buffer drain") {
		t.Errorf("pre-window open stall attributed to the handover: %s", text)
	}
	if !strings.Contains(text, "no media impact observed") {
		t.Errorf("expected the all-clear: %s", text)
	}
}

// TestCorrelateHTTPSteadyErrorsNoBurst: a session erroring at a constant
// rate before and after the handover (chronically failing origin) must not
// read as a handover-coincident error burst — only errors ABOVE the
// pre-handover baseline interval's count are evidence, and the excess is
// labeled against that rate.
func TestCorrelateHTTPSteadyErrorsNoBurst(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(time.Second)

	// Chronic baseline: every interval 10 requests, 5 errors.
	c.observe(mkHTTPSample(AppEndUE, corrBase, metrics.HTTP{
		Requests: 10, Errors: 5, TTFBMsP95: 45,
	}))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	c.observe(mkHTTPSample(AppEndUE, t0.Add(time.Second), metrics.HTTP{
		Requests: 10, Errors: 5, TTFBMsP95: 45,
	}))
	c.observe(mkHTTPSample(AppEndUE, t0.Add(2*time.Second), metrics.HTTP{
		Requests: 10, Errors: 5, TTFBMsP95: 45,
	}))
	c.flush(t0.Add(4 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	if text := (*got)[0].text; strings.Contains(text, "error burst") {
		t.Errorf("steady-state errors annotated as a handover burst: %s", text)
	}

	// The same chronic session with a genuine windowed excess: only the
	// errors above the baseline rate are claimed, labeled against it.
	c2, got2 := captureCorrelator(nil)
	c2.observe(mkHTTPSample(AppEndUE, corrBase, metrics.HTTP{
		Requests: 10, Errors: 5, TTFBMsP95: 45,
	}))
	c2.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	c2.observe(mkHTTPSample(AppEndUE, t0.Add(time.Second), metrics.HTTP{
		Requests: 10, Errors: 9, TTFBMsP95: 45,
	}))
	c2.flush(t0.Add(4 * time.Second))
	if len(*got2) != 1 {
		t.Fatalf("annotations: %v", *got2)
	}
	if text := (*got2)[0].text; !strings.Contains(text,
		"HTTP error burst 4 errors above the pre-handover rate (5/interval) @+1.0s (coincident)") {
		t.Errorf("excess-over-baseline burst missing or mislabeled: %s", text)
	}
}

// TestCorrelateHTTPSingleErrorGrammar: one excess error renders "1 error",
// not "1 errors".
func TestCorrelateHTTPSingleErrorGrammar(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(time.Second)
	c.observe(mkHTTPSample(AppEndUE, corrBase, metrics.HTTP{Requests: 10, TTFBMsP95: 45}))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	c.observe(mkHTTPSample(AppEndUE, t0.Add(time.Second), metrics.HTTP{
		Requests: 10, Errors: 1, TTFBMsP95: 45,
	}))
	c.flush(t0.Add(4 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	if !strings.Contains(text, "HTTP error burst 1 error @+1.0s (coincident)") {
		t.Errorf("singular error grammar: %s", text)
	}
}

// TestCorrelateVideoRemoteStallRestamp pins the honest-clock rules for stall
// events from the far end: re-stamped onto orbit's clock (local = remote −
// offset) with the tracker error bound rendered on the placement.
func TestCorrelateVideoRemoteStallRestamp(t *testing.T) {
	const off = 250 * time.Millisecond
	const errBound = 1200 * time.Microsecond
	c, got := captureCorrelator(func() (time.Duration, time.Duration, bool) {
		return off, errBound, true
	})
	t0 := corrBase.Add(time.Second)
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))

	// The stall is recorded on the N6 clock: +120ms after the handover once
	// re-stamped, 1.8s long.
	remote := rtp.Gap{
		Start: t0.Add(120 * time.Millisecond).Add(off),
		End:   t0.Add(1920 * time.Millisecond).Add(off),
	}
	s := mkVideoSample(AppEndN6, t0.Add(2500*time.Millisecond), metrics.Video{
		StallTimeMs: 1800, StallEvents: []rtp.Gap{remote},
	})
	s.TimeSource, s.TimeErr = "timesync", errBound
	c.observe(s)

	c.flush(t0.Add(4 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	if !strings.Contains(text, "1.8s stall (n6) @+120ms [±1.2ms]") {
		t.Errorf("re-stamped stall missing: %s", text)
	}
	if !strings.Contains(text, "recovered @+1.9s") {
		t.Errorf("completed remote stall should read recovered: %s", text)
	}

	// No offset available: the stall is labeled, never silently placed on
	// orbit's timeline.
	c2, got2 := captureCorrelator(func() (time.Duration, time.Duration, bool) { return 0, 0, false })
	c2.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	s2 := mkVideoSample(AppEndN6, t0.Add(2*time.Second), metrics.Video{
		StallEvents: []rtp.Gap{{Start: t0.Add(200 * time.Millisecond), End: t0.Add(700 * time.Millisecond)}},
	})
	s2.TimeSource = "remote-clock"
	c2.observe(s2)
	c2.flush(t0.Add(4 * time.Second))
	if len(*got2) != 1 {
		t.Fatalf("annotations: %v", *got2)
	}
	if text := (*got2)[0].text; !strings.Contains(text, "500ms stall (n6) [remote clock unsynced]") {
		t.Errorf("unsynced stall not labeled: %s", text)
	}
}

// TestCorrelateHTTPCoincident: a request-error burst and a TTFB-p95 interval
// at ≥ corrTTFBFactor× the pre-handover baseline, overlapping the handover
// window, are annotated as coincident — and only as coincident (no
// recovered/not-recovered verdict from HTTP evidence alone).
func TestCorrelateHTTPCoincident(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(time.Second)

	// Baseline: last rated interval before t0 at 45ms p95, error-free.
	c.observe(mkHTTPSample(AppEndUE, corrBase, metrics.HTTP{
		Requests: 10, TTFBMsP95: 45, GoodputMbps: 20,
	}))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	c.observe(mkHTTPSample(AppEndUE, t0.Add(time.Second), metrics.HTTP{
		Requests: 6, Errors: 5, TTFBMsP95: 320, GoodputMbps: 1.5,
	}))

	c.flush(t0.Add(4 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	wantParts := []string{
		"XnHandover @",
		"HTTP error burst 5 errors @+1.0s (coincident)",
		"TTFB p95 spike 45ms→320ms (≥3× the last pre-handover interval) @+1.0s (coincident)",
	}
	pos := -1
	for _, p := range wantParts {
		i := strings.Index(text, p)
		if i < 0 {
			t.Errorf("annotation missing %q\n  got: %s", p, text)
			continue
		}
		if i < pos {
			t.Errorf("annotation ordering: %q out of order\n  got: %s", p, text)
		}
		pos = i
	}
	// Coincident evidence is neither an all-clear nor a verdict.
	if strings.Contains(text, "no media impact") || strings.Contains(text, "recovered") {
		t.Errorf("HTTP coincident evidence produced a verdict: %s", text)
	}
}

// TestCorrelateHTTPQuietWindow: clean HTTP intervals through a handover
// resolve to the explicit all-clear.
func TestCorrelateHTTPQuietWindow(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(time.Second)
	c.observe(mkHTTPSample(AppEndUE, corrBase, metrics.HTTP{Requests: 10, TTFBMsP95: 45}))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	// Below the ×3 threshold, no errors.
	c.observe(mkHTTPSample(AppEndUE, t0.Add(time.Second), metrics.HTTP{Requests: 9, TTFBMsP95: 90}))
	c.flush(t0.Add(4 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	if text := (*got)[0].text; !strings.Contains(text, "no media impact observed by session end") {
		t.Errorf("quiet HTTP window: %s", text)
	}
}

// TestCorrelateHTTPNoBaselineNoSpike: with no rated interval before t0 there
// is no baseline, so no spike may be claimed — a threshold against nothing
// would be fabricated precision.
func TestCorrelateHTTPNoBaselineNoSpike(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(time.Second)
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	c.observe(mkHTTPSample(AppEndUE, t0.Add(time.Second), metrics.HTTP{Requests: 3, TTFBMsP95: 900}))
	c.flush(t0.Add(4 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	if text := (*got)[0].text; strings.Contains(text, "spike") {
		t.Errorf("spike claimed without a baseline: %s", text)
	}
}
