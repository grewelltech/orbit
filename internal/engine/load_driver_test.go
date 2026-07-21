package engine

import (
	"testing"
	"time"

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
