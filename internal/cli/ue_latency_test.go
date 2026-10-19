package cli

import (
	"bytes"
	"testing"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// The latency line counts replies over the probes sent, adds the probes that
// never left and the first send error only when some did not, and always
// ends with the elapsed time and effective rate; a stopped run says why on
// its own line unless that repeats the send error.
func TestPrintLatency(t *testing.T) {
	tests := []struct {
		name string
		m    *orbitv1.LatencyResponse
		want string
	}{
		{name: "every probe sent", m: &orbitv1.LatencyResponse{
			Sent: 20, Received: 19, Lost: 1, LossPct: 5, MinMs: 1.2, MeanMs: 1.5, MaxMs: 2.25, JitterMs: 0.3,
			ElapsedMs: 1260, RatePerS: 15.87,
		}, want: "19/20 replies (5% loss)  rtt min/mean/max 1.20/1.50/2.25 ms  jitter 0.30 ms  over 1.3 s, 15.9/s\n"},
		{name: "stopped when the UE was deregistered", m: &orbitv1.LatencyResponse{
			Sent: 2913, Received: 2912, Lost: 1, LossPct: 100.0 / 2913, MinMs: 2.1, MeanMs: 2.8, MaxMs: 9.4, JitterMs: 0.4,
			NotSent: 87, NotSentError: "UE 208930000000001 has no open N3 data path",
			Stopped: "UE 208930000000001 has no open N3 data path", ElapsedMs: 37400, RatePerS: 77.89,
		}, want: "2912/2913 replies (0% loss); 87 not sent: UE 208930000000001 has no open N3 data path" +
			"  rtt min/mean/max 2.10/2.80/9.40 ms  jitter 0.40 ms  over 37.4 s, 77.9/s\n"},
		{name: "stopped for another reason than the send error", m: &orbitv1.LatencyResponse{
			Sent: 5, Received: 5, NotSent: 995, Stopped: "context canceled", ElapsedMs: 60, RatePerS: 83.3,
		}, want: "5/5 replies (0% loss); 995 not sent  rtt min/mean/max 0.00/0.00/0.00 ms  jitter 0.00 ms  over 0.1 s, 83.3/s\n" +
			"stopped: context canceled\n"},
		{name: "transient send failure, not stopped", m: &orbitv1.LatencyResponse{
			Sent: 9, Received: 9, NotSent: 1, NotSentError: "sendto: no buffer space available", ElapsedMs: 500, RatePerS: 18,
		}, want: "9/9 replies (0% loss); 1 not sent: sendto: no buffer space available" +
			"  rtt min/mean/max 0.00/0.00/0.00 ms  jitter 0.00 ms  over 0.5 s, 18.0/s\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printLatency(&b, tc.m)
			if b.String() != tc.want {
				t.Errorf("printed\n%q\nwant\n%q", b.String(), tc.want)
			}
		})
	}
}
