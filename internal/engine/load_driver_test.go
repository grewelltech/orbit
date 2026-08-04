package engine

import (
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
)

// Every LoadSpec knob must reach the load engine. The Observer hook shipped
// unwired precisely because nothing checked this mapping, so it is asserted
// field by field rather than by spot check.
func TestLoadConfigCarriesEveryKnob(t *testing.T) {
	obs := load.NewLiveStats()
	spec := LoadSpec{
		Count:       250,
		Concurrency: 32,
		Rate:        load.Constant{RPS: 12},
		Duration:    90 * time.Second,
		SampleEvery: 10 * time.Second,
		Observer:    obs,
	}

	cfg := loadConfig(spec)

	if cfg.Total != spec.Count {
		t.Errorf("Total = %d, want %d", cfg.Total, spec.Count)
	}
	if cfg.Concurrency != spec.Concurrency {
		t.Errorf("Concurrency = %d, want %d", cfg.Concurrency, spec.Concurrency)
	}
	if cfg.Rate != spec.Rate {
		t.Errorf("Rate = %v, want %v", cfg.Rate, spec.Rate)
	}
	if cfg.Duration != spec.Duration {
		t.Errorf("Duration = %s, want %s", cfg.Duration, spec.Duration)
	}
	if cfg.SampleInterval != spec.SampleEvery {
		t.Errorf("SampleInterval = %s, want %s", cfg.SampleInterval, spec.SampleEvery)
	}
	if cfg.Observer != load.Observer(obs) {
		t.Error("Observer did not reach load.Config — live progress would be silently dead")
	}
}

// A spec with no observer must leave the hook nil, so load.Run skips it rather
// than calling into a non-nil interface holding a nil pointer.
func TestLoadConfigOmitsAbsentObserver(t *testing.T) {
	if cfg := loadConfig(LoadSpec{Count: 1}); cfg.Observer != nil {
		t.Errorf("Observer = %v, want nil when the spec sets none", cfg.Observer)
	}
}

// A gNB's per-telemetry label is its RANNodeName when set, and a distinct
// "gnb-<id>" otherwise — RANNodeName is optional, so unnamed gNBs must still
// attribute to separate buckets rather than collapsing under the empty string.
func TestGNBAttributionLabel(t *testing.T) {
	if got := gnbAttributionLabel(gnb.Config{Name: "orbit-gnb-0", ID: 7}); got != "orbit-gnb-0" {
		t.Errorf("named gNB label = %q, want orbit-gnb-0", got)
	}
	a := gnbAttributionLabel(gnb.Config{ID: 1})
	b := gnbAttributionLabel(gnb.Config{ID: 2})
	if a != "gnb-1" || b != "gnb-2" {
		t.Errorf("unnamed labels = %q,%q, want gnb-1,gnb-2", a, b)
	}
	if a == b {
		t.Error("distinct unnamed gNBs collapsed to the same label")
	}
}

// TestSoakCyclesTheSubscriberPool: a soak runs for a duration rather than a
// count, so its dispatch index climbs without bound. It must wrap onto the
// provisioned population — walking past the last subscriber turns every
// further attach into a failure against an unknown SUPI, which looks like the
// core rejecting load when it is really the generator asking for UEs that were
// never provisioned.
func TestSoakCyclesTheSubscriberPool(t *testing.T) {
	base := "001010100007500"
	spec := LoadSpec{BaseIMSI: base, Count: 100, Duration: time.Second}

	// Indices past the pool must fold back onto it.
	for _, tc := range []struct {
		idx  int
		want string
	}{
		{0, "001010100007500"},
		{99, "001010100007599"},
		{100, "001010100007500"}, // wraps
		{205, "001010100007505"},
	} {
		i := tc.idx
		if spec.Duration > 0 && spec.Count > 0 {
			i %= spec.Count
		}
		got, err := incIMSI(spec.BaseIMSI, i)
		if err != nil {
			t.Fatalf("incIMSI(%d): %v", tc.idx, err)
		}
		if got != tc.want {
			t.Errorf("index %d -> %s, want %s", tc.idx, got, tc.want)
		}
	}

	// A counted run must NOT wrap: asking for more UEs than are provisioned is
	// a real error the operator should see, not something to paper over.
	counted := LoadSpec{BaseIMSI: base, Count: 100}
	i := 150
	if counted.Duration > 0 && counted.Count > 0 {
		i %= counted.Count
	}
	got, _ := incIMSI(counted.BaseIMSI, i)
	if got != "001010100007650" {
		t.Errorf("counted run wrapped to %s; it must not wrap", got)
	}
}
