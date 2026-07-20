package engine

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bgrewell/loom/core/metrics"
	"github.com/bgrewell/loom/core/rtp"
)

// corrBase is a fixed instant so annotation math is deterministic.
var corrBase = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

// mkVoIPSample builds one interval quality sample.
func mkVoIPSample(end string, at time.Time, mos float64, gaps ...rtp.Gap) AppSample {
	return AppSample{
		Time:       at,
		TimeSource: "local",
		End:        end,
		VoIP:       &metrics.VoIP{MOSCQ: mos, MediaGaps: gaps},
	}
}

func mkEvent(event, detail string, at time.Time) AppSample {
	return AppSample{Time: at, TimeSource: "local", Event: event, Detail: detail}
}

// captureCorrelator returns a correlator whose emissions land in the returned
// slice pointer (single-goroutine tests, no locking needed).
func captureCorrelator(offset offsetFunc) (*correlator, *[]pendingNote) {
	var got []pendingNote
	c := newCorrelator(offset, func(at time.Time, text string) {
		got = append(got, pendingNote{at: at, text: text})
	})
	return c, &got
}

// TestCorrelateHandoverJoin drives the design demo sequence: a handover
// between two rated intervals, a DL gap on the local end, an UL gap on the
// re-stamped remote end, a both-end MOS dip and recovery — and checks the
// single composed annotation's content and internal ordering.
func TestCorrelateHandoverJoin(t *testing.T) {
	const off = 250 * time.Millisecond // remote clock ahead of orbit's
	const errBound = 1200 * time.Microsecond
	c, got := captureCorrelator(func() (time.Duration, time.Duration, bool) {
		return off, errBound, true
	})

	t0 := corrBase.Add(1500 * time.Millisecond)
	dlGap := rtp.Gap{Start: corrBase.Add(1600 * time.Millisecond), End: corrBase.Add(1840 * time.Millisecond), PacketsLost: 12}
	// The remote gap is recorded on the N6 clock: +120ms after the handover
	// once re-stamped, 180ms long.
	ulGap := rtp.Gap{
		Start: corrBase.Add(1620 * time.Millisecond).Add(off),
		End:   corrBase.Add(1800 * time.Millisecond).Add(off),
	}

	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	c.observe(mkVoIPSample(AppEndN6, corrBase.Add(200*time.Millisecond), 4.3))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	c.observe(mkEvent(StatePathSwitchComplete, "PathSwitchRequestAcknowledge", corrBase.Add(1650*time.Millisecond)))
	c.observe(mkEvent(AppEventEndMarker, "GTP-U End Marker on the UE downlink lane", corrBase.Add(1700*time.Millisecond)))

	gaps := c.observe(mkVoIPSample(AppEndUE, corrBase.Add(2*time.Second), 2.1, dlGap))
	if len(gaps) != 1 || gaps[0].Dur != 240*time.Millisecond || gaps[0].Err != 0 || !gaps[0].Synced {
		t.Fatalf("DL gap extraction: %+v", gaps)
	}
	gaps = c.observe(mkVoIPSample(AppEndN6, corrBase.Add(2200*time.Millisecond), 2.6, ulGap))
	if len(gaps) != 1 {
		t.Fatalf("UL gap extraction: %+v", gaps)
	}
	if want := corrBase.Add(1620 * time.Millisecond); !gaps[0].Start.Equal(want) {
		t.Errorf("UL gap re-stamp: got %v want %v", gaps[0].Start, want)
	}
	if gaps[0].Err != errBound || !gaps[0].Synced {
		t.Errorf("UL gap error bound: %+v", gaps[0])
	}

	// Cumulative gap lists repeat earlier gaps: the dedup must swallow them.
	if gaps := c.observe(mkVoIPSample(AppEndUE, corrBase.Add(3*time.Second), 2.5, dlGap)); len(gaps) != 0 {
		t.Fatalf("gap dedup: re-observed %+v", gaps)
	}
	if len(*got) != 0 {
		t.Fatalf("annotation before recovery: %q", (*got)[0].text)
	}

	c.observe(mkVoIPSample(AppEndUE, corrBase.Add(4500*time.Millisecond), 4.3)) // ue recovered
	c.observe(mkVoIPSample(AppEndN6, corrBase.Add(4700*time.Millisecond), 4.2)) // n6 recovered → resolve

	if len(*got) != 1 {
		t.Fatalf("want exactly 1 annotation, got %d: %v", len(*got), *got)
	}
	n := (*got)[0]
	if !n.at.Equal(corrBase.Add(4700 * time.Millisecond)) {
		t.Errorf("annotation emitted at %v", n.at)
	}
	wantParts := []string{
		"XnHandover @12:00:01.500",
		"path switch @+150ms",
		"End Marker @+200ms",
		"DL media gap 240ms (12 pkts) @+100ms",
		// The ± bound is the re-stamp uncertainty of the gap's PLACEMENT, so
		// it rides with the @offset — the duration is a same-clock difference
		// with no re-stamp error.
		"UL media gap 180ms @+120ms [±1.2ms]",
		"MOS-CQ(ue) 4.40→2.10 (interval +0.00ms..+500ms)",
		"MOS-CQ(n6) 4.30→2.60",
		"recovered @+3.2s",
	}
	pos := -1
	for _, p := range wantParts {
		i := strings.Index(n.text, p)
		if i < 0 {
			t.Errorf("annotation missing %q\n  got: %s", p, n.text)
			continue
		}
		if i < pos {
			t.Errorf("annotation ordering: %q appears before an earlier part\n  got: %s", p, n.text)
		}
		pos = i
	}
	// The DL gap was measured on orbit's own clock: no error bracket.
	if dl := n.text[strings.Index(n.text, "DL media gap"):]; strings.Contains(strings.SplitN(dl, "→", 2)[0], "±") {
		t.Errorf("local DL gap must not carry a re-stamp error bar: %s", n.text)
	}
	if list := c.list(); len(list) != 1 || list[0] != n.text {
		t.Errorf("list(): %v", list)
	}
	// Window closed: flush must not produce a second annotation.
	c.flush(corrBase.Add(6 * time.Second))
	if len(*got) != 1 {
		t.Errorf("flush after resolution added annotations: %v", *got)
	}
}

// TestCorrelateNoHandoverNoAnnotation: media trouble with no handover in
// sight is not annotated (gaps are still surfaced for the histogram).
func TestCorrelateNoHandoverNoAnnotation(t *testing.T) {
	c, got := captureCorrelator(nil)
	gap := rtp.Gap{Start: corrBase.Add(time.Second), End: corrBase.Add(1300 * time.Millisecond)}

	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	if gaps := c.observe(mkVoIPSample(AppEndUE, corrBase.Add(2*time.Second), 1.8, gap)); len(gaps) != 1 {
		t.Fatalf("gap should still surface for the histogram: %+v", gaps)
	}
	c.observe(mkVoIPSample(AppEndUE, corrBase.Add(3*time.Second), 4.4))
	c.flush(corrBase.Add(4 * time.Second))

	if len(*got) != 0 || len(c.list()) != 0 {
		t.Fatalf("no handover must mean no annotation, got %v", c.list())
	}
}

// TestCorrelateRestamp pins the re-stamping math: local = remote − offset,
// error bound carried; and an unsyncable remote gap stays labeled unsynced.
func TestCorrelateRestamp(t *testing.T) {
	const off = -3 * time.Second // remote clock BEHIND orbit's
	const errBound = 2 * time.Millisecond
	c, _ := captureCorrelator(func() (time.Duration, time.Duration, bool) {
		return off, errBound, true
	})
	remoteStart := corrBase // on the remote clock
	gaps := c.observe(mkVoIPSample(AppEndN6, corrBase.Add(time.Second), 4.0,
		rtp.Gap{Start: remoteStart, End: remoteStart.Add(240 * time.Millisecond)}))
	if len(gaps) != 1 {
		t.Fatalf("gaps: %+v", gaps)
	}
	if want := corrBase.Add(3 * time.Second); !gaps[0].Start.Equal(want) {
		t.Errorf("re-stamp: got %v want %v (local = remote − offset)", gaps[0].Start, want)
	}
	if gaps[0].Err != errBound || !gaps[0].Synced || gaps[0].Dur != 240*time.Millisecond {
		t.Errorf("re-stamp metadata: %+v", gaps[0])
	}

	// No offset available: the gap is surfaced but labeled, never silently
	// placed on orbit's timeline.
	c2, got2 := captureCorrelator(func() (time.Duration, time.Duration, bool) { return 0, 0, false })
	c2.observe(mkVoIPSample(AppEndN6, corrBase, 4.3))
	c2.observe(mkEvent(StateHandoverStarted, "N2 handover gnb1 → gnb2", corrBase.Add(time.Second)))
	gaps = c2.observe(mkVoIPSample(AppEndN6, corrBase.Add(2*time.Second), 4.2,
		rtp.Gap{Start: corrBase.Add(1200 * time.Millisecond), End: corrBase.Add(1500 * time.Millisecond)}))
	if len(gaps) != 1 || gaps[0].Synced {
		t.Fatalf("unsynced gap: %+v", gaps)
	}
	c2.flush(corrBase.Add(3 * time.Second))
	if len(*got2) != 1 {
		t.Fatalf("annotations: %v", *got2)
	}
	text := (*got2)[0].text
	if !strings.Contains(text, "N2Handover @") ||
		!strings.Contains(text, "UL media gap 300ms [remote clock unsynced]") {
		t.Errorf("unsynced annotation text: %s", text)
	}
}

// TestCorrelateBlackout: media that stops at the handover and NEVER resumes
// (loom records no Gap without a next packet, and unrated intervals never
// dip) must be called out as silence — the SD-Core D-4 signature — not
// blessed with "no media impact observed".
func TestCorrelateBlackout(t *testing.T) {
	c, got := captureCorrelator(nil)
	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	c.observe(mkEvent(StateHandoverStarted, "N2 handover gnb1 → gnb2", corrBase.Add(time.Second)))
	// Downlink dead from the handover on: every later interval is unrated.
	for i := 0; i < 5; i++ {
		c.observe(mkVoIPSample(AppEndUE, corrBase.Add(time.Duration(2+i)*time.Second), 0))
	}
	c.flush(corrBase.Add(8 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	if !strings.Contains(text, "DL media silent since handover (last rated interval @-1.0s)") ||
		!strings.Contains(text, "not recovered by session end") {
		t.Errorf("blackout not called out: %s", text)
	}
	if strings.Contains(text, "no media impact") {
		t.Errorf("total silence presented as all-clear: %s", text)
	}
}

// TestCorrelateBlackoutGraceWindow: a window resolved right at the handover
// (no time for a post-t0 interval yet) must not be misread as silence.
func TestCorrelateBlackoutGraceWindow(t *testing.T) {
	c, got := captureCorrelator(nil)
	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", corrBase.Add(time.Second)))
	c.flush(corrBase.Add(1500 * time.Millisecond)) // < corrSilenceMin after t0
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	if text := (*got)[0].text; strings.Contains(text, "silent since handover") {
		t.Errorf("silence claimed inside the grace window: %s", text)
	}
}

// TestCorrelateGapEndedBeforeHandover: a hole that both opened AND healed
// before HANDOVER_STARTED is pre-existing trouble, not handover impact —
// the slack only admits gaps whose silence straddles t0.
func TestCorrelateGapEndedBeforeHandover(t *testing.T) {
	c, got := captureCorrelator(nil)
	t0 := corrBase.Add(2 * time.Second)
	// Start within the 500ms slack before t0, but over 100ms BEFORE t0.
	stale := rtp.Gap{Start: t0.Add(-400 * time.Millisecond), End: t0.Add(-100 * time.Millisecond)}
	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	if gaps := c.observe(mkVoIPSample(AppEndUE, t0.Add(time.Second), 4.4, stale)); len(gaps) != 1 {
		t.Fatalf("gap must still surface for the histogram: %+v", gaps)
	}
	c.flush(t0.Add(5 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	if strings.Contains(text, "media gap") {
		t.Errorf("pre-handover gap attributed to the handover: %s", text)
	}
	if !strings.Contains(text, "no media impact observed") {
		t.Errorf("expected the all-clear: %s", text)
	}
}

// TestCorrelateRemoteClockJoins: samples still on the remote clock (no
// offset yet) must neither settle the window deadline nor enter the MOS
// baseline/dip join — cross-clock comparisons fabricate precision.
func TestCorrelateRemoteClockJoins(t *testing.T) {
	c, got := captureCorrelator(func() (time.Duration, time.Duration, bool) { return 0, 0, false })
	t0 := corrBase.Add(time.Second)
	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", t0))
	// A remote clock skewed far ahead: would trip the lazy 15s deadline (and
	// register a bogus dip) if joined against the local t0.
	skewed := AppSample{
		Time:       t0.Add(20 * time.Second),
		TimeSource: "remote-clock",
		End:        AppEndN6,
		VoIP:       &metrics.VoIP{MOSCQ: 1.5},
	}
	c.observe(skewed)
	if len(*got) != 0 {
		t.Fatalf("remote-clock sample settled the window: %v", *got)
	}
	// A local sample inside the window keeps it open too (sanity).
	c.observe(mkVoIPSample(AppEndUE, t0.Add(2*time.Second), 4.4))
	if len(*got) != 0 {
		t.Fatalf("window closed early: %v", *got)
	}
	c.flush(t0.Add(5 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	if text := (*got)[0].text; strings.Contains(text, "MOS-CQ(n6)") {
		t.Errorf("unsynced remote MOS joined cross-clock: %s", text)
	}
}

// TestCorrelateQuietWindow: a handover with no media impact resolves at the
// window deadline with an explicit all-clear.
func TestCorrelateQuietWindow(t *testing.T) {
	c, got := captureCorrelator(nil)
	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	c.observe(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", corrBase.Add(time.Second)))
	for i := 1; i <= 14; i++ {
		c.observe(mkVoIPSample(AppEndUE, corrBase.Add(time.Duration(i)*time.Second+500*time.Millisecond), 4.4))
	}
	if len(*got) != 0 {
		t.Fatalf("annotation before the window deadline: %v", *got)
	}
	// First observation at/after t0+window settles it.
	c.observe(mkVoIPSample(AppEndUE, corrBase.Add(16500*time.Millisecond), 4.4))
	if len(*got) != 1 {
		t.Fatalf("want 1 annotation, got %v", *got)
	}
	if text := (*got)[0].text; !strings.Contains(text, "no media impact observed within 15.0s") {
		t.Errorf("quiet-window text: %s", text)
	}
}

// TestCorrelateFlushUnrecovered: a dip with no recovery by teardown is
// reported honestly.
func TestCorrelateFlushUnrecovered(t *testing.T) {
	c, got := captureCorrelator(nil)
	c.observe(mkVoIPSample(AppEndUE, corrBase, 4.4))
	c.observe(mkEvent(StateHandoverStarted, "N2 handover gnb1 → gnb2", corrBase.Add(time.Second)))
	c.observe(mkVoIPSample(AppEndUE, corrBase.Add(2*time.Second), 1.2))
	c.flush(corrBase.Add(3 * time.Second))
	if len(*got) != 1 {
		t.Fatalf("annotations: %v", *got)
	}
	text := (*got)[0].text
	if !strings.Contains(text, "MOS-CQ(ue) 4.40→1.20") || !strings.Contains(text, "not recovered by session end") {
		t.Errorf("unrecovered text: %s", text)
	}
}

// TestAppSessionPublishWiring drives publish() on a bare appSession and
// checks the full seam: the correlator sees the stream, its annotation lands
// back in the session's event series live, gauges track the latest interval
// sample, first-seen gaps hit the histogram once, and cleanup removes the
// session's series.
func TestAppSessionPublishWiring(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewManager(slog.New(slog.DiscardHandler))
	m.EnableAppMetrics(reg)

	s := &appSession{
		id:   "app-test",
		supi: appTestSUPI,
		cfg:  AppSessionConfig{App: "voip"},
		m:    m,
		subs: map[int]chan AppSample{},
	}
	s.corr = newCorrelator(nil, func(at time.Time, text string) {
		s.publish(AppSample{Time: at, TimeSource: "local", Event: AppEventAnnotation, Detail: text})
	})

	gap := rtp.Gap{Start: corrBase.Add(1100 * time.Millisecond), End: corrBase.Add(1340 * time.Millisecond)}
	s.publish(mkVoIPSample(AppEndUE, corrBase, 4.4))
	s.publish(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", corrBase.Add(time.Second)))
	sample := mkVoIPSample(AppEndUE, corrBase.Add(2*time.Second), 2.1, gap)
	sample.VoIP.JitterMs, sample.VoIP.LossPct = 7.5, 3.25
	sample.VoIP.OWDMs, sample.VoIP.OWDErrMs = 12.5, 0.8
	s.publish(sample)
	s.publish(mkVoIPSample(AppEndUE, corrBase.Add(3*time.Second), 2.1, gap)) // dup gap
	s.publish(mkVoIPSample(AppEndUE, corrBase.Add(4*time.Second), 4.35))     // recovery → annotation

	s.mu.Lock()
	var ann []AppSample
	for _, ev := range s.events {
		if ev.Event == AppEventAnnotation {
			ann = append(ann, ev)
		}
	}
	s.mu.Unlock()
	if len(ann) != 1 {
		t.Fatalf("want 1 live annotation event, got %d", len(ann))
	}
	if !strings.Contains(ann[0].Detail, "DL media gap 240ms") || !strings.Contains(ann[0].Detail, "recovered @+3.0s") {
		t.Errorf("annotation detail: %s", ann[0].Detail)
	}

	labels := map[string]string{"supi": appTestSUPI, "app": "voip", "end": AppEndUE}
	// Gauges hold the LATEST interval sample (the recovery one).
	if v, ok := gaugeValue(t, reg, "orbit_app_mos", labels); !ok || v != 4.35 {
		t.Errorf("orbit_app_mos = %v (ok=%v)", v, ok)
	}
	if v, ok := gaugeValue(t, reg, "orbit_app_owd_err_ms", labels); !ok || v != 0 {
		t.Errorf("orbit_app_owd_err_ms after zero-err sample = %v (ok=%v)", v, ok)
	}
	// The gap was observed exactly once despite the cumulative re-send.
	if n := histogramCount(t, reg, "orbit_app_media_gap_ms"); n != 1 {
		t.Errorf("media gap observations = %d", n)
	}

	m.appMetrics.cleanupSession(appTestSUPI, "voip")
	for _, name := range []string{
		"orbit_app_mos", "orbit_app_jitter_ms", "orbit_app_loss_pct",
		"orbit_app_owd_ms", "orbit_app_owd_err_ms",
	} {
		if left := gatherFamily(t, reg, name); len(left) != 0 {
			t.Errorf("%s: series left after cleanup: %v", name, left)
		}
	}
	// The media-gap histogram is cumulative and survives session cleanup.
	if n := histogramCount(t, reg, "orbit_app_media_gap_ms"); n != 1 {
		t.Errorf("media gap histogram after cleanup = %d observations, want 1", n)
	}
}
