// Event correlation for app sessions (docs/design/real-app-traffic.md §7):
// orbit is the single clock and join point. The correlator consumes the same
// stream an app session publishes — hub mobility phases (HANDOVER_STARTED /
// HANDED_OVER / PATH_SWITCH_COMPLETE), GTP-U End Markers from the demux, and
// both ends' interval quality samples — and joins them into ordered, human-
// readable annotations:
//
//	XnHandover @14:03:07.512 → End Marker @+180ms → DL media gap 240ms @+160ms
//	  → MOS-CQ(ue) 4.40→2.10 (interval +1.0s..+2.0s) → recovered @+3.0s
//
// TCP apps join the same window (design Phases 6-7). Video stall events are
// media gaps in all but name (same rtp.Gap record, same honest-clock rules —
// remote ones re-stamped with the tracker offset, error bounds carried, and a
// stall still open at session end reported as "not recovered"):
//
//	XnHandover @t0 → buffer drain 4.2s→0.00ms → 1.8s stall @+200ms
//	  → ABR downshift 2500→1200 kbps @+2.0s (coincident) → recovered @+2.0s
//
// ABR downshifts and HTTP trouble (request errors above the pre-handover
// baseline interval's error count, or a TTFB-p95 interval at least
// corrTTFBFactor× the pre-handover baseline interval's p95) inside a handover
// window are CONSEQUENCE CANDIDATES: the join is temporal overlap, not a
// causal proof, so those parts are labeled "coincident".
//
// Honest uncertainty (the design mandate): quantities measured on the remote
// (N6) clock are re-stamped onto orbit's clock via the session's owd.Tracker
// offset, and the tracker's error bound is propagated into the annotation
// text ("UL media gap 180ms [±1.2ms]"). Locally-observed quantities share
// orbit's clock and carry no bracket. A remote quantity that cannot be
// re-stamped (no offset yet) is labeled, never silently presented as aligned.
//
// The correlator is pure joined state — no goroutines, no timers. Deadlines
// resolve lazily on the next observation (interval samples arrive every
// second, so resolution lag is one interval), and flush() settles any open
// window at teardown.
package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// AppEventAnnotation is the Event tag of correlator-produced samples on the
// app session stream: Detail carries the composed annotation text.
const AppEventAnnotation = "ANNOTATION"

// Correlation tuning (correlator fields; these are the defaults).
const (
	// corrWindow bounds the join: media impact within this long after
	// HANDOVER_STARTED is attributed to the handover.
	corrWindow = 15 * time.Second
	// corrGapSlack tolerates a media gap that opened just before the
	// HANDOVER_STARTED stamp (the gap start is the last packet BEFORE the
	// silence, so it legitimately precedes the event by up to one ptime
	// plus clock scatter).
	corrGapSlack = 500 * time.Millisecond
	// corrMOSDip is the MOS-CQ drop below the pre-handover baseline that
	// counts as media impact.
	corrMOSDip = 0.5
	// corrMOSRecover is the band below the baseline within which MOS-CQ
	// counts as recovered again.
	corrMOSRecover = 0.25
	// corrSilenceMin is the least time past HANDOVER_STARTED before total
	// media silence (rated intervals before the handover, none after) is
	// called out as a blackout — a couple of sample intervals of grace so a
	// window resolved immediately is not misread as silence.
	corrSilenceMin = 2 * time.Second
	// corrTTFBFactor is the deliberately simple HTTP spike threshold (design
	// Phase 6): an interval whose TTFB p95 reaches the pre-handover baseline
	// interval's p95 × this factor, inside a handover window, is annotated as
	// a coincident TTFB spike. The baseline is the last rated (Requests > 0)
	// interval before t0 on the same end; with no such baseline no spike is
	// claimed (a threshold against nothing would be fabricated precision).
	corrTTFBFactor = 3.0
)

// offsetFunc reports the current remote-minus-local clock offset and its
// half-width error bound (owd.Tracker.Offset's signature).
type offsetFunc func() (offset, errBound time.Duration, ok bool)

// mediaGap is one hole in media arrival — or, for video sessions, one player
// stall (Stall) — placed on orbit's clock.
type mediaGap struct {
	End   string        // AppEndUE (a DL gap) or AppEndN6 (an UL gap)
	Start time.Time     // on orbit's clock when Synced
	Dur   time.Duration // gap length (same-clock difference: no re-stamp error)
	Err   time.Duration // half-width uncertainty of Start vs orbit's clock
	Lost  uint32        // sequence numbers missing inside the gap (0 for stalls)
	// Synced is false when a remote gap could not be re-stamped (no
	// tracker offset yet): Start is then on the remote clock.
	Synced bool
}

// mosPoint is one end's latest interval MOS reading, with the re-stamp
// provenance of its timestamp (design §7 — remote quantities carry their
// clock and error bound through every join, exactly like media gaps do).
type mosPoint struct {
	t   time.Time
	mos float64
	ok  bool
	// err is the half-width uncertainty of t vs orbit's clock (re-stamped
	// remote samples); synced is false when t is on the remote clock (no
	// tracker offset yet) — such points are never joined against local
	// event times.
	err    time.Duration
	synced bool
}

// mosDip tracks one end's MOS excursion inside a handover window.
type mosDip struct {
	pre                float64 // baseline: last interval MOS before t0
	worst              float64
	worstFrom, worstTo time.Time     // the interval that scored worst
	err                time.Duration // half-width re-stamp uncertainty of the interval bounds
	recovered          bool
	recoveredAt        time.Time
}

// httpPoint is one end's latest rated HTTP interval (Requests > 0) — the
// baseline material for the TTFB-spike and error-burst thresholds — with the
// same re-stamp provenance discipline as mosPoint.
type httpPoint struct {
	t       time.Time
	ttfbP95 float64
	errs    uint64 // the interval's request errors (chronic-failure baseline)
	ok      bool
	synced  bool // false: t is on the remote clock, never joined against t0
}

// videoPoint is one end's latest video interval: the pre-handover buffer
// level (buffer-drain baseline) and interval average bitrate (the "from" side
// of an ABR downshift — vidstream reports no per-switch bitrates, so interval
// averages are the honest stand-in).
type videoPoint struct {
	t       time.Time
	buffer  float64 // BufferMs at the sample instant
	bitrate float64 // interval AvgBitrateKbps
	ok      bool
	synced  bool
}

// videoImpact tracks one end's video evidence inside a handover window.
type videoImpact struct {
	// started sums interval Stalls of windowed samples; closed counts stall
	// events first seen while the window was open (attributed or not, so a
	// pre-handover stall whose event arrives in the straddling interval nets
	// out). started > closed at resolution = a stall still open.
	started, closed uint64
	stallTimeMs     float64 // interval stall time accumulated in the window
	stall           mediaGap
	haveStall       bool      // an attributed COMPLETED stall exists
	lastEnd         time.Time // latest attributed stall end (re-stamped)
	bufBase         float64   // buffer level of the last interval before t0
	bitBase         float64
	haveBase        bool
	bufMin          float64 // lowest windowed buffer level
	haveMin         bool
	abrFrom, abrTo  float64 // interval avg bitrate around the first downshift
	abrAt           time.Time
	abrSeen         bool
}

// httpImpact tracks one end's HTTP evidence inside a handover window.
type httpImpact struct {
	// errs sums windowed request errors ABOVE the baseline interval's error
	// count: a session erroring at a steady rate before and after the
	// handover (chronically bad origin/path) is background failure, not a
	// handover-coincident burst — only the excess is evidence.
	errs     uint64
	errAt    time.Time
	errBase  uint64  // pre-t0 baseline interval's request errors
	ttfbBase float64 // pre-t0 baseline interval's TTFB p95
	haveBase bool
	ttfbPeak float64
	spikeAt  time.Time
	spike    bool
	err      time.Duration // worst re-stamp uncertainty of joined samples
}

// hoWindow is one open handover being joined against media evidence.
type hoWindow struct {
	kind         string // "XnHandover", "N2Handover", "Handover"
	t0           time.Time
	failedAt     time.Time
	completeAt   time.Time
	pathSwitchAt time.Time
	endMarkerAt  time.Time
	pre          map[string]mosPoint     // per End: baseline before t0
	dip          map[string]*mosDip      // per End: registered impact
	gap          map[string]mediaGap     // per End: longest attributed gap
	vid          map[string]*videoImpact // per End: video evidence
	http         map[string]*httpImpact  // per End: HTTP evidence
}

// video returns (creating on first use) the window's per-end video evidence.
func (w *hoWindow) video(end string) *videoImpact {
	v, ok := w.vid[end]
	if !ok {
		v = &videoImpact{}
		w.vid[end] = v
	}
	return v
}

// httpFor returns (creating on first use) the window's per-end HTTP evidence.
func (w *hoWindow) httpFor(end string) *httpImpact {
	h, ok := w.http[end]
	if !ok {
		h = &httpImpact{}
		w.http[end] = h
	}
	return h
}

// correlator joins handover events with media evidence into annotations.
// All methods are safe for concurrent use; emit is called without the
// internal lock held (it may re-enter observe, which ignores annotations).
type correlator struct {
	offset offsetFunc                      // nil: remote gaps stay unsynced
	emit   func(at time.Time, text string) // nil: annotations only retained

	window     time.Duration
	gapSlack   time.Duration
	mosDip     float64
	mosRecover float64
	ttfbFactor float64

	mu          sync.Mutex
	last        map[string]mosPoint   // per End: latest interval MOS
	lastHTTP    map[string]httpPoint  // per End: latest rated HTTP interval
	lastVideo   map[string]videoPoint // per End: latest video interval
	seenGaps    map[string]struct{}   // End|startUnixNano dedup (gap lists are cumulative)
	seenStalls  map[string]struct{}   // stall-event dedup (FINAL samples replay the whole-call list)
	win         *hoWindow
	annotations []string
}

// newCorrelator returns a correlator with the default tuning. offset supplies
// the remote-minus-local clock offset for re-stamping (typically
// owd.Tracker.Offset); emit receives each finished annotation.
func newCorrelator(offset offsetFunc, emit func(at time.Time, text string)) *correlator {
	return &correlator{
		offset:     offset,
		emit:       emit,
		window:     corrWindow,
		gapSlack:   corrGapSlack,
		mosDip:     corrMOSDip,
		mosRecover: corrMOSRecover,
		ttfbFactor: corrTTFBFactor,
		last:       make(map[string]mosPoint),
		lastHTTP:   make(map[string]httpPoint),
		lastVideo:  make(map[string]videoPoint),
		seenGaps:   make(map[string]struct{}),
		seenStalls: make(map[string]struct{}),
	}
}

// observe feeds one published AppSample through the join. It returns the
// media gaps first seen in this sample — re-stamped onto orbit's clock where
// possible — so the caller can feed them onward (Prometheus histogram)
// without duplicating the dedup state.
func (c *correlator) observe(a AppSample) []mediaGap {
	if a.Event == AppEventAnnotation {
		return nil // our own output; never joined again
	}

	c.mu.Lock()
	var pending []pendingNote
	// Lazy deadline: any observation past the window settles it first —
	// except samples still on the remote clock, whose stamps cannot be
	// compared with the local t0 (a skewed-ahead remote clock would close
	// the join before local evidence arrived).
	if c.win != nil && !a.Time.IsZero() && a.TimeSource != "remote-clock" &&
		a.Time.Sub(c.win.t0) >= c.window {
		pending = append(pending, c.resolveLocked(a.Time, "within "+fmtDur(c.window)))
	}

	var newGaps []mediaGap
	switch {
	case a.Event != "":
		switch a.Event {
		case StateHandoverStarted:
			if c.win != nil {
				pending = append(pending, c.resolveLocked(a.Time, "before the next handover"))
			}
			c.win = &hoWindow{
				kind: handoverKind(a.Detail),
				t0:   a.Time,
				pre:  make(map[string]mosPoint),
				dip:  make(map[string]*mosDip),
				gap:  make(map[string]mediaGap),
				vid:  make(map[string]*videoImpact),
				http: make(map[string]*httpImpact),
			}
		case StateHandoverComplete:
			if c.win != nil && c.win.completeAt.IsZero() {
				c.win.completeAt = a.Time
			}
		case StateHandoverFailed:
			if c.win != nil && c.win.failedAt.IsZero() {
				c.win.failedAt = a.Time
			}
		case StatePathSwitchComplete:
			if c.win != nil && c.win.pathSwitchAt.IsZero() {
				c.win.pathSwitchAt = a.Time
			}
		case AppEventEndMarker:
			if c.win != nil && c.win.endMarkerAt.IsZero() {
				c.win.endMarkerAt = a.Time
			}
		}
	case a.VoIP != nil && a.End != "":
		newGaps = c.gapsLocked(a)
		if note, resolved := c.mosLocked(a); resolved {
			pending = append(pending, note)
		}
	case a.Video != nil && a.End != "":
		c.videoLocked(a)
	case a.HTTP != nil && a.End != "":
		c.httpLocked(a)
	}
	c.mu.Unlock()

	for _, n := range pending {
		if c.emit != nil {
			c.emit(n.at, n.text)
		}
	}
	return newGaps
}

// flush settles any open window at session teardown.
func (c *correlator) flush(at time.Time) {
	c.mu.Lock()
	var pending []pendingNote
	if c.win != nil {
		pending = append(pending, c.resolveLocked(at, "by session end"))
	}
	c.mu.Unlock()
	for _, n := range pending {
		if c.emit != nil {
			c.emit(n.at, n.text)
		}
	}
}

// list returns the annotations composed so far, in emission order.
func (c *correlator) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.annotations...)
}

// gapsLocked extracts the sample's first-seen media gaps, re-stamps remote
// ones onto orbit's clock, and attributes them to the open window.
func (c *correlator) gapsLocked(a AppSample) []mediaGap {
	var out []mediaGap
	for _, g := range a.VoIP.MediaGaps {
		key := a.End + "|" + fmt.Sprint(g.Start.UnixNano())
		if _, dup := c.seenGaps[key]; dup {
			continue
		}
		c.seenGaps[key] = struct{}{}

		mg := mediaGap{
			End:    a.End,
			Start:  g.Start,
			Dur:    g.End.Sub(g.Start),
			Lost:   g.PacketsLost,
			Synced: true,
		}
		if a.End == AppEndN6 {
			// Remote clock: re-stamp with the tracker offset (remote −
			// local, so local = remote − offset) and carry its error bound.
			mg.Synced = false
			if c.offset != nil {
				if off, errBound, ok := c.offset(); ok {
					mg.Start = g.Start.Add(-off)
					mg.Err = errBound
					mg.Synced = true
				}
			}
		}

		// Attribute to the open window only when the SILENCE overlaps it:
		// the gap must end after t0 (a hole that opened and healed before
		// the handover began is pre-existing trouble, not handover impact)
		// and start within [t0−slack, t0+window]. For an unsynced remote
		// gap this comparison is cross-clock best-effort; the annotation
		// labels it and renders no precise offset.
		if w := c.win; w != nil &&
			!mg.Start.Before(w.t0.Add(-c.gapSlack)) &&
			mg.Start.Before(w.t0.Add(c.window)) &&
			mg.Start.Add(mg.Dur).After(w.t0) {
			if cur, ok := w.gap[a.End]; !ok || mg.Dur > cur.Dur {
				w.gap[a.End] = mg
			}
		}
		out = append(out, mg)
	}
	return out
}

// mosLocked runs one end's interval MOS through the dip/recovery state
// machine, resolving the window early when every dipped end has recovered.
func (c *correlator) mosLocked(a AppSample) (pendingNote, bool) {
	prev := c.last[a.End]
	mos := a.VoIP.MOSCQ
	pt := mosPoint{t: a.Time, mos: mos, ok: true, err: a.TimeErr,
		synced: a.TimeSource != "remote-clock"}
	if mos > 0 { // 0 = no rating yet (no packets / no RTCP), never a dip
		c.last[a.End] = pt
	}
	w := c.win
	// Points still on the remote clock cannot be joined against the local
	// t0 (baseline ordering, interval offsets, and the window deadline would
	// all be cross-clock fabrications); they only refresh c.last above.
	if w == nil || mos <= 0 || !pt.synced {
		return pendingNote{}, false
	}

	// Baseline: the last rated interval strictly before the handover.
	if _, have := w.pre[a.End]; !have && prev.ok && prev.synced && prev.t.Before(w.t0) {
		w.pre[a.End] = prev
	}
	base, haveBase := w.pre[a.End]
	if !haveBase {
		return pendingNote{}, false
	}

	d := w.dip[a.End]
	if mos <= base.mos-c.mosDip {
		from := prev.t
		if !prev.ok || !prev.synced || from.Before(w.t0) {
			from = w.t0
		}
		if d == nil {
			d = &mosDip{pre: base.mos, worst: mos, worstFrom: from, worstTo: a.Time, err: pt.err}
			w.dip[a.End] = d
		} else if mos < d.worst {
			d.worst, d.worstFrom, d.worstTo = mos, from, a.Time
		}
		if pt.err > d.err {
			d.err = pt.err
		}
		d.recovered = false
	} else if d != nil && !d.recovered && mos >= base.mos-c.mosRecover {
		d.recovered = true
		d.recoveredAt = a.Time
	}

	// Early resolution: at least one dip, and every dipped end recovered.
	if len(w.dip) > 0 {
		for _, dd := range w.dip {
			if !dd.recovered {
				return pendingNote{}, false
			}
		}
		return c.resolveLocked(a.Time, ""), true
	}
	return pendingNote{}, false
}

// videoLocked runs one end's video interval through the join: stall events
// under exactly the media-gap rules (dedup, remote re-stamp with error bound,
// silence-overlap attribution), plus the buffer/ABR evidence around them.
// There is no early resolution — a downshift often lands one interval after
// its stall, so video windows settle at the deadline or flush.
func (c *correlator) videoLocked(a AppSample) {
	v := a.Video
	prev := c.lastVideo[a.End]
	pt := videoPoint{t: a.Time, buffer: v.BufferMs, bitrate: v.AvgBitrateKbps,
		ok: true, synced: a.TimeSource != "remote-clock"}
	if !a.Final { // FINAL samples are whole-call cumulative, not an interval
		c.lastVideo[a.End] = pt
	}
	w := c.win

	// Stall events: first-seen only (a remote FINAL sample replays the
	// whole-call list), re-stamped like media gaps, attributed to the window
	// when the STALLED SPAN overlaps it. Every first-seen event while the
	// window is open also counts as a closed stall, so a pre-handover stall
	// whose event arrives in the straddling interval balances that
	// interval's started count instead of reading as still open.
	for _, g := range v.StallEvents {
		key := a.End + "|stall|" + fmt.Sprint(g.Start.UnixNano())
		if _, dup := c.seenStalls[key]; dup {
			continue
		}
		c.seenStalls[key] = struct{}{}

		mg := mediaGap{End: a.End, Start: g.Start, Dur: g.End.Sub(g.Start), Synced: true}
		if a.End == AppEndN6 {
			mg.Synced = false
			if c.offset != nil {
				if off, errBound, ok := c.offset(); ok {
					mg.Start = g.Start.Add(-off)
					mg.Err = errBound
					mg.Synced = true
				}
			}
		}
		if w == nil {
			continue
		}
		vi := w.video(a.End)
		vi.closed++
		if !mg.Start.Before(w.t0.Add(-c.gapSlack)) &&
			mg.Start.Before(w.t0.Add(c.window)) &&
			mg.Start.Add(mg.Dur).After(w.t0) {
			if !vi.haveStall || mg.Dur > vi.stall.Dur {
				vi.stall, vi.haveStall = mg, true
			}
			if end := mg.Start.Add(mg.Dur); mg.Synced && end.After(vi.lastEnd) {
				vi.lastEnd = end
			}
		}
	}

	// Interval evidence joins need a shared clock (same rule as mosLocked:
	// remote-clock points refresh lastVideo above but are never compared
	// with the local t0).
	if w == nil || !pt.synced {
		return
	}
	vi := w.video(a.End)
	if !vi.haveBase && prev.ok && prev.synced && prev.t.Before(w.t0) {
		vi.bufBase, vi.bitBase, vi.haveBase = prev.buffer, prev.bitrate, true
	}
	if a.Final || a.Time.Before(w.t0) {
		return // cumulative counters would double-count; pre-t0 intervals are baseline only
	}
	// Interval Stalls counters carry no start times, so bound the start from
	// the interval's own stall time: a stall that accumulated S ms by the
	// sample instant began no LATER than a.Time − S. If even that latest
	// possible start precedes t0−slack, the stall predates the attribution
	// window — the same rule the completed-stall path applies to exact
	// starts — and it must not be counted toward "stall ongoing": a
	// never-closing stall that opened before the handover is pre-existing
	// trouble, not handover impact (its close event, whenever it lands,
	// still nets the unconditional closed count above).
	if v.Stalls > 0 {
		latestStart := a.Time.Add(-time.Duration(v.StallTimeMs * float64(time.Millisecond)))
		if !latestStart.Before(w.t0.Add(-c.gapSlack)) {
			vi.started += v.Stalls
		}
	}
	vi.stallTimeMs += v.StallTimeMs
	if !vi.haveMin || v.BufferMs < vi.bufMin {
		vi.bufMin, vi.haveMin = v.BufferMs, true
	}
	if v.RepSwitchesDown > 0 && !vi.abrSeen {
		vi.abrSeen, vi.abrAt, vi.abrTo = true, a.Time, v.AvgBitrateKbps
		// The "from" bitrate is the previous interval's average (per-switch
		// rungs are not reported); fall back to the pre-handover baseline.
		switch {
		case prev.ok && prev.synced && prev.bitrate > 0:
			vi.abrFrom = prev.bitrate
		case vi.haveBase:
			vi.abrFrom = vi.bitBase
		}
	}
}

// httpLocked runs one end's HTTP interval through the join: request-error
// bursts and TTFB-p95 spikes inside the window become coincident annotations.
// BOTH are thresholded against the last rated pre-handover interval (errors:
// only the count above the baseline interval's errors; TTFB: baseline p95 ×
// corrTTFBFactor) — with no baseline neither is claimed, since a chronically
// failing origin or a threshold against nothing would fabricate handover
// evidence. Deliberately simple (design Phase 6): no dip/recovery state
// machine, no early resolution.
func (c *correlator) httpLocked(a AppSample) {
	h := a.HTTP
	prev := c.lastHTTP[a.End]
	pt := httpPoint{t: a.Time, ttfbP95: h.TTFBMsP95, errs: h.Errors,
		ok: h.Requests > 0, synced: a.TimeSource != "remote-clock"}
	if !a.Final && pt.ok { // only rated intervals make a baseline
		c.lastHTTP[a.End] = pt
	}
	w := c.win
	if w == nil || !pt.synced || a.Final || a.Time.Before(w.t0) {
		return
	}
	hi := w.httpFor(a.End)
	if !hi.haveBase && prev.ok && prev.synced && prev.t.Before(w.t0) {
		hi.ttfbBase, hi.errBase, hi.haveBase = prev.ttfbP95, prev.errs, true
	}
	if a.TimeErr > hi.err {
		hi.err = a.TimeErr
	}
	if hi.haveBase && h.Errors > hi.errBase {
		if hi.errs == 0 {
			hi.errAt = a.Time
		}
		hi.errs += h.Errors - hi.errBase
	}
	if hi.haveBase && hi.ttfbBase > 0 && h.TTFBMsP95 >= hi.ttfbBase*c.ttfbFactor {
		if !hi.spike || h.TTFBMsP95 > hi.ttfbPeak {
			hi.ttfbPeak, hi.spikeAt = h.TTFBMsP95, a.Time
		}
		hi.spike = true
	}
}

// pendingNote is a composed annotation awaiting emission outside the lock.
type pendingNote struct {
	at   time.Time
	text string
}

// resolveLocked closes the open window into an annotation. tail phrases the
// bound that closed an unrecovered/quiet window ("within 15.0s", "by session
// end", "before the next handover"); empty means recovery closed it.
func (c *correlator) resolveLocked(at time.Time, tail string) pendingNote {
	w := c.win
	c.win = nil

	parts := []string{fmt.Sprintf("%s @%s", w.kind, w.t0.Format("15:04:05.000"))}
	if !w.failedAt.IsZero() {
		parts = append(parts, "handover FAILED @"+relTime(w.failedAt, w.t0))
	}
	if !w.pathSwitchAt.IsZero() {
		parts = append(parts, "path switch @"+relTime(w.pathSwitchAt, w.t0))
	}
	if !w.endMarkerAt.IsZero() {
		parts = append(parts, "End Marker @"+relTime(w.endMarkerAt, w.t0))
	}
	for _, end := range []string{AppEndUE, AppEndN6} {
		g, ok := w.gap[end]
		if !ok {
			continue
		}
		dir := "DL"
		if end == AppEndN6 {
			dir = "UL"
		}
		s := fmt.Sprintf("%s media gap %s", dir, fmtDur(g.Dur))
		if g.Lost > 0 {
			s += fmt.Sprintf(" (%d pkts)", g.Lost)
		}
		if g.Synced {
			// The error bound belongs to the gap's PLACEMENT (Start was
			// re-stamped); the duration is a same-clock difference with no
			// re-stamp error, so the bracket rides with the @offset.
			s += " @" + relTime(g.Start, w.t0)
			if g.Err > 0 {
				s += fmt.Sprintf(" [±%s]", fmtDur(g.Err))
			}
		} else {
			// No offset against orbit's clock exists: printing a precise
			// @offset would be a cross-clock fabrication.
			s += " [remote clock unsynced]"
		}
		parts = append(parts, s)
	}
	// Video evidence: buffer drain → stall (completed and/or still open) →
	// ABR downshift (a consequence CANDIDATE: temporal overlap only, so it
	// is labeled coincident). Stall placement carries the same re-stamp
	// error-bar discipline as media gaps.
	stallImpact, stallOpen := false, false
	var lastStallEnd time.Time
	for _, end := range []string{AppEndUE, AppEndN6} {
		vi, ok := w.vid[end]
		if !ok {
			continue
		}
		tag := ""
		if end == AppEndN6 {
			tag = " (n6)"
		}
		open := vi.started > vi.closed
		if (vi.haveStall || open) && vi.haveBase && vi.haveMin && vi.bufMin < vi.bufBase {
			parts = append(parts, fmt.Sprintf("buffer drain%s %s→%s", tag, fmtMs(vi.bufBase), fmtMs(vi.bufMin)))
		}
		if vi.haveStall {
			stallImpact = true
			s := fmt.Sprintf("%s stall%s", fmtDur(vi.stall.Dur), tag)
			if vi.stall.Synced {
				s += " @" + relTime(vi.stall.Start, w.t0)
				if vi.stall.Err > 0 {
					s += fmt.Sprintf(" [±%s]", fmtDur(vi.stall.Err))
				}
			} else {
				s += " [remote clock unsynced]"
			}
			parts = append(parts, s)
			if vi.lastEnd.After(lastStallEnd) {
				lastStallEnd = vi.lastEnd
			}
		}
		if open {
			// A stall with no closing event by resolution time: report the
			// window's accumulated stall time as a floor, never a made-up
			// duration.
			stallImpact, stallOpen = true, true
			s := "stall" + tag + " ongoing"
			if vi.stallTimeMs > 0 {
				s += fmt.Sprintf(" (≥%s stalled)", fmtMs(vi.stallTimeMs))
			}
			parts = append(parts, s)
		}
		if vi.abrSeen {
			s := "ABR downshift"
			if vi.abrFrom > 0 && vi.abrTo > 0 {
				s += fmt.Sprintf(" %.0f→%.0f kbps", vi.abrFrom, vi.abrTo)
			}
			s += tag + " @" + relTime(vi.abrAt, w.t0) + " (coincident)"
			parts = append(parts, s)
		}
	}
	dipped, allRecovered := false, true
	var lastRecover time.Time
	for _, end := range []string{AppEndUE, AppEndN6} {
		d, ok := w.dip[end]
		if !ok {
			continue
		}
		dipped = true
		iv := fmt.Sprintf("interval %s..%s", relTime(d.worstFrom, w.t0), relTime(d.worstTo, w.t0))
		if d.err > 0 {
			iv += fmt.Sprintf(" [±%s]", fmtDur(d.err))
		}
		parts = append(parts, fmt.Sprintf("MOS-CQ(%s) %.2f→%.2f (%s)", end, d.pre, d.worst, iv))
		if !d.recovered {
			allRecovered = false
		} else if d.recoveredAt.After(lastRecover) {
			lastRecover = d.recoveredAt
		}
	}
	// HTTP evidence, coincident by construction (design Phase 6: correlation,
	// not causation — a request-error burst or TTFB spike merely OVERLAPS the
	// handover window).
	httpEvidence := false
	for _, end := range []string{AppEndUE, AppEndN6} {
		hi, ok := w.http[end]
		if !ok {
			continue
		}
		tag := ""
		if end == AppEndN6 {
			tag = " (n6)"
		}
		if hi.errs > 0 {
			httpEvidence = true
			word := "errors"
			if hi.errs == 1 {
				word = "error"
			}
			s := fmt.Sprintf("HTTP error burst%s %d %s", tag, hi.errs, word)
			if hi.errBase > 0 {
				// Excess over a nonzero steady error rate: say so, or a
				// chronically failing origin reads as handover evidence.
				s += fmt.Sprintf(" above the pre-handover rate (%d/interval)", hi.errBase)
			}
			s += " @" + relTime(hi.errAt, w.t0)
			if hi.err > 0 {
				s += fmt.Sprintf(" [±%s]", fmtDur(hi.err))
			}
			parts = append(parts, s+" (coincident)")
		}
		if hi.spike {
			httpEvidence = true
			// Named for what it is: the baseline is ONE interval's p95 (the
			// last rated interval before t0), not a long-run reference.
			s := fmt.Sprintf("TTFB p95 spike%s %s→%s (≥%.0f× the last pre-handover interval) @%s",
				tag, fmtMs(hi.ttfbBase), fmtMs(hi.ttfbPeak), c.ttfbFactor, relTime(hi.spikeAt, w.t0))
			if hi.err > 0 {
				s += fmt.Sprintf(" [±%s]", fmtDur(hi.err))
			}
			parts = append(parts, s+" (coincident)")
		}
	}
	abrEvidence := false
	for _, vi := range w.vid {
		if vi.abrSeen {
			abrEvidence = true
		}
	}
	// Blackout: an end that was rated before the handover but never again
	// after it. loom only records a Gap when the NEXT packet arrives, and
	// unrated intervals never dip, so without this check total silence —
	// the worst outcome — would read as "no media impact" (the SD-Core D-4
	// signature the report exists to pinpoint: DL gap begins at the
	// handover, never recovers).
	silent := false
	if at.Sub(w.t0) >= corrSilenceMin {
		for _, end := range []string{AppEndUE, AppEndN6} {
			if _, hasDip := w.dip[end]; hasDip {
				continue
			}
			p := c.last[end]
			if !p.ok || !p.synced || !p.t.Before(w.t0) {
				continue
			}
			dir := "DL"
			if end == AppEndN6 {
				dir = "UL"
			}
			parts = append(parts, fmt.Sprintf("%s media silent since handover (last rated interval @%s)",
				dir, relTime(p.t, w.t0)))
			silent = true
		}
	}
	impact := dipped || stallImpact
	recoveredAll := !stallOpen && !silent && (!dipped || allRecovered)
	switch {
	case impact && recoveredAll:
		// Recovery instant: the later of the last MOS recovery and the last
		// attributed stall's end (a completed stall IS resumed playback). An
		// unsynced stall end yields a bare "recovered" rather than a
		// cross-clock offset.
		at := lastRecover
		if lastStallEnd.After(at) {
			at = lastStallEnd
		}
		if at.IsZero() {
			parts = append(parts, "recovered")
		} else {
			parts = append(parts, "recovered @"+relTime(at, w.t0))
		}
	case impact || silent:
		msg := "not recovered"
		if tail != "" {
			msg += " " + tail
		}
		parts = append(parts, msg)
	case len(w.gap) == 0 && !httpEvidence && !abrEvidence:
		// Coincident-only evidence (HTTP trouble, an ABR downshift with no
		// stall) neither recovers nor fails — but it is evidence, so the
		// all-clear is reserved for a genuinely quiet window.
		parts = append(parts, "no media impact observed "+tail)
	}

	text := strings.Join(parts, " → ")
	c.annotations = append(c.annotations, text)
	return pendingNote{at: at, text: text}
}

// handoverKind names the handover flavor from the HANDOVER_STARTED detail
// ("Xn handover gnb1 → gnb2" / "N2 handover …" — handover.go, xn.go).
func handoverKind(detail string) string {
	switch {
	case strings.HasPrefix(detail, "Xn"):
		return "XnHandover"
	case strings.HasPrefix(detail, "N2"):
		return "N2Handover"
	default:
		return "Handover"
	}
}

// relTime renders t relative to t0: "+240ms", "-1.5s".
func relTime(t, t0 time.Time) string {
	d := t.Sub(t0)
	if d < 0 {
		return "-" + fmtDur(-d)
	}
	return "+" + fmtDur(d)
}

// fmtMs renders a float millisecond quantity (loom's *_ms snapshot fields)
// through fmtDur's compaction.
func fmtMs(ms float64) string {
	return fmtDur(time.Duration(ms * float64(time.Millisecond)))
}

// fmtDur renders a non-negative duration compactly: sub-10ms with decimals
// ("1.2ms"), sub-second in whole milliseconds ("240ms"), else seconds
// ("3.0s").
func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}
