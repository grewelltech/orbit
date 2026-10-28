package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

const (
	testKi  = "5122250214c33e723a5dd523fc145fc0"
	testOPc = "981d464c7c52eb6e5036234984ad0bcf"
)

// classifyEndMarker judges only the old tunnel at the source gNB, inside the
// window, and orders G-PDUs against the first counted marker.
func TestClassifyEndMarker(t *testing.T) {
	const oldTEID, newTEID = 0x100, 0x200
	src, tgt := "10.0.0.1:2152", "10.0.0.2:2152"
	t0 := time.Unix(1000, 0)
	started, ack := t0, t0.Add(100*time.Millisecond)
	grace := 5 * time.Second
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	em := func(d time.Duration, teid uint32, gnb string) datapath.EndMarker {
		return datapath.EndMarker{Arrival: at(d), TEID: teid, GNB: gnb}
	}
	gp := func(d time.Duration, teid uint32, gnb string) datapath.GPDUArrival {
		return datapath.GPDUArrival{Arrival: at(d), TEID: teid, GNB: gnb}
	}
	tests := []struct {
		name       string
		markers    []datapath.EndMarker
		gpdus      []datapath.GPDUArrival
		want       Verdict
		wantDetail []string
	}{
		{name: "marker before the ack counts", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, src)},
			want: Pass, wantDetail: []string{"End Markers counted 1 (before the handover 0, after the window 0, on other tunnels 0)"}},
		{name: "marker after the ack within the window counts", markers: []datapath.EndMarker{em(3*time.Second, oldTEID, src)},
			want: Pass},
		{name: "no marker", want: Fail,
			wantDetail: []string{"no GTP-U End Marker on the old F-TEID between Path Switch Request and 5s after its Acknowledge. TS 23.501 5.8.2.9.1", "enable_end_marker", "TS 29.244 8.2.31"}},
		{name: "marker on the new TEID does not count", markers: []datapath.EndMarker{em(50*time.Millisecond, newTEID, src)},
			want: Fail, wantDetail: []string{"on other tunnels 1"}},
		{name: "marker reported in IPv4-mapped form counts", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, "[::ffff:10.0.0.1]:2152")},
			want: Pass, wantDetail: []string{"End Markers counted 1"}},
		{name: "marker on another socket of the same host does not count", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, "10.0.0.1:2153")},
			want: Fail, wantDetail: []string{"on other tunnels 1"}},
		{name: "g-pdu reported in IPv4-mapped form after the marker", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, src)},
			gpdus: []datapath.GPDUArrival{gp(60*time.Millisecond, oldTEID, "[::ffff:10.0.0.1]:2152")}, want: Fail},
		{name: "marker at the target gNB does not count", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, tgt)},
			want: Fail, wantDetail: []string{"on other tunnels 1"}},
		{name: "marker after the window does not count", markers: []datapath.EndMarker{em(ackPlus(ack, grace, t0)+time.Millisecond, oldTEID, src)},
			want: Fail, wantDetail: []string{"after the window 1"}},
		{name: "marker before the handover is recorded, not counted", markers: []datapath.EndMarker{em(-time.Millisecond, oldTEID, src)},
			want: Fail, wantDetail: []string{"before the handover 1"}},
		{name: "g-pdu on the old tunnel after the marker", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, src)},
			gpdus: []datapath.GPDUArrival{gp(60*time.Millisecond, oldTEID, src)},
			want:  Fail, wantDetail: []string{"G-PDU on the old F-TEID after the End Marker: end markers are sent after the last PDU on the old path (TS 23.501 5.8.2.9.1; TS 29.281 7.3.2)."}},
		{name: "g-pdu after a second marker still counts against the first", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, src), em(80*time.Millisecond, oldTEID, src)},
			gpdus: []datapath.GPDUArrival{gp(60*time.Millisecond, oldTEID, src)}, want: Fail},
		{name: "g-pdus before the marker are downlink before the switch", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, src)},
			gpdus: []datapath.GPDUArrival{gp(-time.Second, oldTEID, src), gp(10*time.Millisecond, oldTEID, src)},
			want:  Pass, wantDetail: []string{"downlink G-PDUs on the old path before the Acknowledge 2"}},
		{name: "g-pdu on the new tunnel after the marker is fine", markers: []datapath.EndMarker{em(50*time.Millisecond, oldTEID, src)},
			gpdus: []datapath.GPDUArrival{gp(60*time.Millisecond, newTEID, tgt)}, want: Pass,
			wantDetail: []string{"downlink G-PDUs on the old path before the Acknowledge 0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := PathObservation{OldTEID: oldTEID, SourceN3: netip.MustParseAddrPort(src), Started: started, Ack: ack, AckFromEvent: true,
				EndMarkers: tc.markers, GPDUs: tc.gpdus}
			v, observed, detail := classifyEndMarker(o, grace)
			if v != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", v, observed, detail, tc.want)
			}
			for _, want := range append([]string{"old F-TEID 0x100 at source N3 10.0.0.1:2152", "QFI container", "no traffic configured"}, tc.wantDetail...) {
				if !strings.Contains(detail, want) {
					t.Errorf("detail %q lacks %q", detail, want)
				}
			}
			if strings.Contains(detail, "Acknowledge time taken") {
				t.Error("detail claims a fallback Acknowledge time")
			}
		})
	}
}

// sameSocket compares socket addresses as values: IPv4-mapped and
// non-canonical IPv6 spellings of one socket are equal; another port, host
// or an unparseable report is not.
func TestSameSocket(t *testing.T) {
	v4 := canonical(netip.MustParseAddrPort("[::ffff:127.0.0.1]:2152"))
	v6 := canonical(netip.MustParseAddrPort("[0:0:0:0:0:0:0:1]:2152"))
	if v4.String() != "127.0.0.1:2152" || v6.String() != "[::1]:2152" {
		t.Fatalf("canonical forms %s and %s, want 127.0.0.1:2152 and [::1]:2152", v4, v6)
	}
	tests := []struct {
		ap       netip.AddrPort
		reported string
		want     bool
	}{
		{v4, "127.0.0.1:2152", true},
		{v4, "[::ffff:127.0.0.1]:2152", true},
		{v4, "127.0.0.1:2153", false},
		{v4, "127.0.0.2:2152", false},
		{v6, "[::1]:2152", true},
		{v6, "[0:0:0:0:0:0:0:1]:2152", true},
		{v6, "[::2]:2152", false},
		{v4, "not-an-address", false},
		{v4, "", false},
	}
	for _, tc := range tests {
		if got := sameSocket(tc.ap, tc.reported); got != tc.want {
			t.Errorf("sameSocket(%s, %q) = %v, want %v", tc.ap, tc.reported, got, tc.want)
		}
	}
}

// ackPlus is the offset from t0 of the window's close.
func ackPlus(ack time.Time, grace time.Duration, t0 time.Time) time.Duration {
	return ack.Add(grace).Sub(t0)
}

// The detail records the QFI container (never asserted), the traffic and an
// Acknowledge time that had to fall back to the handover's return.
func TestClassifyEndMarkerRecords(t *testing.T) {
	t0 := time.Unix(1000, 0)
	src := "10.0.0.1:2152"
	mk := func(hasQFI bool, qfi uint8) datapath.EndMarker {
		return datapath.EndMarker{Arrival: t0.Add(time.Millisecond), TEID: 1, GNB: src, HasQFI: hasQFI, QFI: qfi}
	}
	tests := []struct {
		name    string
		markers []datapath.EndMarker
		gpdus   []datapath.GPDUArrival
		traffic string
		ackEv   bool
		want    []string
	}{
		{name: "qfi present", markers: []datapath.EndMarker{mk(true, 5)}, ackEv: true,
			want: []string{"QFI container present, QFI 5 (recorded only)"}},
		{name: "qfi absent", markers: []datapath.EndMarker{mk(false, 0)}, ackEv: true,
			want: []string{"QFI container absent (not required on N3; recorded only)"}},
		{name: "qfi on some", markers: []datapath.EndMarker{mk(true, 1), mk(false, 0)}, ackEv: true,
			want: []string{"QFI container on 1 of 2 markers, QFI 1"}},
		{name: "traffic flowing", markers: []datapath.EndMarker{mk(false, 0)}, traffic: "10.1.1.1:7",
			gpdus: []datapath.GPDUArrival{{Arrival: t0.Add(-time.Millisecond), TEID: 1, GNB: src}}, ackEv: true,
			want: []string{"traffic to 10.1.1.1:7 flowing on the old path"}},
		{name: "traffic configured, none arrived", traffic: "10.1.1.1:7", ackEv: true,
			want: []string{"traffic to 10.1.1.1:7 configured, but no downlink reached the old path", "QFI container n/a"}},
		{name: "ack from the handover's return", markers: []datapath.EndMarker{mk(false, 0)},
			want: []string{"Acknowledge time taken from the handover's return"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := PathObservation{OldTEID: 1, SourceN3: netip.MustParseAddrPort(src), Started: t0, Ack: t0.Add(10 * time.Millisecond),
				AckFromEvent: tc.ackEv, EndMarkers: tc.markers, GPDUs: tc.gpdus, Traffic: tc.traffic}
			_, _, detail := classifyEndMarker(o, time.Second)
			for _, w := range tc.want {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// ueEnv is an Env for gNB A with a UE section, PLMN 208/93.
func ueEnv(amfAddr, bind, n3 string) Env {
	return Env{
		AMFAddr: amfAddr, Bind: bind,
		GNB: gnb.Config{ID: 0x100, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1,
			Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}},
		UE: UESpec{SUPI: "208930100000011", Ki: testKi, OPc: testOPc, DNN: "internet", GNBN3: n3},
	}
}

// Without a UE, or with an env the harness refuses, the check SKIPs or
// ERRORs before any association is opened (the AMF address is unroutable,
// so a dial would fail differently).
func TestEndMarkerCheckWithoutHarness(t *testing.T) {
	env := ueEnv("192.0.2.1:38412", "", "127.0.0.1")
	noUE := env
	noUE.UE = UESpec{}
	fixed := env
	fixed.Bind = "127.0.0.1:38999"
	badKey := env
	badKey.UE.Ki = "zz"
	noN3 := env
	noN3.UE.GNBN3 = ""
	badSUPI := env
	badSUPI.UE.SUPI = "001010000000001" // other PLMN
	ipv6 := env
	ipv6.UE.GNBN3 = "0:0:0:0:0:0:0:1"
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "no UE", env: noUE, want: Skip, wantObserved: "no UE configured", wantDetail: "--supi"},
		{name: "fixed bind port", env: fixed, want: Error, wantObserved: "harness setup failed", wantDetail: "fixes a port"},
		{name: "bad Ki", env: badKey, want: Error, wantObserved: "harness setup failed", wantDetail: "Ki"},
		{name: "no gNB N3", env: noN3, want: Error, wantObserved: "harness setup failed", wantDetail: "--gnb-n3"},
		{name: "SUPI outside the PLMN", env: badSUPI, want: Error, wantObserved: "harness setup failed", wantDetail: "mcc+mnc"},
		// Refused up front instead of panicking in PDU session setup (#156).
		{name: "IPv6 gNB N3", env: ipv6, want: Error, wantObserved: "harness setup failed",
			wantDetail: "gNB N3 address must be IPv4 (IPv4-mapped accepted); IPv6 N3 is not supported yet (#156)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := endMarkerOnPathSwitch{}.Run(context.Background(), tc.env)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved || !strings.Contains(r.Detail, tc.wantDetail) {
				t.Errorf("result %s / %q / %q, want %s / %q / containing %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
		})
	}
}

// The registry stamps the three references on the result, and --json
// carries them.
func TestEndMarkerResultJSON(t *testing.T) {
	r := runOne(context.Background(), endMarkerOnPathSwitch{}, Env{}, time.Second)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID         string    `json:"id"`
		Category   string    `json:"category"`
		SpecRef    string    `json:"spec_ref"`
		SpecRefs   []SpecRef `json:"spec_refs"`
		MinRelease Release   `json:"min_release"`
		Verdict    string    `json:"verdict"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "GTPU-END-MARKER-ON-PATH-SWITCH" || got.Category != "mobility" || got.Verdict != "SKIP" || got.MinRelease != 15 {
		t.Errorf("result = %+v", got)
	}
	var clauses []string
	for _, s := range got.SpecRefs {
		clauses = append(clauses, s.Spec+" "+s.Clause)
	}
	if fmt.Sprint(clauses) != "[TS 23.501 5.8.2.9.1 TS 29.281 7.3.2 TS 29.244 8.2.31]" {
		t.Errorf("spec_refs = %v", clauses)
	}
	for _, c := range clauses {
		if !strings.Contains(got.SpecRef, c) {
			t.Errorf("spec_ref %q lacks %s", got.SpecRef, c)
		}
	}
}

// mockCore is the mock AMF behind a real SCTP listener plus the stub UPF,
// on its own loopback /24 (the engine holds the source N3 socket through the
// production End-Marker grace window, so tests never share an address).
type mockCore struct {
	front *sctpfront.Front
	upf   *mockupf.UPF
	amf   *mockamf.AMF
	n3    string
}

func newMockCore(t *testing.T, net24 int, opts mockupf.Options, ki string, amfOpts ...mockamf.Option) *mockCore {
	t.Helper()
	base := testnet.Base(net24)
	return newMockCoreAt(t, base+"10:2152", base+"10", base+"11", opts, ki, amfOpts...)
}

// newMockCoreAt is newMockCore with explicit addresses: the stub UPF's
// listen address, the IPv4 the mock AMF advertises for it, and the gNB N3
// address the UE is configured with. amfOpts adjust the mock AMF.
func newMockCoreAt(t *testing.T, upfListen, upfAddr, n3 string, opts mockupf.Options, ki string, amfOpts ...mockamf.Option) *mockCore {
	t.Helper()
	upf, err := mockupf.Listen(upfListen, opts)
	testnet.RequireBind(t, upfListen, err)
	t.Cleanup(func() { _ = upf.Close() })
	k, err := auth.ParseHexKey("Ki", ki)
	if err != nil {
		t.Fatal(err)
	}
	opc, err := auth.ParseHexKey("OPc", testOPc)
	if err != nil {
		t.Fatal(err)
	}
	amf, err := mockamf.New(mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: upf, UPFAddress: upfAddr,
	}, amfOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if n := amf.EncodeErrors(); n != 0 {
			t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
		}
	})
	return &mockCore{front: sctpfront.Start(t, amf), upf: upf, amf: amf, n3: n3}
}

// assertClosed waits for every gNB association to close and checks the core
// still completes a fresh NG Setup.
func (c *mockCore) assertClosed(t *testing.T, env Env) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.front.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := c.front.Active(); n != 0 {
		t.Errorf("%d gNB associations still open after the check", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if alive, err := env.Alive(ctx); err != nil || !alive {
		t.Errorf("liveness probe after the check: alive %v, err %v", alive, err)
	}
}

// The three CI verdicts over the real-SCTP mock pair: no End Marker FAILs
// (absent), an End Marker PASSes, and a late G-PDU after it FAILs
// (ordering). The handover opens a second association from the same
// unbound source, and both close afterwards.
func TestEndMarkerCheckAgainstMockPair(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		opts         mockupf.Options
		traffic      string
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "end marker disabled", net24: testnet.ConformanceNoEndMarker, opts: mockupf.Options{}, want: Fail,
			wantObserved: "no End Marker on the old F-TEID",
			wantDetail:   []string{"no GTP-U End Marker on the old F-TEID between Path Switch Request and 300ms after its Acknowledge.", "End Markers counted 0"}},
		{name: "end marker", net24: testnet.ConformanceEndMarker, opts: mockupf.Options{EndMarker: true}, want: Pass,
			wantObserved: "1 End Marker(s) on the old F-TEID, no G-PDU after",
			wantDetail:   []string{"End Markers counted 1 (before the handover 0, after the window 0, on other tunnels 0)", "QFI container absent"}},
		{name: "end marker with QFI", net24: testnet.ConformanceEndMarkerQFI, opts: mockupf.Options{EndMarker: true, QFI: true}, want: Pass,
			wantDetail: []string{"QFI container present, QFI 1"}},
		{name: "late g-pdu after the end marker", net24: testnet.ConformanceLateGPDU, opts: mockupf.Options{EndMarker: true, LateGPDU: true}, want: Fail,
			wantObserved: "1 G-PDU(s) on the old F-TEID after the End Marker",
			wantDetail:   []string{"G-PDU on the old F-TEID after the End Marker: end markers are sent after the last PDU on the old path"}},
		// The stub UPF does not echo, so the flow reaches the UPF but brings
		// no downlink back.
		{name: "traffic configured", net24: testnet.ConformanceTraffic, opts: mockupf.Options{EndMarker: true}, traffic: "192.0.2.7:7", want: Pass,
			wantDetail: []string{"traffic to 192.0.2.7:7 configured, but no downlink reached the old path"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, tc.opts, testKi)
			env := ueEnv(c.front.Addr, "", c.n3)
			env.UE.Traffic = tc.traffic
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r := endMarkerOnPathSwitch{window: 300 * time.Millisecond}.Run(ctx, env)
			if r.Verdict != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", r.Verdict, r.Observed, r.Detail, tc.want)
			}
			if tc.wantObserved != "" && r.Observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", r.Observed, tc.wantObserved)
			}
			for _, w := range append([]string{"at source N3 " + c.n3 + ":2152"}, tc.wantDetail...) {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, "Acknowledge time taken") {
				t.Error("the Acknowledge event was missed")
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if sw := c.upf.Switches(); len(sw) != 1 {
				t.Errorf("UPF saw %d path switches, want 1", len(sw))
			}
			if tc.traffic != "" && len(c.upf.Uplink()) == 0 {
				t.Error("the configured flow sent no uplink")
			}
			c.assertClosed(t, env)
		})
	}
}

// --gnb-n3 spelled in a non-canonical form still PASSes against a core that
// sends the End Marker: the source socket comes from the engine's bound
// socket, and sockets compare as values. The detail shows the canonical
// form. (An IPv6 gNB N3 is refused up front, #156: see
// TestEndMarkerCheckWithoutHarness.)
func TestEndMarkerCheckNonCanonicalGNBN3(t *testing.T) {
	v4, hex := testnet.ConformanceNonCanonicalV4, testnet.ConformanceNonCanonicalHex
	tests := []struct {
		name      string
		upfListen string
		upfAddr   string
		n3        string
		wantSrc   string
	}{
		{name: "IPv4-mapped", upfListen: testnet.Addr(v4, 10) + ":2152", upfAddr: testnet.Addr(v4, 10), n3: "::ffff:" + testnet.Addr(v4, 11), wantSrc: testnet.Addr(v4, 11) + ":2152"},
		// 127.0.hex.11 in the expanded form: 7f00 is 127.0, then the block and 11 (0b).
		{name: "IPv4-mapped, expanded", upfListen: testnet.Addr(hex, 10) + ":2152", upfAddr: testnet.Addr(hex, 10), n3: fmt.Sprintf("0:0:0:0:0:ffff:7f00:%02x0b", hex), wantSrc: testnet.Addr(hex, 11) + ":2152"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCoreAt(t, tc.upfListen, tc.upfAddr, tc.n3, mockupf.Options{EndMarker: true}, testKi)
			env := ueEnv(c.front.Addr, "", c.n3)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r := endMarkerOnPathSwitch{window: 300 * time.Millisecond}.Run(ctx, env)
			if r.Verdict != Pass {
				t.Fatalf("verdict %s (%s; %s), want PASS", r.Verdict, r.Observed, r.Detail)
			}
			if !strings.Contains(r.Detail, "at source N3 "+tc.wantSrc+";") {
				t.Errorf("detail %q does not show the canonical source %s", r.Detail, tc.wantSrc)
			}
			c.assertClosed(t, env)
		})
	}
}

// A registration the core refuses (the UE's Ki does not match the core's)
// is an ERROR naming the step, with no association left open.
func TestEndMarkerCheckRegistrationFailure(t *testing.T) {
	c := newMockCore(t, testnet.ConformanceRegistrationFail, mockupf.Options{EndMarker: true}, "000102030405060708090a0b0c0d0e0f")
	env := ueEnv(c.front.Addr, "", c.n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := endMarkerOnPathSwitch{window: 300 * time.Millisecond}.Run(ctx, env)
	if r.Verdict != Error || r.Observed != "UE setup on the source gNB failed" || !strings.Contains(r.Detail, "registration on gNB 0x100") {
		t.Errorf("result %s / %q / %q, want an ERROR naming the registration step", r.Verdict, r.Observed, r.Detail)
	}
	if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
		t.Error("detail carries key material")
	}
	c.assertClosed(t, env)
}

// A per-test timeout shorter than the window is an ERROR, not a verdict on
// the core.
func TestEndMarkerCheckTimeoutBeforeWindow(t *testing.T) {
	c := newMockCore(t, testnet.ConformanceTimeoutBefore, mockupf.Options{EndMarker: true}, testKi)
	env := ueEnv(c.front.Addr, "", c.n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := endMarkerOnPathSwitch{window: time.Minute}.Run(withDeadlineAfterSwitch(ctx, c), env)
	if r.Verdict != Error || r.Observed != "timed out before the End Marker window closed" {
		t.Errorf("result %s / %q / %q, want a timeout ERROR", r.Verdict, r.Observed, r.Detail)
	}
	c.assertClosed(t, env)
}

// withDeadlineAfterSwitch cancels ctx a moment after the UPF hears of the
// path switch, so the check is inside its End Marker wait.
func withDeadlineAfterSwitch(ctx context.Context, c *mockCore) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		if c.upf.WaitFor(15*time.Second, func(u *mockupf.UPF) bool { return len(u.Switches()) > 0 }) {
			time.Sleep(200 * time.Millisecond)
		}
		cancel()
	}()
	return ctx
}
