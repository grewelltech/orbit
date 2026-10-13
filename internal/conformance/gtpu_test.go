package conformance

import (
	"context"
	"encoding/binary"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// fakeUPF is a UDP GTP-U peer that answers the first echoes Echo Requests and
// stays silent after, and sends an Error Indication for a G-PDU when errInd.
// seqOffset shifts the sequence number it answers with; lateDup, when set,
// repeats its answer to the first Echo Request that long afterwards.
type fakeUPF struct {
	conn      *net.UDPConn
	echoes    int32
	errInd    bool
	seqOffset uint16
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
				nth := f.seen.Add(1)
				if nth > f.echoes {
					continue
				}
				resp := append([]byte{}, buf[:gtpu.HeaderSizeN3]...)
				resp[1] = gtpu.MsgTypeEchoResponse
				resp = append(resp, 14, 0) // Recovery IE
				binary.BigEndian.PutUint16(resp[2:4], 6)
				binary.BigEndian.PutUint16(resp[8:10], binary.BigEndian.Uint16(resp[8:10])+f.seqOffset)
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upf := startFakeUPFWith(t, &fakeUPF{echoes: tc.echoes, errInd: tc.errInd, seqOffset: tc.seqOffset, lateDup: tc.lateDup})
			c := gtpuUnknownTEID{errWait: 200 * time.Millisecond, echoWait: 200 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res := c.Run(ctx, Env{UPFN3: upf.conn.LocalAddr().String(), N3Bind: "127.0.0.1:0"})
			if res.Verdict != tc.want || res.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved)
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
