package engine

import (
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bgrewell/loom/core/metrics"
)

// gatherFamily returns the named metric family's series as label-set →
// value maps (gauge value, or observation count for histograms). A missing
// family returns nil — the registry drops families with no series left.
func gatherFamily(t *testing.T, reg *prometheus.Registry, name string) []map[string]string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var out []map[string]string
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			out = append(out, labels)
		}
		return out
	}
	return nil
}

// gaugeValue reads one gauge series by exact label match; ok=false when the
// series does not exist.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) (float64, bool) {
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
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// histogramCount sums a histogram family's sample counts across its series.
func histogramCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			n += m.GetHistogram().GetSampleCount()
		}
	}
	return n
}

// TestAppMetricsLifecycle covers registration under the design's metric
// names, per-sample gauge updates for both ends, the handover timestamp, and
// the two cleanup paths (session end, UE deregistration).
func TestAppMetricsLifecycle(t *testing.T) {
	reg := prometheus.NewRegistry()
	am := newAppMetrics(reg)

	ue := AppSample{End: AppEndUE, VoIP: &metrics.VoIP{
		MOSCQ: 4.4, JitterMs: 3.5, LossPct: 0.5, OWDMs: 12.25, OWDErrMs: 0.9,
	}}
	n6 := AppSample{End: AppEndN6, VoIP: &metrics.VoIP{
		MOSCQ: 4.1, JitterMs: 4.5, LossPct: 1.5, OWDMs: 13.5, OWDErrMs: 1.1,
	}}
	am.observeSample("imsi-1", "voip", ue)
	am.observeSample("imsi-1", "voip", n6)
	am.observeSample("imsi-1", "voip", AppSample{Event: AppEventEndMarker}) // events: ignored
	am.observeGap("imsi-1", "voip", AppEndUE, 240*time.Millisecond)

	at := time.Unix(1750000000, 250_000_000)
	am.recordHandover("imsi-1", at)

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
		"orbit_app_mos", "orbit_app_jitter_ms", "orbit_app_loss_pct",
		"orbit_app_owd_ms", "orbit_app_owd_err_ms", "orbit_app_media_gap_ms",
		"orbit_ue_handover_timestamp_seconds",
	} {
		if !names[want] {
			t.Errorf("metric %s not registered (have %v)", want, names)
		}
	}

	// Update: per-end label sets carry the sample values.
	for _, c := range []struct {
		name string
		end  string
		want float64
	}{
		{"orbit_app_mos", AppEndUE, 4.4}, {"orbit_app_mos", AppEndN6, 4.1},
		{"orbit_app_jitter_ms", AppEndUE, 3.5}, {"orbit_app_jitter_ms", AppEndN6, 4.5},
		{"orbit_app_loss_pct", AppEndUE, 0.5}, {"orbit_app_loss_pct", AppEndN6, 1.5},
		{"orbit_app_owd_ms", AppEndUE, 12.25}, {"orbit_app_owd_ms", AppEndN6, 13.5},
		{"orbit_app_owd_err_ms", AppEndUE, 0.9}, {"orbit_app_owd_err_ms", AppEndN6, 1.1},
	} {
		v, ok := gaugeValue(t, reg, c.name, map[string]string{"supi": "imsi-1", "app": "voip", "end": c.end})
		if !ok || v != c.want {
			t.Errorf("%s{end=%s} = %v (ok=%v), want %v", c.name, c.end, v, ok, c.want)
		}
	}
	if v, ok := gaugeValue(t, reg, "orbit_ue_handover_timestamp_seconds", map[string]string{"supi": "imsi-1"}); !ok || math.Abs(v-1750000000.25) > 1e-6 {
		t.Errorf("handover timestamp = %v (ok=%v), want 1750000000.25", v, ok)
	}
	if n := histogramCount(t, reg, "orbit_app_media_gap_ms"); n != 1 {
		t.Errorf("media gap observations = %d, want 1", n)
	}

	// Cleanup: session end deletes the session's GAUGE series but keeps the
	// media-gap histogram (cumulative: deleting on session end would lose
	// observations between scrapes and reset counters) and the UE's handover
	// timestamp.
	am.cleanupSession("imsi-1", "voip")
	for _, name := range []string{
		"orbit_app_mos", "orbit_app_jitter_ms", "orbit_app_loss_pct",
		"orbit_app_owd_ms", "orbit_app_owd_err_ms",
	} {
		if left := gatherFamily(t, reg, name); len(left) != 0 {
			t.Errorf("%s: %d series left after cleanupSession: %v", name, len(left), left)
		}
	}
	if n := histogramCount(t, reg, "orbit_app_media_gap_ms"); n != 1 {
		t.Errorf("media gap histogram must survive session cleanup (scrape-loss/counter-reset), got %d observations", n)
	}
	if n := gatherFamily(t, reg, "orbit_ue_handover_timestamp_seconds"); len(n) != 1 {
		t.Errorf("handover timestamp must survive session cleanup, %d series", len(n))
	}

	// Deregistration forgets the UE entirely.
	am.observeSample("imsi-1", "voip", ue) // simulate a straggler series
	am.forgetUE("imsi-1")
	if n := gatherFamily(t, reg, "orbit_ue_handover_timestamp_seconds"); len(n) != 0 {
		t.Errorf("handover timestamp left after forgetUE: %v", n)
	}
	if n := gatherFamily(t, reg, "orbit_app_mos"); len(n) != 0 {
		t.Errorf("straggler app series left after forgetUE: %v", n)
	}
	if n := gatherFamily(t, reg, "orbit_app_media_gap_ms"); len(n) != 0 {
		t.Errorf("media gap histogram left after forgetUE: %v", n)
	}

	// A nil *appMetrics (metrics disabled) is a no-op receiver everywhere.
	var off *appMetrics
	off.observeSample("x", "voip", ue)
	off.observeGap("x", "voip", AppEndUE, time.Second)
	off.recordHandover("x", at)
	off.cleanupSession("x", "voip")
	off.forgetUE("x")
}

// TestPublishMobilityStampsHandoverGauge checks the Manager seam: a
// HANDOVER_STARTED phase event sets orbit_ue_handover_timestamp_seconds for
// that UE (app session running or not), and other phases do not disturb it.
func TestPublishMobilityStampsHandoverGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewManager(slog.New(slog.DiscardHandler))
	m.EnableAppMetrics(reg)

	at := time.Unix(1750000100, 0)
	m.publishMobility(StateEvent{SUPI: "imsi-2", State: StateHandoverStarted,
		Detail: "Xn handover a → b", Time: at})
	m.publishMobility(StateEvent{SUPI: "imsi-2", State: StatePathSwitchComplete,
		Detail: "ack", Time: at.Add(time.Second)})

	if v, ok := gaugeValue(t, reg, "orbit_ue_handover_timestamp_seconds", map[string]string{"supi": "imsi-2"}); !ok || v != 1750000100 {
		t.Errorf("handover gauge = %v (ok=%v), want 1750000100", v, ok)
	}
	if n := gatherFamily(t, reg, "orbit_ue_handover_timestamp_seconds"); len(n) != 1 {
		t.Errorf("series count = %d, want 1", len(n))
	}
}
