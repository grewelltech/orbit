package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// classifyQFIContainer fails any sampled G-PDU without the container and
// records the QFI values without asserting them.
func TestClassifyQFIContainer(t *testing.T) {
	with := func(qfi uint8) datapath.GPDUArrival { return datapath.GPDUArrival{TEID: 1, HasQFI: true, QFI: qfi} }
	without := datapath.GPDUArrival{TEID: 1}
	tests := []struct {
		name         string
		gpdus        []datapath.GPDUArrival
		want         Outcome
		wantObserved string
		wantDetail   []string
	}{
		{name: "all with the container", gpdus: []datapath.GPDUArrival{with(1), with(1), with(1)}, want: qfiContainerPresent,
			wantObserved: "3 downlink G-PDUs, each with a PDU Session Container",
			wantDetail:   []string{"3 downlink G-PDUs sampled from the echoes of 10.1.1.1:7, 3 with a PDU Session Container and 0 without", "QFI values seen: 1 (recorded only)"}},
		{name: "several QFIs", gpdus: []datapath.GPDUArrival{with(10), with(9), with(2), with(9)}, want: qfiContainerPresent,
			wantDetail: []string{"QFI values seen: 2, 9, 10 (recorded only)"}},
		{name: "QFI zero is still a container", gpdus: []datapath.GPDUArrival{with(0)}, want: qfiContainerPresent,
			wantDetail: []string{"QFI values seen: 0 (recorded only)"}},
		{name: "none with the container", gpdus: []datapath.GPDUArrival{without, without}, want: qfiContainerAbsent,
			wantObserved: "2 of 2 downlink G-PDUs without a PDU Session Container",
			wantDetail:   []string{qfiContainerFailure + " Recorded: ", "0 with a PDU Session Container and 2 without", "QFI values seen: none"}},
		{name: "one without among many", gpdus: []datapath.GPDUArrival{with(1), without, with(1)}, want: qfiContainerAbsent,
			wantObserved: "1 of 3 downlink G-PDUs without a PDU Session Container",
			wantDetail:   []string{qfiContainerFailure, "2 with a PDU Session Container and 1 without"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail := classifyQFIContainer(tc.gpdus, "10.1.1.1:7")
			if covers(o).ID != tc.want.ID {
				t.Fatalf("outcome %s (%s; %s), want %s", o.ID, observed, detail, tc.want.ID)
			}
			if tc.wantObserved != "" && observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", observed, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// The failure message is the issue's, word for word.
func TestQFIContainerFailureMessage(t *testing.T) {
	const want = `downlink G-PDU on N3 without a PDU Session Container. TS 29.281 5.2.2.7 (Rel-15 V15.5.0 onward; V15.2.0-V15.4.0 said "may"). Likely cause: UPF not adding the extension header.`
	if qfiContainerFailure != want {
		t.Errorf("failure message = %q", qfiContainerFailure)
	}
}

// The check SKIPs without a UE, without an echo peer, and for a target
// release older than Rel-15; an env the harness refuses is an ERROR before
// anything is dialled. It cites a shall, so its worst verdict is FAIL.
func TestQFIContainerCheckWithoutHarness(t *testing.T) {
	if got := WorstVerdict(n3DLQFIContainer{}); got != Fail {
		t.Errorf("WorstVerdict = %s, want FAIL", got)
	}
	env := ueEnv("192.0.2.1:38412", "", "127.0.0.1")
	env.UE.Traffic = "192.0.2.7:7"
	noUE := env
	noUE.UE = UESpec{}
	noTraffic := env
	noTraffic.UE.Traffic = ""
	fixed := env
	fixed.Bind = "127.0.0.1:38999"
	old := env
	old.TargetRelease = 14
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "no UE", env: noUE, want: Skip, wantObserved: "no UE configured", wantDetail: "--supi"},
		{name: "no echo peer", env: noTraffic, want: Skip, wantObserved: "no downlink traffic source configured", wantDetail: "--traffic host:port"},
		{name: "fixed bind port", env: fixed, want: Error, wantObserved: "harness setup failed", wantDetail: "fixes a port"},
		{name: "target release before Rel-15", env: old, want: Skip, wantObserved: "requires a newer release than the target",
			wantDetail: "requires Rel-15 (TS 29.281 5.2.2.7); target release is Rel-14"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runOne(context.Background(), n3DLQFIContainer{}, tc.env, time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved || !strings.Contains(r.Detail, tc.wantDetail) {
				t.Errorf("result %s / %q / %q, want %s / %q / containing %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			if r.Group != "GTPv1-U / GTP-U Extension Header / PDU Session Container" {
				t.Errorf("group %s", r.Group)
			}
		})
	}
}

// The check's verdicts over the real-SCTP mock pair, whose stub UPF echoes
// the UE's uplink: with the PDU Session Container on its downlink the check
// PASSes, without it it FAILs, and with an echo peer that returns nothing it
// is an ERROR. The UE is deregistered and no association stays open.
func TestQFIContainerCheckAgainstMockPair(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		upf          mockupf.Options
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "container present", net24: testnet.ConformanceQFIPresent, upf: mockupf.Options{Echo: true, QFI: true}, want: Pass,
			wantDetail: []string{"0 without", "QFI values seen: 1 (recorded only)"}},
		{name: "container absent", net24: testnet.ConformanceQFIAbsent, upf: mockupf.Options{Echo: true}, want: Fail,
			wantDetail: []string{qfiContainerFailure, "0 with a PDU Session Container", "QFI values seen: none"}},
		{name: "echo peer silent", net24: testnet.ConformanceQFISilent, upf: mockupf.Options{QFI: true}, want: Error,
			wantObserved: "no downlink G-PDU arrived", wantDetail: []string{"returned nothing within 400ms"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, tc.upf, testKi)
			env := ueEnv(c.front.Addr, "", c.n3)
			env.UE.Traffic = "192.0.2.7:7"
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r := runOne(ctx, n3DLQFIContainer{target: 10, window: 400 * time.Millisecond}, env, 15*time.Second)
			if r.Verdict != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", r.Verdict, r.Observed, r.Detail, tc.want)
			}
			if tc.wantObserved != "" && r.Observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", r.Observed, tc.wantObserved)
			}
			switch tc.want {
			case Pass:
				if !strings.HasSuffix(r.Observed, " downlink G-PDUs, each with a PDU Session Container") {
					t.Errorf("observed %q", r.Observed)
				}
			case Fail:
				if !strings.HasSuffix(r.Observed, " downlink G-PDUs without a PDU Session Container") {
					t.Errorf("observed %q", r.Observed)
				}
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if len(c.upf.Uplink()) == 0 {
				t.Error("the UE sent no uplink to the echo peer")
			}
			if errs := c.upf.SendErrors(); len(errs) != 0 {
				t.Errorf("stub UPF send errors: %v", errs)
			}
			c.assertClosed(t, env)
		})
	}
}

// A registration the core refuses is BLOCKED on the registration, and a
// per-test timeout that ends while sampling is an ERROR, with no
// association left open.
func TestQFIContainerCheckErrors(t *testing.T) {
	t.Run("registration refused", func(t *testing.T) {
		c := newMockCore(t, testnet.ConformanceQFIRegFail, mockupf.Options{Echo: true, QFI: true}, "000102030405060708090a0b0c0d0e0f")
		env := ueEnv(c.front.Addr, "", c.n3)
		env.UE.Traffic = "192.0.2.7:7"
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r := n3DLQFIContainer{}.Run(ctx, env)
		if r.Verdict != Blocked || r.BlockedOn != ProcRegistration || r.Observed != "UE setup failed" || !strings.Contains(r.Detail, "registration on gNB 0x100") {
			t.Errorf("result %s on %q / %q / %q, want BLOCKED on the registration, naming the step", r.Verdict, r.BlockedOn, r.Observed, r.Detail)
		}
		c.assertClosed(t, env)
	})
	t.Run("timeout while sampling", func(t *testing.T) {
		// The stub does not echo, so the check is still sampling when the
		// context ends, a moment after the session came up.
		c := newMockCore(t, testnet.ConformanceQFITimeout, mockupf.Options{}, testKi)
		env := ueEnv(c.front.Addr, "", c.n3)
		env.UE.Traffic = "192.0.2.7:7"
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		sctx, stop := context.WithCancel(ctx)
		go func() {
			if c.upf.WaitFor(15*time.Second, func(u *mockupf.UPF) bool { return len(u.Uplink()) > 0 }) {
				time.Sleep(100 * time.Millisecond)
			}
			stop()
		}()
		r := n3DLQFIContainer{window: time.Minute}.Run(sctx, env)
		if r.Verdict != Error || r.Observed != "timed out while sampling downlink" {
			t.Errorf("result %s / %q / %q, want a timeout ERROR", r.Verdict, r.Observed, r.Detail)
		}
		c.assertClosed(t, env)
	})
}
