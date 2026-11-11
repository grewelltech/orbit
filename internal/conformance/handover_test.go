package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// classifyPathSwitchAck fails a missing list, a missing or undecodable
// transfer, a session left out, and silence after a switch that kept the
// uplink endpoint; silence after one that moved it is an ERROR; an absent UL
// NG-U UP TNL Information alone never fails.
func TestClassifyPathSwitchAck(t *testing.T) {
	const oldTEID, newTEID = 0x100, 0x200
	t0 := time.Unix(1000, 0)
	started, ack := t0, t0.Add(100*time.Millisecond)
	window := 2 * time.Second
	gp := func(d time.Duration, teid uint32) datapath.GPDUArrival {
		return datapath.GPDUArrival{Arrival: t0.Add(d), TEID: teid}
	}
	before := gp(-time.Second, oldTEID)
	late := 100*time.Millisecond + 1500*time.Millisecond
	item := func(id int64, n int, decodeErr string, ul bool) gnb.PathSwitchAckItem {
		return gnb.PathSwitchAckItem{PDUSessionID: id, TransferLen: n, TransferErr: decodeErr, HasULTNL: ul}
	}
	list := func(items ...gnb.PathSwitchAckItem) *gnb.PathSwitchAck {
		return &gnb.PathSwitchAck{ListPresent: true, Items: items}
	}
	tests := []struct {
		name         string
		switched     *gnb.PathSwitchAck
		traffic      string
		gpdus        []datapath.GPDUArrival
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "transfer without UL TNL, no traffic", switched: list(item(1, 1, "", false)), want: Pass,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; UL NG-U UP TNL Information absent, uplink not probed",
			wantDetail:   []string{"Switched List: PDU session 1: transfer of 1 octets, UL NG-U UP TNL Information absent", "uplink not probed (no --traffic echo peer)"}},
		{name: "transfer with UL TNL, no traffic", switched: list(item(1, 9, "", true)), want: Pass,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; UL NG-U UP TNL Information present, uplink not probed"},
		{name: "uplink answered on the new path", switched: list(item(1, 1, "", false)), traffic: "10.1.1.1:7",
			gpdus: []datapath.GPDUArrival{before, gp(late, newTEID)}, want: Pass,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; uplink answered after the switch",
			wantDetail:   []string{"1 before the handover, 1 on the new path and 0 on the old path between 1s and 2s after the Acknowledge"}},
		{name: "uplink answered on the old path", switched: list(item(1, 1, "", false)), traffic: "10.1.1.1:7",
			gpdus: []datapath.GPDUArrival{before, gp(late, oldTEID)}, want: Pass,
			wantDetail: []string{"0 on the new path and 1 on the old path"}},
		{name: "echo peer silent before the handover", switched: list(item(1, 1, "", false)), traffic: "10.1.1.1:7", want: Pass,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; UL NG-U UP TNL Information absent, uplink not probed",
			wantDetail:   []string{"uplink not probed (the echo peer did not answer before the handover)"}},
		{name: "an echo in the first half of the window does not count", switched: list(item(1, 1, "", false)), traffic: "10.1.1.1:7",
			gpdus: []datapath.GPDUArrival{before, gp(600*time.Millisecond, newTEID)}, want: Fail,
			wantObserved: "uplink not answered after a path switch that kept the uplink endpoint (no UL NG-U UP TNL Information)"},
		{name: "an echo after the window does not count", switched: list(item(1, 1, "", false)), traffic: "10.1.1.1:7",
			gpdus: []datapath.GPDUArrival{before, gp(3*time.Second, newTEID)}, want: Fail},
		{name: "silent after a switch that moved the uplink endpoint", switched: list(item(1, 9, "", true)), traffic: "10.1.1.1:7",
			gpdus: []datapath.GPDUArrival{before}, want: Error,
			wantObserved: "uplink not answered after a path switch that moved the uplink endpoint"},
		{name: "no Switched List", switched: &gnb.PathSwitchAck{}, want: Fail,
			wantObserved: "PATH SWITCH REQUEST ACKNOWLEDGE without a PDU Session Resource Switched List", wantDetail: []string{"Switched List: none"}},
		{name: "nothing recorded", want: Fail,
			wantObserved: "PATH SWITCH REQUEST ACKNOWLEDGE without a PDU Session Resource Switched List"},
		{name: "empty transfer", switched: list(item(1, 0, "", false)), want: Fail,
			wantObserved: "PDU session 1 switched without a Path Switch Request Acknowledge Transfer", wantDetail: []string{"PDU session 1: no transfer"}},
		{name: "transfer does not decode", switched: list(item(1, 3, "bad bits", false)), want: Fail,
			wantObserved: "PDU session 1 switched with a Path Switch Request Acknowledge Transfer that does not decode",
			wantDetail:   []string{"PDU session 1: transfer of 3 octets does not decode (bad bits)"}},
		{name: "another session's transfer missing", switched: list(item(1, 1, "", false), item(2, 0, "", false)), want: Fail,
			wantObserved: "PDU session 2 switched without a Path Switch Request Acknowledge Transfer"},
		{name: "the UE's session not listed", switched: list(item(2, 1, "", false)), want: Fail,
			wantObserved: "PDU session 1 is not in the PDU Session Resource Switched List"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := PathObservation{OldTEID: oldTEID, NewTEID: newTEID, Started: started, Ack: ack, AckFromEvent: true,
				GPDUs: tc.gpdus, Traffic: tc.traffic, Switched: tc.switched}
			v, observed, detail := classifyPathSwitchAck(o, 1, window)
			if v != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", v, observed, detail, tc.want)
			}
			if tc.wantObserved != "" && observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", observed, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
			if (v == Fail) != strings.HasPrefix(detail, pathSwitchAckFailure+" Recorded: ") {
				t.Errorf("detail %q: the failure message leads a FAIL and only a FAIL", detail)
			}
		})
	}
}

// An Acknowledge time taken from the handover's return is recorded.
func TestClassifyPathSwitchAckRecordsAckSource(t *testing.T) {
	o := PathObservation{Switched: &gnb.PathSwitchAck{ListPresent: true, Items: []gnb.PathSwitchAckItem{{PDUSessionID: 1, TransferLen: 1}}}}
	if _, _, detail := classifyPathSwitchAck(o, 1, time.Second); !strings.Contains(detail, "Acknowledge time taken from the handover's return") {
		t.Errorf("detail %q does not record the Acknowledge's source", detail)
	}
}

// The failure message is the issue's, word for word.
func TestPathSwitchAckFailureMessage(t *testing.T) {
	const want = "PATH SWITCH REQUEST ACKNOWLEDGE missing / per-session transfer missing. TS 38.413 8.4.4.2, 9.3.4.9 (UL NG-U UP TNL optional from V15.1.0). Likely cause: AMF/SMF path-switch handling."
	if pathSwitchAckFailure != want {
		t.Errorf("failure message = %q", pathSwitchAckFailure)
	}
}

// Without a UE the check SKIPs, and an env the harness refuses is an ERROR
// before anything is dialled.
func TestPathSwitchAckCheckWithoutHarness(t *testing.T) {
	env := ueEnv("192.0.2.1:38412", "", "127.0.0.1")
	noUE := env
	noUE.UE = UESpec{}
	fixed := env
	fixed.Bind = "127.0.0.1:38999"
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "no UE", env: noUE, want: Skip, wantObserved: "no UE configured", wantDetail: "--supi"},
		{name: "fixed bind port", env: fixed, want: Error, wantObserved: "harness setup failed", wantDetail: "fixes a port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ngapPathSwitchAckTransfer{}.Run(context.Background(), tc.env)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved || !strings.Contains(r.Detail, tc.wantDetail) {
				t.Errorf("result %s / %q / %q, want %s / %q / containing %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
		})
	}
}

// The check's verdicts over the real-SCTP mock pair: an Acknowledge with a
// transfer PASSes with or without UL NG-U UP TNL Information, with the
// uplink answered when an echo peer is there; an omitted transfer, an
// uplink endpoint moved without saying so, a Failure and no answer FAIL.
// Every path leaves no association open.
func TestPathSwitchAckCheckAgainstMockPair(t *testing.T) {
	const peer = "192.0.2.7:7"
	tests := []struct {
		name         string
		net24        int
		upf          mockupf.Options
		answer       mockamf.PathSwitchAnswer
		traffic      string
		want         Verdict
		wantObserved string
		wantDetail   []string
		switches     int
	}{
		{name: "acknowledged, no traffic", net24: testnet.ConformancePSAckPlain, want: Pass, switches: 1,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; UL NG-U UP TNL Information absent, uplink not probed",
			wantDetail:   []string{"Switched List: PDU session 1: transfer of 1 octets, UL NG-U UP TNL Information absent"}},
		{name: "acknowledged, uplink answered", net24: testnet.ConformancePSAckEcho, upf: mockupf.Options{Echo: true}, traffic: peer,
			want: Pass, switches: 1,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; uplink answered after the switch",
			wantDetail:   []string{"UL NG-U UP TNL Information absent", "uplink answered after the path switch", "0 on the old path"}},
		{name: "new uplink endpoint, uplink answered", net24: testnet.ConformancePSAckNewUL, upf: mockupf.Options{Echo: true},
			answer: mockamf.AckWithNewUL, traffic: peer, want: Pass, switches: 1,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; uplink answered after the switch",
			wantDetail:   []string{"UL NG-U UP TNL Information present"}},
		{name: "echo peer silent", net24: testnet.ConformancePSAckSilentPeer, traffic: peer, want: Pass, switches: 1,
			wantObserved: "Acknowledge with a transfer for the switched PDU session; UL NG-U UP TNL Information absent, uplink not probed",
			wantDetail:   []string{"uplink not probed (the echo peer did not answer before the handover)"}},
		{name: "transfer omitted", net24: testnet.ConformancePSAckNoTransfer, answer: mockamf.AckWithoutTransfer, want: Fail, switches: 1,
			wantObserved: "PDU session 1 switched without a Path Switch Request Acknowledge Transfer",
			wantDetail:   []string{pathSwitchAckFailure, "PDU session 1: no transfer"}},
		{name: "uplink endpoint moved without UL TNL", net24: testnet.ConformancePSAckHiddenUL, upf: mockupf.Options{Echo: true},
			answer: mockamf.AckHidingULChange, traffic: peer, want: Fail, switches: 1,
			wantObserved: "uplink not answered after a path switch that kept the uplink endpoint (no UL NG-U UP TNL Information)",
			wantDetail:   []string{pathSwitchAckFailure, "uplink not answered after the path switch", "0 on the new path and 0 on the old path"}},
		{name: "path switch request failure", net24: testnet.ConformancePSAckFailure, answer: mockamf.FailPathSwitch, want: Fail,
			wantObserved: "no PATH SWITCH REQUEST ACKNOWLEDGE",
			wantDetail:   []string{pathSwitchAckFailure, "the core answered with PATH SWITCH REQUEST FAILURE"}},
		{name: "path switch request unanswered", net24: testnet.ConformancePSAckIgnored, answer: mockamf.IgnorePathSwitch, want: Fail,
			wantObserved: "no PATH SWITCH REQUEST ACKNOWLEDGE",
			wantDetail:   []string{pathSwitchAckFailure, "no Acknowledge within 1s", "await PathSwitch response"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, tc.upf, testKi, mockamf.WithPathSwitchAnswer(tc.answer))
			env := ueEnv(c.front.Addr, "", c.n3)
			env.UE.Traffic = tc.traffic
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r := ngapPathSwitchAckTransfer{ackWait: time.Second, uplinkWindow: 600 * time.Millisecond}.Run(ctx, env)
			if r.Verdict != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", r.Verdict, r.Observed, r.Detail, tc.want)
			}
			if r.Observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", r.Observed, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
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
			if sw := c.upf.Switches(); len(sw) != tc.switches {
				t.Errorf("UPF saw %d path switches, want %d", len(sw), tc.switches)
			}
			if errs := c.upf.SendErrors(); len(errs) != 0 {
				t.Errorf("stub UPF send errors: %v", errs)
			}
			c.assertClosed(t, env)
		})
	}
}

// A registration the core refuses, and a per-test timeout that ends inside
// the uplink window, are ERRORs, with no association left open.
func TestPathSwitchAckCheckErrors(t *testing.T) {
	t.Run("registration refused", func(t *testing.T) {
		c := newMockCore(t, testnet.ConformancePSAckRegFail, mockupf.Options{}, "000102030405060708090a0b0c0d0e0f")
		env := ueEnv(c.front.Addr, "", c.n3)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r := ngapPathSwitchAckTransfer{}.Run(ctx, env)
		if r.Verdict != Error || r.Observed != "UE setup on the source gNB failed" || !strings.Contains(r.Detail, "registration on gNB 0x100") {
			t.Errorf("result %s / %q / %q, want an ERROR naming the registration step", r.Verdict, r.Observed, r.Detail)
		}
		c.assertClosed(t, env)
	})
	t.Run("timeout in the uplink window", func(t *testing.T) {
		c := newMockCore(t, testnet.ConformancePSAckTimeout, mockupf.Options{Echo: true}, testKi)
		env := ueEnv(c.front.Addr, "", c.n3)
		env.UE.Traffic = "192.0.2.7:7"
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r := ngapPathSwitchAckTransfer{uplinkWindow: time.Minute}.Run(withDeadlineAfterSwitch(ctx, c), env)
		if r.Verdict != Error || r.Observed != "timed out before the uplink window closed" {
			t.Errorf("result %s / %q / %q, want a timeout ERROR", r.Verdict, r.Observed, r.Detail)
		}
		c.assertClosed(t, env)
	})
}

// classifyHandoverCommand fails a command without the list and an item
// whose transfer is missing or does not decode; a session left out of the
// list is recorded and never fails.
func TestClassifyHandoverCommand(t *testing.T) {
	item := func(id int64, n int, decodeErr string) gnb.HandoverCommandItem {
		return gnb.HandoverCommandItem{PDUSessionID: id, TransferLen: n, TransferErr: decodeErr}
	}
	list := func(items ...gnb.HandoverCommandItem) gnb.HandoverCommandInfo {
		return gnb.HandoverCommandInfo{ListPresent: true, Items: items}
	}
	tests := []struct {
		name         string
		cmd          gnb.HandoverCommandInfo
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "one session with a transfer", cmd: list(item(1, 1, "")), want: Pass,
			wantObserved: "HANDOVER COMMAND lists 1 PDU session(s), each with a Handover Command Transfer",
			wantDetail:   []string{"Handover List: PDU session 1: transfer of 1 octets", "strict-3gpp profile"}},
		{name: "two sessions with transfers", cmd: list(item(1, 1, ""), item(2, 7, "")), want: Pass,
			wantObserved: "HANDOVER COMMAND lists 2 PDU session(s), each with a Handover Command Transfer"},
		{name: "the UE's session not listed", cmd: list(item(2, 1, "")), want: Pass,
			wantDetail: []string{"the UE's PDU session 1 is not listed (not required; recorded only)"}},
		{name: "no list", cmd: gnb.HandoverCommandInfo{}, want: Fail,
			wantObserved: "HANDOVER COMMAND without a PDU Session Resource Handover List", wantDetail: []string{"Handover List: none"}},
		{name: "list without items", cmd: list(), want: Fail,
			wantObserved: "HANDOVER COMMAND without a PDU Session Resource Handover List"},
		{name: "empty transfer", cmd: list(item(1, 0, "")), want: Fail,
			wantObserved: "PDU session 1 listed without a Handover Command Transfer", wantDetail: []string{"PDU session 1: no transfer"}},
		{name: "transfer does not decode", cmd: list(item(1, 1, "bad bits")), want: Fail,
			wantObserved: "PDU session 1 listed with a Handover Command Transfer that does not decode",
			wantDetail:   []string{"PDU session 1: transfer of 1 octets does not decode (bad bits)"}},
		{name: "second item without a transfer", cmd: list(item(1, 1, ""), item(2, 0, "")), want: Fail,
			wantObserved: "PDU session 2 listed without a Handover Command Transfer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := classifyHandoverCommand(tc.cmd, 1)
			if v != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", v, observed, detail, tc.want)
			}
			if tc.wantObserved != "" && observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", observed, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
			if (v == Fail) != strings.HasPrefix(detail, handoverCommandFailure+" Recorded: ") {
				t.Errorf("detail %q: the failure message leads a FAIL and only a FAIL", detail)
			}
		})
	}
}

// The failure message is the issue's, word for word.
func TestHandoverCommandFailureMessage(t *testing.T) {
	const want = "HANDOVER COMMAND lacks the PDU Session Resource Handover List / per-item Handover Command Transfer. TS 38.413 8.4.1.2 (Rel-15 V15.5.0 onward), 9.3.4.10. Likely cause: the known SD-Core transfer-IE issue (docs/interop)."
	if handoverCommandFailure != want {
		t.Errorf("failure message = %q", handoverCommandFailure)
	}
}

// Without a UE the check SKIPs, as it does for a target release older than
// Rel-15; an env the harness refuses is an ERROR before anything is dialled.
func TestHandoverCommandCheckWithoutHarness(t *testing.T) {
	env := ueEnv("192.0.2.1:38412", "", "127.0.0.1")
	noUE := env
	noUE.UE = UESpec{}
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
		{name: "fixed bind port", env: fixed, want: Error, wantObserved: "harness setup failed", wantDetail: "fixes a port"},
		{name: "target release before Rel-15", env: old, want: Skip, wantObserved: "requires a newer release than the target",
			wantDetail: "requires Rel-15 (TS 38.413 8.4.1.2); target release is Rel-14"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runOne(context.Background(), ngapHandoverCommandTransfer{}, tc.env, time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved || !strings.Contains(r.Detail, tc.wantDetail) {
				t.Errorf("result %s / %q / %q, want %s / %q / containing %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
		})
	}
}

// The check's verdicts over the real-SCTP mock pair: a HANDOVER COMMAND
// with the list and a transfer per item PASSes; the list dropped, and an
// item whose transfer is empty or does not decode, FAIL; a handover that
// never reaches the command is an ERROR. The UE is deregistered from
// whichever gNB it ends on, and no association stays open.
func TestHandoverCommandCheckAgainstMockPair(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		answer       mockamf.HandoverAnswer
		want         Verdict
		wantObserved string
		wantDetail   []string
		completed    int64
	}{
		{name: "command with the list", net24: testnet.ConformanceHOCmdPass, want: Pass, completed: 1,
			wantObserved: "HANDOVER COMMAND lists 1 PDU session(s), each with a Handover Command Transfer",
			wantDetail:   []string{"Handover List: PDU session 1: transfer of 1 octets"}},
		{name: "list dropped", net24: testnet.ConformanceHOCmdNoList, answer: mockamf.CommandWithoutList, want: Fail, completed: 1,
			wantObserved: "HANDOVER COMMAND without a PDU Session Resource Handover List",
			wantDetail:   []string{handoverCommandFailure, "Handover List: none"}},
		{name: "transfer omitted", net24: testnet.ConformanceHOCmdNoTransfer, answer: mockamf.CommandWithoutTransfer, want: Fail, completed: 1,
			wantObserved: "PDU session 1 listed without a Handover Command Transfer",
			wantDetail:   []string{handoverCommandFailure, "PDU session 1: no transfer"}},
		{name: "transfer undecodable", net24: testnet.ConformanceHOCmdBadTransfe, answer: mockamf.CommandWithBadTransfer, want: Fail, completed: 1,
			wantObserved: "PDU session 1 listed with a Handover Command Transfer that does not decode",
			wantDetail:   []string{handoverCommandFailure, "PDU session 1: transfer of 1 octets does not decode"}},
		{name: "preparation failure", net24: testnet.ConformanceHOCmdPrepFail, answer: mockamf.FailHandoverPreparation, want: Error,
			wantObserved: "handover did not reach Handover Command",
			wantDetail:   []string{"no HANDOVER COMMAND within 1s of starting the handover", "await HandoverRequest"}},
		{name: "handover required unanswered", net24: testnet.ConformanceHOCmdUnanswered, answer: mockamf.IgnoreHandoverRequired, want: Error,
			wantObserved: "handover did not reach Handover Command"},
		{name: "preparation failure after the acknowledge", net24: testnet.ConformanceHOCmdFailAcked, answer: mockamf.FailHandoverAfterAcknowledge, want: Error,
			wantObserved: "HANDOVER PREPARATION FAILURE instead of HANDOVER COMMAND",
			wantDetail:   []string{"HANDOVER PREPARATION FAILURE, cause radio-network/", "no HANDOVER COMMAND to judge"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, mockupf.Options{}, testKi, mockamf.WithHandoverAnswer(tc.answer), mockamf.WithGUTI())
			env := ueEnv(c.front.Addr, "", c.n3)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			check := ngapHandoverCommandTransfer{}
			if tc.want == Error {
				check.wait = time.Second
			}
			r := check.Run(ctx, env)
			if r.Verdict != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", r.Verdict, r.Observed, r.Detail, tc.want)
			}
			if r.Observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", r.Observed, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, "did not complete after the command") {
				t.Errorf("detail %q: the handover did not complete", r.Detail)
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if got := c.amf.HandoversCompleted(); got != tc.completed {
				t.Errorf("mock AMF completed %d handovers, want %d", got, tc.completed)
			}
			if got := c.amf.Deregistrations(); got != 1 {
				t.Errorf("mock AMF saw %d deregistrations, want 1", got)
			}
			c.assertClosed(t, env)
		})
	}
}

// A registration the core refuses is an ERROR naming the step, with no
// association left open.
func TestHandoverCommandCheckRegistrationFailure(t *testing.T) {
	c := newMockCore(t, testnet.ConformanceHOCmdRegFail, mockupf.Options{}, "000102030405060708090a0b0c0d0e0f")
	env := ueEnv(c.front.Addr, "", c.n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := ngapHandoverCommandTransfer{}.Run(ctx, env)
	if r.Verdict != Error || r.Observed != "UE setup on the source gNB failed" || !strings.Contains(r.Detail, "registration on gNB 0x100") {
		t.Errorf("result %s / %q / %q, want an ERROR naming the registration step", r.Verdict, r.Observed, r.Detail)
	}
	c.assertClosed(t, env)
}

// classifyHandoverCancel reports an acknowledged cancel as INFO and a
// missing acknowledge as INFO-DIFF, recording the target's release and what
// else the source read.
func TestClassifyHandoverCancel(t *testing.T) {
	tests := []struct {
		name         string
		res          engine.HandoverCancelResult
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "acknowledged", res: engine.HandoverCancelResult{Acknowledged: true, AckAfter: 12 * time.Millisecond, TargetReleased: true}, want: Info,
			wantObserved: "HANDOVER CANCEL ACKNOWLEDGE after 12ms",
			wantDetail:   []string{"the AMF released the UE context prepared at the target gNB (recorded only)"}},
		{name: "acknowledged after other PDUs, target not released", want: Info,
			res:        engine.HandoverCancelResult{Acknowledged: true, AckAfter: time.Second, Skipped: []string{"DownlinkNASTransport", "ErrorIndication"}},
			wantDetail: []string{"other PDUs on the source association while waiting: DownlinkNASTransport, ErrorIndication", "no UE Context Release Command at the target gNB"}},
		{name: "not acknowledged", res: engine.HandoverCancelResult{TargetReleased: true}, want: InfoDiff,
			wantObserved: "no HANDOVER CANCEL ACKNOWLEDGE within 5s", wantDetail: []string{handoverCancelText}},
		{name: "source association lost", res: engine.HandoverCancelResult{SourceLost: errReset}, want: InfoDiff,
			wantObserved: "association lost after the HANDOVER CANCEL", wantDetail: []string{"connection reset", "no UE Context Release Command at the target gNB"}},
		{name: "not acknowledged, an Error Indication instead", res: engine.HandoverCancelResult{Skipped: []string{"ErrorIndication"}}, want: InfoDiff,
			wantDetail: []string{handoverCancelText, "other PDUs on the source association while waiting: ErrorIndication"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := classifyHandoverCancel(tc.res, 5*time.Second)
			if v != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", v, observed, detail, tc.want)
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

// The check is informational, on descriptive references alone, and SKIPs
// without a UE.
func TestHandoverCancelCheckIsInformational(t *testing.T) {
	c := ngapHandoverCancelAck{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if !IsInformational(c) || VerdictClass(c) != Info || WorstVerdict(c) != InfoDiff || MinRelease(c) != 15 {
		t.Errorf("informational %v, class %s, worst %s, min %s", IsInformational(c), VerdictClass(c), WorstVerdict(c), MinRelease(c))
	}
	for _, r := range c.SpecRefs() {
		if r.Strength != Descriptive {
			t.Errorf("%s %s is cited as %s", r.Spec, r.Clause, r.Strength)
		}
	}
	r := runOne(context.Background(), c, Env{}, time.Second)
	if r.Verdict != Skip || r.Observed != "no UE configured" {
		t.Errorf("without a UE: %s / %q, want SKIP", r.Verdict, r.Observed)
	}
	fixed := ueEnv("192.0.2.1:38412", "127.0.0.1:38999", "127.0.0.1")
	if r := runOne(context.Background(), c, fixed, time.Second); r.Verdict != Error || r.Observed != "harness setup failed" {
		t.Errorf("with a fixed bind port: %s / %q, want ERROR", r.Verdict, r.Observed)
	}
}

// The check's verdicts over the real-SCTP mock pair, through the registry's
// scoring: the acknowledge is INFO, a withheld one INFO-DIFF, neither of
// which gates, and a handover that is never prepared is an ERROR. A core
// that stops answering on the cancel did not survive: FAIL, which gates.
// The UE is deregistered from its source gNB every time the core is still
// there, and no association stays open.
func TestHandoverCancelCheckAgainstMockPair(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		opts         []mockamf.Option
		want         Verdict
		wantObserved string
		wantDetail   []string
		cancels      int64
		dead         bool
	}{
		{name: "acknowledged", net24: testnet.ConformanceCancelAcked, want: Info, cancels: 1,
			wantDetail: []string{"the AMF released the UE context prepared at the target gNB (recorded only)", "a fresh NG Setup afterwards completes"}},
		{name: "acknowledge withheld", net24: testnet.ConformanceCancelNoAck, opts: []mockamf.Option{mockamf.WithNoHandoverCancelAcknowledge()},
			want: InfoDiff, cancels: 1, wantObserved: "no HANDOVER CANCEL ACKNOWLEDGE within 400ms",
			wantDetail: []string{handoverCancelText, "a fresh NG Setup afterwards completes"}},
		{name: "preparation failure", net24: testnet.ConformanceCancelNoPrep, opts: []mockamf.Option{mockamf.WithHandoverAnswer(mockamf.FailHandoverPreparation)},
			want: Error, wantObserved: "handover did not reach Handover Command", wantDetail: []string{"handover not prepared", "await HandoverRequest"}},
		{name: "stops answering", net24: testnet.ConformanceCancelCrash, opts: []mockamf.Option{mockamf.WithCrashOnHandoverCancel()},
			want: Fail, cancels: 1, dead: true, wantObserved: "AMF unresponsive after the message (association lost after the HANDOVER CANCEL)",
			wantDetail: []string{"the source association ended before a HANDOVER CANCEL ACKNOWLEDGE", "core did not survive the HANDOVER CANCEL: the AMF no longer completes a fresh NG Setup"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, mockupf.Options{}, testKi, append(tc.opts, mockamf.WithGUTI())...)
			env := ueEnv(c.front.Addr, "", c.n3)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := runOne(ctx, ngapHandoverCancelAck{prepWait: 500 * time.Millisecond, ackWait: 400 * time.Millisecond}, env, 20*time.Second)
			if r.Verdict != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", r.Verdict, r.Observed, r.Detail, tc.want)
			}
			if tc.wantObserved != "" && r.Observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", r.Observed, tc.wantObserved)
			}
			if tc.want == Info && !strings.HasPrefix(r.Observed, "HANDOVER CANCEL ACKNOWLEDGE after ") {
				t.Errorf("observed %q, want the acknowledge", r.Observed)
			}
			for _, w := range append(tc.wantDetail, informationalNote, "requires Rel-15 or later; TS 38.413 8.4.5.2") {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if gated, want := !Summarize([]Result{r}).OK(), tc.want == Fail || tc.want == Error; gated != want {
				t.Errorf("gated = %v for %s, want %v", gated, tc.want, want)
			}
			if got := c.amf.HandoverCancels(); got != tc.cancels {
				t.Errorf("mock AMF saw %d Handover Cancels, want %d", got, tc.cancels)
			}
			if got := c.amf.HandoversCompleted(); got != 0 {
				t.Errorf("mock AMF completed %d handovers, want 0", got)
			}
			if tc.dead {
				deadline := time.Now().Add(3 * time.Second)
				for c.front.Active() != 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if n := c.front.Active(); n != 0 {
					t.Errorf("%d gNB associations still open after the check", n)
				}
				return
			}
			if got := c.amf.Deregistrations(); got != 1 {
				t.Errorf("mock AMF saw %d deregistrations, want 1", got)
			}
			if n := c.amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d stray UE Context Release Completes", n)
			}
			c.assertClosed(t, env)
		})
	}
}

// A registration the core refuses is an ERROR naming the step, with no
// association left open.
func TestHandoverCancelCheckRegistrationFailure(t *testing.T) {
	c := newMockCore(t, testnet.ConformanceCancelRegFail, mockupf.Options{}, "000102030405060708090a0b0c0d0e0f")
	env := ueEnv(c.front.Addr, "", c.n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := ngapHandoverCancelAck{}.Run(ctx, env)
	if r.Verdict != Error || r.Observed != "UE setup on the source gNB failed" || !strings.Contains(r.Detail, "registration on gNB 0x100") {
		t.Errorf("result %s / %q / %q, want an ERROR naming the registration step", r.Verdict, r.Observed, r.Detail)
	}
	c.assertClosed(t, env)
}
