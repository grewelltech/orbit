// App sessions: real application traffic (VoIP first) from a registered UE
// through its GTP-U data path to a stock loomd agent on the N6 network
// (docs/design/real-app-traffic.md §7 — the "controller-lite").
//
// The Manager plays a minimal loom controller for exactly one flow pair:
//
//  1. dial the peer loomd's control plane (the MANAGEMENT network, never the
//     tunnel) and gate on Capabilities (version skew → actionable refusal);
//  2. run the loom TimeSync exchange every ~10s into an owd.Tracker, so the
//     media engine's one-way delay is measured, not guessed;
//  3. Configure+Start the far end (FLOW_ROLE_APP_SERVER, network "host",
//     duration-bounded for orphan protection) and learn its data_port;
//  4. build the LOCAL side in process — no local loomd — over the session's
//     N3 data path via loomgtp.NetworkFor (dgram over the demuxed tunnel);
//  5. run the client as a managed session: local interval samples, the remote
//     agent's StreamTelemetry series (re-stamped onto orbit's clock with the
//     tracker offset, error bound carried), and correlation events (handover
//     phases from the hub, GTP-U End Markers) fan out on one stream.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
	"github.com/bgrewell/loom/control"
	loomapp "github.com/bgrewell/loom/core/app"
	"github.com/bgrewell/loom/core/components"
	"github.com/bgrewell/loom/core/metrics"
	"github.com/bgrewell/loom/core/netpath"
	"github.com/bgrewell/loom/core/owd"
	"github.com/bgrewell/loom/core/quality/emodel"
	"github.com/bgrewell/loom/core/rtp"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/loomgtp"

	// Register the loom app engines this build can run in-process. The voip
	// pair self-registers into the global AppClients/AppServers registries
	// (loom ADR-0006), which components.Default() exposes.
	_ "github.com/bgrewell/loom/core/app/voip"
)

// appMinLoomVersion is the first loom release whose agents carry the app
// engines — the version named in skew-gate refusals (design §2.12).
const appMinLoomVersion = "v0.10"

// AppSample ends (AppSample.End): which side of the call measured the sample.
const (
	AppEndUE = "ue" // the in-process client behind the UE (downlink-as-received)
	AppEndN6 = "n6" // the remote loomd server (uplink-as-received)
)

// AppEventEndMarker is the correlation event emitted when a GTP-U End Marker
// arrives on the UE's downlink lane — the UPF's "old path drained" signal
// after a handover path switch (TS 29.281 §7.3). Handover phase events
// (HANDOVER_STARTED/…) arrive under their hub state names.
const AppEventEndMarker = "END_MARKER_RECEIVED"

// AppEventUEReleased is the correlation event stamped when the UE is
// deregistered while the call is still running.
const AppEventUEReleased = "UE_RELEASED"

// AppSessionConfig configures StartAppSession.
type AppSessionConfig struct {
	// App names the loom application engine. Only "voip" is supported today.
	App string
	// PeerAgent is the N6 loomd's control-plane address ("host:port").
	// Empty falls back to the server-level default (SetLoomDefaults).
	PeerAgent string
	// Token is the loomd control-plane bearer token; empty falls back to the
	// server-level default, and an empty result means no authentication.
	Token string
	// PeerDataIP overrides the N6-side media address. By default media is
	// aimed at PeerAgent's host, but the management address and the N6 data
	// address may differ (the firewall matrix in docs/USAGE.md allows control
	// on the management network while RTP enters from the UPF's N6 subnet).
	PeerDataIP string
	// Params are the app's tuning knobs, passed verbatim to both ends
	// (voip: codec, ptime, jb_ms, port_min/port_max, …).
	Params map[string]string
	// Duration bounds the call. It must be positive: the far-end flow is
	// always duration-bounded (loom's orphan protection), so an unbounded
	// session cannot be expressed.
	Duration time.Duration
}

// AppSample is one item on an app session's event stream: an interval quality
// sample from either end of the call, or a correlation event.
type AppSample struct {
	// Time is the sample instant on orbit's clock. Remote (n6) samples are
	// re-stamped using the TimeSync offset when one is available; TimeSource
	// says which clock the stamp is on and TimeErr its half-width uncertainty
	// (design §7 — re-stamped timestamps carry honest error bars).
	Time       time.Time
	TimeErr    time.Duration
	TimeSource string // "local", "timesync" (re-stamped), "remote-clock" (no offset yet)

	// End tags quality samples: AppEndUE or AppEndN6. Empty on events.
	End string
	// Final marks the closing whole-call sample of a telemetry series.
	Final bool
	// VoIP is the quality snapshot for App "voip" samples (nil on events).
	VoIP *metrics.VoIP

	// Event/Detail carry correlation events (hub state phases such as
	// HANDOVER_STARTED, AppEventEndMarker, AppEventUEReleased); Event is
	// empty on quality samples.
	Event  string
	Detail string
}

// AppSessionReport is the both-end result of one app session.
type AppSessionReport struct {
	ID        string
	SUPI      string
	App       string
	PeerAgent string
	// DataPort is the far end's advertised media port.
	DataPort uint32
	Started  time.Time
	Ended    time.Time
	// Local is the in-process client's whole-call cumulative snapshot;
	// Remote is the far end's final telemetry sample (nil if none arrived).
	Local  metrics.VoIP
	Remote *metrics.VoIP
	// LocalSeries and RemoteSeries are the retained per-interval samples of
	// each end; Events the correlation events, all in arrival order.
	LocalSeries  []AppSample
	RemoteSeries []AppSample
	Events       []AppSample
	// MediaGaps are both ends' media gaps on ONE timeline where possible:
	// remote (n6) gaps are re-stamped onto orbit's clock via the session's
	// TimeSync offset (Clock "timesync", TimeErr carried); with no offset
	// they stay on the remote clock and are labeled so (design §5/§7 — a
	// quantity that cannot be re-stamped is labeled, never silently
	// presented as aligned). Local (ue) gaps are Clock "local".
	MediaGaps []MediaGapReport
	// Annotations are the correlator's composed handover-vs-media joins
	// ("XnHandover @t → DL media gap 240ms → …"), in emission order. Each
	// also appears in Events as an AppEventAnnotation sample.
	Annotations []string
	// Err is the client engine's terminal error (e.g. a handshake timeout),
	// empty for a clean run.
	Err string
}

// MediaGapReport is one media gap in the final report, tagged with the end
// that observed it and the clock domain of its timestamps.
type MediaGapReport struct {
	End         string // AppEndUE (a DL gap) or AppEndN6 (an UL gap)
	Start, Stop time.Time
	PacketsLost uint32
	// Clock says whose clock Start/Stop are on: "local" (orbit's), "timesync"
	// (remote, re-stamped onto orbit's clock; TimeErr is the half-width
	// uncertainty of the re-stamp), or "remote-clock" (no offset available —
	// NOT comparable with local timestamps).
	Clock   string
	TimeErr time.Duration
}

// appTuning groups the session's cadences and bounded waits so tests can
// shorten them; zero values select the defaults.
type appTuning struct {
	syncInterval   time.Duration // TimeSync cadence on the management connection
	syncBurst      int           // extra initial exchanges to seed the tracker
	sampleInterval time.Duration // local sampling + remote report_interval
	trackerWindow  time.Duration // owd.Tracker window (0 = owd default)
	trackerN       int           // owd.Tracker window count (0 = owd default)
	serverSlack    time.Duration // far-end duration margin beyond the client's
	rpcTimeout     time.Duration // per-RPC bound for setup/teardown calls
	stopWait       time.Duration // bounded teardown waits (BYE, final sample)
}

func (t appTuning) withDefaults() appTuning {
	if t.syncInterval <= 0 {
		t.syncInterval = 10 * time.Second
	}
	if t.syncBurst <= 0 {
		t.syncBurst = 3
	}
	if t.sampleInterval <= 0 {
		t.sampleInterval = time.Second
	}
	if t.serverSlack <= 0 {
		t.serverSlack = 30 * time.Second
	}
	if t.rpcTimeout <= 0 {
		t.rpcTimeout = 10 * time.Second
	}
	if t.stopWait <= 0 {
		t.stopWait = 5 * time.Second
	}
	return t
}

// appTable holds the Manager's live app sessions. The zero value is usable
// (lazily initialized under mu), so Manager construction is untouched.
type appTable struct {
	mu           sync.Mutex
	tab          map[string]*appSession
	starting     map[string]bool // SUPIs with a StartAppSession in flight
	seq          int
	tuning       appTuning
	defaultAgent string
	defaultToken string
}

// reserve claims the per-UE call slot BEFORE the seconds of setup RPCs run:
// the single-call-per-UE guard must be one atomic check-and-claim, or two
// concurrent StartAppSession calls both pass it and the second one's
// SubscribeUDPAll silently replaces (and closes) the first one's downlink
// lane. Release with release() once the session is in tab (or setup failed).
func (t *appTable) reserve(supi string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.starting[supi] {
		return fmt.Errorf("UE %s already has an app session starting; wait for it", supi)
	}
	for _, s := range t.tab {
		if s.supi == supi && !s.finished() {
			return fmt.Errorf("UE %s already has a running app session (%s); stop it first", supi, s.id)
		}
	}
	if t.starting == nil {
		t.starting = make(map[string]bool)
	}
	t.starting[supi] = true
	return nil
}

func (t *appTable) release(supi string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.starting, supi)
}

func (t *appTable) add(s *appSession) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tab == nil {
		t.tab = make(map[string]*appSession)
	}
	t.seq++
	s.id = fmt.Sprintf("app-%d", t.seq)
	t.tab[s.id] = s
	return s.id
}

func (t *appTable) get(id string) *appSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tab[id]
}

func (t *appTable) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tab, id)
}

// tuningNow snapshots the tuning under the table lock.
func (t *appTable) tuningNow() appTuning {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tuning.withDefaults()
}

func (t *appTable) forSUPI(supi string) []*appSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*appSession
	for _, s := range t.tab {
		if s.supi == supi {
			out = append(out, s)
		}
	}
	return out
}

// SetLoomDefaults installs server-level defaults for the peer loomd control
// address and auth token (the `orbit serve --loom-agent/--loom-token` seam);
// per-call AppSessionConfig values override them.
func (m *Manager) SetLoomDefaults(agent, token string) {
	m.apps.mu.Lock()
	defer m.apps.mu.Unlock()
	m.apps.defaultAgent, m.apps.defaultToken = agent, token
}

// appSession is one running app call: the controller-lite state around an
// in-process loom app client and its remote loomd server flow.
type appSession struct {
	id   string
	supi string
	cfg  AppSessionConfig
	tun  appTuning
	m    *Manager

	agent      loomv1.ControlClient
	conn       io.Closer // the control-plane grpc connection
	remoteFlow string
	dataPort   uint32

	tracker *owd.Tracker
	network netpath.Network
	client  loomapp.Client
	rx      *datapath.UERx
	corr    *correlator // joins handover events with media evidence (correlate.go)

	ctx     context.Context // session lifetime; cancel = stop the call
	cancel  context.CancelFunc
	tctx    context.Context    // telemetry stream lifetime: outlives ctx so the
	tcancel context.CancelFunc // remote FINAL sample (sent after Stop) arrives

	hubCh    <-chan StateEvent
	unsubHub func()

	done chan struct{} // closed when run() has fully wound down

	mu           sync.Mutex
	started      time.Time
	ended        time.Time
	localSeries  []AppSample
	remoteSeries []AppSample
	events       []AppSample
	localFinal   metrics.VoIP
	remoteFinal  *metrics.VoIP
	runErr       error
	subs         map[int]chan AppSample
	subSeq       int
	finalized    bool
}

// StartAppSession starts an application-traffic session (design §7) from a
// registered UE to the loomd agent at cfg.PeerAgent and returns its id. It
// requires an active PDU session and a gNB N3 address, exactly like Traffic;
// media rides the session's demuxed tunnel socket (no new 2152 bind). The
// call runs asynchronously — consume AppSessionEvents and finish with
// StopAppSession (which also reaps a call that ended by itself).
func (m *Manager) StartAppSession(ctx context.Context, supi string, cfg AppSessionConfig) (string, error) {
	m.apps.mu.Lock()
	tun := m.apps.tuning.withDefaults()
	if cfg.PeerAgent == "" {
		cfg.PeerAgent = m.apps.defaultAgent
	}
	if cfg.Token == "" {
		cfg.Token = m.apps.defaultToken
	}
	m.apps.mu.Unlock()

	if cfg.App != "voip" {
		return "", fmt.Errorf("app %q is not supported (this build supports \"voip\")", cfg.App)
	}
	if cfg.PeerAgent == "" {
		return "", errors.New("no peer loomd agent: pass one per call or configure a server-level default")
	}
	if cfg.Duration <= 0 {
		return "", errors.New("app sessions require a positive duration (the far-end flow is duration-bounded for orphan protection)")
	}

	// Session gating — the same preconditions as Traffic/Latency.
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("UE %s is not registered", supi)
	}
	if !sess.Result.SessionActive {
		return "", fmt.Errorf("UE %s has no active PDU session", supi)
	}
	if sess.gnbN3 == "" {
		return "", fmt.Errorf("UE %s registered without a gNB N3 address; data path disabled", supi)
	}
	ueIP := net.ParseIP(sess.Result.PDUAddress)
	if ueIP == nil {
		return "", fmt.Errorf("UE %s has an invalid PDU address %q", supi, sess.Result.PDUAddress)
	}
	// Phase 4 is single-call per UE: the demux wildcard UDP lane and the
	// End-Marker callback are single-consumer seams, so a second concurrent
	// call would steal the first one's downlink. The slot is RESERVED (not
	// just checked) because the setup below takes seconds of RPCs — a
	// check-then-act guard would let two racing calls both pass.
	if err := m.apps.reserve(supi); err != nil {
		return "", err
	}
	defer m.apps.release(supi)

	// Resolve the N6 media address: PeerDataIP override, else the control
	// host (management and data addresses may legitimately differ).
	ctrlHost, _, err := net.SplitHostPort(cfg.PeerAgent)
	if err != nil {
		return "", fmt.Errorf("peer agent %q is not host:port: %w", cfg.PeerAgent, err)
	}
	dataHost := cfg.PeerDataIP
	if dataHost == "" {
		dataHost = ctrlHost
	}
	dataIP, err := net.ResolveIPAddr("ip4", dataHost)
	if err != nil {
		return "", fmt.Errorf("resolve peer data address %q (use PeerDataIP when the N6 data address differs from the management address): %w", dataHost, err)
	}

	// 1. Control-plane dial — the management network, never the tunnel.
	agent, conn, err := control.Dial(cfg.PeerAgent, control.WithToken(cfg.Token))
	if err != nil {
		return "", fmt.Errorf("dial loomd at %s: %w", cfg.PeerAgent, err)
	}
	ok = false // from here, cleanup on failure
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	// Each setup RPC gets its own rpcTimeout budget (the documented per-RPC
	// bound, matching finalize's teardown calls): one shared deadline would
	// let slow-but-healthy early RPCs starve Start's budget and misattribute
	// the failure.
	rpc1 := func(f func(rctx context.Context) error) error {
		rctx, rcancel := context.WithTimeout(ctx, tun.rpcTimeout)
		defer rcancel()
		return f(rctx)
	}

	// 2. Capabilities gate (version skew → actionable refusal, design §2.12).
	var caps *loomv1.CapabilitiesResponse
	if err := rpc1(func(rctx context.Context) error {
		caps, err = agent.Capabilities(rctx, &loomv1.CapabilitiesRequest{})
		return err
	}); err != nil {
		return "", fmt.Errorf("loomd at %s: Capabilities: %w", cfg.PeerAgent, err)
	}
	if !agentServesApp(caps, cfg.App) {
		version := "unknown version"
		_ = rpc1(func(rctx context.Context) error {
			if h, herr := agent.Health(rctx, &loomv1.HealthRequest{}); herr == nil && h.GetVersion() != "" {
				version = h.GetVersion()
			}
			return nil
		})
		return "", fmt.Errorf("loomd at %s (%s) lacks app %q; run loom >= %s",
			cfg.PeerAgent, version, cfg.App, appMinLoomVersion)
	}

	// 3. Seed the OWD tracker with one exchange now (also proves the token
	// works); the session's TimeSync loop keeps feeding it every ~10s.
	tracker := owd.NewTracker(tun.trackerWindow, tun.trackerN)
	if err := rpc1(func(rctx context.Context) error {
		smp, serr := control.Sync(rctx, agent)
		if serr != nil {
			return serr
		}
		tracker.Feed(smp, time.Now())
		return nil
	}); err != nil {
		return "", fmt.Errorf("loomd at %s: TimeSync: %w", cfg.PeerAgent, err)
	}

	// 4. Far end: APP_SERVER on the host network, duration-bounded with slack
	// beyond the client's bound so the near end always finishes first and the
	// far end never orphans (loom refuses unbounded app servers).
	srvSpec := &loomv1.FlowSpec{
		Role:     loomv1.FlowRole_FLOW_ROLE_APP_SERVER,
		App:      cfg.App,
		Network:  "host",
		Params:   cloneParams(cfg.Params),
		Duration: durationpb.New(cfg.Duration + tun.serverSlack),
	}
	var srvCfg *loomv1.ConfigureResponse
	if err := rpc1(func(rctx context.Context) error {
		srvCfg, err = agent.Configure(rctx, &loomv1.ConfigureRequest{Flow: srvSpec})
		return err
	}); err != nil {
		return "", fmt.Errorf("configure %s server on loomd at %s: %w", cfg.App, cfg.PeerAgent, err)
	}
	remoteFlow := srvCfg.GetFlowId()
	defer func() {
		if !ok && remoteFlow != "" {
			dctx, dcancel := context.WithTimeout(context.Background(), tun.rpcTimeout)
			_, _ = agent.Destroy(dctx, &loomv1.DestroyRequest{FlowId: remoteFlow})
			dcancel()
		}
	}()
	if srvCfg.GetDataPort() == 0 {
		return "", fmt.Errorf("loomd at %s advertised no data_port for the %s server", cfg.PeerAgent, cfg.App)
	}
	if err := rpc1(func(rctx context.Context) error {
		_, serr := agent.Start(rctx, &loomv1.StartRequest{
			FlowId:              remoteFlow,
			ReportIntervalNanos: tun.sampleInterval.Nanoseconds(),
		})
		return serr
	}); err != nil {
		return "", fmt.Errorf("start %s server on loomd at %s: %w", cfg.App, cfg.PeerAgent, err)
	}

	// 5. Local side, in process: the session's data plane (tunnel + demuxed
	// downlink lane) bridged onto loom's netpath seam, then the app client
	// built from the registry with the tracker as its OWD source. The
	// Session itself is the uplink (not a tunnel snapshot), so the call's
	// media follows a handover's data-path rebind.
	_, rx, err := sess.dataplane()
	if err != nil {
		return "", err
	}
	network, err := loomgtp.NetworkFor(sess, rx, ueIP, 0)
	if err != nil {
		return "", err
	}
	defer func() {
		if !ok {
			network.Close()
		}
	}()
	target := net.JoinHostPort(dataIP.String(), fmt.Sprint(srvCfg.GetDataPort()))
	client, err := components.Default().AppClients.Build(cfg.App, loomapp.Options{
		Params:  cloneParams(cfg.Params),
		Network: network,
		Target:  target,
		OWD:     tracker,
	})
	if err != nil {
		return "", fmt.Errorf("build %s client: %w", cfg.App, err)
	}

	sctx, scancel := context.WithCancel(context.Background())
	tctx, tcancel := context.WithCancel(context.Background())
	s := &appSession{
		supi:       supi,
		cfg:        cfg,
		tun:        tun,
		m:          m,
		agent:      agent,
		conn:       conn,
		remoteFlow: remoteFlow,
		dataPort:   srvCfg.GetDataPort(),
		tracker:    tracker,
		network:    network,
		client:     client,
		rx:         rx,
		ctx:        sctx,
		cancel:     scancel,
		tctx:       tctx,
		tcancel:    tcancel,
		done:       make(chan struct{}),
		started:    time.Now(),
		subs:       make(map[int]chan AppSample),
	}
	s.hubCh, s.unsubHub = m.hub.subscribe()
	// The correlator joins this session's handover events with its media
	// evidence into annotations (design §7); each finished annotation goes
	// out live on the sample stream and into the serve log.
	s.corr = newCorrelator(tracker.Offset, func(at time.Time, text string) {
		m.log.Info("app correlation", "id", s.id, "supi", s.supi, "annotation", text)
		s.publish(AppSample{Time: at, TimeSource: "local", Event: AppEventAnnotation, Detail: text})
	})
	// End Markers are correlation events (design §6); the callback runs on
	// the demux reader and must not block — publish never does.
	rx.SetEndMarkerFunc(func(at time.Time) {
		s.publish(AppSample{Time: at, TimeSource: "local", Event: AppEventEndMarker,
			Detail: "GTP-U End Marker on the UE downlink lane"})
	})

	id := m.apps.add(s)
	// Re-check the UE now that the session is discoverable: Deregister's
	// sweep (releaseAppSessions → closeDataPath) only covers sessions already
	// in the table, so a deregistration that ran during the seconds of setup
	// RPCs above would otherwise leave this session running against a
	// torn-down — and, via dataplane() above, lazily re-created — data path,
	// leaking the demux reader and the gNB N3 bind until process exit.
	m.mu.Lock()
	cur := m.sessions[supi]
	m.mu.Unlock()
	if cur != sess {
		m.apps.remove(id)
		s.unsubHub()
		rx.SetEndMarkerFunc(nil)
		s.cancel()
		s.tcancel()
		sess.closeDataPath() // reap a data path re-created after the teardown
		return "", fmt.Errorf("UE %s was deregistered while the app session was being set up", supi)
	}
	ok = true
	m.log.Info("app session started", "id", id, "supi", supi, "app", cfg.App,
		"peer", cfg.PeerAgent, "target", target, "duration", cfg.Duration)
	go s.run()
	return id, nil
}

// AppSessionEvents subscribes to a session's live stream of both-end interval
// samples and correlation events. The channel closes when the session ends
// (or immediately for an unknown/finished id); cancel unsubscribes early.
// History is not replayed — StopAppSession's report retains the full series.
func (m *Manager) AppSessionEvents(id string) (<-chan AppSample, func()) {
	s := m.apps.get(id)
	if s == nil {
		ch := make(chan AppSample)
		close(ch)
		return ch, func() {}
	}
	return s.subscribe()
}

// StopAppSession ends the call (RTCP BYE from the in-process client, then a
// best-effort Stop of the remote flow), waits for teardown, and returns the
// both-end report. A session that already ended on its own is reaped the
// same way. The session id is forgotten once the report is returned.
func (m *Manager) StopAppSession(ctx context.Context, id string) (AppSessionReport, error) {
	s := m.apps.get(id)
	if s == nil {
		return AppSessionReport{}, fmt.Errorf("app session %s not found", id)
	}
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		return AppSessionReport{}, ctx.Err()
	}
	m.apps.remove(id)
	m.log.Info("app session stopped", "id", id, "supi", s.supi)
	return s.report(), nil
}

// releaseAppSessions ends every app session of a UE being released: the
// clients get cancelled (sending their RTCP BYE while the tunnel is still
// open, with the remote Stop following best-effort inside each session's
// teardown) and we wait — bounded — for them to wind down before the caller
// tears the data path away. Reports remain collectable via StopAppSession.
func (m *Manager) releaseAppSessions(supi string) {
	sessions := m.apps.forSUPI(supi)
	if len(sessions) == 0 {
		return
	}
	now := time.Now()
	for _, s := range sessions {
		s.publish(AppSample{Time: now, TimeSource: "local", Event: AppEventUEReleased,
			Detail: "UE deregistered mid-call"})
		s.cancel()
	}
	deadline := time.After(m.apps.tuningNow().stopWait)
	for _, s := range sessions {
		select {
		case <-s.done:
		case <-deadline:
			return // bounded: never hold up deregistration on a stuck call
		}
	}
}

// finished reports whether the session has fully wound down.
func (s *appSession) finished() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// run is the session's managed lifecycle: helper loops around the client
// engine's Run, then teardown. Every goroutine is panic-contained so a bug in
// a measurement loop degrades to a failed session, never a crashed server.
func (s *appSession) run() {
	defer close(s.done)

	var wg sync.WaitGroup
	wg.Add(3)
	go s.contain("timesync", func() { defer wg.Done(); s.timesyncLoop() })
	go s.contain("sample", func() { defer wg.Done(); s.sampleLoop() })
	go s.contain("events", func() { defer wg.Done(); s.eventLoop() })
	tdone := make(chan struct{})
	go s.contain("telemetry", func() { defer close(tdone); s.telemetryLoop() })

	callCtx, callCancel := context.WithTimeout(s.ctx, s.cfg.Duration)
	err := s.runClient(callCtx)
	callCancel()
	if err != nil && s.ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
		s.mu.Lock()
		s.runErr = err
		s.mu.Unlock()
		s.m.log.Warn("app client ended with error", "id", s.id, "supi", s.supi, "err", err)
	}
	s.cancel()
	wg.Wait()
	s.finalize(tdone)
}

// runClient runs the app engine with panic containment (the engine is the
// least-trusted code in the loop: it parses network input).
func (s *appSession) runClient(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("app client panicked: %v", r)
		}
	}()
	return s.client.Run(ctx)
}

// contain runs f, containing panics like loom's flowManager does for flows.
func (s *appSession) contain(name string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			s.m.log.Error("app session goroutine panicked", "id", s.id, "goroutine", name, "panic", r)
		}
	}()
	f()
}

// finalize tears the session down in dependency order: capture the client's
// whole-call snapshot, stop the remote flow (which flushes its FINAL
// telemetry sample), bound-wait for the telemetry stream to drain, then
// release the network lane, control connection, and subscribers.
func (s *appSession) finalize(tdone <-chan struct{}) {
	// Whole-call local snapshot; CumulativeMetrics closes no observation
	// interval, so it cannot corrupt the sampler's series.
	if cs, ok := s.client.(interface{ CumulativeMetrics() metrics.Snapshot }); ok {
		if v, ok := cs.CumulativeMetrics().(metrics.VoIP); ok {
			s.mu.Lock()
			s.localFinal = v
			s.mu.Unlock()
		}
	}

	// Remote Stop, best-effort: the far end is duration-bounded anyway.
	rctx, rcancel := context.WithTimeout(context.Background(), s.tun.rpcTimeout)
	if _, err := s.agent.Stop(rctx, &loomv1.StopRequest{FlowId: s.remoteFlow}); err != nil {
		s.m.log.Warn("stop remote app flow", "id", s.id, "flow", s.remoteFlow, "err", err)
	}
	rcancel()

	// Let the telemetry stream deliver the remote FINAL sample, bounded.
	select {
	case <-tdone:
	case <-time.After(s.tun.stopWait):
	}
	s.tcancel()
	<-tdone

	s.rx.SetEndMarkerFunc(nil)
	s.unsubHub()
	// Settle any open correlation window (emits its annotation through
	// publish, so this must precede finalized=true) and drop the session's
	// Prometheus series — a gauge frozen at its last MOS would read as a
	// live call forever.
	if s.corr != nil {
		s.corr.flush(time.Now())
	}
	s.m.appMetrics.cleanupSession(s.supi, s.cfg.App)
	_ = s.network.Close()
	_ = s.conn.Close()

	s.mu.Lock()
	s.ended = time.Now()
	s.finalized = true
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	s.mu.Unlock()
}

// timesyncLoop feeds the owd.Tracker from four-timestamp exchanges over the
// management connection (never the tunnel — design §5), a short seeding burst
// first and then every syncInterval.
func (s *appSession) timesyncLoop() {
	sync1 := func() {
		ctx, cancel := context.WithTimeout(s.ctx, s.tun.rpcTimeout)
		defer cancel()
		if smp, err := control.Sync(ctx, s.agent); err == nil {
			s.tracker.Feed(smp, time.Now())
		}
	}
	for i := 0; i < s.tun.syncBurst && s.ctx.Err() == nil; i++ {
		sync1()
	}
	t := time.NewTicker(s.tun.syncInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			sync1()
		}
	}
}

// sampleLoop reads the local client's quality snapshot once per interval.
// This loop is the engine's only Metrics() consumer, so each read cleanly
// closes one observation interval (the appRunner discipline, single-reader).
func (s *appSession) sampleLoop() {
	src, ok := s.client.(metrics.Source)
	if !ok {
		return
	}
	t := time.NewTicker(s.tun.sampleInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if v, ok := src.Metrics().(metrics.VoIP); ok {
				vc := v
				s.publish(AppSample{Time: time.Now(), TimeSource: "local", End: AppEndUE, VoIP: &vc})
			}
		}
	}
}

// eventLoop forwards this UE's hub state events (handover phases, path
// switch, deregistration) onto the session stream as correlation events.
func (s *appSession) eventLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case ev, ok := <-s.hubCh:
			if !ok {
				return
			}
			if ev.SUPI != s.supi {
				continue
			}
			s.publish(AppSample{Time: ev.Time, TimeSource: "local", Event: ev.State, Detail: ev.Detail})
		}
	}
}

// telemetryLoop subscribes the remote agent's StreamTelemetry for the far-end
// flow and retains its interval series, re-stamping each sample onto orbit's
// clock via the tracker offset with the error bound carried (design §7). It
// runs on tctx, which outlives the call so the FINAL sample (emitted when the
// remote flow stops) is captured during teardown.
//
// A dropped stream (management-network blip, loomd restart) is resubscribed
// rather than abandoned — the far end's series and its FINAL whole-call
// sample are the report's only uplink evidence, and the lazy grpc connection
// reconnects transparently for the retried subscribe. Intervals already
// published are suppressed by interval_index on resubscribe.
func (s *appSession) telemetryLoop() {
	lastIdx := int64(-1) // highest interval_index already published
	for attempt := 0; s.tctx.Err() == nil; attempt++ {
		stream, err := s.agent.StreamTelemetry(s.tctx, &loomv1.TelemetryRequest{FlowId: s.remoteFlow})
		if err != nil {
			if s.tctx.Err() == nil {
				s.m.log.Warn("remote telemetry subscribe; retrying", "id", s.id, "flow", s.remoteFlow, "err", err)
			}
			select {
			case <-s.tctx.Done():
				return
			case <-time.After(s.tun.sampleInterval):
			}
			continue
		}
		if attempt > 0 {
			s.m.log.Info("remote telemetry resubscribed", "id", s.id, "flow", s.remoteFlow)
		}
		for {
			ts, err := stream.Recv()
			if err != nil {
				if s.tctx.Err() != nil {
					return // teardown closed the stream
				}
				// The agent ends the stream after the FINAL sample; reaching
				// here without one means the stream broke mid-call. Log once
				// and resubscribe (duplicates are filtered by lastIdx).
				s.m.log.Warn("remote telemetry stream ended early; resubscribing",
					"id", s.id, "flow", s.remoteFlow, "err", err)
				break
			}
			idx := ts.GetIntervalIndex()
			if !ts.GetFinal() {
				if idx <= lastIdx {
					continue // replayed interval after a resubscribe
				}
				lastIdx = idx
			}
			vp := ts.GetApp().GetVoip()
			if vp == nil {
				if ts.GetFinal() {
					return
				}
				continue
			}
			v := voipFromProto(vp)
			sample := AppSample{End: AppEndN6, Final: ts.GetFinal(), VoIP: &v}
			remoteAt := time.Unix(0, ts.GetNanos())
			if off, errBound, ok := s.tracker.Offset(); ok {
				// offset = remote − local, so local time = remote − offset.
				sample.Time = remoteAt.Add(-off)
				sample.TimeErr = errBound
				sample.TimeSource = "timesync"
			} else {
				sample.Time = remoteAt
				sample.TimeSource = "remote-clock"
			}
			if ts.GetFinal() {
				s.mu.Lock()
				s.remoteFinal = &v
				s.mu.Unlock()
				s.publish(sample)
				return // the series is complete
			}
			s.publish(sample)
		}
	}
}

// publish retains a sample in the right series, fans it out to subscribers
// without blocking (full subscribers drop, hub-style), then feeds the
// retained sample to the correlator and the Prometheus gauges. The post-hook
// runs outside s.mu: a correlator emission re-enters publish (annotations
// are stream samples too), and the correlator ignores its own output.
func (s *appSession) publish(a AppSample) {
	s.mu.Lock()
	if s.finalized {
		s.mu.Unlock()
		return
	}
	switch {
	case a.Event != "":
		s.events = append(s.events, a)
	case a.End == AppEndN6:
		s.remoteSeries = append(s.remoteSeries, a)
	default:
		s.localSeries = append(s.localSeries, a)
	}
	for _, ch := range s.subs {
		select {
		case ch <- a:
		default: // observability never backpressures the session
		}
	}
	s.mu.Unlock()

	var gaps []mediaGap
	if s.corr != nil {
		gaps = s.corr.observe(a)
	}
	am := s.m.appMetrics // nil-safe no-op receiver when metrics are disabled
	am.observeSample(s.supi, s.cfg.App, a)
	for _, g := range gaps {
		am.observeGap(s.supi, s.cfg.App, g.End, g.Dur)
	}
}

// subscribe returns a live sample channel and its cancel. A finished session
// yields a closed channel.
func (s *appSession) subscribe() (<-chan AppSample, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan AppSample, 256)
	if s.finalized {
		close(ch)
		return ch, func() {}
	}
	id := s.subSeq
	s.subSeq++
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

// report snapshots the finished session.
func (s *appSession) report() AppSessionReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := AppSessionReport{
		ID:           s.id,
		SUPI:         s.supi,
		App:          s.cfg.App,
		PeerAgent:    s.cfg.PeerAgent,
		DataPort:     s.dataPort,
		Started:      s.started,
		Ended:        s.ended,
		Local:        s.localFinal,
		Remote:       s.remoteFinal,
		LocalSeries:  append([]AppSample(nil), s.localSeries...),
		RemoteSeries: append([]AppSample(nil), s.remoteSeries...),
		Events:       append([]AppSample(nil), s.events...),
	}
	for _, g := range s.localFinal.MediaGaps {
		r.MediaGaps = append(r.MediaGaps, MediaGapReport{
			End: AppEndUE, Start: g.Start, Stop: g.End,
			PacketsLost: g.PacketsLost, Clock: "local",
		})
	}
	if s.remoteFinal != nil {
		var off, errBound time.Duration
		var synced bool
		if s.tracker != nil {
			off, errBound, synced = s.tracker.Offset()
		}
		for _, g := range s.remoteFinal.MediaGaps {
			mg := MediaGapReport{
				End: AppEndN6, Start: g.Start, Stop: g.End,
				PacketsLost: g.PacketsLost, Clock: "remote-clock",
			}
			if synced {
				// offset = remote − local, so local time = remote − offset.
				mg.Start, mg.Stop = g.Start.Add(-off), g.End.Add(-off)
				mg.Clock, mg.TimeErr = "timesync", errBound
			}
			r.MediaGaps = append(r.MediaGaps, mg)
		}
	}
	if s.corr != nil {
		r.Annotations = s.corr.list()
	}
	if s.runErr != nil {
		r.Err = s.runErr.Error()
	}
	return r
}

// agentServesApp is the per-side capabilities check: an agent advertising the
// per-side lists (loom >= v0.10 slimmed builds included) is gated on
// apps_server; agents predating those fields fall back to the union list —
// the same discipline as loom's own controller gate.
func agentServesApp(caps *loomv1.CapabilitiesResponse, app string) bool {
	if len(caps.GetAppsServer()) > 0 || len(caps.GetAppsClient()) > 0 {
		return contains(caps.GetAppsServer(), app)
	}
	return contains(caps.GetApps(), app)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func cloneParams(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// voipFromProto converts the wire snapshot back into metrics.VoIP — the
// inverse of loom's voipMetricsProto mapping (fields the wire does not carry,
// e.g. Codec and TxPackets, stay zero).
func voipFromProto(p *loomv1.VoipMetrics) metrics.VoIP {
	v := metrics.VoIP{
		MOSCQ:       p.GetMosCq(),
		RFactor:     p.GetRFactor(),
		JitterMs:    p.GetJitterMs(),
		LossPct:     p.GetLossPct(),
		DiscardPct:  p.GetDiscardPct(),
		BurstR:      p.GetBurstR(),
		RTTMs:       p.GetRttMs(),
		OWDMs:       p.GetOwdMs(),
		OWDErrMs:    p.GetOwdErrMs(),
		OWDMethod:   p.GetOwdMethod(),
		RxPackets:   p.GetRxPackets(),
		Lost:        p.GetLost(),
		RemoteMOSCQ: p.GetRemoteMosCq(),
	}
	if em := p.GetEmodel(); em != nil {
		v.EModel = emodel.Components{
			Ro:    em.GetRo(),
			Is:    em.GetIs(),
			Idte:  em.GetIdte(),
			Idle:  em.GetIdle(),
			Idd:   em.GetIdd(),
			IeEff: em.GetIeEff(),
		}
	}
	for _, g := range p.GetGaps() {
		v.MediaGaps = append(v.MediaGaps, rtp.Gap{
			Start:       time.Unix(0, g.GetStartUnixNanos()),
			End:         time.Unix(0, g.GetEndUnixNanos()),
			PacketsLost: g.GetPacketsLost(),
		})
	}
	return v
}
