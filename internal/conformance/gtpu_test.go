package conformance

import (
	"context"
	"encoding/binary"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
	"github.com/bgrewell/orbit/internal/mockupf"
)

// fakeUPF is a UDP GTP-U peer that ignores the first skip Echo Requests,
// answers the next echoes and stays silent after, and sends an Error
// Indication for a G-PDU when errInd. seqOffset shifts the sequence number
// it answers with; delay holds each answer back that long; lateDup, when
// set, repeats its answer to the first Echo Request that long afterwards.
type fakeUPF struct {
	conn      *net.UDPConn
	skip      int32
	echoes    int32
	errInd    bool
	seqOffset uint16
	delay     time.Duration
	lateDup   time.Duration
	seen      atomic.Int32
}

func startFakeUPFWith(t *testing.T, f *fakeUPF) *fakeUPF {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f.conn = conn
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			switch mt, _ := gtpu.MessageType(buf[:n]); mt {
			case gtpu.MsgTypeEchoRequest:
				nth := f.seen.Add(1) - f.skip
				if nth < 1 || nth > f.echoes {
					continue
				}
				resp := append([]byte{}, buf[:gtpu.HeaderSizeN3]...)
				resp[1] = gtpu.MsgTypeEchoResponse
				resp = append(resp, 14, 0) // Recovery IE
				binary.BigEndian.PutUint16(resp[2:4], 6)
				binary.BigEndian.PutUint16(resp[8:10], binary.BigEndian.Uint16(resp[8:10])+f.seqOffset)
				if f.delay > 0 {
					time.AfterFunc(f.delay, func() { _, _ = conn.WriteToUDP(resp, from) })
					continue
				}
				_, _ = conn.WriteToUDP(resp, from)
				if nth == 1 && f.lateDup > 0 {
					time.AfterFunc(f.lateDup, func() { _, _ = conn.WriteToUDP(resp, from) })
				}
			case gtpu.MsgTypeGPDU:
				if f.errInd {
					ind := []byte{0x32, gtpu.MsgTypeErrorInd, 0, 9, 0, 0, 0, 0, 0, 0, 0, 0, 16, 0, 0, 0, 0}
					binary.BigEndian.PutUint32(ind[13:17], bogusN3TEID)
					_, _ = conn.WriteToUDP(ind, from)
				}
			}
		}
	}()
	return f
}

// The check confirms the UPF is alive with GTP-U Echo before and after the
// unknown-TEID G-PDU: no answer before is an ERROR (liveness cannot be
// confirmed), no answer after a silent G-PDU is a FAIL of its own, and only
// a UPF that answers both and sends nothing is "no Error Indication". An
// expected-failures entry for that text matches neither of the other two.
func TestGTPUUnknownTEIDLiveness(t *testing.T) {
	x, err := LoadExpectedFailures(filepath.Join("testdata", "expected-failures.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		echoes       int32
		errInd       bool
		want         Verdict
		wantObserved string
		wantListed   bool
		seqOffset    uint16
		lateDup      time.Duration
		perTest      time.Duration
		wantDetail   string
	}{
		{name: "answers both, discards", echoes: 2, want: Fail, wantObserved: "no Error Indication", wantListed: true},
		{name: "answers neither", echoes: 0, want: Error, wantObserved: ObservedUPFNoEcho},
		{name: "answers the first only", echoes: 1, want: Fail, wantObserved: ObservedUPFUnresponsive},
		{name: "sends the Error Indication", echoes: 2, errInd: true, want: Pass, wantObserved: "Error Indication (TEID-I echoes the probe)"},
		// A response must carry the probe's own sequence number.
		{name: "answers with the wrong sequence", echoes: 2, seqOffset: 100, want: Error, wantObserved: ObservedUPFNoEcho},
		// The first probe's answer, repeated late, arrives while the second
		// probe waits; it must not count as the second answer.
		{name: "late reply to the first probe", echoes: 1, lateDup: 300 * time.Millisecond, want: Fail, wantObserved: ObservedUPFUnresponsive},
		// The per-test timeout expiring during a probe is the harness's
		// doing: ERROR, never a crash FAIL, keeping what was seen before.
		// The reviewer's case: a live UPF that always answers, with a
		// per-test timeout shorter than the Error Indication wait, so the
		// second probe starts on an expired deadline.
		{name: "timeout cuts the after-probe short", echoes: 2, perTest: 150 * time.Millisecond, want: Error,
			wantObserved: ObservedProbeCutShort + " (no Error Indication)", wantDetail: "raise --per-test-timeout (this check needs about 600ms)"},
		{name: "timeout cuts the before-probe short", echoes: 0, perTest: 100 * time.Millisecond, want: Error,
			wantObserved: ObservedProbeCutShort + " (" + ObservedUPFNoEcho + ")"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upf := startFakeUPFWith(t, &fakeUPF{echoes: tc.echoes, errInd: tc.errInd, seqOffset: tc.seqOffset, lateDup: tc.lateDup})
			c := gtpuUnknownTEID{errWait: 200 * time.Millisecond, echoWait: 200 * time.Millisecond}
			perTest := 5 * time.Second
			if tc.perTest > 0 {
				perTest = tc.perTest
			}
			ctx, cancel := context.WithTimeout(context.Background(), perTest)
			defer cancel()
			res := c.Run(ctx, Env{UPFN3: upf.conn.LocalAddr().String(), N3Bind: "127.0.0.1:0"})
			if res.Verdict != tc.want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want %s %q with detail containing %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			res.ID = c.ID()
			results := []Result{res}
			x.Apply(results)
			if (results[0].ExpectedFailure != nil) != tc.wantListed {
				t.Errorf("tolerated by the fixture = %v, want %v", results[0].ExpectedFailure != nil, tc.wantListed)
			}
		})
	}
}

// GTPU-ECHO-RESPONSE against the stub UPF: one that answers Echo PASSes and
// one that withholds the Echo Response FAILs with the issue's message.
func TestGTPUEchoResponseAgainstMockUPF(t *testing.T) {
	tests := []struct {
		name         string
		opts         mockupf.Options
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "answers", want: Pass, wantObserved: "Echo Response (sequence number copied)",
			wantDetail: "sequence number 0x5A01, answering request 1 of 1 sent"},
		{name: "withholds the response", opts: mockupf.Options{NoEcho: true}, want: Fail, wantObserved: observedNoEchoResponse,
			wantDetail: "to 3 Echo Requests, each waited 100ms (absence inferred within this bound)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upf, err := mockupf.Listen("127.0.0.1:0", tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = upf.Close() })
			env := Env{UPFN3: upf.Addr().String(), N3Bind: "127.0.0.1:0"}
			res := runOne(context.Background(), gtpuEchoResponse{wait: 100 * time.Millisecond}, env, 5*time.Second)
			if res.Verdict != tc.want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want %s %q with detail containing %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			if (tc.want == Fail) != strings.Contains(res.Detail, echoFailText) {
				t.Errorf("detail %q: the failure message belongs on a FAIL only", res.Detail)
			}
			if !strings.Contains(res.Detail, "requires Rel-15 or later; TS 29.281 7.2.1 (Rel-8 onward") {
				t.Errorf("detail lacks the requirement line: %q", res.Detail)
			}
		})
	}
}

// The sequence number rule (TS 29.281 4.3.1) and the retries: a response
// must carry a sequence number the check sent, a request that goes
// unanswered is retried with the next number, and a late answer to an
// earlier request still counts.
func TestGTPUEchoResponseSequence(t *testing.T) {
	tests := []struct {
		name         string
		upf          *fakeUPF
		perTest      time.Duration
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "copies the sequence number", upf: &fakeUPF{echoes: 3}, want: Pass,
			wantObserved: "Echo Response (sequence number copied)", wantDetail: "sequence number 0x5A01"},
		{name: "answers with another sequence number", upf: &fakeUPF{echoes: 3, seqOffset: 100}, want: Fail,
			wantObserved: observedEchoSeqDiffers, wantDetail: "carries sequence number 0x5A65; the 1 request(s) sent carried 0x5A01 to 0x5A01; " + echoFailText},
		{name: "answers with sequence number zero", upf: &fakeUPF{echoes: 3, seqOffset: 0xA5FF}, want: Fail,
			wantObserved: observedEchoSeqDiffers, wantDetail: "carries sequence number 0x0000"},
		// The number of a request not yet sent was not copied from a request.
		{name: "answers with the next request's number", upf: &fakeUPF{echoes: 3, seqOffset: 1}, want: Fail,
			wantObserved: observedEchoSeqDiffers, wantDetail: "carries sequence number 0x5A02"},
		{name: "first request lost", upf: &fakeUPF{skip: 1, echoes: 1}, want: Pass,
			wantObserved: "Echo Response (sequence number copied)", wantDetail: "sequence number 0x5A02, answering request 2 of 2 sent"},
		{name: "only the last request answered", upf: &fakeUPF{skip: 2, echoes: 1}, want: Pass,
			wantObserved: "Echo Response (sequence number copied)", wantDetail: "sequence number 0x5A03, answering request 3 of 3 sent"},
		{name: "never answers", upf: &fakeUPF{}, want: Fail, wantObserved: observedNoEchoResponse, wantDetail: echoFailText},
		// The answer to request 1 arrives while the check waits on request
		// 2: it copies a sequence number that was sent.
		{name: "late answer to the first request", upf: &fakeUPF{echoes: 1, delay: 150 * time.Millisecond}, want: Pass,
			wantObserved: "Echo Response (sequence number copied)", wantDetail: "sequence number 0x5A01, answering request 1 of 2 sent"},
		// The per-test timeout expiring before every request was waited
		// out is the harness's doing: ERROR, not a missing response.
		{name: "timeout cuts the wait short", upf: &fakeUPF{}, perTest: 150 * time.Millisecond, want: Error,
			wantObserved: ObservedProbeCutShort + " (" + observedNoEchoResponse + ")", wantDetail: "raise --per-test-timeout (this check needs about 300ms)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upf := startFakeUPFWith(t, tc.upf)
			perTest := 5 * time.Second
			if tc.perTest > 0 {
				perTest = tc.perTest
			}
			ctx, cancel := context.WithTimeout(context.Background(), perTest)
			defer cancel()
			res := gtpuEchoResponse{wait: 100 * time.Millisecond}.Run(ctx, Env{UPFN3: upf.conn.LocalAddr().String(), N3Bind: "127.0.0.1:0"})
			if res.Verdict != tc.want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want %s %q with detail containing %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
		})
	}
}

// Without N3 params the check SKIPs, and an address that does not resolve
// or bind is an ERROR; none of them sends anything.
func TestGTPUEchoResponseWithoutUPF(t *testing.T) {
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantObserved string
	}{
		{name: "no N3 params", env: Env{}, want: Skip, wantObserved: "no N3 params (run from the UPF access network with --upf-n3 / --n3-bind)"},
		{name: "no N3 bind", env: Env{UPFN3: "127.0.0.1:2152"}, want: Skip, wantObserved: "no N3 params (run from the UPF access network with --upf-n3 / --n3-bind)"},
		{name: "bad N3 bind", env: Env{UPFN3: "127.0.0.1:2152", N3Bind: "not an address"}, want: Error, wantObserved: "bad N3 bind"},
		{name: "bad UPF N3", env: Env{UPFN3: "not an address", N3Bind: "127.0.0.1:0"}, want: Error, wantObserved: "bad UPF N3"},
		// 192.0.2.1 (TEST-NET-1) is not a local address.
		{name: "N3 bind not local", env: Env{UPFN3: "127.0.0.1:2152", N3Bind: "192.0.2.1:0"}, want: Error, wantObserved: "bind N3 socket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := gtpuEchoResponse{wait: 100 * time.Millisecond}.Run(context.Background(), tc.env)
			if res.Verdict != tc.want || res.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved)
			}
		})
	}
}

// A lab whose UPF has no Echo handling can list either FAIL: neither text
// is one of the liveness probes' own.
func TestGTPUEchoResponseFailsCanBeListed(t *testing.T) {
	for _, observed := range []string{observedNoEchoResponse, observedEchoSeqDiffers} {
		if hasPrefixAny(observed, livenessTexts) {
			t.Errorf("%q is a liveness text, which no expected-failures entry may name", observed)
		}
	}
}
