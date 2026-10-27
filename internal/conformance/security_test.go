package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/engine"
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
	// A ciphered header is refused, not read: past it is ciphertext.
	ciphered := smcPDU(t, nas.NIA2, nas.NEA0, normalUECaps.Buffer)
	ciphered[1] = nas.SecHdrIntegrityCiphered
	if _, err := parseSMC(ciphered); err == nil || !strings.Contains(err.Error(), "ciphered security header") {
		t.Errorf("parseSMC of a ciphered header: err = %v, want a refusal", err)
	}
}

// Each sub-result: NIA0, differing capabilities and a skipped command FAIL
// every UE; a reject or release passes the two non-conforming UEs but is
// ERROR for the normal one; an algorithm the UE did not offer is recorded
// only. A FAIL of the NIA0-only UE notes that the core's algorithm order
// decides which FAIL it gives, and a cleanup is recorded.
func TestJudgeSMC(t *testing.T) {
	tampered := append([]byte(nil), normalUECaps.Buffer...)
	tampered[0] ^= 0x01
	tests := []struct {
		name         string
		o            smcOutcome
		u            smcStimulus
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "normal: NIA2", o: smcOutcome{smc: smcPDU(t, nas.NIA2, nas.NEA0, normalUECaps.Buffer)}, want: Pass,
			wantObserved: "NIA2 selected, capabilities replayed", wantDetail: []string{"selected 128-NIA2 and NEA0"}},
		{name: "normal: NIA0", o: smcOutcome{smc: smcPDU(t, nas.NIA0, nas.NEA0, normalUECaps.Buffer)}, want: Fail,
			wantObserved: "NIA0 selected",
			wantDetail:   []string{"AMF selected NIA0 for a normal registration (UE offered NEA0/NEA1/NEA2, NIA0/NIA1/NIA2). TS 33.501 5.5.2."}},
		{name: "normal: capabilities differ", o: smcOutcome{smc: smcPDU(t, nas.NIA2, nas.NEA0, tampered)}, want: Fail,
			wantObserved: "replayed UE security capabilities differ",
			wantDetail:   []string{"Replayed UE security capabilities differ from those sent. TS 33.501 6.7.2 step 1b.", "Sent e0e0, replayed e1e0."}},
		{name: "normal: NIA0 and capabilities differ", o: smcOutcome{smc: smcPDU(t, nas.NIA0, nas.NEA0, tampered)}, want: Fail,
			wantObserved: "NIA0 selected", wantDetail: []string{"TS 33.501 5.5.2", "TS 33.501 6.7.2 step 1b"}},
		{name: "normal: skipped", o: smcOutcome{acceptFirst: true}, want: Fail, wantObserved: "Security Mode Command skipped",
			wantDetail: []string{"no Security Mode Command before the Registration Accept (UE offered NEA0/NEA1/NEA2, NIA0/NIA1/NIA2). TS 33.501 5.5.2. " + noEmergencyNote}},
		{name: "normal: skipped, plain Accept", o: smcOutcome{acceptFirst: true, plainAccept: true, cleanup: "x"}, want: Fail,
			wantObserved: "Security Mode Command skipped",
			wantDetail:   []string{"no Security Mode Command; registration proceeded without NAS security (UE offered", "Cleanup: x."}},
		{name: "normal: rejected", o: smcOutcome{reject: "Registration Reject"}, want: Error, wantObserved: "Registration Reject before the Security Mode Command"},
		{name: "normal: released", o: smcOutcome{released: true}, want: Error, wantObserved: "released before the Security Mode Command"},
		{name: "normal: nothing", o: smcOutcome{noAnswer: true}, want: Error, wantObserved: "no Security Mode Command, reject or release"},
		{name: "NIA0/NIA3: NIA3", o: smcOutcome{smc: smcPDU(t, nas.NIA3, nas.NEA0, nia0nia3Caps.Buffer)}, u: nia0nia3UE, want: Pass,
			wantObserved: "NIA3 selected, capabilities replayed"},
		{name: "NIA0/NIA3: NIA2, not offered", o: smcOutcome{smc: smcPDU(t, nas.NIA2, nas.NEA0, nia0nia3Caps.Buffer)}, u: nia0nia3UE, want: Pass,
			wantDetail: []string{"an algorithm the UE did not offer (recorded, not scored)"}},
		{name: "NIA0/NIA3: NIA0", o: smcOutcome{smc: smcPDU(t, nas.NIA0, nas.NEA0, nia0nia3Caps.Buffer)}, u: nia0nia3UE, want: Fail,
			wantObserved: "NIA0 selected", wantDetail: []string{"UE offered NEA0/NEA3, NIA0/NIA3"}},
		{name: "NIA0/NIA3: rejected", o: smcOutcome{reject: "Registration Reject"}, u: nia0nia3UE, want: Pass, wantObserved: "Registration Reject"},
		{name: "NIA0/NIA3: released", o: smcOutcome{released: true}, u: nia0nia3UE, want: Pass, wantObserved: "UE Context Release Command"},
		{name: "NIA0/NIA3: skipped", o: smcOutcome{acceptFirst: true}, u: nia0nia3UE, want: Fail, wantObserved: "Security Mode Command skipped"},
		{name: "NIA0-only: rejected", o: smcOutcome{reject: "Registration Reject"}, u: nia0OnlyUE, want: Pass, wantObserved: "Registration Reject",
			wantDetail: []string{"rejected the UE offering NEA0/NEA1/NEA2, NIA0"}},
		{name: "NIA0-only: released", o: smcOutcome{released: true}, u: nia0OnlyUE, want: Pass, wantObserved: "UE Context Release Command"},
		{name: "NIA0-only: NIA0", o: smcOutcome{smc: smcPDU(t, nas.NIA0, nas.NEA2, nia0OnlyCaps.Buffer)}, u: nia0OnlyUE, want: Fail,
			wantObserved: "NIA0 selected", wantDetail: []string{"AMF selected NIA0", noEmergencyNote, nia0OnlyOrderNote}},
		{name: "NIA0-only: plain Accept", o: smcOutcome{acceptFirst: true, plainAccept: true, cleanup: "plain switch-off Deregistration Request sent; the core released the UE context (UE Context Release Complete sent)"},
			u: nia0OnlyUE, want: Fail, wantObserved: "Security Mode Command skipped",
			wantDetail: []string{"no Security Mode Command; registration proceeded without NAS security (UE offered NEA0/NEA1/NEA2, NIA0). TS 33.501 5.5.2.",
				nia0OnlyOrderNote, "Cleanup: plain switch-off Deregistration Request sent; the core released the UE context (UE Context Release Complete sent)."}},
		{name: "NIA0-only: plain Accept that did not decode", o: smcOutcome{acceptFirst: true, plainAccept: true,
			noCleanup: fmt.Errorf("%w: bad", engine.ErrDownlinkNASDecode)},
			u: nia0OnlyUE, want: Fail, wantObserved: "Security Mode Command skipped",
			wantDetail: []string{"Cleanup: skipped, the plain Registration Accept did not decode: decode downlink NAS: bad."}},
		{name: "NIA0-only: attach ended before the cleanup", o: smcOutcome{acceptFirst: true, plainAccept: true, noCleanup: context.DeadlineExceeded},
			u: nia0OnlyUE, want: Fail, wantObserved: "Security Mode Command skipped",
			wantDetail: []string{"Cleanup: skipped, the attach ended before cleanup: context deadline exceeded."}},
		{name: "NIA0-only: NIA2, not offered", o: smcOutcome{smc: smcPDU(t, nas.NIA2, nas.NEA0, nia0OnlyCaps.Buffer)}, u: nia0OnlyUE, want: Pass,
			wantDetail: []string{"an algorithm the UE did not offer (recorded, not scored)"}},
		{name: "NIA0-only: nothing", o: smcOutcome{noAnswer: true}, u: nia0OnlyUE, want: Error, wantObserved: "no Security Mode Command, reject or release"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := judgeSMC(tc.o, tc.u)
			if s.v != tc.want || (tc.wantObserved != "" && s.observed != tc.wantObserved) {
				t.Errorf("= %s %q (%s), want %s %q", s.v, s.observed, s.detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(s.detail, w) {
					t.Errorf("detail %q lacks %q", s.detail, w)
				}
			}
			if tc.u != nia0OnlyUE && strings.Contains(s.detail, nia0OnlyOrderNote) {
				t.Errorf("detail %q carries the NIA0-only note", s.detail)
			}
		})
	}
}

// FAIL beats ERROR beats PASS, Observed names the first sub-result with the
// worst verdict, and Detail says when the two non-conforming UEs failed the
// same way.
func TestCombineSMC(t *testing.T) {
	sub := func(n string, v Verdict, obs string) subResult { return subResult{name: n, v: v, observed: obs} }
	const same = "failed the same way"
	tests := []struct {
		name         string
		subs         []subResult
		want         Verdict
		wantObserved string
		wantSame     bool
	}{
		{name: "all pass", subs: []subResult{sub("normal UE", Pass, "ok"), sub("NIA0/NIA3 UE", Pass, "ok"), sub("NIA0-only UE", Pass, "ok")},
			want: Pass, wantObserved: "normal UE: ok; NIA0/NIA3 UE: ok; NIA0-only UE: ok"},
		{name: "second fails", subs: []subResult{sub("normal UE", Pass, "ok"), sub("NIA0/NIA3 UE", Fail, "NIA0 selected"), sub("NIA0-only UE", Pass, "ok")},
			want: Fail, wantObserved: "NIA0/NIA3 UE: NIA0 selected"},
		{name: "only the last fails", subs: []subResult{sub("normal UE", Pass, "ok"), sub("NIA0/NIA3 UE", Pass, "ok"), sub("NIA0-only UE", Fail, "Security Mode Command skipped")},
			want: Fail, wantObserved: "NIA0-only UE: Security Mode Command skipped"},
		{name: "error then fail", subs: []subResult{sub("normal UE", Error, "x"), sub("NIA0/NIA3 UE", Fail, "y"), sub("NIA0-only UE", Pass, "ok")},
			want: Fail, wantObserved: "NIA0/NIA3 UE: y"},
		{name: "error, rest not run", subs: []subResult{sub("normal UE", Error, "x"), sub("NIA0/NIA3 UE", Skip, "not run"), sub("NIA0-only UE", Skip, "not run")},
			want: Error, wantObserved: "normal UE: x"},
		{name: "last two fail the same way", subs: []subResult{sub("normal UE", Pass, "ok"), sub("NIA0/NIA3 UE", Fail, "Security Mode Command skipped"), sub("NIA0-only UE", Fail, "Security Mode Command skipped")},
			want: Fail, wantObserved: "NIA0/NIA3 UE: Security Mode Command skipped", wantSame: true},
		{name: "last two fail differently", subs: []subResult{sub("normal UE", Pass, "ok"), sub("NIA0/NIA3 UE", Fail, "NIA0 selected"), sub("NIA0-only UE", Fail, "Security Mode Command skipped")},
			want: Fail, wantObserved: "NIA0/NIA3 UE: NIA0 selected"},
		{name: "normal and last fail the same way", subs: []subResult{sub("normal UE", Fail, "NIA0 selected"), sub("NIA0/NIA3 UE", Pass, "ok"), sub("NIA0-only UE", Fail, "NIA0 selected")},
			want: Fail, wantObserved: "normal UE: NIA0 selected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := combineSMC(tc.subs...)
			if v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q, want %s %q", v, observed, tc.want, tc.wantObserved)
			}
			for _, s := range tc.subs {
				if !strings.Contains(detail, s.name+": "+string(s.v)) {
					t.Errorf("detail %q lacks %s", detail, s.name)
				}
			}
			if strings.Contains(detail, same) != tc.wantSame {
				t.Errorf("detail %q: same-way note %v, want %v", detail, !tc.wantSame, tc.wantSame)
			}
		})
	}
}

// Every branch against the mock AMF over a real SCTP listener: the default
// mock PASSes (NIA2, NIA3, then a reject of the NIA0-only UE); without NIA3
// in its order it rejects or releases both non-conforming UEs (PASS), falls
// back to NIA0 (FAIL) or skips the command with a plain Accept (FAIL, the
// UE cleaned up); forcing NIA0, tampering the capabilities or skipping the
// command FAILs; an Authentication Reject or no answer is ERROR; a core that
// dies at the release is the crash FAIL; no UE SKIPs.
func TestSMCCheckAgainstMock(t *testing.T) {
	noNIA3 := mockamf.WithIntegrityOrder(nas.NIA2, nas.NIA1)
	skip := mockamf.WithNoCommonIntegrity(mockamf.SkipNoCommonIntegrity)
	const normalPass = "normal UE: NIA2 selected, capabilities replayed"
	const released = "the core released the UE context (UE Context Release Complete sent)"
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		dead         bool
	}{
		{name: "pass", want: Pass,
			wantObserved: normalPass + "; NIA0/NIA3 UE: NIA3 selected, capabilities replayed; NIA0-only UE: Registration Reject"},
		{name: "rejects both non-conforming UEs", opts: []mockamf.Option{noNIA3}, want: Pass,
			wantObserved: normalPass + "; NIA0/NIA3 UE: Registration Reject; NIA0-only UE: Registration Reject"},
		{name: "releases both non-conforming UEs", opts: []mockamf.Option{noNIA3, mockamf.WithNoCommonIntegrity(mockamf.ReleaseNoCommonIntegrity)}, want: Pass,
			wantObserved: normalPass + "; NIA0/NIA3 UE: UE Context Release Command; NIA0-only UE: UE Context Release Command"},
		{name: "NIA0 fallback", opts: []mockamf.Option{noNIA3, mockamf.WithNoCommonIntegrity(mockamf.NIA0NoCommonIntegrity)}, want: Fail,
			wantObserved: "NIA0/NIA3 UE: NIA0 selected",
			wantDetail:   []string{"The NIA0/NIA3 UE and the NIA0-only UE failed the same way (NIA0 selected)", nia0OnlyOrderNote}},
		{name: "plain Accept for both non-conforming UEs", opts: []mockamf.Option{noNIA3, skip, mockamf.WithGUTI()}, want: Fail,
			wantObserved: "NIA0/NIA3 UE: Security Mode Command skipped",
			wantDetail: []string{
				"no Security Mode Command; registration proceeded without NAS security (UE offered NEA0/NEA3, NIA0/NIA3)",
				"no Security Mode Command; registration proceeded without NAS security (UE offered NEA0/NEA1/NEA2, NIA0)",
				"The NIA0/NIA3 UE and the NIA0-only UE failed the same way (Security Mode Command skipped)", nia0OnlyOrderNote,
				"Cleanup: plain switch-off Deregistration Request sent; no UE Context Release Command within 500ms; UE Context Release Request sent; " + released + ".",
			}},
		{name: "plain Accept for the NIA0-only UE", opts: []mockamf.Option{skip, mockamf.WithReleaseOnDeregistration()}, want: Fail,
			wantObserved: "NIA0-only UE: Security Mode Command skipped",
			wantDetail:   []string{"NIA0/NIA3 UE: PASS NIA3 selected", "Cleanup: plain switch-off Deregistration Request sent; " + released + "; requires Rel-15"}},
		{name: "plain Accept, release request ignored", opts: []mockamf.Option{skip, mockamf.WithIgnoreUEContextReleaseRequest()}, want: Fail,
			wantObserved: "NIA0-only UE: Security Mode Command skipped",
			wantDetail:   []string{"UE Context Release Request sent; no UE Context Release Command within 500ms; the association is closed with the UE context still held; requires Rel-15"}},
		{name: "NIA0 always", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA0)}, want: Fail, wantObserved: "normal UE: NIA0 selected"},
		{name: "tampered capabilities", opts: []mockamf.Option{mockamf.WithSMCTamperCaps()}, want: Fail,
			wantObserved: "normal UE: replayed UE security capabilities differ"},
		{name: "skipped", opts: []mockamf.Option{mockamf.WithSkipSMC()}, want: Fail, wantObserved: "normal UE: Security Mode Command skipped",
			wantDetail: []string{"no Security Mode Command before the Registration Accept (UE offered NEA0/NEA1/NEA2, NIA0/NIA1/NIA2)"}},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "normal UE: Authentication Reject before the Security Mode Command",
			wantDetail:   []string{"NIA0/NIA3 UE: SKIP not run", "NIA0-only UE: SKIP not run"}},
		{name: "no answer", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()}, want: Error,
			wantObserved: "normal UE: no Security Mode Command, reject or release"},
		{name: "core dies at the release", dead: true, want: Fail,
			opts:         []mockamf.Option{noNIA3, mockamf.WithNoCommonIntegrity(mockamf.ReleaseNoCommonIntegrity), mockamf.WithCrashAfterRelease()},
			wantObserved: ObservedAMFUnresponsive + " (NIA0-only UE: UE setup failed)"},
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
			r := runOne(ctx, nasSMCNoNIA0{wait: 300 * time.Millisecond, cleanup: 500 * time.Millisecond}, env, 15*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
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
