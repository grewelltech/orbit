package engine

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bgrewell/loom/core/metrics"
)

// counterValue reads one counter series by exact label match; ok=false when
// the series does not exist.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(want) {
				continue
			}
			for _, lp := range m.GetLabel() {
				if want[lp.GetName()] != lp.GetValue() {
					continue metric
				}
			}
			return m.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

// TestAppMetricsTCPApps covers the Phase 6-7 families: registration under the
// design's metric names, gauge sets and counter adds from HTTP/video samples
// (Final samples excluded from counters — whole-call cumulative snapshots
// would double-count), and the two cleanup paths: session end deletes the
// gauges but keeps the cumulative counters (the media-gap-histogram
// rationale: deleting mid-run drops scrape-window increments and resets
// increase()/rate()), UE deregistration reaps everything.
func TestAppMetricsTCPApps(t *testing.T) {
	reg := prometheus.NewRegistry()
	am := newAppMetrics(reg)

	// HTTP: two ue intervals plus one n6 interval, then a FINAL sample.
	am.observeSample("imsi-1", "http", AppSample{End: AppEndUE, HTTP: &metrics.HTTP{
		Requests: 10, Errors: 2, TTFBMsP95: 45.5, GoodputMbps: 12.25,
	}})
	am.observeSample("imsi-1", "http", AppSample{End: AppEndUE, HTTP: &metrics.HTTP{
		Requests: 8, Errors: 3, TTFBMsP95: 62, GoodputMbps: 9.5,
	}})
	am.observeSample("imsi-1", "http", AppSample{End: AppEndN6, HTTP: &metrics.HTTP{
		Requests: 18, Errors: 1, TTFBMsP95: 30, GoodputMbps: 13,
	}})
	am.observeSample("imsi-1", "http", AppSample{End: AppEndUE, Final: true, HTTP: &metrics.HTTP{
		Requests: 18, Errors: 5, TTFBMsP95: 55, GoodputMbps: 11,
	}})

	// Video: two ue intervals, then a FINAL sample.
	am.observeSample("imsi-1", "video", AppSample{End: AppEndUE, Video: &metrics.Video{
		Stalls: 1, StallTimeMs: 800, BufferMs: 4200, AvgBitrateKbps: 2500,
	}})
	am.observeSample("imsi-1", "video", AppSample{End: AppEndUE, Video: &metrics.Video{
		Stalls: 1, StallTimeMs: 950, BufferMs: 0, AvgBitrateKbps: 1200,
	}})
	am.observeSample("imsi-1", "video", AppSample{End: AppEndUE, Final: true, Video: &metrics.Video{
		Stalls: 2, StallTimeMs: 1750, BufferMs: 0, AvgBitrateKbps: 1850,
	}})

	// Events are ignored by the sample path.
	am.observeSample("imsi-1", "http", AppSample{Event: AppEventEndMarker})

	// Registration: the design's metric names are present on the registry.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"orbit_app_ttfb_ms", "orbit_app_goodput_mbps", "orbit_app_http_errors_total",
		"orbit_app_stalls_total", "orbit_app_stall_time_ms",
		"orbit_app_buffer_ms", "orbit_app_bitrate_kbps",
	} {
		if !names[want] {
			t.Errorf("metric %s not registered (have %v)", want, names)
		}
	}

	httpUE := map[string]string{"supi": "imsi-1", "app": "http", "end": AppEndUE}
	httpN6 := map[string]string{"supi": "imsi-1", "app": "http", "end": AppEndN6}
	vidUE := map[string]string{"supi": "imsi-1", "app": "video", "end": AppEndUE}

	// Gauges hold the LATEST sample (Final included: a whole-call value is a
	// fine last reading for a gauge about to be deleted).
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"orbit_app_ttfb_ms", httpUE, 55},
		{"orbit_app_ttfb_ms", httpN6, 30},
		{"orbit_app_goodput_mbps", httpUE, 11},
		{"orbit_app_goodput_mbps", httpN6, 13},
		{"orbit_app_buffer_ms", vidUE, 0},
		{"orbit_app_bitrate_kbps", vidUE, 1850},
	} {
		v, ok := gaugeValue(t, reg, c.name, c.labels)
		if !ok || v != c.want {
			t.Errorf("%s%v = %v (ok=%v), want %v", c.name, c.labels, v, ok, c.want)
		}
	}

	// Counters accumulate interval deltas; the FINAL cumulative sample must
	// NOT be added on top.
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"orbit_app_http_errors_total", httpUE, 5},
		{"orbit_app_http_errors_total", httpN6, 1},
		{"orbit_app_stalls_total", vidUE, 2},
		{"orbit_app_stall_time_ms", vidUE, 1750},
	} {
		v, ok := counterValue(t, reg, c.name, c.labels)
		if !ok || v != c.want {
			t.Errorf("%s%v = %v (ok=%v), want %v", c.name, c.labels, v, ok, c.want)
		}
	}

	// Session end: the TCP-app gauges go, the cumulative counters stay.
	am.cleanupSession("imsi-1", "http")
	am.cleanupSession("imsi-1", "video")
	for _, name := range []string{
		"orbit_app_ttfb_ms", "orbit_app_goodput_mbps",
		"orbit_app_buffer_ms", "orbit_app_bitrate_kbps",
	} {
		if left := gatherFamily(t, reg, name); len(left) != 0 {
			t.Errorf("%s: %d series left after cleanupSession: %v", name, len(left), left)
		}
	}
	if v, ok := counterValue(t, reg, "orbit_app_http_errors_total", httpUE); !ok || v != 5 {
		t.Errorf("http_errors_total must survive session cleanup, got %v (ok=%v)", v, ok)
	}
	if v, ok := counterValue(t, reg, "orbit_app_stalls_total", vidUE); !ok || v != 2 {
		t.Errorf("stalls_total must survive session cleanup, got %v (ok=%v)", v, ok)
	}
	if v, ok := counterValue(t, reg, "orbit_app_stall_time_ms", vidUE); !ok || v != 1750 {
		t.Errorf("stall_time_ms must survive session cleanup, got %v (ok=%v)", v, ok)
	}

	// Deregistration forgets the UE entirely, straggler gauges included.
	am.observeSample("imsi-1", "http", AppSample{End: AppEndUE, HTTP: &metrics.HTTP{TTFBMsP95: 40}})
	am.forgetUE("imsi-1")
	for _, name := range []string{
		"orbit_app_ttfb_ms", "orbit_app_goodput_mbps", "orbit_app_http_errors_total",
		"orbit_app_stalls_total", "orbit_app_stall_time_ms",
		"orbit_app_buffer_ms", "orbit_app_bitrate_kbps",
	} {
		if left := gatherFamily(t, reg, name); len(left) != 0 {
			t.Errorf("%s: series left after forgetUE: %v", name, left)
		}
	}

	// A nil *appMetrics (metrics disabled) is a no-op receiver for the new
	// sample kinds too.
	var off *appMetrics
	off.observeSample("x", "http", AppSample{End: AppEndUE, HTTP: &metrics.HTTP{Errors: 1}})
	off.observeSample("x", "video", AppSample{End: AppEndUE, Video: &metrics.Video{Stalls: 1}})
	off.observeGap("x", "video", AppEndUE, time.Second)
	off.cleanupSession("x", "http")
	off.forgetUE("x")
}
