package scenario

import (
	"strings"
	"testing"
)

// App-cohort grammar coverage (design §8): mix entries with app: alongside
// synthetic profile: entries — parsing, share-based sizing, and the
// fail-fast validations.

const fleetAppSample = `
kind: fleet
core:
  amf: 1.2.3.4:38412
  plmn: { mcc: "208", mnc: "93" }
topology:
  gnbs: { count: 2, source_ips: [10.0.0.1, 10.0.0.2] }
fleet:
  count: 10
  supi_base: "208930100007500"
  pdu_session: true
behaviors:
  traffic:
    mix:
      - { profile: web, share: 0.5 }
      - { app: voip, name: voip-calls, share: 0.3, peer: 172.17.60.10:9551,
          peer_data_ip: 172.17.60.11,
          params: { codec: pcmu, port_min: "40000", port_max: "40100" } }
      - { app: http, share: 0.2, peer: 172.17.60.10:9551,
          params: { object_size: 64KB } }
run: { duration: 1m }
`

func TestFleetAppCohortsParse(t *testing.T) {
	f, err := ParseFleet([]byte(fleetAppSample))
	if err != nil {
		t.Fatal(err)
	}
	cohorts := f.AppCohorts()
	if len(cohorts) != 2 {
		t.Fatalf("got %d app cohorts, want 2", len(cohorts))
	}
	v, h := cohorts[0], cohorts[1]
	if v.Name != "voip-calls" || v.App != "voip" || v.Count != 3 {
		t.Errorf("voip cohort = %+v, want name voip-calls, 3 UEs (0.3 of 10)", v)
	}
	if v.Peer != "172.17.60.10:9551" || v.PeerDataIP != "172.17.60.11" {
		t.Errorf("voip cohort peers = %q / %q", v.Peer, v.PeerDataIP)
	}
	if v.Params["codec"] != "pcmu" || v.Params["port_min"] != "40000" {
		t.Errorf("voip params = %v", v.Params)
	}
	if h.Name != "http" || h.App != "http" || h.Count != 2 {
		t.Errorf("http cohort = %+v, want default name http, 2 UEs (last entry absorbs rounding)", h)
	}

	// The per-UE labels use the same allocation: 5 web, 3 voip-calls, 2 http.
	counts := map[string]int{}
	for _, u := range f.GenFleet(f.GenGNBs()) {
		counts[u.Profile]++
	}
	if counts["web"] != 5 || counts["voip-calls"] != 3 || counts["http"] != 2 {
		t.Errorf("labels = %v, want web:5 voip-calls:3 http:2", counts)
	}
}

func TestFleetAppCohortValidateErrors(t *testing.T) {
	base := func(mix string) string {
		return `kind: fleet
core: { amf: x }
topology: { gnbs: { count: 1, source_ips: [a] } }
fleet: { count: 10, supi_base: "1", pdu_session: PDU }
behaviors: { traffic: { mix: [` + mix + `] } }`
	}
	cases := []struct {
		name, mix, pdu, want string
	}{
		{"both profile and app", `{ profile: web, app: voip, share: 1.0, peer: p:1 }`, "true", "pick one"},
		{"neither", `{ share: 1.0 }`, "true", "needs a profile"},
		{"unknown app", `{ app: ftp, share: 1.0, peer: p:1 }`, "true", "not supported"},
		{"missing peer", `{ app: voip, share: 1.0 }`, "true", "needs peer"},
		{"no pdu session", `{ app: voip, share: 1.0, peer: p:1 }`, "false", "pdu_session"},
		{"duplicate names", `{ app: voip, share: 0.5, peer: p:1 }, { app: voip, share: 0.5, peer: p:1 }`, "true", "used twice"},
	}
	for _, tc := range cases {
		y := strings.Replace(base(tc.mix), "PDU", tc.pdu, 1)
		_, err := ParseFleet([]byte(y))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v does not contain %q", tc.name, err, tc.want)
		}
	}

	// Distinct names on same-app cohorts are fine.
	ok := strings.Replace(base(`{ app: voip, name: a, share: 0.5, peer: p:1 }, { app: voip, name: b, share: 0.5, peer: p:1 }`), "PDU", "true", 1)
	if _, err := ParseFleet([]byte(ok)); err != nil {
		t.Errorf("distinct names refused: %v", err)
	}
}
