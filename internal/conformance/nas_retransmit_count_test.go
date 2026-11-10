package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Every branch of the verdict from constructed transmissions.
func TestJudgeRetransmitCounts(t *testing.T) {
	ok := func(counts ...uint32) []acceptTx {
		out := make([]acceptTx, len(counts))
		for i, c := range counts {
			out[i] = acceptTx{count: c, verified: true, raw: []byte{0x7e, 0x02, byte(c)}}
		}
		return out
	}
	identical := ok(1, 1)
	identical[1].raw = identical[0].raw
	sameCount := ok(1, 2, 2)
	sameCount[2].raw = []byte{0x7e, 0x02, 0xbb}
	unverified := append(ok(1, 2), acceptTx{candidates: [2]uint32{3, 259}, raw: []byte{0x7e, 0x02, 0xaa}})
	tests := []struct {
		name         string
		tx           []acceptTx
		reject       string
		want         Verdict
		wantObserved string
		wantDetail   []string
		notDetail    string
	}{
		{name: "reject", reject: "Registration Reject", want: Error, wantObserved: "Registration Reject before the Registration Accept"},
		{name: "no accept", want: Error, wantObserved: "registration did not reach a Registration Accept that verifies", wantDetail: []string{"within 10s"}},
		{name: "no retransmission", tx: ok(1), want: Skip, wantObserved: "no retransmission of Registration Accept within the bound",
			wantDetail: []string{"at NAS COUNT 1", "NAS-T3550-ACCEPT-RETRANSMIT"}},
		{name: "four higher", tx: ok(1, 2, 3, 4, 5), want: Pass,
			wantObserved: "4 retransmissions of Registration Accept, each at a higher NAS COUNT (COUNTs 1, 2, 3, 4, 5)"},
		{name: "gaps are fine", tx: ok(1, 4, 9), want: Pass, wantObserved: "2 retransmissions of Registration Accept, each at a higher NAS COUNT (COUNTs 1, 4, 9)"},
		{name: "wrap", tx: ok(255, 256), want: Pass, wantObserved: "1 retransmissions of Registration Accept, each at a higher NAS COUNT (COUNTs 255, 256)"},
		{name: "identical", tx: identical, want: Fail,
			wantObserved: "retransmission 1 of Registration Accept at NAS COUNT 1, the same as the transmission before it (COUNTs 1, 1)",
			wantDetail:   []string{retransmitCountFailure, "byte-identical"}},
		{name: "same COUNT, other bytes", tx: sameCount, want: Fail,
			wantObserved: "retransmission 2 of Registration Accept at NAS COUNT 2, the same as the transmission before it (COUNTs 1, 2, 2)",
			wantDetail:   []string{retransmitCountFailure}, notDetail: "byte-identical"},
		{name: "lower", tx: ok(5, 4), want: Fail,
			wantObserved: "retransmission 1 of Registration Accept at NAS COUNT 4, lower than the transmission before it (COUNTs 5, 4)"},
		{name: "unverifiable", tx: unverified, want: Error, wantObserved: "retransmission 2 of Registration Accept verifies at no candidate NAS COUNT",
			wantDetail: []string{"after transmissions at NAS COUNT 1, 2", "3 and 259", "NAS 7e02aa"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeRetransmitCounts(tc.tx, tc.reject, authRequestWait)
			if v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
			if tc.notDetail != "" && strings.Contains(detail, tc.notDetail) {
				t.Errorf("detail %q has %q", detail, tc.notDetail)
			}
		})
	}
}

// The failure message is the one the check's issue fixes, and the budget is
// the Accept wait plus four first-retransmission windows.
func TestRetransmitCountConstants(t *testing.T) {
	const want = "AMF retransmitted a protected NAS message without increasing the NAS COUNT. TS 24.501 4.4.3.1. Likely cause: the retransmission resends the stored PDU instead of re-protecting it."
	if retransmitCountFailure != want {
		t.Errorf("retransmitCountFailure = %q", retransmitCountFailure)
	}
	if got := (nasRetransmitNewCount{}).Budget(); got != 118*time.Second {
		t.Errorf("Budget = %v, want 118s", got)
	}
}

// Every verdict branch against the mock AMF over a real SCTP listener, with
// a 100 ms T3550: retransmissions protected afresh PASS, also across the
// sequence number's wrap and when fewer than four come; a byte-identical
// resend and a lower COUNT FAIL; a retransmission with a bad MAC is ERROR,
// as are a reject and no Accept; no retransmission, 5G-IA0 and no UE SKIP.
func TestRetransmitNewCountAgainstMock(t *testing.T) {
	const T, Tmax = 100 * time.Millisecond, 400 * time.Millisecond
	guti, t3550 := mockamf.WithGUTI(), mockamf.WithT3550(T, 4)
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "re-protected", opts: []mockamf.Option{guti, t3550}, want: Pass,
			wantObserved: "4 retransmissions of Registration Accept, each at a higher NAS COUNT (COUNTs 1, 2, 3, 4, 5)"},
		{name: "two retransmissions", opts: []mockamf.Option{guti, mockamf.WithT3550(T, 2)}, want: Pass,
			wantObserved: "2 retransmissions of Registration Accept, each at a higher NAS COUNT (COUNTs 1, 2, 3)"},
		{name: "sequence number wraps", opts: []mockamf.Option{guti, t3550, mockamf.WithAcceptDownlinkCount(255)}, want: Pass,
			wantObserved: "4 retransmissions of Registration Accept, each at a higher NAS COUNT (COUNTs 255, 256, 257, 258, 259)"},
		{name: "byte-identical", opts: []mockamf.Option{guti, t3550, mockamf.WithT3550Retransmission(mockamf.IdenticalT3550)}, want: Fail,
			wantObserved: "retransmission 1 of Registration Accept at NAS COUNT 1, the same as the transmission before it (COUNTs 1, 1)",
			wantDetail:   "The retransmission is byte-identical to the transmission before it."},
		{name: "lower COUNT", opts: []mockamf.Option{guti, t3550, mockamf.WithAcceptDownlinkCount(5), mockamf.WithT3550Retransmission(mockamf.LowerCountT3550)}, want: Fail,
			wantObserved: "retransmission 1 of Registration Accept at NAS COUNT 4, lower than the transmission before it (COUNTs 5, 4)",
			wantDetail:   retransmitCountFailure},
		{name: "bad MAC", opts: []mockamf.Option{guti, t3550, mockamf.WithT3550Retransmission(mockamf.BadMACT3550)}, want: Error,
			wantObserved: "retransmission 1 of Registration Accept verifies at no candidate NAS COUNT", wantDetail: "2 and 258"},
		{name: "no retransmission", opts: []mockamf.Option{guti}, want: Skip, wantObserved: "no retransmission of Registration Accept within the bound"},
		{name: "5G-IA0 selected", opts: []mockamf.Option{guti, t3550, mockamf.WithSMCIntegrity(nas.NIA0)}, want: Skip,
			wantObserved: "the core selected 5G-IA0; the UE harness refuses it, so no protected Registration Accept was seen"},
		{name: "no registration accept", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()}, want: Error,
			wantObserved: "registration did not reach a Registration Accept that verifies"},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "Authentication Reject before the Registration Accept"},
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
			r := runOne(ctx, nasRetransmitNewCount{t3550: T, tmax: Tmax, acceptWait: time.Second}, env, 15*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			if !strings.Contains(r.Detail, tc.wantDetail) {
				t.Errorf("detail %q lacks %q", r.Detail, tc.wantDetail)
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
			assertNoAssociations(t, front)
		})
	}
}
