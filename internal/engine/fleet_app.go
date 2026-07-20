// Fleet app cohorts (design §8, Phase 7): subsets of an attached fleet run
// REAL loom application traffic (voip / http / video) concurrently for the
// behaviour window, with the far-end plumbing shared instead of multiplied:
//
//   - ONE control connection, ONE Capabilities/skew gate, and ONE TimeSync
//     loop per distinct N6 loomd across the whole fleet (fleetAgentPool,
//     refcounted) — 10 000 UEs against one loomd cost one grpc conn and one
//     ~10s-cadence sync loop feeding one shared owd.Tracker, not 10 000;
//   - ONE APP_SERVER Configure per (agent, app, param-set) where loom's
//     server model serves many clients: the http origin (and the video far
//     end, which IS the http origin) is net/http — any number of concurrent
//     cohort members share it. The voip answerer instead latches a single
//     (addr, SSRC) source (loom design §2.9), so voip cohorts get one
//     duration-bounded server per UE, and a cohort larger than its
//     port_min..port_max range is refused up front with an actionable error;
//   - per-gNB netstack reuse: TCP cohorts ride each gNB's ONE shared gVisor
//     Stack via Session.appNetwork exactly like single-UE app sessions — the
//     netstack budget stays O(gNBs), never O(UEs) (n3Pool.netstackCount is
//     the test hook asserting no per-UE stacks appear);
//   - RTCP sync storms are a non-issue by construction: loom's rtcp.Interval
//     implements the RFC 3550 §6.3/A.7 randomized-interval schedule, so a
//     thousand simultaneous calls decorrelate their reports on their own.
//
// Results are per-cohort DISTRIBUTIONS (p5/p50/p95 across members) in the
// FleetReport and as orbit_fleet_app_* Prometheus gauges labeled by cohort
// name — bounded cardinality (one short operator-chosen label per cohort,
// never a SUPI).
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/durationpb"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
	"github.com/bgrewell/loom/control"
	loomapp "github.com/bgrewell/loom/core/app"
	"github.com/bgrewell/loom/core/components"
	"github.com/bgrewell/loom/core/metrics"
	"github.com/bgrewell/loom/core/owd"
)

// FleetAppCohort configures one application-traffic cohort of a fleet run
// (design §8): Count members of the attached fleet each run one loom app
// client through their GTP-U data path against the loomd agent at Peer.
type FleetAppCohort struct {
	// Name labels the cohort in the report and on the orbit_fleet_app_*
	// metrics ("voip-calls"). Empty defaults to "<app>-<n>". Cohort names
	// must be unique — they are the aggregate key.
	Name string
	// App names the loom engine: "voip" (dgram over the shared tunnel —
	// fleet voice never pays the gVisor cost), "http" or "video" (the
	// per-gNB netstack).
	App string
	// Peer is the N6 loomd control-plane address ("host:port"); Token its
	// bearer token ("" = unauthenticated). Cohorts naming the same
	// (Peer, Token) share one control connection and one TimeSync loop.
	Peer  string
	Token string
	// PeerDataIP overrides the N6 media address when it differs from the
	// control host (the docs/USAGE.md firewall matrix).
	PeerDataIP string
	// Params are the app engine's knobs, passed verbatim to both ends —
	// the same grammar as `orbit ue app` (voip: codec, ptime, jb_ms,
	// port_min/port_max; http: object_size, think, tls, …; video: ladder,
	// seg_duration, …).
	Params map[string]string
	// Count is the cohort size. Members are carved from the tail of the
	// attached, non-mobile fleet; a cohort that no longer fits (attach
	// failures, other cohorts) is clamped, with a warning.
	Count int
}

// FleetQuantiles is a p5/p50/p95 distribution summary across cohort members.
type FleetQuantiles struct {
	P5, P50, P95 float64
}

// FleetAppCohortReport is one cohort's outcome: whole-run per-member QoE
// values summarised as distributions. Only the family matching the cohort's
// app is populated (MOS for voip; TTFB/goodput for http; stall time and
// rebuffer ratio for video).
type FleetAppCohortReport struct {
	Name string
	App  string
	// UEs counts members that ran a client; Failed the clients that ended
	// with an error (setup or run); Servers the far-end APP_SERVER flows
	// the cohort uses — one answerer per member for voip (the latch serves
	// exactly one source), 1 for an http/video cohort (the shared origin).
	UEs     int
	Failed  int
	Servers int
	// Distributions across members' whole-run values. TTFBMs summarises
	// each member's MEDIAN (p50) time-to-first-byte.
	MOS           *FleetQuantiles
	TTFBMs        *FleetQuantiles
	GoodputMbps   *FleetQuantiles
	StallTimeMs   *FleetQuantiles
	RebufferRatio *FleetQuantiles
	// Err is a cohort-level setup failure (agent unreachable, skew gate);
	// empty when the cohort ran.
	Err string
}

// fleetAppServerApp maps a cohort app to the engine placed at the far end:
// every app serves itself except video, whose far end is the http origin
// (loom's ABR player is client-only by design).
func fleetAppServerApp(app string) string {
	if app == "video" {
		return "http"
	}
	return app
}

// cohortName applies the default-name rule ("<app>-<n>", 1-based).
func cohortName(c FleetAppCohort, i int) string {
	if c.Name != "" {
		return c.Name
	}
	return fmt.Sprintf("%s-%d", c.App, i+1)
}

// validateFleetAppCohorts fails a fleet run fast — before any gNB dials or
// attaches — on a cohort spec that cannot run. The voip port-range check is
// the load-bearing one: each voip member needs its OWN far-end answerer (the
// latch serves exactly one source) binding one RTP port (rtcp-mux), so a
// cohort larger than port_min..port_max would strand its surplus members in
// per-UE "no free port" failures minutes into the run instead of an error
// now. The check is per-cohort AND aggregate: voip cohorts pointing at the
// SAME loomd with OVERLAPPING ranges draw from one shared port pool, so
// their combined demand is validated against the union of their ranges —
// two individually-fitting cohorts must not collectively exhaust it.
func validateFleetAppCohorts(cohorts []FleetAppCohort) error {
	type voipDemand struct {
		name     string
		min, max int // inclusive far-end port range
		count    int
	}
	voipByPeer := make(map[string][]voipDemand)
	seen := make(map[string]bool, len(cohorts))
	for i, c := range cohorts {
		name := cohortName(c, i)
		if seen[name] {
			return fmt.Errorf("fleet app cohort %q is declared twice; cohort names are the aggregate key and must be unique", name)
		}
		seen[name] = true
		switch c.App {
		case "voip", "http", "video":
		default:
			return fmt.Errorf("fleet app cohort %q: app %q is not supported (this build supports \"voip\", \"http\", \"video\")", name, c.App)
		}
		if c.Peer == "" {
			return fmt.Errorf("fleet app cohort %q: no peer loomd agent (set peer: host:port)", name)
		}
		if c.Count < 1 {
			return fmt.Errorf("fleet app cohort %q: count must be >= 1", name)
		}
		if err := refuseUnusableTLSCA(c.App, c.Params); err != nil {
			return fmt.Errorf("fleet app cohort %q: %w", name, err)
		}
		if c.App != "voip" {
			continue
		}
		portMin, portMax, err := cohortPortRange(c.Params)
		if err != nil {
			return fmt.Errorf("fleet app cohort %q: %w", name, err)
		}
		if portMin == 0 {
			continue // ephemeral far-end ports: no range to exhaust
		}
		if capacity := portMax - portMin + 1; c.Count > capacity {
			return fmt.Errorf("fleet app cohort %q (voip): %d UEs each need their own far-end answerer port "+
				"(the voip server latches a single source, one RTP port per call), but port_min..port_max "+
				"%d..%d holds only %d port(s); widen the range or shrink the cohort",
				name, c.Count, portMin, portMax, capacity)
		}
		voipByPeer[c.Peer] = append(voipByPeer[c.Peer], voipDemand{name: name, min: portMin, max: portMax, count: c.Count})
	}
	// Aggregate voip demand per loomd: cluster overlapping ranges (the union
	// of an overlapping cluster is one contiguous interval, since each range
	// merged in overlaps the running interval) and compare the cluster's
	// summed member count against the union's capacity.
	for peer, ds := range voipByPeer {
		if len(ds) < 2 {
			continue
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].min < ds[j].min })
		flush := func(names []string, lo, hi, total int) error {
			capacity := hi - lo + 1
			if len(names) < 2 || total <= capacity {
				return nil
			}
			return fmt.Errorf("fleet app cohorts %s (voip) all target loomd %s with overlapping port ranges: "+
				"together they need %d far-end answerer ports but the combined range %d..%d holds only %d; "+
				"use disjoint ranges, widen them, or shrink the cohorts",
				strings.Join(names, ", "), peer, total, lo, hi, capacity)
		}
		names := []string{ds[0].name}
		lo, hi, total := ds[0].min, ds[0].max, ds[0].count
		for _, d := range ds[1:] {
			if d.min <= hi { // shares ports with the running cluster
				names = append(names, d.name)
				if d.max > hi {
					hi = d.max
				}
				total += d.count
				continue
			}
			if err := flush(names, lo, hi, total); err != nil {
				return err
			}
			names, lo, hi, total = []string{d.name}, d.min, d.max, d.count
		}
		if err := flush(names, lo, hi, total); err != nil {
			return err
		}
	}
	return nil
}

// cohortPortRange parses port_min/port_max with loom's voip semantics
// (port_max defaults to port_min; port_max alone is refused).
func cohortPortRange(params map[string]string) (portMin, portMax int, err error) {
	if v, ok := params["port_min"]; ok {
		if portMin, err = strconv.Atoi(v); err != nil {
			return 0, 0, fmt.Errorf("port_min %q is not a number", v)
		}
	}
	portMax = portMin
	if v, ok := params["port_max"]; ok {
		if portMax, err = strconv.Atoi(v); err != nil {
			return 0, 0, fmt.Errorf("port_max %q is not a number", v)
		}
	}
	if portMin < 0 || portMax > 65535 || portMax < portMin {
		return 0, 0, fmt.Errorf("invalid port range %d..%d", portMin, portMax)
	}
	if portMax > 0 && portMin == 0 {
		return 0, 0, fmt.Errorf("port_max %d given without port_min", portMax)
	}
	return portMin, portMax, nil
}

// carveAppCohorts assigns cohort members from the TAIL of the attached fleet
// — the head carries mobility (ues[:mobile]) and the synthetic traffic flows,
// so the two behaviour populations never overlap. Returns the per-cohort
// member slices (index-aligned with cohorts) and the total UEs taken; a
// cohort that no longer fits the attached population is clamped with a
// warning (attach failures shrink the fleet, and a soak run beats a refusal).
func carveAppCohorts(ues []*fleetUE, mobile int, cohorts []FleetAppCohort, log *slog.Logger) ([][]*fleetUE, int) {
	avail := len(ues) - mobile
	if avail < 0 {
		avail = 0
	}
	members := make([][]*fleetUE, len(cohorts))
	total := 0
	for i, c := range cohorts {
		n := c.Count
		if n > avail-total {
			n = avail - total
			log.Warn("fleet app cohort clamped to the attached non-mobile population",
				"cohort", cohortName(c, i), "configured", c.Count, "running", n)
		}
		start := len(ues) - total - n
		members[i] = ues[start : start+n]
		total += n
	}
	return members, total
}

// fleetAgentPool shares the far-end control plumbing across the whole fleet:
// one dial, one skew gate, and one TimeSync loop per distinct (address,
// token) loomd, refcounted across the cohorts using it. The dial/sync
// counters exist for tests to pin the sharing invariant.
type fleetAgentPool struct {
	tun appTuning

	mu     sync.Mutex
	agents map[string]*fleetAgent

	dials     int // lifetime control dials
	syncLoops int // lifetime TimeSync loops started
	// configures counts lifetime APP_SERVER Configure calls. Atomic, not
	// under mu: per-UE voip Configure loops run while another cohort's
	// acquire may be holding mu across its (slow, possibly timing-out)
	// setup RPCs — the counter must not queue behind that stall.
	configures atomic.Int64
}

// fleetAgent is one shared loomd handle: its control client, capabilities,
// advertised version (for skew-gate wording), and the owd.Tracker its single
// TimeSync loop feeds — the OWD source for every client on this agent.
type fleetAgent struct {
	addr    string
	client  loomv1.ControlClient
	conn    io.Closer
	caps    *loomv1.CapabilitiesResponse
	version string
	tracker *owd.Tracker

	refs   int
	cancel context.CancelFunc
	done   chan struct{}
}

func newFleetAgentPool(tun appTuning) *fleetAgentPool {
	return &fleetAgentPool{tun: tun.withDefaults(), agents: make(map[string]*fleetAgent)}
}

// acquire returns the shared handle for (addr, token), dialing, fetching
// Capabilities, seeding the tracker (which also proves the token), and
// starting the ONE TimeSync loop on first use. Pair with release. The pool
// lock is held across the setup RPCs deliberately: two cohorts racing for
// one agent must never produce two dials. KNOWN TRADEOFF: p.mu is
// pool-wide, so cohorts targeting DIFFERENT agents serialize their setup
// behind each other — one unreachable agent delays the others' setup by its
// dial+RPC timeout budget. At today's cohort counts that is bounded and
// setup-only (the run itself never holds p.mu); a per-key entry lock would
// remove it if fleets grow many loomds.
func (p *fleetAgentPool) acquire(ctx context.Context, addr, token string) (*fleetAgent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := addr + "\x00" + token
	if a := p.agents[key]; a != nil {
		a.refs++
		return a, nil
	}
	client, conn, err := control.Dial(addr, control.WithToken(token))
	if err != nil {
		return nil, fmt.Errorf("dial loomd at %s: %w", addr, err)
	}
	p.dials++
	rpc1 := func(f func(rctx context.Context) error) error {
		rctx, rcancel := context.WithTimeout(ctx, p.tun.rpcTimeout)
		defer rcancel()
		return f(rctx)
	}
	a := &fleetAgent{
		addr:    addr,
		client:  client,
		conn:    conn,
		version: "unknown version",
		tracker: owd.NewTracker(p.tun.trackerWindow, p.tun.trackerN),
		refs:    1,
		done:    make(chan struct{}),
	}
	if err := rpc1(func(rctx context.Context) error {
		a.caps, err = client.Capabilities(rctx, &loomv1.CapabilitiesRequest{})
		return err
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("loomd at %s: Capabilities: %w", addr, err)
	}
	_ = rpc1(func(rctx context.Context) error {
		if h, herr := client.Health(rctx, &loomv1.HealthRequest{}); herr == nil && h.GetVersion() != "" {
			a.version = h.GetVersion()
		}
		return nil
	})
	if err := rpc1(func(rctx context.Context) error {
		smp, serr := control.Sync(rctx, client)
		if serr != nil {
			return serr
		}
		a.tracker.Feed(smp, time.Now())
		return nil
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("loomd at %s: TimeSync: %w", addr, err)
	}
	var lctx context.Context
	lctx, a.cancel = context.WithCancel(context.Background())
	p.syncLoops++
	go p.timesyncLoop(lctx, a)
	p.agents[key] = a
	return a, nil
}

// timesyncLoop is the agent's ONE four-timestamp exchange loop (management
// network, never the tunnel — design §5): a seeding burst, then every
// syncInterval, feeding the shared tracker until the last release cancels it.
func (p *fleetAgentPool) timesyncLoop(ctx context.Context, a *fleetAgent) {
	defer close(a.done)
	sync1 := func() {
		rctx, rcancel := context.WithTimeout(ctx, p.tun.rpcTimeout)
		defer rcancel()
		if smp, err := control.Sync(rctx, a.client); err == nil {
			a.tracker.Feed(smp, time.Now())
		}
	}
	for i := 0; i < p.tun.syncBurst && ctx.Err() == nil; i++ {
		sync1()
	}
	t := time.NewTicker(p.tun.syncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sync1()
		}
	}
}

// retain bumps an already-acquired handle's refcount (shared origins hold
// their agent until the post-run Stop).
func (p *fleetAgentPool) retain(a *fleetAgent) {
	p.mu.Lock()
	a.refs++
	p.mu.Unlock()
}

// release drops one reference; the last one stops the TimeSync loop and
// closes the control connection.
func (p *fleetAgentPool) release(a *fleetAgent) {
	if a == nil {
		return
	}
	p.mu.Lock()
	a.refs--
	last := a.refs == 0
	if last {
		// The map key embeds the token, which the handle does not retain;
		// sweep by identity instead of reconstructing the key.
		for k, v := range p.agents {
			if v == a {
				delete(p.agents, k)
			}
		}
	}
	p.mu.Unlock()
	if last {
		a.cancel()
		<-a.done
		_ = a.conn.Close()
	}
}

// counters reports lifetime dials, TimeSync loops, and APP_SERVER Configure
// calls (tests pin the sharing invariants with them: one dial + one sync
// loop per loomd; one Configure per voip member, one per shared origin).
func (p *fleetAgentPool) counters() (dials, syncLoops, configures int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dials, p.syncLoops, int(p.configures.Load())
}

// fleetOrigin is one shared far-end server flow: the single APP_SERVER
// Configure a whole http/video cohort (or several cohorts with the same
// agent + app + params) fans into.
type fleetOrigin struct {
	agent    *fleetAgent
	flowID   string
	dataPort uint32
}

// originKey identifies a shareable server: same agent handle, same server
// app, same param-set (order-independent).
func originKey(a *fleetAgent, serverApp string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := fmt.Sprintf("%p|%s", a, serverApp)
	for _, k := range keys {
		s += "|" + k + "=" + params[k]
	}
	return s
}

// configureAppServer runs the Configure+Start pair for one far-end server
// flow, duration-bounded with slack beyond the clients' bound (loom's orphan
// protection: the far end outlives the near end, never the reverse).
func (p *fleetAgentPool) configureAppServer(ctx context.Context, a *fleetAgent, serverApp string,
	params map[string]string, duration time.Duration) (flowID string, dataPort uint32, err error) {
	rpc1 := func(f func(rctx context.Context) error) error {
		rctx, rcancel := context.WithTimeout(ctx, p.tun.rpcTimeout)
		defer rcancel()
		return f(rctx)
	}
	p.configures.Add(1)
	var cfg *loomv1.ConfigureResponse
	if err := rpc1(func(rctx context.Context) error {
		cfg, err = a.client.Configure(rctx, &loomv1.ConfigureRequest{Flow: &loomv1.FlowSpec{
			Role:     loomv1.FlowRole_FLOW_ROLE_APP_SERVER,
			App:      serverApp,
			Network:  "host",
			Params:   cloneParams(params),
			Duration: durationpb.New(duration + p.tun.serverSlack),
		}})
		return err
	}); err != nil {
		return "", 0, fmt.Errorf("configure %s server on loomd at %s: %w", serverApp, a.addr, err)
	}
	if cfg.GetDataPort() == 0 {
		p.destroyFlow(a, cfg.GetFlowId())
		return "", 0, fmt.Errorf("loomd at %s advertised no data_port for the %s server", a.addr, serverApp)
	}
	if err := rpc1(func(rctx context.Context) error {
		_, serr := a.client.Start(rctx, &loomv1.StartRequest{
			FlowId:              cfg.GetFlowId(),
			ReportIntervalNanos: p.tun.sampleInterval.Nanoseconds(),
		})
		return serr
	}); err != nil {
		p.destroyFlow(a, cfg.GetFlowId())
		return "", 0, fmt.Errorf("start %s server on loomd at %s: %w", serverApp, a.addr, err)
	}
	return cfg.GetFlowId(), cfg.GetDataPort(), nil
}

// stopFlow / destroyFlow are the best-effort far-end teardowns (the flows are
// duration-bounded regardless).
func (p *fleetAgentPool) stopFlow(a *fleetAgent, flowID string) {
	if flowID == "" {
		return
	}
	rctx, rcancel := context.WithTimeout(context.Background(), p.tun.rpcTimeout)
	defer rcancel()
	_, _ = a.client.Stop(rctx, &loomv1.StopRequest{FlowId: flowID})
}

func (p *fleetAgentPool) destroyFlow(a *fleetAgent, flowID string) {
	if flowID == "" {
		return
	}
	rctx, rcancel := context.WithTimeout(context.Background(), p.tun.rpcTimeout)
	defer rcancel()
	_, _ = a.client.Destroy(rctx, &loomv1.DestroyRequest{FlowId: flowID})
}

// fleetAppMetrics is the per-cohort Prometheus surface: distribution gauges
// labeled {cohort, q} with q in p5/p50/p95 — bounded cardinality by
// construction (cohort names are the operator's short labels, never SUPIs).
// A nil receiver is a valid no-op (metrics disabled).
type fleetAppMetrics struct {
	mos       *prometheus.GaugeVec
	ttfb      *prometheus.GaugeVec
	goodput   *prometheus.GaugeVec
	stallTime *prometheus.GaugeVec
	rebuffer  *prometheus.GaugeVec
	ues       *prometheus.GaugeVec
}

// Distribution-gauge help texts state the two-phase semantics explicitly:
// while a cohort runs, each write summarises the members' per-INTERVAL
// snapshots; the cohort's last write is the WHOLE-RUN distribution (the
// report's numbers), which then remains as the series' final value.
const fleetAppPhaseNote = " Interval values while the cohort runs; the last written value is the whole-run distribution."

func newFleetAppMetrics(reg prometheus.Registerer) *fleetAppMetrics {
	if reg == nil {
		return nil
	}
	gauge := func(name, help string) *prometheus.GaugeVec {
		return registerGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit", Subsystem: "fleet_app", Name: name, Help: help,
		}, []string{"cohort", "q"}))
	}
	return &fleetAppMetrics{
		mos:       gauge("mos", "Distribution (p5/p50/p95 across cohort members) of MOS-CQ in a fleet voip cohort."+fleetAppPhaseNote),
		ttfb:      gauge("ttfb_ms", "Distribution across cohort members of median (per-member p50) HTTP time-to-first-byte, milliseconds."+fleetAppPhaseNote),
		goodput:   gauge("goodput_mbps", "Distribution across cohort members of HTTP goodput, megabits per second."+fleetAppPhaseNote),
		stallTime: gauge("stall_time_ms", "Distribution across cohort members of accumulated video stall time, milliseconds."+fleetAppPhaseNote),
		rebuffer:  gauge("rebuffer_ratio", "Distribution across cohort members of the video rebuffer ratio (stall time over stall+play time)."+fleetAppPhaseNote),
		ues: registerGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit", Subsystem: "fleet_app", Name: "ues",
			Help: "Members running an app client in the cohort.",
		}, []string{"cohort"})),
	}
}

// registerGaugeVec registers g, reusing an identical collector a previous
// RunFleet on the same long-lived Registerer already registered — a second
// run must update the existing series, never panic with a duplicate-collector
// error (the CLI builds a fresh registry per run, but embedders may not). Any
// other registration error degrades to an unregistered (process-local) vec
// rather than failing the fleet run over observability.
func registerGaugeVec(reg prometheus.Registerer, g *prometheus.GaugeVec) *prometheus.GaugeVec {
	err := reg.Register(g)
	if err == nil {
		return g
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if ex, ok := are.ExistingCollector.(*prometheus.GaugeVec); ok {
			return ex
		}
	}
	return g
}

func (f *fleetAppMetrics) set(vec *prometheus.GaugeVec, cohort string, q *FleetQuantiles) {
	if f == nil || q == nil {
		return
	}
	vec.WithLabelValues(cohort, "p5").Set(q.P5)
	vec.WithLabelValues(cohort, "p50").Set(q.P50)
	vec.WithLabelValues(cohort, "p95").Set(q.P95)
}

func (f *fleetAppMetrics) setUEs(cohort string, n int) {
	if f == nil {
		return
	}
	f.ues.WithLabelValues(cohort).Set(float64(n))
}

// fleetQuantiles summarises member values as p5/p50/p95 (linear
// interpolation); nil when no member produced a value.
func fleetQuantiles(vals []float64) *FleetQuantiles {
	if len(vals) == 0 {
		return nil
	}
	sort.Float64s(vals)
	q := func(p float64) float64 {
		if len(vals) == 1 {
			return vals[0]
		}
		pos := p * float64(len(vals)-1)
		lo := int(math.Floor(pos))
		hi := int(math.Ceil(pos))
		frac := pos - float64(lo)
		return vals[lo]*(1-frac) + vals[hi]*frac
	}
	return &FleetQuantiles{P5: q(0.05), P50: q(0.50), P95: q(0.95)}
}

// cohortRun is one cohort's live state: the members' clients (for the
// interval sampler) and their whole-run final snapshots (for the report).
type cohortRun struct {
	name string
	app  string

	mu      sync.Mutex
	clients map[loomapp.Client]struct{}
	finals  []metrics.Snapshot
	failed  int
}

func (cr *cohortRun) addClient(c loomapp.Client) {
	cr.mu.Lock()
	cr.clients[c] = struct{}{}
	cr.mu.Unlock()
}
func (cr *cohortRun) removeClient(c loomapp.Client) {
	cr.mu.Lock()
	delete(cr.clients, c)
	cr.mu.Unlock()
}
func (cr *cohortRun) fail() { cr.mu.Lock(); cr.failed++; cr.mu.Unlock() }
func (cr *cohortRun) final(s metrics.Snapshot) {
	cr.mu.Lock()
	cr.finals = append(cr.finals, s)
	cr.mu.Unlock()
}

// values extracts the cohort's per-member metric families from a snapshot
// list: the app decides which families exist.
func (cr *cohortRun) values(snaps []metrics.Snapshot) (mos, ttfb, goodput, stallTime, rebuffer []float64) {
	for _, s := range snaps {
		switch v := s.(type) {
		case metrics.VoIP:
			mos = append(mos, v.MOSCQ)
		case metrics.HTTP:
			ttfb = append(ttfb, v.TTFBMsP50)
			goodput = append(goodput, v.GoodputMbps)
		case metrics.Video:
			stallTime = append(stallTime, v.StallTimeMs)
			rebuffer = append(rebuffer, v.RebufferRatio)
		}
	}
	return
}

// publish sets the cohort's gauges from a snapshot list.
func (cr *cohortRun) publish(fm *fleetAppMetrics, snaps []metrics.Snapshot) {
	if fm == nil {
		return
	}
	mos, ttfb, goodput, stallTime, rebuffer := cr.values(snaps)
	fm.set(fm.mos, cr.name, fleetQuantiles(mos))
	fm.set(fm.ttfb, cr.name, fleetQuantiles(ttfb))
	fm.set(fm.goodput, cr.name, fleetQuantiles(goodput))
	fm.set(fm.stallTime, cr.name, fleetQuantiles(stallTime))
	fm.set(fm.rebuffer, cr.name, fleetQuantiles(rebuffer))
}

// intervalSnapshots reads every live client's interval Metrics() — the
// sampler is the single Metrics() reader per client, so each read cleanly
// closes one observation interval (the appsession sampleLoop discipline).
func (cr *cohortRun) intervalSnapshots() []metrics.Snapshot {
	cr.mu.Lock()
	clients := make([]loomapp.Client, 0, len(cr.clients))
	for c := range cr.clients {
		clients = append(clients, c)
	}
	cr.mu.Unlock()
	snaps := make([]metrics.Snapshot, 0, len(clients))
	for _, c := range clients {
		if src, ok := c.(metrics.Source); ok {
			snaps = append(snaps, src.Metrics())
		}
	}
	return snaps
}

// runFleetApps runs the app cohorts over their carved members for the
// behaviour window and returns the per-cohort reports (index-aligned with
// cohorts). Setup failures are per-cohort (Err), never fatal to the rest of
// the fleet run. reg == nil disables the Prometheus gauges.
func runFleetApps(ctx context.Context, log *slog.Logger, pool *fleetAgentPool,
	cohorts []FleetAppCohort, members [][]*fleetUE, duration time.Duration,
	reg prometheus.Registerer) []FleetAppCohortReport {

	fm := newFleetAppMetrics(reg)
	reports := make([]FleetAppCohortReport, len(cohorts))

	// Shared origins across all cohorts of this run: one Configure per
	// (agent, app, param-set) — loom's http origin serves any number of
	// concurrent clients, so identical cohorts pointing at one loomd share
	// one far-end flow.
	var originMu sync.Mutex
	origins := make(map[string]*fleetOrigin)

	var wg sync.WaitGroup
	for i := range cohorts {
		c := cohorts[i]
		rep := &reports[i]
		rep.Name, rep.App = cohortName(c, i), c.App
		if len(members[i]) == 0 {
			continue
		}
		wg.Add(1)
		go func(c FleetAppCohort, rep *FleetAppCohortReport, mem []*fleetUE) {
			defer wg.Done()
			runFleetCohort(ctx, log, pool, fm, c, rep, mem, duration, origins, &originMu)
		}(c, rep, members[i])
	}
	wg.Wait()

	// Stop the shared origins once, after every cohort using them is done
	// (each origin retains its agent handle, so the control connection is
	// still up here even though the cohorts released theirs).
	originMu.Lock()
	for _, o := range origins {
		pool.stopFlow(o.agent, o.flowID)
		pool.release(o.agent)
	}
	originMu.Unlock()
	return reports
}

// runFleetCohort provisions and runs one cohort: shared agent acquire + skew
// gate, far-end server placement (per-UE for voip, shared origin for
// http/video), one client goroutine per member, and an interval sampler
// feeding the cohort gauges while the members run.
func runFleetCohort(ctx context.Context, log *slog.Logger, pool *fleetAgentPool, fm *fleetAppMetrics,
	c FleetAppCohort, rep *FleetAppCohortReport, mem []*fleetUE, duration time.Duration,
	origins map[string]*fleetOrigin, originMu *sync.Mutex) {

	serverApp := fleetAppServerApp(c.App)
	agent, err := pool.acquire(ctx, c.Peer, c.Token)
	if err != nil {
		rep.Err = err.Error()
		log.Warn("fleet app cohort setup failed", "cohort", rep.Name, "err", err)
		return
	}
	defer pool.release(agent)

	// Version-skew gate, once per cohort (design §2.12) — same actionable
	// wording as single-UE app sessions.
	if !agentServesApp(agent.caps, serverApp) {
		want := fmt.Sprintf("app %q", serverApp)
		if serverApp != c.App {
			want = fmt.Sprintf("app %q (the far end of %q is the http origin)", serverApp, c.App)
		}
		rep.Err = fmt.Sprintf("loomd at %s (%s) lacks %s; run loom >= %s",
			c.Peer, agent.version, want, appMinLoomVersion(serverApp))
		log.Warn("fleet app cohort refused by the skew gate", "cohort", rep.Name, "err", rep.Err)
		return
	}

	// N6 media address: PeerDataIP override, else the control host.
	ctrlHost, _, err := net.SplitHostPort(c.Peer)
	if err != nil {
		rep.Err = fmt.Sprintf("peer agent %q is not host:port: %v", c.Peer, err)
		return
	}
	dataHost := c.PeerDataIP
	if dataHost == "" {
		dataHost = ctrlHost
	}
	dataIP, err := net.ResolveIPAddr("ip4", dataHost)
	if err != nil {
		rep.Err = fmt.Sprintf("resolve peer data address %q: %v", dataHost, err)
		return
	}

	// Far-end placement. http/video: ONE origin shared by the cohort (and by
	// any other cohort with the same agent+app+params). voip: one answerer
	// per member — the latch serves exactly one source.
	perUEServer := serverApp == "voip"
	var sharedPort uint32
	if !perUEServer {
		key := originKey(agent, serverApp, c.Params)
		originMu.Lock()
		o := origins[key]
		if o == nil {
			flowID, port, cerr := pool.configureAppServer(ctx, agent, serverApp, c.Params, duration)
			if cerr != nil {
				originMu.Unlock()
				rep.Err = cerr.Error()
				log.Warn("fleet app cohort origin setup failed", "cohort", rep.Name, "err", cerr)
				return
			}
			o = &fleetOrigin{agent: agent, flowID: flowID, dataPort: port}
			pool.retain(agent) // held until the post-run origin Stop
			origins[key] = o
		}
		// Servers counts the far-end flows this cohort USES: one shared
		// origin, whichever cohort paid the Configure (pool.counters pins
		// the actual Configure count for tests).
		rep.Servers = 1
		sharedPort = o.dataPort
		originMu.Unlock()
	}

	cr := &cohortRun{name: rep.Name, app: c.App, clients: make(map[loomapp.Client]struct{})}

	var cwg sync.WaitGroup
	for _, fu := range mem {
		flowID, port := "", sharedPort
		if perUEServer {
			var cerr error
			flowID, port, cerr = pool.configureAppServer(ctx, agent, serverApp, c.Params, duration)
			if cerr != nil {
				cr.fail()
				log.Warn("fleet app cohort: per-UE voip server setup failed",
					"cohort", rep.Name, "supi", fu.sess.SUPI, "err", cerr)
				continue
			}
			rep.Servers++
		}
		rep.UEs++
		// The gauge tracks members that actually run a client (its help
		// text, and exactly rep.UEs) — set as members clear per-UE server
		// setup, so a failed voip answerer never inflates it.
		fm.setUEs(cr.name, rep.UEs)
		target := net.JoinHostPort(dataIP.String(), fmt.Sprint(port))
		cwg.Add(1)
		go func(fu *fleetUE, target, flowID string) {
			defer cwg.Done()
			runFleetAppClient(ctx, log, pool, cr, c, agent, fu, target, flowID, duration)
		}(fu, target, flowID)
	}

	// Interval sampler: while members run, fold their interval snapshots
	// into the cohort's live distribution gauges. It is stopped (and waited
	// for) BEFORE the whole-run publish below, so the final gauge values are
	// the report's distributions, not a late interval tick.
	stopSampler := func() {}
	if fm != nil {
		stop := make(chan struct{})
		samplerDone := make(chan struct{})
		go func() {
			defer close(samplerDone)
			t := time.NewTicker(pool.tun.sampleInterval)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					cr.publish(fm, cr.intervalSnapshots())
				}
			}
		}()
		stopSampler = func() { close(stop); <-samplerDone }
	}

	cwg.Wait()
	stopSampler()

	// Whole-run distributions for the report (and the gauges' final word).
	mos, ttfb, goodput, stallTime, rebuffer := cr.values(cr.finals)
	rep.MOS = fleetQuantiles(mos)
	rep.TTFBMs = fleetQuantiles(ttfb)
	rep.GoodputMbps = fleetQuantiles(goodput)
	rep.StallTimeMs = fleetQuantiles(stallTime)
	rep.RebufferRatio = fleetQuantiles(rebuffer)
	rep.Failed = cr.failed
	cr.publish(fm, cr.finals)
}

// runFleetAppClient runs one member's app client for the window: the
// session's data plane bridged onto loom's netpath seam (dgram for voip, the
// per-gNB shared netstack for http/video — Session.appNetwork), the client
// built from the registry with the agent's SHARED tracker as its OWD source,
// then the whole-run snapshot folded into the cohort. A per-UE far-end flow
// (voip) is stopped best-effort on the way out.
func runFleetAppClient(ctx context.Context, log *slog.Logger, pool *fleetAgentPool, cr *cohortRun,
	c FleetAppCohort, agent *fleetAgent, fu *fleetUE, target, flowID string, duration time.Duration) {

	defer func() {
		if flowID != "" {
			pool.stopFlow(agent, flowID)
		}
	}()
	sess := fu.sess
	ueIP := net.ParseIP(sess.Result.PDUAddress)
	if ueIP == nil {
		cr.fail()
		log.Warn("fleet app client: invalid PDU address", "cohort", cr.name, "supi", sess.SUPI,
			"addr", sess.Result.PDUAddress)
		return
	}
	network, err := sess.appNetwork(c.App, ueIP)
	if err != nil {
		cr.fail()
		log.Warn("fleet app client: data-plane bridge failed", "cohort", cr.name, "supi", sess.SUPI, "err", err)
		return
	}
	defer network.Close()
	client, err := components.Default().AppClients.Build(c.App, loomapp.Options{
		Params:  cloneParams(c.Params),
		Network: network,
		Target:  target,
		OWD:     agent.tracker,
	})
	if err != nil {
		cr.fail()
		log.Warn("fleet app client: build failed", "cohort", cr.name, "supi", sess.SUPI, "err", err)
		return
	}
	cr.addClient(client)
	cctx, cancel := context.WithTimeout(ctx, duration)
	err = runFleetClientContained(cctx, client)
	cancel()
	cr.removeClient(client)
	if err != nil && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
		cr.fail()
		log.Warn("fleet app client ended with error", "cohort", cr.name, "supi", sess.SUPI, "err", err)
	}
	// Whole-run snapshot: CumulativeMetrics closes no observation interval,
	// so it cannot corrupt the sampler's series; engines without it fall
	// back to a final interval read (safe here: the sampler only reads
	// clients still in the live set).
	if cs, ok := client.(interface{ CumulativeMetrics() metrics.Snapshot }); ok {
		cr.final(cs.CumulativeMetrics())
	} else if src, ok := client.(metrics.Source); ok {
		cr.final(src.Metrics())
	}
}

// runFleetClientContained runs the engine with panic containment (the client
// parses network input — a bug degrades to one failed member, never a
// crashed fleet run).
func runFleetClientContained(ctx context.Context, client loomapp.Client) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("app client panicked: %v", r)
		}
	}()
	return client.Run(ctx)
}
