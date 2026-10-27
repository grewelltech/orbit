package engine

import (
	"errors"
	"io"
	"log/slog"
	"testing"
)

// A hold and a run's leases exclude each other, naming the holder, and a
// released hold frees them.
func TestHold(t *testing.T) {
	r := NewRunRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	needs := Needs{GNBs: []GNBKey{{AMF: "192.0.2.1:38412", PLMN: "208/93", ID: 7}}, SUPIs: []SUPIBlock{{PLMN: "208/93", First: 1, Last: 1}}}
	release, err := r.Hold("probe-1", needs)
	if err != nil {
		t.Fatal(err)
	}
	var lc *ErrLeaseConflict
	if _, err := r.Hold("probe-2", needs); !errors.As(err, &lc) || lc.Holder != "probe-1" {
		t.Errorf("a second hold: %v", err)
	}
	release()
	release()
	again, err := r.Hold("probe-2", needs)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}
