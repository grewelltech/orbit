package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// classifySecCapAck passes only an Acknowledge that carries the stored
// capabilities; one without the IE, one that echoes what was sent and one
// with other capabilities fail, each with its own Observed.
func TestClassifySecCapAck(t *testing.T) {
	stored := gnb.NewUESecurityCapabilities(0xc000, 0x4000, 0, 0)
	sent := mismatchingCapabilities(stored)
	if gnb.SameUESecurityCapabilities(stored, sent) {
		t.Fatal("the mismatching capabilities equal the stored ones")
	}
	if got := describeCapabilities(sent); got != "NR encryption e000, NR integrity 6000, E-UTRA encryption 0000, E-UTRA integrity 0000" {
		t.Errorf("sent capabilities = %s, want algorithm 3 inverted in both NR bitmaps", got)
	}
	if got := describeCapabilities(stored); got != "NR encryption c000, NR integrity 4000, E-UTRA encryption 0000, E-UTRA integrity 0000" {
		t.Errorf("the stored capabilities were changed: %s", got)
	}
	tests := []struct {
		name         string
		got          *ngapType.UESecurityCapabilities
		want         Verdict
		wantObserved string
	}{
		{name: "stored", got: gnb.CloneUESecurityCapabilities(stored), want: Pass, wantObserved: secCapStored},
		{name: "absent", want: Fail, wantObserved: secCapAbsent},
		{name: "echoed", got: gnb.CloneUESecurityCapabilities(sent), want: Fail, wantObserved: secCapEchoed},
		{name: "others", got: gnb.NewUESecurityCapabilities(0x8000, 0x8000, 0, 0), want: Fail, wantObserved: secCapOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := classifySecCapAck(stored, sent, tc.got)
			if v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			if (v == Fail) != strings.Contains(detail, pathSwitchSecCapFailure) {
				t.Errorf("detail %q: the failure message goes with FAIL only", detail)
			}
		})
	}
}

// The check's verdicts over the real-SCTP mock pair, through the registry's
// scoring. An AMF that answers the mismatch with its stored capabilities
// PASSes; one that acknowledges without them, one that echoes the request's,
// one that answers with a failure and one that does not answer FAIL; one
// that stops answering is the crash FAIL. The request carried capabilities
// other than the ones the mock supplied, the UE is deregistered every time
// the core is still there, and no association stays open.
func TestPathSwitchSecCapCheckAgainstMockPair(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		opts         []mockamf.Option
		want         Verdict
		wantObserved string
		wantDetail   []string
		mismatches   int64
		switched     int
		dead         bool
	}{
		{name: "stored capabilities", net24: testnet.ConformanceSecCapStored, opts: []mockamf.Option{mockamf.WithStoredCapabilitiesInPathSwitchAck()},
			want: Pass, wantObserved: secCapStored, mismatches: 1, switched: 1,
			wantDetail: []string{"supplied by the AMF at registration: NR encryption 0000, NR integrity 4000", "sent in the PATH SWITCH REQUEST: NR encryption 2000, NR integrity 6000",
				"in the Acknowledge: NR encryption 0000, NR integrity 4000", "state left: the UE was deregistered"}},
		{name: "no capabilities", net24: testnet.ConformanceSecCapAbsent, want: Fail, wantObserved: secCapAbsent, mismatches: 1, switched: 1,
			wantDetail: []string{pathSwitchSecCapFailure, "in the Acknowledge: none"}},
		{name: "echoed capabilities", net24: testnet.ConformanceSecCapEchoed, opts: []mockamf.Option{mockamf.WithEchoedCapabilitiesInPathSwitchAck()},
			want: Fail, wantObserved: secCapEchoed, mismatches: 1, switched: 1,
			wantDetail: []string{pathSwitchSecCapFailure, "in the Acknowledge: NR encryption 2000, NR integrity 6000"}},
		{name: "path switch request failure", net24: testnet.ConformanceSecCapFailure, opts: []mockamf.Option{mockamf.WithPathSwitchAnswer(mockamf.FailPathSwitch)},
			want: Fail, wantObserved: secCapNoAck, wantDetail: []string{pathSwitchSecCapFailure, "PATH SWITCH REQUEST FAILURE"}},
		{name: "path switch request unanswered", net24: testnet.ConformanceSecCapIgnored, opts: []mockamf.Option{mockamf.WithPathSwitchAnswer(mockamf.IgnorePathSwitch)},
			want: Fail, wantObserved: secCapNoAck, wantDetail: []string{pathSwitchSecCapFailure, "no Acknowledge within 400ms"}},
		{name: "stops answering", net24: testnet.ConformanceSecCapCrash, opts: []mockamf.Option{mockamf.WithPathSwitchAnswer(mockamf.CrashOnPathSwitch)},
			want: Crash, wantObserved: ObservedAMFUnresponsive + " (" + secCapNoAck + ")", dead: true,
			wantDetail: []string{"the AMF stopped completing NG Setup after the PATH SWITCH REQUEST with mismatching UE security capabilities"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, mockupf.Options{}, testKi, append(tc.opts, mockamf.WithGUTI())...)
			env := ueEnv(c.front.Addr, "", c.n3)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			r := runOne(ctx, ngapPathSwitchSecCapMismatch{ackWait: 400 * time.Millisecond}, env, 15*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if got := c.amf.PathSwitchCapabilityMismatches(); got != tc.mismatches {
				t.Errorf("mock AMF counted %d capability mismatches, want %d", got, tc.mismatches)
			}
			if got := len(c.upf.Switches()); got != tc.switched {
				t.Errorf("stub UPF saw %d path switches, want %d", got, tc.switched)
			}
			if n := c.amf.EncodeErrors(); n != 0 {
				t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
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
			c.assertClosed(t, env)
		})
	}
}

// A registration the core refuses is an ERROR naming the step, no UE SKIPs,
// and the check is FAIL-class on its shall.
func TestPathSwitchSecCapCheckErrors(t *testing.T) {
	c := newMockCore(t, testnet.ConformanceSecCapRegFail, mockupf.Options{}, "000102030405060708090a0b0c0d0e0f")
	env := ueEnv(c.front.Addr, "", c.n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := ngapPathSwitchSecCapMismatch{}.Run(ctx, env)
	if r.Verdict != Error || r.Observed != "UE setup on the source gNB failed" || !strings.Contains(r.Detail, "registration on gNB 0x100") {
		t.Errorf("result %s / %q / %q, want an ERROR naming the registration step", r.Verdict, r.Observed, r.Detail)
	}
	c.assertClosed(t, env)

	check := ngapPathSwitchSecCapMismatch{}
	if r := runOne(context.Background(), check, Env{}, time.Second); r.Verdict != Skip || r.Observed != "no UE configured" {
		t.Errorf("without a UE: %s / %q, want SKIP", r.Verdict, r.Observed)
	}
	if err := ValidateTest(check); err != nil {
		t.Error(err)
	}
	if VerdictClass(check) != Fail || check.Budget() != 25*time.Second || NeedsOf(check).UEs != 1 {
		t.Errorf("class %s, budget %s, UEs %d", VerdictClass(check), check.Budget(), NeedsOf(check).UEs)
	}
}
