package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
)

// RunLoad and RunFleet are wrappers over RunPopulation: a load or fleet
// spec through PopulationSpec and back is the spec each engine ran before.
func TestPopulationRoundTrips(t *testing.T) {
	cfg := func(id uint32, name string) gnb.Config {
		return gnb.Config{ID: id, IDBits: 24, Name: name, MCC: "208", MNC: "93", TAC: 1}
	}
	ls := LoadSpec{
		GNBs: []GNBSpec{
			{Config: cfg(1, "a"), AMFAddr: "10.0.0.1:38412", BindAddr: "10.0.1.1"},
			{Config: cfg(2, "b"), AMFAddr: "10.0.0.1:38412", BindAddr: "10.0.1.2"},
		},
		BaseIMSI: "208930100007500", Count: 10, MCC: "208", MNC: "93", Ki: []byte{1}, OPc: []byte{2},
		Concurrency: 4, Rate: load.LinearRamp{Start: 1, End: 10, Over: time.Minute}, Duration: time.Hour, SampleEvery: time.Second,
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, DNN: "internet"}, GNBN3Addr: "10.0.2.1",
	}
	if got := PopulationFromLoad(ls).loadSpec(); !reflect.DeepEqual(got, ls) {
		t.Errorf("load round trip =\n%+v\nwant\n%+v", got, ls)
	}
	fs := FleetRunSpec{
		AMFAddr: "10.0.0.1:38412", GNBs: []FleetGNB{{Config: cfg(7, "g"), BindAddr: "10.0.1.7", N3Addr: "10.0.2.7", X: 1, Y: 2}},
		BaseIMSI: "208930100007500", Count: 3, MCC: "208", MNC: "93", Ki: []byte{1}, OPc: []byte{2},
		Rate: load.Constant{RPS: 5}, Concurrency: 8, GNBAssign: []int{0, 0, 0},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1},
	}
	if got := PopulationFromFleet(fs).fleetSpec(); !reflect.DeepEqual(got, fs) {
		t.Errorf("fleet round trip =\n%+v\nwant\n%+v", got, fs)
	}
	// A constant RateRPS becomes the same curve the attach phase uses.
	if got := PopulationFromFleet(FleetRunSpec{RateRPS: 5}).Rate; got != (load.Constant{RPS: 5}) {
		t.Errorf("RateRPS 5 → %v, want Constant{5}", got)
	}
}

// The strategy follows the behaviours unless set; a storm cannot carry
// behaviours.
func TestPopulationStrategy(t *testing.T) {
	beh := FleetBehaviors{Duration: time.Second}
	if !beh.hasBehaviors() || (FleetBehaviors{}).hasBehaviors() {
		t.Fatal("hasBehaviors")
	}
	_, err := RunPopulation(context.Background(), testLogger(), PopulationSpec{Strategy: Storm}, beh, FleetDeps{})
	if err == nil || !strings.Contains(err.Error(), "storm population has no behaviours") {
		t.Errorf("storm with behaviours: err = %v", err)
	}
	// Persistent without gNBs fails in the fleet engine, which shows the
	// default picked it for a population with behaviours.
	_, err = RunPopulation(context.Background(), testLogger(), PopulationSpec{}, beh, FleetDeps{})
	if err == nil || !strings.Contains(err.Error(), "fleet run needs at least one gNB") {
		t.Errorf("default with behaviours: err = %v, want the persistent engine's", err)
	}
}
