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

// Storm and persistent attach one population through the same controller,
// so against the same mock core they report the same attach figures: every
// UE attached, the same procedures timed, as many samples of each.
func TestPopulationStormPersistentParity(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	spec := func(s Strategy) PopulationSpec {
		return PopulationSpec{Strategy: s, AMFAddr: addr, GNBs: []FleetGNB{{Config: cfg}},
			BaseIMSI: "208930100007500", Count: 6, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
			Rate: load.Constant{RPS: 200}, Concurrency: 2}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	storm, err := RunPopulation(ctx, testLogger(), spec(Storm), FleetBehaviors{}, FleetDeps{})
	if err != nil {
		t.Fatalf("storm: %v", err)
	}
	kept, err := RunPopulation(ctx, testLogger(), spec(Persistent), FleetBehaviors{}, FleetDeps{Stats: NewFleetLiveStats()})
	if err != nil {
		t.Fatalf("persistent: %v", err)
	}
	l, f := storm.Load, kept.Fleet
	if l == nil || f == nil {
		t.Fatalf("reports: storm %v, persistent %v", l, f)
	}
	if l.Succeeded != 6 || l.Failed != 0 || f.Attached != 6 || f.AttachFailed != 0 {
		t.Fatalf("attached: storm %d/%d, persistent %d/%d; want 6/0 each", l.Succeeded, l.Failed, f.Attached, f.AttachFailed)
	}
	if l.AchievedRate <= 0 || f.AchievedRate <= 0 {
		t.Errorf("achieved rate: storm %v, persistent %v", l.AchievedRate, f.AchievedRate)
	}
	for proc, ls := range l.Latencies {
		fs, ok := f.Latencies[proc]
		if !ok || fs.Count != ls.Count {
			t.Errorf("%s: storm timed %d, persistent %d (present %t)", proc, ls.Count, fs.Count, ok)
		}
	}
	if len(f.Latencies) != len(l.Latencies) {
		t.Errorf("procedures: storm %v, persistent %v", keys(l.Latencies), keys(f.Latencies))
	}
}

func keys(m map[string]load.Stats) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
