package conformance

import (
	"bytes"
	"context"
	"errors"
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
	smc.SetNasKeySetIdentifiler(3)
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
	if err != nil || f.integrity != nas.NIA2 || f.ciphering != nas.NEA0 || string(f.replayed) != string(normalUECaps.Buffer) || f.ngKSI != 3 {
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
		{name: "plain Accept for both non-conforming UEs", opts: []mockamf.Option{noNIA3, skip, mockamf.WithGUTI(), mockamf.WithNoReleaseOnDeregistration()}, want: Fail,
			wantObserved: "NIA0/NIA3 UE: Security Mode Command skipped",
			wantDetail: []string{
				"no Security Mode Command; registration proceeded without NAS security (UE offered NEA0/NEA3, NIA0/NIA3)",
				"no Security Mode Command; registration proceeded without NAS security (UE offered NEA0/NEA1/NEA2, NIA0)",
				"The NIA0/NIA3 UE and the NIA0-only UE failed the same way (Security Mode Command skipped)", nia0OnlyOrderNote,
				"Cleanup: plain switch-off Deregistration Request sent; no UE Context Release Command within 500ms; UE Context Release Request sent; " + released + ".",
			}},
		{name: "plain Accept for the NIA0-only UE", opts: []mockamf.Option{skip}, want: Fail,
			wantObserved: "NIA0-only UE: Security Mode Command skipped",
			wantDetail:   []string{"NIA0/NIA3 UE: PASS NIA3 selected", "Cleanup: plain switch-off Deregistration Request sent; " + released + "; requires Rel-15"}},
		{name: "plain Accept, release request ignored", opts: []mockamf.Option{skip, mockamf.WithNoReleaseOnDeregistration(), mockamf.WithIgnoreUEContextReleaseRequest()}, want: Fail,
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
		{name: "core dies at the release", dead: true, want: Crash,
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

// testUplinkWaits are an uplink check's bounds scaled for the mock.
var testUplinkWaits = uplinkWaits{register: 3 * time.Second, answer: 1500 * time.Millisecond, quiet: 300 * time.Millisecond, settle: 30 * time.Millisecond, cleanup: 400 * time.Millisecond}

// uplinkMock starts a mock AMF behind a real SCTP listener for the checks
// that drive uplink NAS by hand: it assigns a 5G-GUTI, and with upf it
// answers PDU session requests.
func uplinkMock(t *testing.T, upf bool, opts ...mockamf.Option) (*mockamf.AMF, *sctpfront.Front) {
	t.Helper()
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	cfg := mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}
	if upf {
		cfg.UPF, cfg.UPFAddress = nopUPF{}, "127.0.0.1"
	}
	amf, err := mockamf.New(cfg, append([]mockamf.Option{mockamf.WithGUTI()}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return amf, sctpfront.Start(t, amf)
}

// uplinkCase is one row of an uplink check's table against the mock.
type uplinkCase struct {
	name         string
	opts         []mockamf.Option
	noUPF, noUE  bool
	want         Verdict
	wantObserved string
	wantDetail   []string
	// wantDereg and wantReleases are the Deregistration Requests and PDU
	// Session Release Requests the mock must have received.
	wantDereg, wantReleases int64
	// wantReplays is how many uplinks the mock must have seen repeat an
	// accepted NAS COUNT.
	wantReplays int64
	// wantBadMACs, when set, is how many uplinks the mock must have seen
	// fail the integrity check.
	wantBadMACs int64
}

// runUplinkCases runs check for each row against its own mock.
func runUplinkCases(t *testing.T, check Test, tests []uplinkCase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, front := uplinkMock(t, !tc.noUPF, tc.opts...)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := runOne(ctx, check, env, 15*time.Second)
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
			if got := amf.Deregistrations(); got != tc.wantDereg {
				t.Errorf("mock saw %d Deregistration Requests, want %d", got, tc.wantDereg)
			}
			if got := amf.PDUSessionReleaseRequests(); got != tc.wantReleases {
				t.Errorf("mock saw %d PDU Session Release Requests, want %d", got, tc.wantReleases)
			}
			if got := amf.BadMACUplinks(); tc.wantBadMACs != 0 && got != tc.wantBadMACs {
				t.Errorf("mock saw %d bad-MAC uplinks, want %d", got, tc.wantBadMACs)
			}
			if got := amf.ReplayedUplinks(); got != tc.wantReplays {
				t.Errorf("mock saw %d replayed uplinks, want %d", got, tc.wantReplays)
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
			assertNoAssociations(t, front)
		})
	}
}

const (
	cleanReleased = "Deregistration Request sent and the core released the UE context"
	cleanDropped  = "the association was dropped, and the core may keep a half-registered UE until its own timers expire"
	cleanGone     = "cleanup: the UE was already released, so the association was only dropped"
)

// Every verdict branch of the bad-MAC discard check against the mock AMF
// over a real SCTP listener: the conformant mock PASSes; one that processes
// the message, answers it with a 5GMM STATUS, or stops answering after it
// FAILs; a release after it, an unanswered first request and a failed
// registration are ERROR; 5G-IA0 and no UE SKIP. The UE is deregistered and
// its sessions released wherever it still can be.
func TestIntegrityFailDiscardAgainstMock(t *testing.T) {
	bad := func(b mockamf.UplinkAction) []mockamf.Option { return []mockamf.Option{mockamf.WithBadMACUplink(b)} }
	const failure = "AMF processed an uplink NAS message that failed integrity check. TS 24.501 4.4.4.3. Likely cause: AMF integrity check not enforced on this message type."
	runUplinkCases(t, nasIntegrityFailDiscard{waits: testUplinkWaits}, []uplinkCase{
		{name: "discarded", want: Pass, wantObserved: "bad-MAC request discarded; a valid request after it was answered",
			wantDetail: []string{"nothing answered the bad-MAC request within 300ms", "PDU Session Release Request sent for session 2, 4", cleanReleased}, wantDereg: 1, wantReleases: 2},
		{name: "processed", opts: bad(mockamf.ProcessUplink), want: Fail, wantObserved: "bad-MAC request answered: PDU Session Resource Setup Request",
			wantDetail: []string{failure, "session 2, 3", cleanReleased}, wantDereg: 1, wantReleases: 2},
		{name: "5GMM STATUS", opts: bad(mockamf.StatusOnUplink), want: Fail, wantObserved: "5GMM STATUS after the bad-MAC request",
			wantDetail: []string{failure, "A 5GMM STATUS is an answer", cleanReleased}, wantDereg: 1, wantReleases: 1},
		{name: "state changed", opts: bad(mockamf.MuteOnUplink), want: Fail, wantObserved: "valid request not answered after the bad-MAC request",
			wantDetail: []string{failure, "got none within 1.5s", "the message changed the AMF's state", "no UE Context Release Command within 400ms", cleanDropped}},
		{name: "released", opts: bad(mockamf.ReleaseOnUplink), want: Error, wantObserved: "UE released after the bad-MAC request; discard not confirmed",
			wantDetail: []string{"the AMF sent a UE Context Release Command", "neither the discard the clause requires nor an answer", cleanGone}},
		{name: "first request unanswered", noUPF: true, want: Error, wantObserved: "precondition failed: a valid PDU Session Establishment Request got no answer",
			wantDetail: []string{"within 1.5s", cleanReleased}, wantDereg: 1},
		{name: "5G-IA0 selected", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA0)}, want: Skip, wantObserved: "the core selected 5G-IA0",
			wantDetail: []string{"is not applicable when 5G-IA0 is used"}},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error, wantObserved: "UE setup failed"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	})
}

// Every verdict branch of the replay discard check against the mock AMF
// over a real SCTP listener: the conformant mock PASSes; one that processes
// the repeat, answers it with a 5GMM STATUS, or stops answering after it
// FAILs; a release after it and an unanswered first request are ERROR;
// 5G-IA0 and no UE SKIP. The mock counts exactly one repeated NAS COUNT.
func TestReplayDiscardAgainstMock(t *testing.T) {
	replay := func(act mockamf.UplinkAction) []mockamf.Option {
		return []mockamf.Option{mockamf.WithReplayedUplink(act)}
	}
	const failure = "AMF accepted a replayed NAS message (repeated NAS COUNT). TS 33.501 6.4.3.2. Likely cause: NAS COUNT replay check missing."
	runUplinkCases(t, nasReplayDiscard{waits: testUplinkWaits}, []uplinkCase{
		{name: "discarded", want: Pass, wantObserved: "replayed request discarded; a valid request after it was answered",
			wantDetail: []string{"nothing answered the replayed request within 300ms", "PDU Session Release Request sent for session 2, 3", cleanReleased}, wantDereg: 1, wantReleases: 2, wantReplays: 1},
		{name: "processed", opts: replay(mockamf.ProcessUplink), want: Fail, wantObserved: "replayed request answered: PDU Session Resource Setup Request",
			wantDetail: []string{failure, cleanReleased}, wantDereg: 1, wantReleases: 1, wantReplays: 1},
		{name: "5GMM STATUS", opts: replay(mockamf.StatusOnUplink), want: Fail, wantObserved: "5GMM STATUS after the replayed request",
			wantDetail: []string{failure, cleanReleased}, wantDereg: 1, wantReleases: 1, wantReplays: 1},
		{name: "state changed", opts: replay(mockamf.MuteOnUplink), want: Fail, wantObserved: "valid request not answered after the replayed request",
			wantDetail: []string{failure, "the message changed the AMF's state", cleanDropped}, wantReplays: 1},
		{name: "released", opts: replay(mockamf.ReleaseOnUplink), want: Error, wantObserved: "UE released after the replayed request; discard not confirmed",
			wantDetail: []string{cleanGone}, wantReplays: 1},
		{name: "first request unanswered", noUPF: true, want: Error, wantObserved: "precondition failed: a valid PDU Session Establishment Request got no answer",
			wantDetail: []string{cleanReleased}, wantDereg: 1},
		{name: "5G-IA0 selected", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA0)}, want: Skip, wantObserved: "the core selected 5G-IA0"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	})
}

// The replay check's texts name the replay, and its failure message cites
// the pinned clause.
func TestJudgeDiscardReplay(t *testing.T) {
	w := uplinkWaits{}.withDefaults()
	tests := []struct {
		name         string
		o            discardOutcome
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "answered again", o: discardOutcome{first: "a", reply: &smReply{name: "PDU Session Establishment Accept"}}, want: Fail,
			wantObserved: "replayed request answered: PDU Session Establishment Accept", wantDetail: "TS 33.501 6.4.3.2."},
		{name: "follow-up unanswered", o: discardOutcome{first: "a"}, want: Fail, wantObserved: "valid request not answered after the replayed request", wantDetail: "NAS COUNT replay check missing"},
		{name: "pass", o: discardOutcome{first: "a", followUp: "b"}, want: Pass, wantObserved: "replayed request discarded; a valid request after it was answered", wantDetail: "nothing answered the replayed request within 5s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeDiscard(tc.o, replayStimulus, w)
			if v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q (%s), want %s %q with %q", v, observed, detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
		})
	}
}

// Every verdict branch of the NAS COUNT check against the mock AMF over a
// real SCTP listener: the conformant mock PASSes, and so does one that
// checks the old sequence number at its original COUNT and then discards
// the repeat; a mock that stores the COUNT before the MAC check FAILs (C
// unanswered), and one that answers the re-sent A FAILs under 4.4.3.2; A or
// B unanswered and a release after the bad MAC are ERROR; 5G-IA0 and no UE
// SKIP. Where the security context still works the UE is deregistered;
// where it does not, the association is dropped.
func TestCountNotAdvancedAgainstMock(t *testing.T) {
	runUplinkCases(t, nasCountNotAdvanced{waits: testUplinkWaits}, []uplinkCase{
		{name: "count kept", want: Pass, wantObserved: "valid request answered after the re-sent one; the stored NAS COUNT was not advanced",
			wantDetail: []string{"A sent again got no answer within 300ms", "PDU Session Release Request sent for session 2, 3, 4", cleanReleased}, wantDereg: 1, wantReleases: 3, wantBadMACs: 1},
		{name: "old COUNT verified, then discarded", opts: []mockamf.Option{mockamf.WithLaxUplinkCountEstimate()}, want: Pass,
			wantObserved: "valid request answered after the re-sent one; the stored NAS COUNT was not advanced", wantDetail: []string{cleanReleased}, wantDereg: 1, wantReleases: 3, wantReplays: 1},
		{name: "count stored before the MAC check", opts: []mockamf.Option{mockamf.WithUplinkCountBeforeMAC()}, want: Fail, wantObserved: countFailObserved,
			wantDetail: []string{countFailure, "got none within 1.5s with the association up", "no UE Context Release Command within 400ms", cleanDropped}, wantBadMACs: 5},
		{name: "re-sent request answered", opts: []mockamf.Option{mockamf.WithLaxUplinkCountEstimate(), mockamf.WithReplayedUplink(mockamf.ProcessUplink)}, want: Fail,
			wantObserved: "re-sent request answered (PDU Session Resource Setup Request): an already accepted NAS COUNT was accepted again",
			wantDetail:   []string{"TS 24.501 4.4.3.2.", "The request sent after it was not judged.", cleanReleased}, wantDereg: 1, wantReleases: 2, wantReplays: 1},
		{name: "A unanswered", noUPF: true, want: Error, wantObserved: "precondition failed: PDU Session Establishment Request A got no answer",
			wantDetail: []string{cleanReleased}, wantDereg: 1},
		{name: "B unanswered", opts: []mockamf.Option{mockamf.WithPDUSessionAnswers(1)}, want: Error, wantObserved: "precondition failed: PDU Session Establishment Request B got no answer",
			wantDetail: []string{"PDU Session Release Request sent for session 2;", cleanReleased}, wantDereg: 1, wantReleases: 1},
		{name: "released after the bad MAC", opts: []mockamf.Option{mockamf.WithBadMACUplink(mockamf.ReleaseOnUplink)}, want: Error,
			wantObserved: "UE released after the re-sent request; the stored NAS COUNT could not be judged",
			wantDetail:   []string{"the AMF sent a UE Context Release Command", cleanGone}, wantBadMACs: 1},
		{name: "5G-IA0 selected", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA0)}, want: Skip, wantObserved: "the core selected 5G-IA0",
			wantDetail: []string{"Integrity verification", "5G-IA0"}},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	})
}

// Every branch of the NAS COUNT verdict from a constructed outcome.
func TestJudgeCount(t *testing.T) {
	w := uplinkWaits{}.withDefaults()
	both := [2]string{"a", "b"}
	tests := []struct {
		name         string
		o            countOutcome
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "harness failure", o: countOutcome{err: errors.New("write failed")}, want: Error, wantObserved: "harness failure"},
		{name: "released early", o: countOutcome{answers: [2]string{"a", ""}, goneEarly: "the AMF sent a UE Context Release Command"}, want: Error, wantObserved: "UE released before the re-sent request"},
		{name: "A unanswered", o: countOutcome{}, want: Error, wantObserved: "precondition failed: PDU Session Establishment Request A got no answer", wantDetail: []string{"within 10s"}},
		{name: "B unanswered", o: countOutcome{answers: [2]string{"a", ""}}, want: Error, wantObserved: "precondition failed: PDU Session Establishment Request B got no answer"},
		{name: "re-sent answered", o: countOutcome{answers: both, replayed: &smReply{name: "PDU Session Establishment Accept"}, third: "c"}, want: Fail,
			wantObserved: "re-sent request answered (PDU Session Establishment Accept): an already accepted NAS COUNT was accepted again", wantDetail: []string{"TS 24.501 4.4.3.2."}},
		{name: "C answered", o: countOutcome{answers: both, third: "c"}, want: Pass, wantObserved: "valid request answered after the re-sent one; the stored NAS COUNT was not advanced",
			wantDetail: []string{"within 5s", "C was answered (c)"}},
		{name: "C answered, then released", o: countOutcome{answers: both, third: "c", gone: "gone"}, want: Pass},
		{name: "released before C", o: countOutcome{answers: both, gone: "the association was lost (EOF)"}, want: Error,
			wantObserved: "UE released after the re-sent request; the stored NAS COUNT could not be judged", wantDetail: []string{"the association was lost (EOF)"}},
		{name: "C unanswered", o: countOutcome{answers: both}, want: Fail, wantObserved: countFailObserved, wantDetail: []string{countFailure, "got none within 10s"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeCount(tc.o, w)
			if v != tc.want || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(detail, want) {
					t.Errorf("detail %q lacks %q", detail, want)
				}
			}
		})
	}
}

// The failure message is the one the check's issue fixes.
func TestCountFailureMessage(t *testing.T) {
	const want = "AMF did not process a valid uplink NAS message sent after a MAC-failed message with a lower sequence number. TS 24.501 4.4.3.3. Likely cause: the stored uplink NAS COUNT is advanced before the integrity check passes."
	if countFailure != want {
		t.Errorf("countFailure = %q", countFailure)
	}
}

// Every verdict branch of the bad-MAC registration check against the mock
// AMF over a real SCTP listener: no answer and a release PASS; a
// Registration Accept, a Registration Reject, a Security Mode Command and a
// 5GMM STATUS with no authentication first FAIL; an Authentication Request
// SKIPs with the ambiguity named; a PDU session that is rejected or not
// answered, and a UE that does not register, are ERROR; no UE SKIPs. The
// session is released and the UE deregistered wherever it still is one.
func TestBadMACRegistrationAgainstMock(t *testing.T) {
	reg := func(b mockamf.BadMACRegistration) []mockamf.Option {
		return []mockamf.Option{mockamf.WithBadMACRegistration(b)}
	}
	const released = "PDU Session Release Request sent for session 2; " + cleanReleased
	runUplinkCases(t, nasBadMACRegistration{waits: testUplinkWaits}, []uplinkCase{
		{name: "discarded", want: Pass, wantObserved: "no answer to the bad-MAC REGISTRATION REQUEST",
			wantDetail: []string{"nothing answered it within 300ms", released}, wantDereg: 1, wantReleases: 1, wantBadMACs: 1},
		{name: "released", opts: []mockamf.Option{mockamf.WithBadMACUplink(mockamf.ReleaseOnUplink)}, want: Pass,
			wantObserved: "UE released after the bad-MAC REGISTRATION REQUEST; the message was not processed",
			wantDetail:   []string{"the AMF sent a UE Context Release Command", "both readings are satisfied", cleanGone}, wantBadMACs: 1},
		{name: "accepted without authentication", opts: reg(mockamf.AcceptBadMACRegistration), want: Fail,
			wantObserved: "REGISTRATION ACCEPT after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it",
			wantDetail:   []string{badMACRegFailure, "both readings of the clause forbid it", released}, wantDereg: 1, wantReleases: 1, wantBadMACs: 1},
		{name: "re-authenticated", opts: reg(mockamf.ReauthenticateBadMACRegistration), want: Skip, wantObserved: badMACRegReauth,
			wantDetail: []string{"the AMF answered with AUTHENTICATION REQUEST", "it is not graded", released}, wantDereg: 1, wantReleases: 1, wantBadMACs: 1},
		{name: "rejected", opts: reg(mockamf.RejectBadMACRegistration), want: Fail,
			wantObserved: "REGISTRATION REJECT after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it",
			wantDetail:   []string{badMACRegFailure, released}, wantDereg: 1, wantReleases: 1, wantBadMACs: 1},
		{name: "security mode command", opts: reg(mockamf.SMCBadMACRegistration), want: Fail,
			wantObserved: "SECURITY MODE COMMAND after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it",
			wantDetail:   []string{badMACRegFailure, released}, wantDereg: 1, wantReleases: 1, wantBadMACs: 1},
		{name: "5GMM STATUS", opts: []mockamf.Option{mockamf.WithBadMACUplink(mockamf.StatusOnUplink)}, want: Fail,
			wantObserved: "5GMM STATUS after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it",
			wantDetail:   []string{badMACRegFailure, released}, wantDereg: 1, wantReleases: 1, wantBadMACs: 1},
		{name: "PDU session rejected", opts: []mockamf.Option{mockamf.WithUnknownDNN("internet", mockamf.RejectDNN, 0)}, want: Error,
			wantObserved: "precondition failed: no PDU session established",
			wantDetail:   []string{"answered with PDU Session Establishment Reject", cleanReleased}, wantDereg: 1},
		{name: "PDU session unanswered", noUPF: true, want: Error, wantObserved: "precondition failed: no PDU session established",
			wantDetail: []string{"got no answer within 1.5s", cleanReleased}, wantDereg: 1},
		{name: "registration fails", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "precondition failed: the UE did not register"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	})
}

// The grading rule: FAIL only for an answer both readings of 4.4.4.3
// forbid, SKIP for one a reading allows, and other messages are not answers.
func TestClassifyBadMACRegAnswer(t *testing.T) {
	tests := []struct {
		mt       uint8
		want     badMACRegAnswer
		wantName string
	}{
		{f5nas.MsgTypeAuthenticationRequest, allowedByOneReading, "AUTHENTICATION REQUEST"},
		{f5nas.MsgTypeIdentityRequest, allowedByOneReading, "IDENTITY REQUEST"},
		{f5nas.MsgTypeRegistrationAccept, forbiddenByBoth, "REGISTRATION ACCEPT"},
		{f5nas.MsgTypeRegistrationReject, forbiddenByBoth, "REGISTRATION REJECT"},
		{f5nas.MsgTypeSecurityModeCommand, forbiddenByBoth, "SECURITY MODE COMMAND"},
		{f5nas.MsgTypeStatus5GMM, forbiddenByBoth, "5GMM STATUS"},
		{f5nas.MsgTypeAuthenticationReject, forbiddenByBoth, "AUTHENTICATION REJECT"},
		{f5nas.MsgTypeDeregistrationRequestUETerminatedDeregistration, forbiddenByBoth, "DEREGISTRATION REQUEST"},
		{f5nas.MsgTypeConfigurationUpdateCommand, unrelatedAnswer, "5GMM message type 0x54"},
		{f5nas.MsgTypeDLNASTransport, unrelatedAnswer, "5GMM message type 0x68"},
	}
	for _, tc := range tests {
		if got, name := classifyBadMACRegAnswer(tc.mt); got != tc.want || name != tc.wantName {
			t.Errorf("classifyBadMACRegAnswer(%#x) = %d %q, want %d %q", tc.mt, got, name, tc.want, tc.wantName)
		}
	}
}

// Every branch of the bad-MAC registration verdict from a constructed
// outcome; the first answer the rule classifies decides.
func TestJudgeBadMACRegistration(t *testing.T) {
	w := uplinkWaits{}.withDefaults()
	with := func(answers ...uint8) badMACRegOutcome { return badMACRegOutcome{session: "s", answers: answers} }
	undecoded := with(f5nas.MsgTypeConfigurationUpdateCommand)
	undecoded.undecoded = []string{"bad (NAS 7e02)"}
	authThenUndecoded := with(f5nas.MsgTypeAuthenticationRequest)
	authThenUndecoded.undecoded = []string{"bad (NAS 7e02)"}
	released := with()
	released.gone = "the association was lost (EOF)"
	acceptThenReleased := with(f5nas.MsgTypeRegistrationAccept)
	acceptThenReleased.gone = "the AMF sent a UE Context Release Command"
	tests := []struct {
		name         string
		o            badMACRegOutcome
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "harness failure", o: badMACRegOutcome{err: errors.New("no 5G-GUTI")}, want: Error, wantObserved: "harness failure", wantDetail: []string{"no 5G-GUTI"}},
		{name: "released early", o: badMACRegOutcome{goneEarly: "the AMF sent a UE Context Release Command"}, want: Error, wantObserved: "precondition failed: the UE was released before the stimulus"},
		{name: "session rejected", o: badMACRegOutcome{sessionAnswer: "PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN"}, want: Error,
			wantObserved: "precondition failed: no PDU session established", wantDetail: []string{"answered with PDU Session Establishment Reject"}},
		{name: "session unanswered", o: badMACRegOutcome{}, want: Error, wantObserved: "precondition failed: no PDU session established", wantDetail: []string{"within 10s"}},
		{name: "silence", o: with(), want: Pass, wantObserved: "no answer to the bad-MAC REGISTRATION REQUEST", wantDetail: []string{"within 5s"}},
		{name: "unrelated message only", o: with(f5nas.MsgTypeConfigurationUpdateCommand), want: Pass, wantObserved: "no answer to the bad-MAC REGISTRATION REQUEST",
			wantDetail: []string{"not answers to a registration: 5GMM message type 0x54"}},
		{name: "released", o: released, want: Pass, wantObserved: "UE released after the bad-MAC REGISTRATION REQUEST; the message was not processed", wantDetail: []string{"the association was lost (EOF)"}},
		{name: "authentication request", o: with(f5nas.MsgTypeAuthenticationRequest), want: Skip, wantObserved: badMACRegReauth, wantDetail: []string{"it is not graded"}},
		{name: "authentication, then accept", o: with(f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeRegistrationAccept), want: Skip, wantObserved: badMACRegReauth},
		{name: "authentication, then an undecodable PDU", o: authThenUndecoded, want: Skip, wantObserved: badMACRegReauth},
		{name: "identity request", o: with(f5nas.MsgTypeIdentityRequest), want: Skip, wantObserved: badMACRegIdentity},
		{name: "accept", o: with(f5nas.MsgTypeRegistrationAccept), want: Fail,
			wantObserved: "REGISTRATION ACCEPT after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it", wantDetail: []string{badMACRegFailure}},
		{name: "unrelated, then accept", o: with(f5nas.MsgTypeConfigurationUpdateCommand, f5nas.MsgTypeRegistrationAccept), want: Fail},
		{name: "accept, then released", o: acceptThenReleased, want: Fail},
		{name: "reject", o: with(f5nas.MsgTypeRegistrationReject), want: Fail, wantObserved: "REGISTRATION REJECT after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it"},
		{name: "status", o: with(f5nas.MsgTypeStatus5GMM), want: Fail},
		{name: "security mode command", o: with(f5nas.MsgTypeSecurityModeCommand), want: Fail},
		{name: "network deregistration", o: with(f5nas.MsgTypeDeregistrationRequestUETerminatedDeregistration), want: Fail},
		{name: "undecodable downlink", o: undecoded, want: Error, wantObserved: "a downlink NAS PDU after the stimulus did not decode",
			wantDetail: []string{"bad (NAS 7e02)", "5GMM message type 0x54"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeBadMACRegistration(tc.o, w)
			if v != tc.want || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(detail, want) {
					t.Errorf("detail %q lacks %q", detail, want)
				}
			}
		})
	}
}

// The texts are the ones the check's issue fixes.
func TestBadMACRegistrationTexts(t *testing.T) {
	const fail = "AMF processed a REGISTRATION REQUEST that failed the integrity check after secure exchange was established, without re-authenticating. TS 24.501 4.4.4.3. Likely cause: the AMF applies the integrity-failure exemption list after secure exchange."
	const skip = "AMF re-authenticated after a bad-MAC REGISTRATION REQUEST after secure exchange; TS 24.501 4.4.4.3 does not settle whether its message-specific rules apply after secure exchange"
	if badMACRegFailure != fail || badMACRegReauth != skip {
		t.Errorf("failure %q, skip %q", badMACRegFailure, badMACRegReauth)
	}
}

// Every branch of the discard verdict from a constructed outcome.
func TestJudgeDiscard(t *testing.T) {
	w := uplinkWaits{}.withDefaults()
	tests := []struct {
		name         string
		o            discardOutcome
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "harness failure", o: discardOutcome{err: errors.New("write failed")}, want: Error, wantObserved: "harness failure", wantDetail: []string{"write failed"}},
		{name: "released before", o: discardOutcome{goneFirst: "the AMF sent a UE Context Release Command"}, want: Error, wantObserved: "UE released before the bad-MAC request"},
		{name: "no first answer", o: discardOutcome{}, want: Error, wantObserved: "precondition failed: a valid PDU Session Establishment Request got no answer", wantDetail: []string{"within 10s"}},
		{name: "answered", o: discardOutcome{first: "a", reply: &smReply{name: "PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN"}}, want: Fail,
			wantObserved: "bad-MAC request answered: PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN", wantDetail: []string{"TS 24.501 4.4.4.3."}},
		{name: "answer wins over a release", o: discardOutcome{first: "a", reply: &smReply{name: "x"}, gone: "gone"}, want: Fail, wantObserved: "bad-MAC request answered: x"},
		{name: "status", o: discardOutcome{first: "a", status: true}, want: Fail, wantObserved: "5GMM STATUS after the bad-MAC request"},
		{name: "released after", o: discardOutcome{first: "a", gone: "the association was lost (EOF)"}, want: Error,
			wantObserved: "UE released after the bad-MAC request; discard not confirmed", wantDetail: []string{"the association was lost (EOF)"}},
		{name: "follow-up unanswered", o: discardOutcome{first: "a"}, want: Fail, wantObserved: "valid request not answered after the bad-MAC request", wantDetail: []string{"got none within 10s"}},
		{name: "pass", o: discardOutcome{first: "a", followUp: "b", undecoded: []string{"bad (NAS 7e02)"}}, want: Pass,
			wantObserved: "bad-MAC request discarded; a valid request after it was answered", wantDetail: []string{"within 5s", "1 downlink NAS PDU(s) after the bad-MAC request did not decode: bad (NAS 7e02)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeDiscard(tc.o, badMACStimulus, w)
			if v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(detail, want) {
					t.Errorf("detail %q lacks %q", detail, want)
				}
			}
		})
	}
}

// corruptMAC inverts octets 3 to 6 and nothing else, on a copy.
func TestCorruptMAC(t *testing.T) {
	tests := []struct{ in, want []byte }{
		{[]byte{0x7e, 0x02, 1, 2, 3, 4, 9, 0x7e}, []byte{0x7e, 0x02, 0xfe, 0xfd, 0xfc, 0xfb, 9, 0x7e}},
		{[]byte{0x7e, 0x02, 1}, []byte{0x7e, 0x02, 0xfe}},
		{nil, nil},
	}
	for _, tc := range tests {
		in := append([]byte(nil), tc.in...)
		if got := corruptMAC(in); !bytes.Equal(got, tc.want) || !bytes.Equal(in, tc.in) {
			t.Errorf("corruptMAC(%x) = %x (input now %x), want %x", tc.in, got, in, tc.want)
		}
	}
}
