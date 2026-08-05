package engine

import (
	"sort"
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

	// Flows are the UEs actually carrying traffic, cumulative. Unbounded here
	// — the wire bounds it, so the server can rank before truncating.
	Flows []FleetFlow
	// Cohorts is each app cohort's most recent interval quality.
	Cohorts []FleetCohort

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

// FleetFlow is one UE's traffic: what it is, where it goes, and how much has
// crossed. Cumulative rather than rated, for the same reason the run totals
// are — a rate belongs to an observation interval, and the interval belongs to
// whoever is watching (see fleetRates in the server).
type FleetFlow struct {
	SUPI    string
	Cohort  string // app-cohort label; empty for synthetic traffic
	App     string // "voip" | "http" | "video" | "udp"
	Peer    string
	GNB     string
	Started time.Time

	UplinkPackets, UplinkBytes     uint64
	DownlinkPackets, DownlinkBytes uint64
}

// FleetCohort is one app cohort's most recent interval quality, as the cohort
// sampler already computes it for the Prometheus gauges. Families that do not
// apply to the cohort's app stay nil rather than reporting zeros — a voip
// cohort has no time-to-first-byte, and 0 ms would read as an instant one.
type FleetCohort struct {
	Name    string
	App     string
	UEs     int
	Elapsed time.Duration

	MOS           *FleetQuantiles
	TTFBMs        *FleetQuantiles
	GoodputMbps   *FleetQuantiles
	StallTimeMs   *FleetQuantiles
	RebufferRatio *FleetQuantiles
	BitrateKbps   *FleetQuantiles
	StartupMs     *FleetQuantiles
}

// fleetSource is one counter source and what it is carrying. A UE contributes
// one source per data path it uses, so a UE running an app cohort and a UE
// blasting synthetic traffic are each one source, and a UE doing neither still
// registers (its session) so the totals cover ICMP probes and anything else
// that later rides the path.
type fleetSource struct {
	flow FleetFlow
	ctr  n3Counters // nil once retired

	// Frozen at retire: a Session whose data path has closed reports zeros.
	retired            bool
	ulPackets, ulBytes uint64
	dlPackets, dlBytes uint64
}

// totals reports the source's counters live, or its frozen values once retired.
func (s *fleetSource) totals() (ulP, ulB, dlP, dlB uint64) {
	if s.retired || s.ctr == nil {
		return s.ulPackets, s.ulBytes, s.dlPackets, s.dlBytes
	}
	return s.ctr.Totals()
}

// carriesTraffic reports whether this source is a flow worth listing. A bare
// attached session is not: listing every UE as a flow would bury the ones
// actually sending behind a population of idle rows.
func (s *fleetSource) carriesTraffic() bool { return s.flow.App != "" }

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
	sources        []*fleetSource
	bySUPI         map[string]*fleetSource // for upgrading a session to a cohort flow
	cohorts        map[string]*FleetCohort // latest interval quality, by cohort name

	upHist       *hdr.Histogram // nil until the first probe result
	upProbes     uint64
	upProbesLost uint64
}

// NewFleetLiveStats returns stats whose elapsed clock starts now.
func NewFleetLiveStats() *FleetLiveStats {
	return &FleetLiveStats{
		started: time.Now(),
		perGNB:  map[string]int{},
		bySUPI:  map[string]*fleetSource{},
		cohorts: map[string]*FleetCohort{},
	}
}

// AttachOK records one UE attached on the named gNB and registers it as a
// throughput source. An empty gnb leaves the UE out of the per-gNB spread
// (matching load's rule) but still counted and still sampled.
func (f *FleetLiveStats) AttachOK(gnb, supi string, src n3Counters) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached++
	if gnb != "" {
		f.perGNB[gnb]++
	}
	if src == nil {
		return
	}
	fs := &fleetSource{flow: FleetFlow{SUPI: supi, GNB: gnb}, ctr: src}
	f.sources = append(f.sources, fs)
	if supi != "" {
		f.bySUPI[supi] = fs
	}
}

// AddFlow registers a traffic-carrying source that is not a UE's session — the
// synthetic path writes through its own UEFlow handle.
func (f *FleetLiveStats) AddFlow(flow FleetFlow, src n3Counters) {
	if f == nil || src == nil {
		return
	}
	if flow.Started.IsZero() {
		flow.Started = time.Now()
	}
	f.mu.Lock()
	f.sources = append(f.sources, &fleetSource{flow: flow, ctr: src})
	f.mu.Unlock()
}

// MarkCohortFlow labels an already-attached UE as carrying app-cohort traffic.
// An app cohort rides the UE's own session data path, so this upgrades the
// existing source rather than adding a second one — adding one would count the
// same bytes twice.
func (f *FleetLiveStats) MarkCohortFlow(supi, cohort, app, peer string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fs := f.bySUPI[supi]
	if fs == nil {
		return
	}
	fs.flow.Cohort, fs.flow.App, fs.flow.Peer = cohort, app, peer
	if fs.flow.Started.IsZero() {
		fs.flow.Started = time.Now()
	}
}

// RecordCohort stores a cohort's most recent interval quality, replacing the
// previous one — the wire carries the latest sample, and a consumer that wants
// a series keeps its own history from the frames.
func (f *FleetLiveStats) RecordCohort(c FleetCohort) {
	if f == nil || c.Name == "" {
		return
	}
	f.mu.Lock()
	cp := c
	f.cohorts[c.Name] = &cp
	f.mu.Unlock()
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
	// Freeze in place rather than dropping: the flow stays listed with the
	// traffic it carried, and its bytes stay in the totals.
	for _, fs := range f.sources {
		if fs.ctr == src && !fs.retired {
			fs.ulPackets, fs.ulBytes, fs.dlPackets, fs.dlBytes = fs.ctr.Totals()
			fs.retired = true
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
	for _, fs := range f.sources {
		ulp, ulb, dlp, dlb := fs.totals()
		s.UplinkPackets += ulp
		s.UplinkBytes += ulb
		s.DownlinkPackets += dlp
		s.DownlinkBytes += dlb
		if fs.carriesTraffic() {
			flow := fs.flow
			flow.UplinkPackets, flow.UplinkBytes = ulp, ulb
			flow.DownlinkPackets, flow.DownlinkBytes = dlp, dlb
			s.Flows = append(s.Flows, flow)
		}
	}
	for _, c := range f.cohorts {
		s.Cohorts = append(s.Cohorts, *c)
	}
	sort.Slice(s.Cohorts, func(i, j int) bool { return s.Cohorts[i].Name < s.Cohorts[j].Name })
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
