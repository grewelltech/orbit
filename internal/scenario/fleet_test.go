package scenario

import "testing"

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
