package engine

import (
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/bgrewell/orbit/internal/load"
)

// FleetSnapshot is a point-in-time view of a fleet run's aggregates, copied
// out from under the lock so a caller can hold it freely.
//
// Byte and packet counters are cumulative over the run and come from the N3
// tunnels themselves, so they cover every behaviour riding the data path —
// synthetic loom flows and app cohorts alike — rather than only the traffic
// one subsystem happens to report.
type FleetSnapshot struct {
	Elapsed        time.Duration
	Attached       int
	AttachFailed   int
	Handovers      int
	HandoverErrors int
	TrafficFlows   int
	AppSessions    int

	UplinkPackets   uint64
	UplinkBytes     uint64
	DownlinkPackets uint64
	DownlinkBytes   uint64

	// PerGNB counts UEs currently attached on each gNB, keyed by the same
	// attribution label a load run uses.
	PerGNB map[string]int

	// UPLatency summarises ICMP round trips over the UEs' own N3 data paths.
	// HasUPLatency is false when no probe is configured, so a consumer can
	// distinguish "not measured" from "measured as zero".
	HasUPLatency bool
	UPLatency    load.Stats
	UPProbes     uint64
	UPProbesLost uint64
}

// n3Counters is the fleet's view of one UE's cumulative tunnel counters.
// *Session satisfies it; the indirection keeps FleetLiveStats testable without
// a live data path.
type n3Counters interface {
	Totals() (ulPackets, ulBytes, dlPackets, dlBytes uint64)
}

// FleetLiveStats accumulates a fleet run's aggregates while it is in flight,
// so RunTelemetry can report progress instead of making subscribers wait for
// the end-of-run FleetReport.
//
// Throughput is deliberately NOT accumulated here. Counting bytes as each
// behaviour reports them would miss everything still in flight and would
// double-count nothing but cost a write per packet; instead the run registers
// each attached UE as a counter source and Snapshot sums the tunnels' own
// atomics on demand. Sampling cost is one lock-free read per QoS flow.
//
// The zero value is not usable; call NewFleetLiveStats.
type FleetLiveStats struct {
	started time.Time

	mu             sync.Mutex
	attached       int
	attachFailed   int
	handovers      int
	handoverErrors int
	trafficFlows   int
	appSessions    int
	perGNB         map[string]int
	sources        []n3Counters

	// Counters of UEs that have left, folded in at the moment they leave.
	// A Session stops reporting once its data path closes, so a total summed
	// only from live sources would collapse to zero exactly when the run ends
	// — the run's final, most-quoted number.
	retiredULPackets, retiredULBytes uint64
	retiredDLPackets, retiredDLBytes uint64

	upHist       *hdr.Histogram // nil until the first probe result
	upProbes     uint64
	upProbesLost uint64
}

// NewFleetLiveStats returns stats whose elapsed clock starts now.
func NewFleetLiveStats() *FleetLiveStats {
	return &FleetLiveStats{started: time.Now(), perGNB: map[string]int{}}
}

// AttachOK records one UE attached on the named gNB and registers it as a
// throughput source. An empty gnb leaves the UE out of the per-gNB spread
// (matching load's rule) but still counted and still sampled.
func (f *FleetLiveStats) AttachOK(gnb string, src n3Counters) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached++
	if gnb != "" {
		f.perGNB[gnb]++
	}
	if src != nil {
		f.sources = append(f.sources, src)
	}
}

// AttachFailed records one UE that did not attach.
func (f *FleetLiveStats) AttachFailed() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.attachFailed++
	f.mu.Unlock()
}

// Handover records one mobility outcome.
func (f *FleetLiveStats) Handover(err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if err != nil {
		f.handoverErrors++
	} else {
		f.handovers++
	}
	f.mu.Unlock()
}

// MovedGNB re-attributes one UE from one gNB to another after a successful
// handover, keeping the per-gNB spread honest as the population moves.
func (f *FleetLiveStats) MovedGNB(from, to string) {
	if f == nil || from == to {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if from != "" && f.perGNB[from] > 0 {
		f.perGNB[from]--
	}
	if to != "" {
		f.perGNB[to]++
	}
}

// TrafficFlowStarted records one synthetic loom flow.
func (f *FleetLiveStats) TrafficFlowStarted() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.trafficFlows++
	f.mu.Unlock()
}

// AppSessions records how many UEs the app cohorts claimed.
func (f *FleetLiveStats) AppSessions(n int) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.appSessions += n
	f.mu.Unlock()
}

// Detached records one UE leaving the population (deregistration), dropping it
// from the per-gNB spread. Its counters stay in the cumulative totals: the
// bytes crossed the wire, and a total that fell as UEs drained would misreport
// the run.
//
// MUST be called BEFORE the caller tears the UE's data path down. src is read
// here and retired, because a Session whose data path has closed reports
// zeros — reading it afterwards would lose the whole UE's traffic.
func (f *FleetLiveStats) Detached(gnb string, src n3Counters) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attached > 0 {
		f.attached--
	}
	if gnb != "" && f.perGNB[gnb] > 0 {
		f.perGNB[gnb]--
	}
	if src == nil {
		return
	}
	ulp, ulb, dlp, dlb := src.Totals()
	f.retiredULPackets += ulp
	f.retiredULBytes += ulb
	f.retiredDLPackets += dlp
	f.retiredDLBytes += dlb
	for i, existing := range f.sources {
		if existing == src {
			f.sources = append(f.sources[:i], f.sources[i+1:]...)
			break
		}
	}
}

// RecordUPLatency records one user-plane round trip measured over a UE's N3
// data path. A lost probe advances the sent/lost counts without polluting the
// percentiles with a timeout that is not a latency.
func (f *FleetLiveStats) RecordUPLatency(rtt time.Duration, lost bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upProbes++
	if lost {
		f.upProbesLost++
		return
	}
	if f.upHist == nil {
		f.upHist = hdr.New(1, 300_000_000, 3) // 1µs … 300s, as the load histograms
	}
	v := rtt.Microseconds()
	if v < 1 {
		v = 1
	}
	_ = f.upHist.RecordValue(v)
}

// Snapshot copies the aggregates out, summing every registered UE's tunnel
// counters at the moment of the call.
func (f *FleetLiveStats) Snapshot() FleetSnapshot {
	if f == nil {
		return FleetSnapshot{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := FleetSnapshot{
		Elapsed:        time.Since(f.started),
		Attached:       f.attached,
		AttachFailed:   f.attachFailed,
		Handovers:      f.handovers,
		HandoverErrors: f.handoverErrors,
		TrafficFlows:   f.trafficFlows,
		AppSessions:    f.appSessions,
		PerGNB:         make(map[string]int, len(f.perGNB)),
	}
	for k, v := range f.perGNB {
		s.PerGNB[k] = v
	}
	if f.upProbes > 0 {
		s.HasUPLatency = true
		s.UPProbes, s.UPProbesLost = f.upProbes, f.upProbesLost
		if h := f.upHist; h != nil {
			s.UPLatency = load.Stats{
				Count: h.TotalCount(),
				P50:   time.Duration(h.ValueAtQuantile(50)) * time.Microsecond,
				P90:   time.Duration(h.ValueAtQuantile(90)) * time.Microsecond,
				P99:   time.Duration(h.ValueAtQuantile(99)) * time.Microsecond,
				P999:  time.Duration(h.ValueAtQuantile(99.9)) * time.Microsecond,
				Max:   time.Duration(h.Max()) * time.Microsecond,
			}
		}
	}
	s.UplinkPackets, s.UplinkBytes = f.retiredULPackets, f.retiredULBytes
	s.DownlinkPackets, s.DownlinkBytes = f.retiredDLPackets, f.retiredDLBytes
	for _, src := range f.sources {
		ulp, ulb, dlp, dlb := src.Totals()
		s.UplinkPackets += ulp
		s.UplinkBytes += ulb
		s.DownlinkPackets += dlp
		s.DownlinkBytes += dlb
	}
	return s
}

// Totals sums this session's N3 counters across its QoS flows, satisfying
// [n3Counters]. A session with no data path yet (app cohorts open theirs
// lazily) contributes zeros rather than being an error, so a fleet total is
// simply the traffic that has actually flowed.
func (s *Session) Totals() (ulPackets, ulBytes, dlPackets, dlBytes uint64) {
	s.dpMu.Lock()
	ue := s.ue
	s.dpMu.Unlock()
	if ue == nil {
		return 0, 0, 0, 0
	}
	return ue.Totals()
}

// AddSource registers an extra counter source that is not a Session — the
// fleet's synthetic traffic writes through a datapath.UEFlow, which a UE
// carrying no app cohort never backs with a session data path.
func (f *FleetLiveStats) AddSource(src n3Counters) {
	if f == nil || src == nil {
		return
	}
	f.mu.Lock()
	f.sources = append(f.sources, src)
	f.mu.Unlock()
}
