package scenario

import (
	"testing"
	"time"
)

const fleetSample = `
kind: fleet
name: f
core:
  amf: 1.2.3.4:38412
  plmn: { mcc: "208", mnc: "93" }
  slice: { sst: 1, sd: "010203" }
  dnn: internet
credentials: { ki: ${TEST_KI}, opc: ffee }
topology:
  gnbs:
    count: 4
    id_base: 100
    source_ips: [10.0.0.1, 10.0.0.2, 10.0.0.3, 10.0.0.4]
    spacing_m: 500
fleet:
  count: 10
  supi_base: "208930100007500"
  distribution: even
  attach_rate: 10/s
  pdu_session: true
behaviors:
  traffic:
    mix:
      - { profile: web,   share: 0.5 }
      - { profile: video, share: 0.3, rate: 8Mbps }
      - { profile: voip,  share: 0.2, rate: 64kbps }
run: { duration: 5m }
`

func TestFleetParseAndGen(t *testing.T) {
	t.Setenv("TEST_KI", "deadbeef")
	if k, _ := PeekKind([]byte(fleetSample)); k != "fleet" {
		t.Fatalf("kind = %q, want fleet", k)
	}
	f, err := ParseFleet([]byte(fleetSample))
	if err != nil {
		t.Fatal(err)
	}
	if f.Credentials.Ki != "deadbeef" {
		t.Errorf("${ENV} not expanded: %q", f.Credentials.Ki)
	}

	gnbs := f.GenGNBs()
	if len(gnbs) != 4 {
		t.Fatalf("want 4 gNBs, got %d", len(gnbs))
	}
	if gnbs[0].GNB.ID != 100 || gnbs[3].GNB.ID != 103 {
		t.Errorf("gNB IDs = %d..%d", gnbs[0].GNB.ID, gnbs[3].GNB.ID)
	}
	if gnbs[2].GNB.N3 != "10.0.0.3" || gnbs[2].GNB.Bind != "10.0.0.3:0" {
		t.Errorf("gNB[2] addr = %+v", gnbs[2].GNB)
	}
	// grid: side = ceil(sqrt(4)) = 2, spacing 500; gNB[3] at col 1, row 1.
	if gnbs[3].X != 500 || gnbs[3].Y != 500 {
		t.Errorf("gNB[3] pos = (%v,%v), want (500,500)", gnbs[3].X, gnbs[3].Y)
	}

	ues := f.GenFleet(gnbs)
	if len(ues) != 10 {
		t.Fatalf("want 10 UEs, got %d", len(ues))
	}
	if ues[0].SUPI != "208930100007500" || ues[9].SUPI != "208930100007509" {
		t.Errorf("SUPIs = %s..%s", ues[0].SUPI, ues[9].SUPI)
	}
	// even distribution is round-robin across 4 gNBs.
	if ues[4].GNBIndex != 0 || ues[5].GNBIndex != 1 {
		t.Errorf("distribution: ue4->%d ue5->%d", ues[4].GNBIndex, ues[5].GNBIndex)
	}
	// mix over 10 UEs: 0.5/0.3/0.2 -> 5 web, 3 video, 2 voip.
	counts := map[string]int{}
	for _, u := range ues {
		counts[u.Profile]++
	}
	if counts["web"] != 5 || counts["video"] != 3 || counts["voip"] != 2 {
		t.Errorf("profile mix = %v, want 5/3/2", counts)
	}
}

func TestFleetValidateErrors(t *testing.T) {
	tooFewIPs := `kind: fleet
core: { amf: x }
topology: { gnbs: { count: 3, source_ips: [a, b] } }
fleet: { count: 1, supi_base: "1" }`
	if _, err := ParseFleet([]byte(tooFewIPs)); err == nil {
		t.Error("want error for fewer source_ips than gNBs")
	}

	badMix := `kind: fleet
core: { amf: x }
topology: { gnbs: { count: 1, source_ips: [a] } }
fleet: { count: 2, supi_base: "1" }
behaviors: { traffic: { mix: [ { profile: web, share: 0.3 } ] } }`
	if _, err := ParseFleet([]byte(badMix)); err == nil {
		t.Error("want error for mix shares not summing to 1.0")
	}
}

// A synthetic mix entry's target: is honoured. It was previously parsed and
// ignored in favour of a hardcoded address, so a scenario aimed at its own N6
// responder silently addressed the off-testbed default instead.
func TestBuildFleetRunHonoursTrafficTarget(t *testing.T) {
	f := &FleetScenario{
		Fleet:     FleetSpec{Count: 2, SUPIBase: "001010000000001", PDUSession: true},
		Topology:  Topology{GNBs: GNBGen{Count: 1, IDBase: 1, SourceIPs: []string{"10.0.0.1"}}},
		Behaviors: Behaviors{Traffic: &TrafficBehavior{Mix: []TrafficShare{{Profile: "video", Share: 1, Target: "10.106.0.30:9600"}}}},
		Run:       RunSpec{Duration: "5s"},
	}
	_, beh, err := BuildFleetRun(f, make([]byte, 16), make([]byte, 16))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if beh.TrafficTarget != "10.106.0.30:9600" {
		t.Errorf("TrafficTarget = %q, want the entry's own target", beh.TrafficTarget)
	}

	f.Behaviors.Traffic.Mix[0].Target = ""
	_, beh, err = BuildFleetRun(f, make([]byte, 16), make([]byte, 16))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if beh.TrafficTarget != defaultTrafficTarget {
		t.Errorf("TrafficTarget = %q, want the default %q", beh.TrafficTarget, defaultTrafficTarget)
	}
}

// n3_ips gives each gNB a data-plane address distinct from its N2 source, which
// a testbed with separated N2/N3 networks needs — without it the gNB advertises
// an N3 address the UPF cannot reach.
func TestGenGNBsSeparateN3(t *testing.T) {
	f := &FleetScenario{Topology: Topology{GNBs: GNBGen{
		Count: 2, IDBase: 1,
		SourceIPs: []string{"10.102.0.20", "10.102.0.21"},
		N3IPs:     []string{"10.103.0.20", "10.103.0.21"},
	}}}
	got := f.GenGNBs()
	if len(got) != 2 {
		t.Fatalf("got %d gNBs, want 2", len(got))
	}
	if got[0].GNB.Bind != "10.102.0.20:0" || got[0].GNB.N3 != "10.103.0.20" {
		t.Errorf("gNB 0 bind/N3 = %q/%q, want the N2 source and the N3 address",
			got[0].GNB.Bind, got[0].GNB.N3)
	}

	// Without n3_ips, N3 rides the source IP as before.
	f.Topology.GNBs.N3IPs = nil
	got = f.GenGNBs()
	if got[1].GNB.N3 != "10.102.0.21" {
		t.Errorf("gNB 1 N3 = %q, want the source IP when n3_ips is unset", got[1].GNB.N3)
	}
}

// start_after staggers a cohort so a run can show what ADDING load does, not
// just the steady state of a mix that all began at once.
func TestBuildFleetRunStaggersCohorts(t *testing.T) {
	f := &FleetScenario{
		Fleet:    FleetSpec{Count: 4, SUPIBase: "001010000000001", PDUSession: true},
		Topology: Topology{GNBs: GNBGen{Count: 1, IDBase: 1, SourceIPs: []string{"10.0.0.1"}}},
		Behaviors: Behaviors{Traffic: &TrafficBehavior{Mix: []TrafficShare{
			{App: "http", Name: "web", Share: 0.5, Peer: "p:1"},
			{App: "voip", Name: "calls", Share: 0.5, Peer: "p:1", StartAfter: "2m"},
		}}},
		Run: RunSpec{Duration: "10m"},
	}
	_, beh, err := BuildFleetRun(f, make([]byte, 16), make([]byte, 16))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	byName := map[string]time.Duration{}
	for _, c := range beh.Apps {
		byName[c.Name] = c.StartAfter
	}
	if byName["web"] != 0 {
		t.Errorf("web start_after = %v, want 0 (unset starts with the run)", byName["web"])
	}
	if byName["calls"] != 2*time.Minute {
		t.Errorf("calls start_after = %v, want 2m", byName["calls"])
	}
}

// A cohort that would start after the run ends is refused at build time: it
// would otherwise be carved members, occupy them for the whole run, and never
// send anything.
func TestBuildFleetRunRefusesUnreachableStart(t *testing.T) {
	f := &FleetScenario{
		Fleet:    FleetSpec{Count: 2, SUPIBase: "001010000000001", PDUSession: true},
		Topology: Topology{GNBs: GNBGen{Count: 1, IDBase: 1, SourceIPs: []string{"10.0.0.1"}}},
		Behaviors: Behaviors{Traffic: &TrafficBehavior{Mix: []TrafficShare{
			{App: "http", Name: "late", Share: 1.0, Peer: "p:1", StartAfter: "10m"},
		}}},
		Run: RunSpec{Duration: "5m"},
	}
	if _, _, err := BuildFleetRun(f, make([]byte, 16), make([]byte, 16)); err == nil {
		t.Error("a cohort starting after the run ends must be refused, not silently never run")
	}
}
