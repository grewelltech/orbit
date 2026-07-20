// Prometheus metrics for app sessions (docs/design/real-app-traffic.md §7):
// per-interval quality gauges labeled {supi,app,end}, a media-gap histogram,
// and the per-UE handover timestamp gauge that makes Grafana annotations
// free. Registered once on the server's existing observability registry
// (Manager.EnableAppMetrics); every update is a lock-free atomic set on a
// pre-labeled child, so the publish path never blocks on observability
// (internal/observability invariant, same shape as internal/load's
// PrometheusObserver).
//
// Gauges follow the "current value, cleaned up at end" discipline: a
// session's labeled series are deleted when it finalizes (a gauge frozen at
// its last MOS would otherwise read as a live call forever), and a UE's
// handover timestamp is deleted at deregistration.
package engine

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// appMetrics holds the app-session metric families. A nil *appMetrics is a
// valid no-op receiver, so callers never nil-check (metrics are optional:
// only servers call EnableAppMetrics).
//
// Counters (orbit_app_http_errors_total, orbit_app_stalls_total,
// orbit_app_stall_time_ms) follow the media-gap histogram's lifecycle, not
// the gauges': they are cumulative, so deleting them at session end would
// drop increments landed between two scrapes and reset increase()/rate()
// mid-run. They are reaped at UE deregistration (forgetUE).
type appMetrics struct {
	mos      *prometheus.GaugeVec     // orbit_app_mos{supi,app,end}
	jitter   *prometheus.GaugeVec     // orbit_app_jitter_ms{supi,app,end}
	loss     *prometheus.GaugeVec     // orbit_app_loss_pct{supi,app,end}
	owd      *prometheus.GaugeVec     // orbit_app_owd_ms{supi,app,end}
	owdErr   *prometheus.GaugeVec     // orbit_app_owd_err_ms{supi,app,end}
	mediaGap *prometheus.HistogramVec // orbit_app_media_gap_ms{supi,app,end}
	handover *prometheus.GaugeVec     // orbit_ue_handover_timestamp_seconds{supi}

	// HTTP (design Phase 6) and video (Phase 7) families.
	ttfb      *prometheus.GaugeVec   // orbit_app_ttfb_ms{supi,app,end}
	goodput   *prometheus.GaugeVec   // orbit_app_goodput_mbps{supi,app,end}
	httpErrs  *prometheus.CounterVec // orbit_app_http_errors_total{supi,app,end}
	stalls    *prometheus.CounterVec // orbit_app_stalls_total{supi,app,end}
	stallTime *prometheus.CounterVec // orbit_app_stall_time_ms{supi,app,end}
	buffer    *prometheus.GaugeVec   // orbit_app_buffer_ms{supi,app,end}
	bitrate   *prometheus.GaugeVec   // orbit_app_bitrate_kbps{supi,app,end}
}

var appLabels = []string{"supi", "app", "end"}

// newAppMetrics builds and registers the app metric families on reg.
func newAppMetrics(reg prometheus.Registerer) *appMetrics {
	gauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit", Subsystem: "app", Name: name, Help: help,
		}, appLabels)
	}
	counter := func(name, help string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbit", Subsystem: "app", Name: name, Help: help,
		}, appLabels)
	}
	a := &appMetrics{
		mos:    gauge("mos", "Interval MOS-CQ of a running app session, per measuring end (ue=downlink view, n6=uplink view)."),
		jitter: gauge("jitter_ms", "Interval RTP interarrival jitter in milliseconds (RFC 3550 A.8)."),
		loss:   gauge("loss_pct", "Interval network packet loss percentage."),
		owd:    gauge("owd_ms", "One-way delay estimate in milliseconds; read with orbit_app_owd_err_ms."),
		owdErr: gauge("owd_err_ms", "Half-width error bound of orbit_app_owd_ms in milliseconds (honest uncertainty: the true OWD lies within owd_ms ± owd_err_ms)."),
		mediaGap: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbit", Subsystem: "app", Name: "media_gap_ms",
			Help:    "Media arrival gaps (>3·ptime of silence) in milliseconds.",
			Buckets: prometheus.ExponentialBuckets(20, 2, 12), // 20ms … ~41s
		}, appLabels),
		handover: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit", Subsystem: "ue", Name: "handover_timestamp_seconds",
			Help: "Unix time of the UE's most recent HANDOVER_STARTED event (Grafana annotation source).",
		}, []string{"supi"}),

		ttfb:      gauge("ttfb_ms", "Interval HTTP time-to-first-byte p95 in milliseconds."),
		goodput:   gauge("goodput_mbps", "Interval HTTP application-payload goodput in megabits per second."),
		httpErrs:  counter("http_errors_total", "HTTP request errors (transport failures and non-2xx responses), cumulative."),
		stalls:    counter("stalls_total", "Video player buffer-underrun (stall) events, cumulative."),
		stallTime: counter("stall_time_ms", "Total video stall time in milliseconds, cumulative."),
		buffer:    gauge("buffer_ms", "Current video player buffer level in milliseconds."),
		bitrate:   gauge("bitrate_kbps", "Interval average bitrate of fetched video segments in kilobits per second."),
	}
	reg.MustRegister(a.mos, a.jitter, a.loss, a.owd, a.owdErr, a.mediaGap, a.handover,
		a.ttfb, a.goodput, a.httpErrs, a.stalls, a.stallTime, a.buffer, a.bitrate)
	return a
}

// EnableAppMetrics registers the app-session Prometheus metrics on reg and
// starts recording. Call once, before serving traffic (typically right after
// NewManager); registering the same families twice on one registry panics,
// per MustRegister.
func (m *Manager) EnableAppMetrics(reg prometheus.Registerer) {
	m.appMetrics = newAppMetrics(reg)
}

// observeSample updates the per-interval families from one quality sample.
// Events are ignored. Counters add the interval deltas of non-Final samples
// only: a FINAL telemetry sample is whole-call cumulative, so adding it would
// double-count every increment already recorded.
func (a *appMetrics) observeSample(supi, app string, s AppSample) {
	if a == nil || s.End == "" {
		return
	}
	l := prometheus.Labels{"supi": supi, "app": app, "end": s.End}
	switch {
	case s.VoIP != nil:
		a.mos.With(l).Set(s.VoIP.MOSCQ)
		a.jitter.With(l).Set(s.VoIP.JitterMs)
		a.loss.With(l).Set(s.VoIP.LossPct)
		a.owd.With(l).Set(s.VoIP.OWDMs)
		a.owdErr.With(l).Set(s.VoIP.OWDErrMs)
	case s.HTTP != nil:
		a.ttfb.With(l).Set(s.HTTP.TTFBMsP95)
		a.goodput.With(l).Set(s.HTTP.GoodputMbps)
		if !s.Final {
			a.httpErrs.With(l).Add(float64(s.HTTP.Errors))
		}
	case s.Video != nil:
		a.buffer.With(l).Set(s.Video.BufferMs)
		a.bitrate.With(l).Set(s.Video.AvgBitrateKbps)
		if !s.Final {
			a.stalls.With(l).Add(float64(s.Video.Stalls))
			a.stallTime.With(l).Add(s.Video.StallTimeMs)
		}
	}
}

// observeGap records one first-seen media gap (the correlator dedups the
// cumulative per-snapshot gap lists).
func (a *appMetrics) observeGap(supi, app, end string, d time.Duration) {
	if a == nil {
		return
	}
	a.mediaGap.WithLabelValues(supi, app, end).Observe(float64(d) / float64(time.Millisecond))
}

// recordHandover stamps the UE's handover-started gauge.
func (a *appMetrics) recordHandover(supi string, at time.Time) {
	if a == nil {
		return
	}
	a.handover.WithLabelValues(supi).Set(float64(at.UnixNano()) / float64(time.Second))
}

// sessionGauges lists the families cleaned at session end: every per-interval
// gauge, VoIP and TCP-app alike (a gauge frozen at its last value reads as a
// live session forever).
func (a *appMetrics) sessionGauges() []*prometheus.GaugeVec {
	return []*prometheus.GaugeVec{
		a.mos, a.jitter, a.loss, a.owd, a.owdErr,
		a.ttfb, a.goodput, a.buffer, a.bitrate,
	}
}

// cumulativeVecs lists the families that survive session end and are reaped
// only at UE deregistration: the media-gap histogram and the error/stall
// counters (deleting cumulative series mid-run drops observations landed
// between two scrapes and breaks increase()/rate()).
func (a *appMetrics) cumulativeVecs() []interface {
	DeletePartialMatch(prometheus.Labels) int
} {
	return []interface {
		DeletePartialMatch(prometheus.Labels) int
	}{a.mediaGap, a.httpErrs, a.stalls, a.stallTime}
}

// cleanupSession deletes a finished session's labeled GAUGE series so they
// do not linger at their last value after the call ends. The media-gap
// HISTOGRAM and the http-error/stall COUNTERS are deliberately kept: they
// are cumulative, so deleting them here would drop every observation from a
// session that ends between two scrapes and reset the counters mid-run
// (breaking increase()/rate()); the "frozen gauge reads as a live call"
// rationale does not apply to them. Their series are reaped when the UE
// deregisters (forgetUE).
func (a *appMetrics) cleanupSession(supi, app string) {
	if a == nil {
		return
	}
	match := prometheus.Labels{"supi": supi, "app": app}
	for _, g := range a.sessionGauges() {
		g.DeletePartialMatch(match)
	}
}

// forgetUE drops every series of a deregistered UE: its handover timestamp,
// the cumulative histogram/counter series, and — belt and braces, sessions
// clean up after themselves — any app series left behind by a teardown that
// timed out.
func (a *appMetrics) forgetUE(supi string) {
	if a == nil {
		return
	}
	a.handover.DeleteLabelValues(supi)
	match := prometheus.Labels{"supi": supi}
	for _, g := range a.sessionGauges() {
		g.DeletePartialMatch(match)
	}
	for _, v := range a.cumulativeVecs() {
		v.DeletePartialMatch(match)
	}
}
