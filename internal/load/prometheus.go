package load

import "github.com/prometheus/client_golang/prometheus"

// PrometheusObserver records per-procedure latencies and outcomes as
// Prometheus metrics on a registry, so a long (soak) load run can be scraped
// and graphed live. It implements Observer.
type PrometheusObserver struct {
	latency *prometheus.HistogramVec
	total   prometheus.Counter
	failed  prometheus.Counter
}

// NewPrometheusObserver registers the load metrics on reg under namespace
// (e.g. "orbit_load"). Latency is a histogram in seconds, labelled by
// procedure ("attach", "registration", "pdu_session", …).
func NewPrometheusObserver(reg prometheus.Registerer, namespace string) *PrometheusObserver {
	p := &PrometheusObserver{
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "procedure_latency_seconds",
			Help:      "Per-procedure latency of load attempts.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 16), // 1ms … ~32s
		}, []string{"procedure"}),
		total: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "attempts_total", Help: "Total attach attempts.",
		}),
		failed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "failures_total", Help: "Failed attach attempts.",
		}),
	}
	reg.MustRegister(p.latency, p.total, p.failed)
	return p
}

// Observe records one attempt.
func (p *PrometheusObserver) Observe(s Sample) {
	p.total.Inc()
	if s.Err != nil {
		p.failed.Inc()
		return
	}
	for name, d := range s.Metrics {
		p.latency.WithLabelValues(name).Observe(d.Seconds())
	}
}
