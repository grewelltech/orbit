package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/load"
)

// Each procedure's line carries its sample count and P90 next to the other
// percentiles, and the --local printer (printLoadReport) and the server
// printer (printLoadReportProto) print the same lines for the same numbers,
// so the two outputs compare line by line.
func TestLoadReportLatencyLines(t *testing.T) {
	ms := func(f float64) time.Duration { return time.Duration(f * float64(time.Millisecond)) }
	type proc struct {
		name                      string
		n                         int64
		p50, p90, p99, p999, peak float64
	}
	tests := []struct {
		name  string
		procs []proc
		want  []string
	}{
		{name: "every procedure", procs: []proc{
			{"registration", 50, 12.3, 20.1, 31.4, 40.0, 41.2},
			{"pdu_session", 50, 5, 6, 7, 8, 9},
			{"attach", 50, 17.3, 26.1, 38.4, 48.0, 50.2},
		}, want: []string{
			"  registration  n 50     P50 12.3ms   P90 20.1ms   P99 31.4ms   P99.9 40ms     max 41.2ms\n",
			"  pdu_session   n 50     P50 5ms      P90 6ms      P99 7ms      P99.9 8ms      max 9ms\n",
			"  attach        n 50     P50 17.3ms   P90 26.1ms   P99 38.4ms   P99.9 48ms     max 50.2ms\n",
		}},
		{name: "one procedure, a large count", procs: []proc{{"registration", 1234567, 1, 2, 3, 4, 5}},
			want: []string{"  registration  n 1234567 P50 1ms      P90 2ms      P99 3ms      P99.9 4ms      max 5ms\n"}},
		{name: "no samples", procs: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := load.Report{Latencies: map[string]load.Stats{}}
			protoRep := &orbitv1.LoadReport{}
			for _, p := range tc.procs {
				rep.Latencies[p.name] = load.Stats{Count: p.n, P50: ms(p.p50), P90: ms(p.p90), P99: ms(p.p99), P999: ms(p.p999), Max: ms(p.peak)}
				protoRep.Latency = append(protoRep.Latency, &orbitv1.ProcedureLatency{Procedure: p.name, Count: uint64(p.n),
					P50Ms: p.p50, P90Ms: p.p90, P99Ms: p.p99, P999Ms: p.p999, MaxMs: p.peak})
			}
			var local, server bytes.Buffer
			printLoadReport(&local, rep)
			printLoadReportProto(&server, protoRep)
			got := latencyLines(local.String())
			if strings.Join(got, "") != strings.Join(tc.want, "") {
				t.Errorf("--local lines:\n%q\nwant:\n%q", got, tc.want)
			}
			if s := latencyLines(server.String()); strings.Join(s, "") != strings.Join(got, "") {
				t.Errorf("server lines differ from --local:\n%q\n%q", s, got)
			}
		})
	}
}

// latencyLines keeps a report's per-procedure lines.
func latencyLines(out string) []string {
	var lines []string
	for _, l := range strings.SplitAfter(out, "\n") {
		if strings.Contains(l, " P50 ") {
			lines = append(lines, l)
		}
	}
	return lines
}
