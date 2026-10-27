package scenario

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/target"
)

// exampleTargetYAML is a target big enough for every shipped example: ten
// N2 and N3 addresses (both as ranges), a thousand subscribers and an N6
// responder. Fixtures go through the loader and its validator (loadTarget),
// never a hand-built struct, so a schema error cannot slip past a test.
const exampleTargetYAML = `version: 1
kind: target
name: fixture
core:
  amf: 192.0.2.10:38412
  plmn: { mcc: "001", mnc: "01" }
  tac: 1
  slices:
    - { sst: 1, sd: "010203" }
  dnn: internet
subscribers:
  - name: main
    first_supi: "001010100007500"
    count: 1000
    ki: 00112233445566778899aabbccddeeff
    opc: 000102030405060708090a0b0c0d0e0f
ran:
  n2_source_ips: [192.0.2.20-192.0.2.29]
  n3_ips: [198.51.100.20-198.51.100.29]
  gnb_ids: { first: 100 }
app:
  responders:
    - { name: n6, control: 203.0.113.30:9551, data_ip: 203.0.113.30 }
`

// loadTarget loads a target document through the loader and the resolver,
// as -t does.
func loadTarget(t *testing.T, doc string) target.Target {
	t.Helper()
	files, err := target.Parse("fixture.yaml", []byte(doc), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("fixture target: %v", err)
	}
	r, err := target.Resolve(target.DefaultInput(), files[0].Input)
	if err != nil {
		t.Fatalf("fixture target: %v", err)
	}
	return r.Target
}

// fixtureWith is the fixture target with one edit to its YAML.
func fixtureWith(t *testing.T, old, new string) target.Target {
	t.Helper()
	if !strings.Contains(exampleTargetYAML, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	return loadTarget(t, strings.Replace(exampleTargetYAML, old, new, 1))
}

func TestReadTargetRef(t *testing.T) {
	tests := []struct {
		name, doc string
		want      string // Name, or "inline" when an inline document
		wantErr   string
	}{
		{name: "none", doc: "kind: fleet\n"},
		{name: "a name", doc: "target: lab1\nkind: fleet\n", want: "lab1"},
		{name: "inline", doc: "target:\n  core: { amf: 192.0.2.1:38412 }\nkind: fleet\n", want: "inline"},
		{name: "inline, ${VAR} in block style", doc: "target:\n  subscribers:\n    - name: p\n      ki: ${K}\nkind: fleet\n", want: "inline"},
		{name: "inline, ${VAR} in a flow mapping elsewhere", doc: "target:\n  core: { amf: a:1 }\nfleet: { supi_base: ${K} }\n", wantErr: "write ${VAR} values in block style"},
		{name: "a name, ${VAR} in a flow mapping elsewhere", doc: "target: lab1\nfleet: { supi_base: ${K} }\n", want: "lab1"},
		{name: "empty", doc: "target: \"\"\n", wantErr: "target: is empty"},
		{name: "a list", doc: "target: [a]\n", wantErr: "is a target name or a target document"},
		{name: "a fleet behaviour's echo target is not it", doc: "behaviors:\n  latency: { target: 192.0.2.30 }\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ReadTargetRef([]byte(tc.doc))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := r.Name
			if r.Inline != nil {
				got = "inline"
				if !strings.Contains(string(r.Inline), "kind: target") || !strings.Contains(string(r.Inline), "version: 1") {
					t.Errorf("inline document lacks its defaulted header:\n%s", r.Inline)
				}
			}
			if got != tc.want {
				t.Errorf("ref = %q, want %q", got, tc.want)
			}
		})
	}
}

const fleetDoc = `kind: fleet
topology:
  gnbs: { count: 3, layout: grid }
fleet: { count: 10, pdu_session: true }
behaviors:
  traffic:
    mix:
      - { app: http, share: 1.0 }
  latency: { interval: 1s }
`

// A fleet with a target takes the core, keys, first SUPI, gNB IDs, one N2
// and N3 address per gNB and the first responder for its app traffic and
// latency echo.
func TestFleetApplyTarget(t *testing.T) {
	tg := loadTarget(t, exampleTargetYAML)
	f, err := ParseFleetWithTarget([]byte(fleetDoc), &tg)
	if err != nil {
		t.Fatal(err)
	}
	g := f.Topology.GNBs
	if f.Core.AMF != "192.0.2.10:38412" || f.Core.PLMN.MCC != "001" || f.Core.Slice.SD != "010203" || f.Credentials.Ki == "" ||
		f.Fleet.SUPIBase != "001010100007500" || g.IDBase != 100 ||
		!reflect.DeepEqual(g.SourceIPs, []string{"192.0.2.20", "192.0.2.21", "192.0.2.22"}) ||
		!reflect.DeepEqual(g.N3IPs, []string{"198.51.100.20", "198.51.100.21", "198.51.100.22"}) {
		t.Errorf("applied = core %+v cred %+v supi %s topo %+v", f.Core, f.Credentials, f.Fleet.SUPIBase, g)
	}
	if m := f.Behaviors.Traffic.Mix[0]; m.Peer != "203.0.113.30:9551" || m.PeerDataIP != "203.0.113.30" {
		t.Errorf("app peer = %+v", m)
	}
	if f.Behaviors.Latency.Target != "203.0.113.30" {
		t.Errorf("latency target = %q", f.Behaviors.Latency.Target)
	}
	// latency.target is the file's to choose.
	f, err = ParseFleetWithTarget([]byte(strings.Replace(fleetDoc, "{ interval: 1s }", "{ interval: 1s, target: 198.51.100.99 }", 1)), &tg)
	if err != nil || f.Behaviors.Latency.Target != "198.51.100.99" {
		t.Errorf("a set latency.target: %v %q", err, f.Behaviors.Latency.Target)
	}
}

func TestFleetApplyTargetErrors(t *testing.T) {
	exampleTarget := loadTarget(t, exampleTargetYAML)
	noResp := fixtureWith(t, "app:\n  responders:\n    - { name: n6, control: 203.0.113.30:9551, data_ip: 203.0.113.30 }\n", "")
	short := fixtureWith(t, "[192.0.2.20-192.0.2.29]", "[192.0.2.20-192.0.2.21]")
	shortN3 := fixtureWith(t, "[198.51.100.20-198.51.100.29]", "[198.51.100.20/31]")
	small := fixtureWith(t, "count: 1000", "count: 5")
	noPool := fixtureWith(t, "    first_supi: \"001010100007500\"\n", "")
	tests := []struct {
		name string
		doc  string
		tg   target.Target
		want string
	}{
		{name: "own core", doc: fleetDoc + "core: { amf: 192.0.2.1:38412 }\n", tg: exampleTarget, want: "the scenario sets core.amf and uses target fixture"},
		{name: "own addresses", doc: strings.Replace(fleetDoc, "count: 3,", "count: 3, source_ips: [192.0.2.1],", 1), tg: exampleTarget, want: "topology.gnbs.source_ips"},
		{name: "own peer", doc: strings.Replace(fleetDoc, "share: 1.0 }", "share: 1.0, peer: 192.0.2.30:9551 }", 1), tg: exampleTarget, want: "behaviors.traffic.mix[0].peer"},
		{name: "no responder for an app cohort", doc: fleetDoc, tg: noResp, want: "behaviors.traffic.mix[0] (app http) needs an N6 responder: target fixture has no app.responders"},
		{name: "too few N2 addresses", doc: fleetDoc, tg: short, want: "ran.n2_source_ips has 2 addresses, the scenario needs 3 (one per gNB)"},
		{name: "too few N3 addresses (a CIDR)", doc: fleetDoc, tg: shortN3, want: "ran.n3_ips has 2 addresses, the scenario needs 3 (one per gNB)"},
		{name: "pool too small", doc: fleetDoc, tg: small, want: "subscribers[main] has 5 subscribers, the scenario needs 10"},
		{name: "no first SUPI", doc: fleetDoc, tg: noPool, want: "target fixture has no subscriber pool with a first_supi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tg := tc.tg
			if _, err := ParseFleetWithTarget([]byte(tc.doc), &tg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

const stepDoc = `gnbs:
  - { name: gnb-1 }
  - { name: gnb-2 }
ues:
  - { name: ue1, gnb: gnb-1, pdu_session: true }
  - { gnb: gnb-1, range: { count: 3 } }
  - { name: ue2, gnb: gnb-2 }
steps:
  - register: all
  - ping: { ue: ue1 }
  - latency: { ue: ue2, probes: 5 }
  - handover: { ue: ue1, to: gnb-2, type: xn }
  - deregister: ue2
`

// A step file with a target: gNB IDs and addresses from the pools, SUPIs
// allocated in file order across entries, names resolved, and the echo
// destinations from the first responder.
func TestStepApplyTarget(t *testing.T) {
	tg := loadTarget(t, exampleTargetYAML)
	s, err := ParseWithTarget([]byte(stepDoc), &tg)
	if err != nil {
		t.Fatal(err)
	}
	if g := s.GNBs; g[0].ID != 100 || g[1].ID != 101 || g[0].Bind != "192.0.2.20" || g[1].Bind != "192.0.2.21" || g[1].N3 != "198.51.100.21" {
		t.Errorf("gnbs = %+v", g)
	}
	if s.UEs[0].SUPI != "001010100007500" || s.UEs[1].Range.Base != "001010100007501" || s.UEs[2].SUPI != "001010100007504" {
		t.Errorf("SUPIs = %s, range %s, %s", s.UEs[0].SUPI, s.UEs[1].Range.Base, s.UEs[2].SUPI)
	}
	var p pingParams
	if err := s.Steps[1].decode(&p); err != nil || p.UE != "001010100007500" || p.Dst != "203.0.113.30" {
		t.Errorf("ping = %+v (%v)", p, err)
	}
	var l latencyParams
	if err := s.Steps[2].decode(&l); err != nil || l.UE != "001010100007504" || l.Target != "203.0.113.30" {
		t.Errorf("latency = %+v (%v)", l, err)
	}
	if s.Steps[4].str() != "001010100007504" {
		t.Errorf("deregister: %q", s.Steps[4].str())
	}
}

func TestStepApplyTargetErrors(t *testing.T) {
	exampleTarget := loadTarget(t, exampleTargetYAML)
	noResp := fixtureWith(t, "app:\n  responders:\n    - { name: n6, control: 203.0.113.30:9551, data_ip: 203.0.113.30 }\n", "")
	small := fixtureWith(t, "count: 1000", "count: 4")
	tests := []struct {
		name string
		doc  string
		tg   target.Target
		want string
	}{
		{name: "literal SUPI with a target", doc: strings.Replace(stepDoc, "ping: { ue: ue1 }", "ping: { ue: \"001010100007500\" }", 1), tg: exampleTarget,
			want: "step 2 refers to UE 001010100007500; with a target the SUPIs are allocated, so name the UE"},
		{name: "unknown name", doc: strings.Replace(stepDoc, "ping: { ue: ue1 }", "ping: { ue: ue9 }", 1), tg: exampleTarget, want: `step 2 refers to UE "ue9", which no ues[] entry names`},
		{name: "own gNB id", doc: strings.Replace(stepDoc, "{ name: gnb-1 }", "{ name: gnb-1, id: 5 }", 1), tg: exampleTarget, want: "gnbs[0].id"},
		{name: "own SUPI", doc: strings.Replace(stepDoc, "{ name: ue2, gnb: gnb-2 }", "{ supi: \"001010100007599\", gnb: gnb-2 }", 1), tg: exampleTarget, want: "ues[2].supi"},
		{name: "no responder for an omitted ping.dst", doc: stepDoc, tg: noResp, want: "step 2 ping.dst not set and target fixture has no responder"},
		{name: "pool too small", doc: stepDoc, tg: small, want: "subscribers[main] has 4 subscribers, the scenario needs 5"},
		{name: "a digits-only name", doc: strings.Replace(stepDoc, "name: ue2", "name: \"12\"", 1), tg: exampleTarget, want: `ues[2].name "12"`},
		{name: "a duplicate name", doc: strings.Replace(stepDoc, "name: ue2", "name: ue1", 1), tg: exampleTarget, want: `ues[2].name "ue1" is used twice`},
		{name: "a named range", doc: strings.Replace(stepDoc, "{ gnb: gnb-1, range: { count: 3 } }", "{ name: r, gnb: gnb-1, range: { count: 3 } }", 1), tg: exampleTarget, want: "a range entry has no name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tg := tc.tg
			if _, err := ParseWithTarget([]byte(tc.doc), &tg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// Without a target, a self-contained file may refer to a literal SUPI
	// and must give its own echo destinations.
	self := `core: { amf: 192.0.2.1:38412, plmn: { mcc: "001", mnc: "01" } }
gnbs: [ { id: 1, name: g } ]
ues: [ { supi: "001010100007500", gnb: g } ]
steps:
  - ping: { ue: "001010100007500", dst: 192.0.2.30 }
`
	if _, err := Parse([]byte(self)); err != nil {
		t.Errorf("self-contained file: %v", err)
	}
}

// A file that names a target cannot be parsed without one, and the server
// refuses one outright: the client resolves targets.
func TestTargetMustBeResolvedByTheClient(t *testing.T) {
	doc := "target: lab1\n" + fleetDoc
	if _, err := ParseFleet([]byte(doc)); err == nil || !strings.Contains(err.Error(), "names a target, which the caller must resolve") {
		t.Errorf("ParseFleet: %v", err)
	}
	if _, err := ParseFleetNoEnv([]byte(doc)); err == nil || !strings.Contains(err.Error(), "a scenario sent to the server cannot name a target") {
		t.Errorf("ParseFleetNoEnv: %v", err)
	}
	if _, err := Parse([]byte("target: lab1\n" + stepDoc)); err == nil {
		t.Error("Parse accepted a file naming a target")
	}
}

// A SUPI allocation keeps its digit width and its PLMN prefix: an offset
// that would carry into the MNC, or past the SUPI's length, is an error
// naming the pool, the start, the count and the PLMN, on the step and the
// fleet paths alike.
func TestSUPIAllocationStaysInPLMN(t *testing.T) {
	nearEnd := fixtureWith(t, `first_supi: "001010100007500"`, `first_supi: "001019999999998"`)
	top := loadTarget(t, strings.NewReplacer(`plmn: { mcc: "001", mnc: "01" }`, `plmn: { mcc: "999", mnc: "99" }`,
		`first_supi: "001010100007500"`, `first_supi: "999999999999998"`).Replace(exampleTargetYAML))
	// stepDoc allocates 5 SUPIs (ue1, a range of 3, ue2).
	if _, err := ParseWithTarget([]byte(stepDoc), &nearEnd); err == nil || !strings.Contains(err.Error(), "subscribers[main]: 001019999999998 + 5 UEs leaves PLMN 001/01") {
		t.Errorf("step, into the MNC: %v", err)
	}
	if _, err := ParseWithTarget([]byte(stepDoc), &top); err == nil || !strings.Contains(err.Error(), "subscribers[main]: 999999999999998 + 5 UEs leaves PLMN 999/99") {
		t.Errorf("step, past 15 digits: %v", err)
	}
	// fleetDoc has 10 UEs.
	if _, err := ParseFleetWithTarget([]byte(fleetDoc), &nearEnd); err == nil || !strings.Contains(err.Error(), "subscribers[main]: 001019999999998 + 10 UEs leaves PLMN 001/01") {
		t.Errorf("fleet, into the MNC: %v", err)
	}
	// Exactly to the end of the range is fine.
	two := fixtureWith(t, `first_supi: "001010100007500"`, `first_supi: "001019999999998"`)
	if _, err := ParseFleetWithTarget([]byte(strings.Replace(fleetDoc, "count: 10", "count: 2", 1)), &two); err != nil {
		t.Errorf("fleet, up to the last SUPI: %v", err)
	}
}

// Ports in a fleet's pools: an N3 ip:port gives the IP alone, as on the
// step path; an N2 ip:0 is the IP, and a fixed N2 port is an error naming
// the entry, not a silent strip.
func TestFleetPoolPorts(t *testing.T) {
	n3port := fixtureWith(t, "n3_ips: [198.51.100.20-198.51.100.29]", "n3_ips: [198.51.100.20:2152, 198.51.100.21:2152, 198.51.100.22:2152]")
	f, err := ParseFleetWithTarget([]byte(fleetDoc), &n3port)
	if err != nil || !reflect.DeepEqual(f.Topology.GNBs.N3IPs, []string{"198.51.100.20", "198.51.100.21", "198.51.100.22"}) {
		t.Errorf("N3 ip:port: %v %v", err, f)
	}
	n2zero := fixtureWith(t, "n2_source_ips: [192.0.2.20-192.0.2.29]", "n2_source_ips: [192.0.2.20:0, 192.0.2.21:0, 192.0.2.22:0]")
	f, err = ParseFleetWithTarget([]byte(fleetDoc), &n2zero)
	if err != nil || f.Topology.GNBs.SourceIPs[2] != "192.0.2.22" {
		t.Errorf("N2 ip:0: %v %v", err, f)
	}
	n2fixed := fixtureWith(t, "n2_source_ips: [192.0.2.20-192.0.2.29]", "n2_source_ips: [192.0.2.20, 192.0.2.21, 192.0.2.22:38412]")
	if _, err := ParseFleetWithTarget([]byte(fleetDoc), &n2fixed); err == nil ||
		!strings.Contains(err.Error(), "fleet gNB sources take an IP; remove :38412 from ran.n2_source_ips[2]") {
		t.Errorf("N2 fixed port: %v", err)
	}
	// The step path strips an N3 port the same way.
	s, err := ParseWithTarget([]byte(stepDoc), &n3port)
	if err != nil || s.GNBs[1].N3 != "198.51.100.21" {
		t.Errorf("step N3 ip:port: %v", err)
	}
}
