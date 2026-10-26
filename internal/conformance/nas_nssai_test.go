package conformance

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// stim is the stimulus S-NSSAI on the test env's slice (SST 1, SD 010203).
var stim = []byte{1, 0xee, 0xee, 0xee}

// rejectPDU is a plain Registration Reject with cause and the raw IE bytes.
func rejectPDU(cause uint8, ies ...byte) []byte {
	return append([]byte{0x7e, 0x00, 0x44, cause}, ies...)
}

// The stimulus is the run's SST with SD EEEEEE, or DDDDDD when the run's SD
// is EEEEEE.
func TestStimulusSNSSAI(t *testing.T) {
	tests := []struct {
		run  ue.PDUSessionParams
		want ue.SNSSAI
	}{
		{ue.PDUSessionParams{SST: 1, SD: "010203"}, ue.SNSSAI{SST: 1, SD: "eeeeee"}},
		{ue.PDUSessionParams{SST: 2}, ue.SNSSAI{SST: 2, SD: "eeeeee"}},
		{ue.PDUSessionParams{SST: 1, SD: "EEEEEE"}, ue.SNSSAI{SST: 1, SD: "dddddd"}},
		{ue.PDUSessionParams{SST: 1, SD: "eeeeee"}, ue.SNSSAI{SST: 1, SD: "dddddd"}},
	}
	for _, tc := range tests {
		if got := stimulusSNSSAI(tc.run); got != tc.want {
			t.Errorf("stimulusSNSSAI(%+v) = %+v, want %+v", tc.run, got, tc.want)
		}
	}
}

// rejectIEs walks type 1, TLV and TLV-E IEs and refuses a truncated IE or a
// message that is not a Registration Reject.
func TestRejectIEs(t *testing.T) {
	tests := []struct {
		name      string
		pdu       []byte
		wantCause uint8
		wantIEIs  []uint8
		wantErr   bool
	}{
		{name: "no IEs", pdu: rejectPDU(62), wantCause: 62},
		{name: "TLV, type 1 and TLV-E", pdu: rejectPDU(62, 0x69, 2, 0x11, 0x01, 0xa0, 0x78, 0x00, 0x01, 0xff), wantCause: 62,
			wantIEIs: []uint8{0x69, 0xa0, 0x78}},
		{name: "TLV truncated", pdu: rejectPDU(62, 0x69, 5, 0x11), wantErr: true},
		{name: "TLV-E truncated", pdu: rejectPDU(62, 0x78, 0x00), wantErr: true},
		{name: "not a Reject", pdu: []byte{0x7e, 0x00, 0x42, 0x01}, wantErr: true},
		{name: "too short", pdu: []byte{0x7e, 0x00}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cause, ies, err := rejectIEs(tc.pdu)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if cause != tc.wantCause || len(ies) != len(tc.wantIEIs) {
				t.Fatalf("= %d %+v, want %d %x", cause, ies, tc.wantCause, tc.wantIEIs)
			}
			for i, ie := range ies {
				if ie.iei != tc.wantIEIs[i] {
					t.Errorf("IE %d = %#02x, want %#02x", i, ie.iei, tc.wantIEIs[i])
				}
			}
		})
	}
}

// Rejected NSSAI elements are a [length | cause] octet, the SST and an
// optional SD; other lengths and truncation are errors.
func TestDecodeRejectedNSSAI(t *testing.T) {
	tests := []struct {
		name    string
		v       []byte
		want    []rejectedSNSSAI
		wantErr bool
	}{
		{name: "SST and SD", v: []byte{0x40, 1, 0xee, 0xee, 0xee}, want: []rejectedSNSSAI{{snssai: stim, cause: 0}}},
		{name: "SST only, cause 1", v: []byte{0x11, 2}, want: []rejectedSNSSAI{{snssai: []byte{2}, cause: 1}}},
		{name: "two", v: []byte{0x12, 3, 0x40, 1, 0xee, 0xee, 0xee}, want: []rejectedSNSSAI{{snssai: []byte{3}, cause: 2}, {snssai: stim}}},
		{name: "length 2", v: []byte{0x20, 1, 2}, wantErr: true},
		{name: "truncated", v: []byte{0x40, 1, 0xee}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeRejectedNSSAI(tc.v)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && !sameRejected(got, tc.want) {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

// Extended rejected NSSAI: the element count is coded minus one (unused
// values read as 8), type 001 carries a back-off timer octet, elements may
// carry mapped HPLMN octets, and reserved types, bad lengths and truncation
// are errors.
func TestDecodeExtendedRejectedNSSAI(t *testing.T) {
	tests := []struct {
		name    string
		v       []byte
		want    []rejectedSNSSAI
		wantErr bool
	}{
		{name: "one element, type 000", v: []byte{0x00, 0x40, 1, 0xee, 0xee, 0xee}, want: []rejectedSNSSAI{{snssai: stim}}},
		{name: "type 001 with back-off timer", v: []byte{0x10, 0x21, 0x43, 1, 0xee, 0xee, 0xee}, want: []rejectedSNSSAI{{snssai: stim, cause: 3}}},
		{name: "two elements, mapped octets", v: []byte{0x01, 0x20, 5, 6, 0x81, 1, 0xee, 0xee, 0xee, 9, 9, 9, 9}, want: []rejectedSNSSAI{{snssai: []byte{5}}, {snssai: stim, cause: 1}}},
		{name: "two partial lists", v: []byte{0x00, 0x10, 4, 0x10, 0x05, 0x40, 1, 0xee, 0xee, 0xee}, want: []rejectedSNSSAI{{snssai: []byte{4}}, {snssai: stim}}},
		{name: "reserved type", v: []byte{0x71}, wantErr: true},
		{name: "fewer elements than counted", v: []byte{0x01, 0x40, 1, 0xee, 0xee, 0xee}, wantErr: true},
		{name: "length 3", v: []byte{0x00, 0x30, 1, 2, 3}, wantErr: true},
		{name: "type 001 without its timer", v: []byte{0x10}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeExtendedRejectedNSSAI(tc.v)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && !sameRejected(got, tc.want) {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

func sameRejected(a, b []rejectedSNSSAI) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i].snssai, b[i].snssai) || a[i].cause != b[i].cause {
			return false
		}
	}
	return true
}

// Each verdict of a Registration Reject.
func TestJudgeNSSAIReject(t *testing.T) {
	rejected := []byte{0x69, 5, 0x40, 1, 0xee, 0xee, 0xee}
	tests := []struct {
		name         string
		pdu          []byte
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "#62 listing the stimulus", pdu: rejectPDU(62, rejected...), want: Pass,
			wantObserved: "Registration Reject #62, stimulus S-NSSAI in the Rejected NSSAI IE",
			wantDetail:   []string{"Rejected NSSAI [01eeeeee (cause 0)] (causes recorded, not asserted)", "from V18.1.0"}},
		{name: "#62 listing another S-NSSAI", pdu: rejectPDU(62, 0x69, 2, 0x10, 7), want: Fail,
			wantObserved: "Registration Reject #62 without the stimulus S-NSSAI in the Rejected NSSAI IE",
			wantDetail:   []string{"required from V16.6.0", "requested S-NSSAI not allowed but not reported in rejected NSSAI. TS 24.501 5.5.1.2.5"}},
		{name: "#62 bare", pdu: rejectPDU(62), want: Fail, wantObserved: "Registration Reject #62 without the stimulus S-NSSAI in the Rejected NSSAI IE"},
		{name: "#62 with a timer only", pdu: rejectPDU(62, 0x5f, 1, 0x21), want: Fail},
		{name: "#62, stimulus only in the Extended IE", pdu: rejectPDU(62, 0x68, 6, 0x00, 0x40, 1, 0xee, 0xee, 0xee), want: Fail,
			wantObserved: "Registration Reject #62, stimulus S-NSSAI only in the Extended rejected NSSAI IE",
			wantDetail:   []string{"the UE did not set ER-NSSAI", "from V17.1.0"}},
		{name: "#62 in both IEs", pdu: rejectPDU(62, append(append([]byte(nil), rejected...), 0x68, 6, 0x00, 0x40, 1, 0xee, 0xee, 0xee)...), want: Pass},
		{name: "#62, undecodable Extended IE", pdu: rejectPDU(62, 0x68, 1, 0x71), want: Error,
			wantObserved: "Registration Reject #62 with an IE Orbit cannot decode", wantDetail: []string{"IEI 0x68", ": 71"}},
		{name: "#62, unknown IE", pdu: rejectPDU(62, 0x2c, 2, 1, 2), want: Error,
			wantObserved: "Registration Reject #62 with an IE Orbit cannot decode", wantDetail: []string{"IEI 0x2c: 0102"}},
		{name: "#62, unknown IE but stimulus listed", pdu: rejectPDU(62, append([]byte{0x2c, 1, 1}, rejected...)...), want: Pass},
		{name: "#62, undecodable Rejected NSSAI IE", pdu: rejectPDU(62, 0x69, 3, 0x20, 1, 2), want: Error, wantDetail: []string{"IEI 0x69"}},
		{name: "#111", pdu: rejectPDU(111, rejected...), want: Fail, wantObserved: "Registration Reject, 5GMM cause #111",
			wantDetail: []string{"not #62 \"No network slices available\". TS 24.501 5.5.1.2.5 (from V16.2.0)"}},
		{name: "truncated", pdu: rejectPDU(62, 0x69, 9), want: Error, wantObserved: "Registration Reject did not decode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeNSSAIReject(tc.pdu, stim)
			if v != tc.want || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// The outcomes that are not a Reject: errors and the Accept rows.
func TestJudgeNSSAI(t *testing.T) {
	s := ue.SNSSAI{SST: 1, SD: "eeeeee"}
	tests := []struct {
		name         string
		o            nssaiOutcome
		want         Verdict
		wantObserved string
	}{
		{name: "setup failed", o: nssaiOutcome{err: errors.New("x")}, want: Error, wantObserved: "UE setup failed"},
		{name: "authentication reject", o: nssaiOutcome{authReject: true}, want: Error, wantObserved: "Authentication Reject"},
		{name: "reject failed integrity", o: nssaiOutcome{reject: rejectPDU(62), rejectErr: nas.ErrIntegrity}, want: Error,
			wantObserved: "Registration Reject failed integrity check"},
		{name: "reject the codec refused", o: nssaiOutcome{reject: rejectPDU(62, 0x69, 5, 0x40, 1, 0xee, 0xee, 0xee), rejectErr: errors.New("codec")}, want: Pass},
		{name: "accept that did not decode", o: nssaiOutcome{acceptErr: errors.New("x")}, want: Error, wantObserved: "Registration Accept did not decode"},
		{name: "released", o: nssaiOutcome{released: true}, want: Error, wantObserved: "released without a Registration Reject or Accept"},
		{name: "nothing", o: nssaiOutcome{noAnswer: true}, want: Error, wantObserved: "no Registration Reject or Accept"},
		{name: "no outcome", o: nssaiOutcome{}, want: Error, wantObserved: "no outcome"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeNSSAI(tc.o, s, time.Second)
			if v != tc.want || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
		})
	}
}

// Every branch against the mock AMF over a real SCTP listener. The UE
// requests only the stimulus S-NSSAI with the 5GMM capability's octets 3 to
// 5 zero, and an accepted UE is deregistered.
func TestNSSAICheckAgainstMock(t *testing.T) {
	nssai := func(a mockamf.NSSAIAnswer) []mockamf.Option {
		return []mockamf.Option{mockamf.WithUnservedNSSAI(a)}
	}
	const dereg = "UE deregistered"
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "#62 with Rejected NSSAI", opts: nssai(mockamf.Reject62NSSAI), want: Pass,
			wantObserved: "Registration Reject #62, stimulus S-NSSAI in the Rejected NSSAI IE",
			wantDetail:   []string{"Rejected NSSAI [01eeeeee (cause 0)]", "NSSAA and ER-NSSAI bits sent as 0 (not supported)", "b) 2)"}},
		{name: "#62 bare", opts: nssai(mockamf.Reject62BareNSSAI), want: Fail,
			wantObserved: "Registration Reject #62 without the stimulus S-NSSAI in the Rejected NSSAI IE", wantDetail: []string{"required from V16.6.0"}},
		{name: "#62 Extended only", opts: nssai(mockamf.Reject62ExtendedNSSAI), want: Fail,
			wantObserved: "Registration Reject #62, stimulus S-NSSAI only in the Extended rejected NSSAI IE"},
		{name: "#62 undecodable", opts: nssai(mockamf.Reject62UndecodableNSSAI), want: Error,
			wantObserved: "Registration Reject #62 with an IE Orbit cannot decode", wantDetail: []string{"IEI 0x68"}},
		{name: "#111", opts: nssai(mockamf.RejectProtocolErrorNSSAI), want: Fail, wantObserved: "Registration Reject, 5GMM cause #111"},
		{name: "default slice allowed", opts: nssai(mockamf.AcceptDefaultNSSAI), want: Skip,
			wantObserved: "core allowed default S-NSSAI(s); the #62 rule did not apply",
			wantDetail:   []string{"allowed NSSAI [01010203]", "rejected NSSAI in the Accept (a may, recorded only): [01eeeeee (cause 0)]", dereg}},
		{name: "stimulus allowed", opts: nssai(mockamf.AcceptRequestedNSSAI), want: Error,
			wantObserved: "stimulus S-NSSAI is allowed for this subscriber", wantDetail: []string{dereg}},
		{name: "default mock: no allowed NSSAI", want: Skip, wantObserved: "core allowed default S-NSSAI(s); the #62 rule did not apply",
			wantDetail: []string{"allowed NSSAI []", dereg}},
		{name: "no answer", opts: nssai(mockamf.IgnoreNSSAI), want: Error, wantObserved: "no Registration Reject or Accept"},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error, wantObserved: "Authentication Reject"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := auth.ParseHexKey("Ki", testKi)
			opc, _ := auth.ParseHexKey("OPc", testOPc)
			amf, err := mockamf.New(mockamf.Config{
				Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, append([]mockamf.Option{mockamf.WithGUTI()}, tc.opts...)...)
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
			r := runOne(ctx, nasRegRejectedNSSAI{wait: 500 * time.Millisecond}, env, 15*time.Second)
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
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
			if !tc.noUE {
				checkStimulusRequest(t, amf.RegistrationRequests())
			}
			deadline := time.Now().Add(3 * time.Second)
			for front.Active() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if front.Active() != 0 {
				t.Errorf("%d associations still open after the check", front.Active())
			}
		})
	}
}

// checkStimulusRequest checks the check's Registration Request (the first
// the mock saw; the liveness probe sends none): only the stimulus S-NSSAI
// requested, and the 5GMM capability's octets 3 to 5 sent, all zero.
func checkStimulusRequest(t *testing.T, reqs [][]byte) {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("the mock saw no Registration Request")
	}
	m, err := nas.DecodePlain(reqs[0])
	if err != nil {
		t.Fatal(err)
	}
	rr := m.RegistrationRequest
	if rr.RequestedNSSAI == nil || !bytes.Equal(rr.RequestedNSSAI.Buffer[:rr.RequestedNSSAI.GetLen()], append([]byte{4}, stim...)) {
		t.Errorf("requested NSSAI = %+v, want only %x", rr.RequestedNSSAI, stim)
	}
	if c := rr.Capability5GMM; c == nil || c.GetLen() != 3 || !bytes.Equal(c.Octet[:3], []byte{0, 0, 0}) {
		t.Errorf("5GMM capability = %+v, want octets 3 to 5 zero", c)
	}
}
