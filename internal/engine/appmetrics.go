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
type appMetrics struct {
	mos      *prometheus.GaugeVec     // orbit_app_mos{supi,app,end}
	jitter   *prometheus.GaugeVec     // orbit_app_jitter_ms{supi,app,end}
	loss     *prometheus.GaugeVec     // orbit_app_loss_pct{supi,app,end}
	owd      *prometheus.GaugeVec     // orbit_app_owd_ms{supi,app,end}
	owdErr   *prometheus.GaugeVec     // orbit_app_owd_err_ms{supi,app,end}
	mediaGap *prometheus.HistogramVec // orbit_app_media_gap_ms{supi,app,end}
	handover *prometheus.GaugeVec     // orbit_ue_handover_timestamp_seconds{supi}
}

var appLabels = []string{"supi", "app", "end"}

// newAppMetrics builds and registers the app metric families on reg.
func newAppMetrics(reg prometheus.Registerer) *appMetrics {
	gauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{
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
	}
	reg.MustRegister(a.mos, a.jitter, a.loss, a.owd, a.owdErr, a.mediaGap, a.handover)
	return a
}

// EnableAppMetrics registers the app-session Prometheus metrics on reg and
// starts recording. Call once, before serving traffic (typically right after
// NewManager); registering the same families twice on one registry panics,
// per MustRegister.
func (m *Manager) EnableAppMetrics(reg prometheus.Registerer) {
	m.appMetrics = newAppMetrics(reg)
}

// observeSample updates the per-interval gauges from one quality sample.
// Events and non-VoIP samples are ignored.
func (a *appMetrics) observeSample(supi, app string, s AppSample) {
	if a == nil || s.VoIP == nil || s.End == "" {
		return
	}
	l := prometheus.Labels{"supi": supi, "app": app, "end": s.End}
	a.mos.With(l).Set(s.VoIP.MOSCQ)
	a.jitter.With(l).Set(s.VoIP.JitterMs)
	a.loss.With(l).Set(s.VoIP.LossPct)
	a.owd.With(l).Set(s.VoIP.OWDMs)
	a.owdErr.With(l).Set(s.VoIP.OWDErrMs)
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

// cleanupSession deletes a finished session's labeled GAUGE series so they
// do not linger at their last value after the call ends. The media-gap
// HISTOGRAM is deliberately kept: it is cumulative, so deleting it here
// would drop every observation from a session that ends between two scrapes
// and reset the counters mid-run (breaking increase()/rate()); the "frozen
// gauge reads as a live call" rationale does not apply to it. Its series are
// reaped when the UE deregisters (forgetUE).
func (a *appMetrics) cleanupSession(supi, app string) {
	if a == nil {
		return
	}
	match := prometheus.Labels{"supi": supi, "app": app}
	for _, g := range []*prometheus.GaugeVec{a.mos, a.jitter, a.loss, a.owd, a.owdErr} {
		g.DeletePartialMatch(match)
	}
}

// forgetUE drops every series of a deregistered UE: its handover timestamp
// and — belt and braces, sessions clean up after themselves — any app series
// left behind by a teardown that timed out.
func (a *appMetrics) forgetUE(supi string) {
	if a == nil {
		return
	}
	a.handover.DeleteLabelValues(supi)
	match := prometheus.Labels{"supi": supi}
	for _, g := range []*prometheus.GaugeVec{a.mos, a.jitter, a.loss, a.owd, a.owdErr} {
		g.DeletePartialMatch(match)
	}
	a.mediaGap.DeletePartialMatch(match)
}
