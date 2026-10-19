package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// smcPDU builds a Security Mode Command NAS PDU as the AMF sends it:
// integrity-protected with a new context (a 7-octet header), not ciphered.
func smcPDU(t *testing.T, ia, ea uint8, replayed []byte) []byte {
	t.Helper()
	m := f5nas.NewMessage()
	m.GmmMessage = f5nas.NewGmmMessage()
	m.SecurityHeader = f5nas.SecurityHeader{ProtocolDiscriminator: nasMessage.Epd5GSMobilityManagementMessage, SecurityHeaderType: f5nas.SecurityHeaderTypePlainNas}
	m.GmmHeader.SetMessageType(f5nas.MsgTypeSecurityModeCommand)
	smc := nasMessage.NewSecurityModeCommand(f5nas.MsgTypeSecurityModeCommand)
	smc.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSMobilityManagementMessage)
	smc.SetMessageType(f5nas.MsgTypeSecurityModeCommand)
	smc.SelectedNASSecurityAlgorithms.SetTypeOfIntegrityProtectionAlgorithm(ia)
	smc.SelectedNASSecurityAlgorithms.SetTypeOfCipheringAlgorithm(ea)
	smc.ReplayedUESecurityCapabilities.SetLen(uint8(len(replayed)))
	copy(smc.ReplayedUESecurityCapabilities.Buffer, replayed)
	m.SecurityModeCommand = smc
	plain, err := m.PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{0x7e, nas.SecHdrIntegrityNewContext, 1, 2, 3, 4, 0}, plain...)
}

// parseSMC reads the selected algorithms and the replayed capabilities from
// the protected PDU without the key, and refuses anything else.
func TestParseSMC(t *testing.T) {
	f, err := parseSMC(smcPDU(t, nas.NIA2, nas.NEA0, normalUECaps.Buffer))
	if err != nil || f.integrity != nas.NIA2 || f.ciphering != nas.NEA0 || string(f.replayed) != string(normalUECaps.Buffer) {
		t.Errorf("parseSMC = %+v, %v", f, err)
	}
	if _, err := parseSMC([]byte{0x7e, 0x00, 0x56}); err == nil {
		t.Error("parseSMC accepted a truncated Authentication Request")
	}
}

// Each sub-result: NIA0, differing capabilities and a skipped command FAIL
// both UEs; a reject or release passes the NIA0/NIA3 UE but is ERROR for the
// normal one; an algorithm the UE did not offer is recorded only.
func TestJudgeSMC(t *testing.T) {
	tampered := append([]byte(nil), normalUECaps.Buffer...)
	tampered[0] ^= 0x01
	tests := []struct {
		name         string
		o            smcOutcome
		odd          bool
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "normal: NIA2", o: smcOutcome{smc: smcPDU(t, nas.NIA2, nas.NEA0, normalUECaps.Buffer)}, want: Pass,
			wantObserved: "NIA2 selected, capabilities replayed", wantDetail: []string{"selected 128-NIA2 and NEA0"}},
		{name: "normal: NIA0", o: smcOutcome{smc: smcPDU(t, nas.NIA0, nas.NEA0, normalUECaps.Buffer)}, want: Fail,
			wantObserved: "NIA0 selected",
			wantDetail:   []string{"AMF selected NIA0 / skipped the Security Mode Command for a normal registration (UE offered NEA0/NEA1/NEA2, NIA0/NIA1/NIA2). TS 33.501 5.5.2."}},
		{name: "normal: capabilities differ", o: smcOutcome{smc: smcPDU(t, nas.NIA2, nas.NEA0, tampered)}, want: Fail,
			wantObserved: "replayed UE security capabilities differ",
			wantDetail:   []string{"Replayed UE security capabilities differ from those sent. TS 33.501 6.7.2 step 1b.", "Sent e0e0, replayed e1e0."}},
		{name: "normal: NIA0 and capabilities differ", o: smcOutcome{smc: smcPDU(t, nas.NIA0, nas.NEA0, tampered)}, want: Fail,
			wantObserved: "NIA0 selected", wantDetail: []string{"TS 33.501 5.5.2", "TS 33.501 6.7.2 step 1b"}},
		{name: "normal: skipped", o: smcOutcome{acceptFirst: true}, want: Fail, wantObserved: "Security Mode Command skipped"},
		{name: "normal: rejected", o: smcOutcome{reject: "Registration Reject"}, want: Error, wantObserved: "Registration Reject before the Security Mode Command"},
		{name: "normal: released", o: smcOutcome{released: true}, want: Error, wantObserved: "released before the Security Mode Command"},
		{name: "normal: nothing", o: smcOutcome{noAnswer: true}, want: Error, wantObserved: "no Security Mode Command, reject or release"},
		{name: "NIA0/NIA3: NIA3", o: smcOutcome{smc: smcPDU(t, nas.NIA3, nas.NEA0, nia0nia3Caps.Buffer)}, odd: true, want: Pass,
			wantObserved: "NIA3 selected, capabilities replayed"},
		{name: "NIA0/NIA3: NIA2, not offered", o: smcOutcome{smc: smcPDU(t, nas.NIA2, nas.NEA0, nia0nia3Caps.Buffer)}, odd: true, want: Pass,
			wantDetail: []string{"an algorithm the UE did not offer (recorded, not scored)"}},
		{name: "NIA0/NIA3: NIA0", o: smcOutcome{smc: smcPDU(t, nas.NIA0, nas.NEA0, nia0nia3Caps.Buffer)}, odd: true, want: Fail,
			wantObserved: "NIA0 selected", wantDetail: []string{"UE offered NEA0/NEA3, NIA0/NIA3"}},
		{name: "NIA0/NIA3: rejected", o: smcOutcome{reject: "Registration Reject"}, odd: true, want: Pass, wantObserved: "Registration Reject"},
		{name: "NIA0/NIA3: released", o: smcOutcome{released: true}, odd: true, want: Pass, wantObserved: "UE Context Release Command"},
		{name: "NIA0/NIA3: skipped", o: smcOutcome{acceptFirst: true}, odd: true, want: Fail, wantObserved: "Security Mode Command skipped"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := normalUECaps
			if tc.odd {
				caps = nia0nia3Caps
			}
			s := judgeSMC(tc.o, caps, tc.odd)
			if s.v != tc.want || (tc.wantObserved != "" && s.observed != tc.wantObserved) {
				t.Errorf("= %s %q (%s), want %s %q", s.v, s.observed, s.detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(s.detail, w) {
					t.Errorf("detail %q lacks %q", s.detail, w)
				}
			}
		})
	}
}

// FAIL beats ERROR beats PASS, and Observed names the deciding sub-result.
func TestCombineSMC(t *testing.T) {
	pass := func(n string) subResult { return subResult{name: n, v: Pass, observed: "ok"} }
	tests := []struct {
		normal, odd  subResult
		want         Verdict
		wantObserved string
	}{
		{pass("normal UE"), pass("NIA0/NIA3 UE"), Pass, "normal UE: ok; NIA0/NIA3 UE: ok"},
		{pass("normal UE"), subResult{name: "NIA0/NIA3 UE", v: Fail, observed: "NIA0 selected"}, Fail, "NIA0/NIA3 UE: NIA0 selected"},
		{subResult{name: "normal UE", v: Error, observed: "x"}, subResult{name: "NIA0/NIA3 UE", v: Fail, observed: "y"}, Fail, "NIA0/NIA3 UE: y"},
		{subResult{name: "normal UE", v: Error, observed: "x"}, subResult{name: "NIA0/NIA3 UE", v: Skip, observed: "not run"}, Error, "normal UE: x"},
	}
	for _, tc := range tests {
		v, observed, detail := combineSMC(tc.normal, tc.odd)
		if v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.normal.name) || !strings.Contains(detail, tc.odd.name) {
			t.Errorf("= %s %q %q, want %s %q", v, observed, detail, tc.want, tc.wantObserved)
		}
	}
}

// Every branch against the mock AMF over a real SCTP listener: the default
// mock PASSes (NIA2, then NIA3); without NIA3 in its order it rejects or
// releases the NIA0/NIA3 UE (PASS) or falls back to NIA0 (FAIL); forcing
// NIA0, tampering the capabilities or skipping the command FAILs; an
// Authentication Reject or no answer is ERROR; a core that dies at the
// release is the crash FAIL; no UE SKIPs.
func TestSMCCheckAgainstMock(t *testing.T) {
	noNIA3 := mockamf.WithIntegrityOrder(nas.NIA2, nas.NIA1)
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		dead         bool
	}{
		{name: "pass", want: Pass,
			wantObserved: "normal UE: NIA2 selected, capabilities replayed; NIA0/NIA3 UE: NIA3 selected, capabilities replayed"},
		{name: "rejects the NIA0/NIA3 UE", opts: []mockamf.Option{noNIA3}, want: Pass,
			wantObserved: "normal UE: NIA2 selected, capabilities replayed; NIA0/NIA3 UE: Registration Reject"},
		{name: "releases the NIA0/NIA3 UE", opts: []mockamf.Option{noNIA3, mockamf.WithNoCommonIntegrity(mockamf.ReleaseNoCommonIntegrity)}, want: Pass,
			wantObserved: "normal UE: NIA2 selected, capabilities replayed; NIA0/NIA3 UE: UE Context Release Command"},
		{name: "NIA0 fallback", opts: []mockamf.Option{noNIA3, mockamf.WithNoCommonIntegrity(mockamf.NIA0NoCommonIntegrity)}, want: Fail,
			wantObserved: "NIA0/NIA3 UE: NIA0 selected"},
		{name: "NIA0 always", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA0)}, want: Fail, wantObserved: "normal UE: NIA0 selected"},
		{name: "tampered capabilities", opts: []mockamf.Option{mockamf.WithSMCTamperCaps()}, want: Fail,
			wantObserved: "normal UE: replayed UE security capabilities differ"},
		{name: "skipped", opts: []mockamf.Option{mockamf.WithSkipSMC()}, want: Fail, wantObserved: "normal UE: Security Mode Command skipped"},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "normal UE: Authentication Reject before the Security Mode Command"},
		{name: "no answer", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()}, want: Error,
			wantObserved: "normal UE: no Security Mode Command, reject or release"},
		{name: "core dies at the release", dead: true, want: Fail,
			opts:         []mockamf.Option{noNIA3, mockamf.WithNoCommonIntegrity(mockamf.ReleaseNoCommonIntegrity), mockamf.WithCrashAfterRelease()},
			wantObserved: ObservedAMFUnresponsive + " (normal UE: NIA2 selected, capabilities replayed; NIA0/NIA3 UE: UE Context Release Command)"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := auth.ParseHexKey("Ki", testKi)
			opc, _ := auth.ParseHexKey("OPc", testOPc)
			amf, err := mockamf.New(mockamf.Config{
				Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := runOne(ctx, nasSMCNoNIA0{wait: 300 * time.Millisecond}, env, 15*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			deadline := time.Now().Add(3 * time.Second)
			for front.Active() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !tc.dead && front.Active() != 0 {
				t.Errorf("%d associations still open after the check", front.Active())
			}
		})
	}
}
