package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Fleet app cohort coverage (design §8, Phase 7 test shape): 4 UEs in two
// cohorts — voip on the fakeUPF UDP relay, http through per-UE n6Gateways
// (netstack TCP terminate + splice, the Phase-6 rig) — against ONE in-process
// loomd, all four UEs sharing ONE gNB N3 socket on one n3Pool. Pins the
// sharing invariants the design mandates:
//
//	one control dial + one TimeSync loop for the whole fleet;
//	one APP_SERVER Configure per voip member, ONE for the whole http cohort;
//	one shared tunnel and ONE gVisor netstack for the gNB (never per-UE);
//	per-cohort p5/p50/p95 aggregates in the report and on the
//	orbit_fleet_app_* gauges.

// mkFleetAppUE builds one synthetic SESSION_ACTIVE fleet UE on pool (the
// same shape as newDataSession, without a Manager — fleet mode is
// direct-drive).
func mkFleetAppUE(t *testing.T, pool *n3Pool, supi, ueIP, upfAddr string, ulTEID, dlTEID uint32) *fleetUE {
	t.Helper()
	sess := &Session{
		SUPI: supi,
		Result: &AttachResult{
			SessionActive: true,
			PDUAddress:    ueIP,
			UPFAddress:    upfAddr, // host:port — upfN3() respects it
			UPFTEID:       ulTEID,
			DLTEID:        dlTEID,
			QFI:           appTestQFI,
		},
		conn:  stubTransport{},
		gnbN3: "127.0.0.1",
	}
	sess.n3 = pool
	sess.n3Port = "0" // every UE shares the ONE ephemeral gNB socket
	t.Cleanup(sess.closeDataPath)
	return &fleetUE{sess: sess}
}

func fleetTestTuning() appTuning {
	return appTuning{
		syncInterval:   100 * time.Millisecond,
		syncBurst:      3,
		sampleInterval: 100 * time.Millisecond,
		trackerWindow:  200 * time.Millisecond,
		trackerN:       4,
		serverSlack:    10 * time.Second,
		rpcTimeout:     5 * time.Second,
		stopWait:       5 * time.Second,
	}
}

// gatherGaugeCohorts returns the cohort label values present on a gathered
// gauge family, keyed by family name.
func gatherGaugeCohorts(t *testing.T, reg *prometheus.Registry) map[string]map[string]float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]float64{} // family → cohort|q → value
	for _, f := range fams {
		vals := map[string]float64{}
		for _, m := range f.GetMetric() {
			var cohort, q string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "cohort":
					cohort = l.GetValue()
				case "q":
					q = l.GetValue()
				}
			}
			key := cohort
			if q != "" {
				key += "|" + q
			}
			vals[key] = m.GetGauge().GetValue()
		}
		out[f.GetName()] = vals
	}
	return out
}

// TestFleetAppCohortsSharedPlumbing is the two-cohort e2e rig: shared control
// plumbing, per-app server placement, per-gNB netstack budget, and sane
// cohort aggregates, all under 30s.
func TestFleetAppCohortsSharedPlumbing(t *testing.T) {
	const (
		voipSUPI1, voipIP1 = "001010000000101", "192.168.100.101"
		voipSUPI2, voipIP2 = "001010000000102", "192.168.100.102"
		httpSUPI1, httpIP1 = "001010000000103", "192.168.100.103"
		httpSUPI2, httpIP2 = "001010000000104", "192.168.100.104"
	)
	agent := startLoomAgent(t, "v0.11.0-test")
	pool := newN3Pool()

	// voip data path: the fakeUPF UDP relay, one tunnel pair per member.
	upf := newFakeUPF(t)
	upf.addUE(0x0121, 0x0222)
	v1 := mkFleetAppUE(t, pool, voipSUPI1, voipIP1, upf.addr(), appTestULTEID, appTestDLTEID)
	v2 := mkFleetAppUE(t, pool, voipSUPI2, voipIP2, upf.addr(), 0x0121, 0x0222)

	// http data path: one n6Gateway (netstack TCP terminate + kernel splice)
	// per member — far-side TEST plumbing standing in for the UPF's N6
	// boundary; the UE side still shares one gNB socket and ONE gVisor
	// stack, which is what the budget assertion below pins. Both gateways
	// splice onto the SAME kernel port: the single shared loom origin.
	port := freeTCPPortInt(t)
	origin := fmt.Sprintf("127.0.0.1:%d", port)
	upfH1 := newFakeUPFSocket(t)
	upfH2 := newFakeUPFSocket(t)
	newN6Gateway(t, upfH1, 0x0302, port, origin)
	newN6Gateway(t, upfH2, 0x0402, port, origin)
	h1 := mkFleetAppUE(t, pool, httpSUPI1, httpIP1, upfH1.LocalAddr().String(), 0x0301, 0x0302)
	h2 := mkFleetAppUE(t, pool, httpSUPI2, httpIP2, upfH2.LocalAddr().String(), 0x0401, 0x0402)

	cohorts := []FleetAppCohort{
		{Name: "calls", App: "voip", Peer: agent, PeerDataIP: "127.0.0.1",
			Params: map[string]string{"codec": "pcmu"}, Count: 2},
		{Name: "web", App: "http", Peer: agent, PeerDataIP: webGatewayIP,
			Params: map[string]string{
				"port_min":     fmt.Sprint(port),
				"port_max":     fmt.Sprint(port),
				"object_size":  "16KB",
				"think":        "20ms",
				"tls":          "true",
				"tls_insecure": "true",
			}, Count: 2},
	}
	members := [][]*fleetUE{{v1, v2}, {h1, h2}}
	if err := validateFleetAppCohorts(cohorts); err != nil {
		t.Fatalf("validate: %v", err)
	}

	agents := newFleetAgentPool(fleetTestTuning())
	reg := prometheus.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	reports := runFleetApps(ctx, testLogger(), agents, cohorts, members, 4*time.Second, reg)

	// ONE control connection + ONE TimeSync loop for the whole fleet, and
	// exactly 2 (per-UE voip) + 1 (shared http origin) = 3 Configures.
	dials, syncLoops, configures := agents.counters()
	if dials != 1 || syncLoops != 1 {
		t.Errorf("agent plumbing not shared: %d dial(s), %d TimeSync loop(s), want 1 and 1", dials, syncLoops)
	}
	if configures != 3 {
		t.Errorf("APP_SERVER Configures = %d, want 3 (one per voip member + one shared origin)", configures)
	}
	agents.mu.Lock()
	live := len(agents.agents)
	agents.mu.Unlock()
	if live != 0 {
		t.Errorf("%d agent handle(s) still held after the run (refcount leak)", live)
	}

	// Per-gNB budget (design §8): four UEs, TWO running TCP — still one
	// shared tunnel and ONE gVisor stack on the gNB, never per-UE.
	if pool.size() != 1 {
		t.Errorf("pool size = %d, want 1 (one N3 socket per gNB)", pool.size())
	}
	if n := pool.netstackCount(); n != 1 {
		t.Errorf("netstack count = %d, want 1 (TCP cohorts share the gNB's stack)", n)
	}

	if len(reports) != 2 {
		t.Fatalf("got %d cohort reports, want 2", len(reports))
	}
	voip, web := reports[0], reports[1]
	if voip.Err != "" || web.Err != "" {
		t.Fatalf("cohort setup errors: voip %q, web %q", voip.Err, web.Err)
	}
	if voip.UEs != 2 || voip.Failed != 0 || voip.Servers != 2 {
		t.Errorf("voip cohort = %d UEs, %d failed, %d servers; want 2/0/2 (one answerer per UE — the latch serves one source)",
			voip.UEs, voip.Failed, voip.Servers)
	}
	if web.UEs != 2 || web.Failed != 0 || web.Servers != 1 {
		t.Errorf("http cohort = %d UEs, %d failed, %d servers; want 2/0/1 (one origin serves the cohort)",
			web.UEs, web.Failed, web.Servers)
	}

	// Aggregates populated and sane.
	if voip.MOS == nil {
		t.Fatal("voip cohort has no MOS distribution")
	}
	if voip.MOS.P50 < 3.0 || voip.MOS.P50 > 5.0 {
		t.Errorf("voip cohort MOS p50 = %v, want a sane loopback score in [3, 5]", voip.MOS.P50)
	}
	if voip.MOS.P5 > voip.MOS.P50 || voip.MOS.P50 > voip.MOS.P95 {
		t.Errorf("voip MOS quantiles not ordered: %+v", voip.MOS)
	}
	if voip.TTFBMs != nil || voip.GoodputMbps != nil {
		t.Error("voip cohort carries HTTP aggregates")
	}
	if web.TTFBMs == nil || web.GoodputMbps == nil {
		t.Fatalf("http cohort aggregates missing: ttfb %v, goodput %v", web.TTFBMs, web.GoodputMbps)
	}
	if web.TTFBMs.P50 <= 0 {
		t.Errorf("http cohort TTFB p50 = %v, want > 0", web.TTFBMs.P50)
	}
	if web.GoodputMbps.P50 <= 0 {
		t.Errorf("http cohort goodput p50 = %v, want > 0", web.GoodputMbps.P50)
	}
	if web.MOS != nil {
		t.Error("http cohort carries a MOS aggregate")
	}

	// The Prometheus gauges carry the same distributions, labeled by cohort
	// name (bounded cardinality — never a SUPI).
	fams := gatherGaugeCohorts(t, reg)
	if got := fams["orbit_fleet_app_mos"]["calls|p50"]; got < 3.0 || got > 5.0 {
		t.Errorf("orbit_fleet_app_mos{cohort=calls,q=p50} = %v, want [3, 5]", got)
	}
	if got := fams["orbit_fleet_app_ttfb_ms"]["web|p50"]; got <= 0 {
		t.Errorf("orbit_fleet_app_ttfb_ms{cohort=web,q=p50} = %v, want > 0", got)
	}
	if got := fams["orbit_fleet_app_ues"]["calls"]; got != 2 {
		t.Errorf("orbit_fleet_app_ues{cohort=calls} = %v, want 2", got)
	}
	for fam, vals := range fams {
		for key := range vals {
			if strings.Contains(key, "00101000000") {
				t.Errorf("%s carries a SUPI-labeled series %q (unbounded cardinality)", fam, key)
			}
		}
	}

	// Harness sockets: the fakeUPF's relay flows keep goroutines; close them
	// so the test does not leak into later ones.
	upf.closeFlows()
}

// TestFleetAppCohortPortRangeRefused pins the actionable port-exhaustion
// refusal: a voip cohort larger than its far-end port range must be refused
// up front (each member needs its own answerer port), both by the validator
// and by RunFleet before it touches the network.
func TestFleetAppCohortPortRangeRefused(t *testing.T) {
	cohorts := []FleetAppCohort{{
		Name: "calls", App: "voip", Peer: "n6:9551", Count: 10,
		Params: map[string]string{"port_min": "40000", "port_max": "40004"},
	}}
	err := validateFleetAppCohorts(cohorts)
	if err == nil {
		t.Fatal("expected the oversized voip cohort to be refused")
	}
	for _, want := range []string{`"calls"`, "10 UEs", "40000..40004", "5 port(s)", "widen the range or shrink the cohort"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}

	// RunFleet refuses before dialing anything: a spec pointing at a dead
	// AMF must still fail with the port-range error, not a dial error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spec := FleetRunSpec{AMFAddr: "127.0.0.1:1", GNBs: []FleetGNB{{BindAddr: "", N3Addr: "127.0.0.1"}}}
	_, rerr := RunFleet(ctx, testLogger(), spec, FleetBehaviors{Duration: time.Second, Apps: cohorts}, nil, nil)
	if rerr == nil || !strings.Contains(rerr.Error(), "port(s); widen the range") {
		t.Errorf("RunFleet did not refuse up front: %v", rerr)
	}
}

// TestFleetAppCohortValidation covers the remaining fail-fast paths and the
// port-range edge cases.
func TestFleetAppCohortValidation(t *testing.T) {
	cases := []struct {
		name    string
		cohorts []FleetAppCohort
		want    string // "" = valid
	}{
		{"unknown app", []FleetAppCohort{{App: "ftp", Peer: "n6:9551", Count: 1}}, "not supported"},
		{"missing peer", []FleetAppCohort{{App: "voip", Count: 1}}, "no peer loomd agent"},
		{"zero count", []FleetAppCohort{{App: "voip", Peer: "n6:9551", Count: 0}}, "count must be >= 1"},
		{"duplicate names", []FleetAppCohort{
			{Name: "x", App: "voip", Peer: "n6:9551", Count: 1},
			{Name: "x", App: "http", Peer: "n6:9551", Count: 1},
		}, "declared twice"},
		{"port_max without port_min", []FleetAppCohort{{App: "voip", Peer: "n6:9551", Count: 1,
			Params: map[string]string{"port_max": "41000"}}}, "port_max 41000 given without port_min"},
		{"bad port_min", []FleetAppCohort{{App: "voip", Peer: "n6:9551", Count: 1,
			Params: map[string]string{"port_min": "forty"}}}, "not a number"},
		{"range fits", []FleetAppCohort{{App: "voip", Peer: "n6:9551", Count: 5,
			Params: map[string]string{"port_min": "40000", "port_max": "40004"}}}, ""},
		{"http ignores the voip range rule", []FleetAppCohort{{App: "http", Peer: "n6:9551", Count: 50,
			Params: map[string]string{"port_min": "40000", "port_max": "40000"}}}, ""},
		{"ephemeral voip has no cap", []FleetAppCohort{{App: "voip", Peer: "n6:9551", Count: 500}}, ""},
		// Aggregate capacity: two voip cohorts on ONE loomd with overlapping
		// ranges draw from a shared pool — individually fitting, collectively
		// exhausting it (the mid-run "no free port" stranding the validator
		// exists to prevent).
		{"overlapping ranges collectively exhausted", []FleetAppCohort{
			{Name: "a", App: "voip", Peer: "n6:9551", Count: 60,
				Params: map[string]string{"port_min": "40000", "port_max": "40099"}},
			{Name: "b", App: "voip", Peer: "n6:9551", Count: 60,
				Params: map[string]string{"port_min": "40000", "port_max": "40099"}},
		}, "together they need 120 far-end answerer ports but the combined range 40000..40099 holds only 100"},
		{"overlapping ranges that fit", []FleetAppCohort{
			{Name: "a", App: "voip", Peer: "n6:9551", Count: 40,
				Params: map[string]string{"port_min": "40000", "port_max": "40099"}},
			{Name: "b", App: "voip", Peer: "n6:9551", Count: 60,
				Params: map[string]string{"port_min": "40050", "port_max": "40149"}},
		}, ""},
		{"partial overlap collectively exhausted", []FleetAppCohort{
			{Name: "a", App: "voip", Peer: "n6:9551", Count: 95,
				Params: map[string]string{"port_min": "40000", "port_max": "40099"}},
			{Name: "b", App: "voip", Peer: "n6:9551", Count: 60,
				Params: map[string]string{"port_min": "40090", "port_max": "40149"}},
		}, "combined range 40000..40149 holds only 150"},
		{"disjoint ranges on one loomd are independent", []FleetAppCohort{
			{Name: "a", App: "voip", Peer: "n6:9551", Count: 50,
				Params: map[string]string{"port_min": "40000", "port_max": "40049"}},
			{Name: "b", App: "voip", Peer: "n6:9551", Count: 50,
				Params: map[string]string{"port_min": "40050", "port_max": "40099"}},
		}, ""},
		{"same range on different loomds is independent", []FleetAppCohort{
			{Name: "a", App: "voip", Peer: "n6a:9551", Count: 60,
				Params: map[string]string{"port_min": "40000", "port_max": "40099"}},
			{Name: "b", App: "voip", Peer: "n6b:9551", Count: 60,
				Params: map[string]string{"port_min": "40000", "port_max": "40099"}},
		}, ""},
		// tls_ca is structurally unusable against a stock loomd origin
		// (per-flow self-signed cert; see refuseUnusableTLSCA): a cohort
		// carrying it must be refused now, not soak with 100%-error members.
		{"tls_ca refused for http", []FleetAppCohort{{App: "http", Peer: "n6:9551", Count: 1,
			Params: map[string]string{"tls": "true", "tls_ca": "QQ=="}}}, "tls_ca cannot verify a stock loomd origin"},
		{"tls_ca refused for video", []FleetAppCohort{{App: "video", Peer: "n6:9551", Count: 1,
			Params: map[string]string{"tls": "true", "tls_ca": "QQ=="}}}, "tls_ca cannot verify a stock loomd origin"},
		{"voip ignores tls_ca", []FleetAppCohort{{App: "voip", Peer: "n6:9551", Count: 1,
			Params: map[string]string{"tls_ca": "QQ=="}}}, ""},
	}
	for _, tc := range cases {
		err := validateFleetAppCohorts(tc.cohorts)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v does not contain %q", tc.name, err, tc.want)
		}
	}
}

// TestCarveAppCohorts pins the membership carve: cohorts take from the TAIL
// of the fleet, mobility and synthetic traffic keep the head, oversized
// cohorts clamp to the attached non-mobile population.
func TestCarveAppCohorts(t *testing.T) {
	ues := make([]*fleetUE, 10)
	for i := range ues {
		ues[i] = &fleetUE{sess: &Session{SUPI: fmt.Sprint(i)}}
	}
	cohorts := []FleetAppCohort{
		{Name: "a", App: "voip", Peer: "p", Count: 3},
		{Name: "b", App: "http", Peer: "p", Count: 4},
	}
	members, total := carveAppCohorts(ues, 2, cohorts, testLogger())
	if total != 7 {
		t.Fatalf("total = %d, want 7", total)
	}
	if len(members[0]) != 3 || members[0][0] != ues[7] {
		t.Errorf("cohort a got %d members starting at %v, want 3 from the tail", len(members[0]), members[0])
	}
	if len(members[1]) != 4 || members[1][0] != ues[3] {
		t.Errorf("cohort b got %d members, want the 4 before cohort a's", len(members[1]))
	}

	// Oversized: 2 mobile + 9 requested > 10 UEs → clamped to 8.
	cohorts[1].Count = 6
	members, total = carveAppCohorts(ues, 2, cohorts, testLogger())
	if total != 8 || len(members[1]) != 5 {
		t.Errorf("clamp: total %d members[1] %d, want 8 and 5", total, len(members[1]))
	}
}

// TestFleetQuantiles pins the p5/p50/p95 summary.
func TestFleetQuantiles(t *testing.T) {
	if q := fleetQuantiles(nil); q != nil {
		t.Errorf("empty input gave %+v", q)
	}
	q := fleetQuantiles([]float64{4.0})
	if q.P5 != 4.0 || q.P50 != 4.0 || q.P95 != 4.0 {
		t.Errorf("single value: %+v", q)
	}
	q = fleetQuantiles([]float64{3, 1, 2})
	if q.P50 != 2 {
		t.Errorf("median of 1..3 = %v", q.P50)
	}
	if !(q.P5 >= 1 && q.P5 <= q.P50 && q.P95 >= q.P50 && q.P95 <= 3) {
		t.Errorf("quantiles out of range: %+v", q)
	}
}
