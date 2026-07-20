package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/coreprofile"
	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/sctp"
)

// UE lifecycle states reported on StateStream and by Status.
const (
	StateRegistering         = "REGISTERING"
	StateAuthenticated       = "AUTHENTICATED"
	StateSecurityEstablished = "SECURITY_ESTABLISHED"
	StateRegistered          = "REGISTERED"
	StateSessionActive       = "SESSION_ACTIVE"
	StateDeregistered        = "DEREGISTERED"
)

// StateEvent is one UE FSM transition, streamed to StateStream subscribers.
type StateEvent struct {
	SUPI   string
	State  string
	Detail string
	Time   time.Time
}

// Session is a registered UE holding the live N2 association and the NAS
// security context, so the UE can be inspected and deregistered later.
type Session struct {
	SUPI   string
	Result *AttachResult

	conn  gnb.Transport
	sec   *nas.SecurityContext
	amfID int64
	ranID int64
	guti  []byte
	gnbN3 string

	// stMu guards the externally observable state below. Handover mutates the
	// serving cell and the mobility phase while List/Status readers hold only
	// the Manager's map lock, which guards the map and not its values.
	stMu     sync.RWMutex
	gnbCfg   gnb.Config
	state    string
	mobState string    // last mobility phase; empty until the UE first moves
	mobAt    time.Time // when mobState was recorded

	// N3 data path, created lazily on first use over the per-gNB shared
	// tunnel pool (design §6, Phase 5): ONE socket per gNB N3 address,
	// shared by every UE on that gNB. This session holds a refcounted handle
	// (n3ref) plus its own view (ue) that stamps the session's UL TEID/QFI
	// on uplink and carries per-UE per-QFI counters; every downlink consumer
	// (ping, latency probe, media lanes) subscribes on rx — the session's
	// lane on the shared Demux, keyed by its DL TEID.
	dpMu   sync.Mutex // guards lazy create/teardown of the fields below
	n3     *n3Pool    // the Manager's shared-tunnel pool (tests inject one)
	n3ref  *sharedN3
	ue     *datapath.UETunnel
	rx     *datapath.UERx // this UE's downlink lane (keyed by DL TEID)
	rxTEID uint32         // the DL TEID rx is currently registered under
	n3Port string         // local N3 port; "" = "2152" (tests bind ephemeral)
}

// SendUplink sends one inner IP packet up the session's CURRENT tunnel,
// following handover rebinds — long-lived uplink consumers (app sessions)
// hold the Session, never a tunnel snapshot, so a data path replaced by
// rebindDataPath keeps carrying their media. *Session satisfies
// loomgtp.Uplink.
func (s *Session) SendUplink(innerIP []byte) error {
	s.dpMu.Lock()
	t := s.ue
	s.dpMu.Unlock()
	if t == nil {
		return fmt.Errorf("UE %s has no open N3 data path", s.SUPI)
	}
	return t.SendUplink(innerIP)
}

// Manager owns registered UE sessions and the StateStream event hub. It is
// the engine surface the API server calls; nothing above it touches sockets.
type Manager struct {
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session

	hub     *hub
	profile coreprofile.Profile // core-compatibility profile (default strict-3gpp)

	// n3 is the per-gNB shared N3 tunnel pool (n3pool.go): one socket per
	// gNB N3 address, refcounted across the sessions using it.
	n3 *n3Pool

	apps appTable // live application-traffic sessions (appsession.go)

	// appMetrics is the optional Prometheus surface for app sessions and
	// handover timestamps (appmetrics.go). Set once via EnableAppMetrics
	// before serving; a nil *appMetrics is a valid no-op receiver.
	appMetrics *appMetrics
}

// NewManager returns an empty session manager.
func NewManager(log *slog.Logger) *Manager {
	return &Manager{
		log:      log,
		sessions: make(map[string]*Session),
		hub:      newHub(),
		profile:  coreprofile.Default(),
		n3:       newN3Pool(),
	}
}

// Register attaches one UE: it opens an SCTP association to amfAddr, performs
// NG Setup, runs the attach FSM (emitting StateStream events), and retains
// the session. A UE already registered under the same SUPI is rejected.
func (m *Manager) Register(ctx context.Context, amfAddr string, gnbCfg gnb.Config, ueCfg UEConfig) (*AttachResult, error) {
	m.mu.Lock()
	if _, ok := m.sessions[ueCfg.Sub.SUPI]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("UE %s is already registered", ueCfg.Sub.SUPI)
	}
	m.mu.Unlock()

	conn, err := sctp.Dial("", amfAddr)
	if err != nil {
		return nil, fmt.Errorf("SCTP dial: %w", err)
	}

	ng, err := gnb.NGSetup(ctx, conn, gnbCfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("NG Setup: %w", err)
	}
	if !ng.Accepted {
		conn.Close()
		return nil, fmt.Errorf("NG Setup rejected: %s", ng.Cause)
	}

	sess, err := Attach(ctx, conn, gnbCfg, ueCfg, m.log, m.hub.publish)
	if err != nil {
		conn.Close()
		return nil, err
	}
	sess.setState(stateFromResult(sess.Result))
	sess.gnbN3 = ueCfg.GNBN3Addr
	sess.n3 = m.n3 // data paths open on the Manager's shared per-gNB pool

	m.mu.Lock()
	m.sessions[sess.SUPI] = sess
	m.mu.Unlock()
	return sess.Result, nil
}

// Status returns a snapshot of a registered UE.
func (m *Manager) Status(supi string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[supi]
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	return sess, nil
}

// List returns all registered UEs.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// Deregister sends a UE-originating (switch-off) Deregistration Request and
// tears down the association (TS 24.501 §5.5.2.2). Switch-off means the
// network sends no Deregistration Accept, so this does not block on one.
func (m *Manager) Deregister(ctx context.Context, supi string) error {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	if ok {
		delete(m.sessions, supi)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("UE %s is not registered", supi)
	}
	// End any live app sessions first, while the tunnel is still open: each
	// in-process client sends its RTCP BYE and stops its remote flow
	// best-effort before the data path is torn away (design §7).
	m.releaseAppSessions(supi)
	defer sess.conn.Close()
	sess.closeDataPath()

	if err := sess.deregister(ctx); err != nil {
		return err
	}
	m.hub.publish(StateEvent{SUPI: supi, State: StateDeregistered, Detail: "UE deregistered (switch-off)"})
	m.appMetrics.forgetUE(supi)
	return nil
}

// Subscribe returns a channel of state events and a cancel func to stop it.
func (m *Manager) Subscribe() (<-chan StateEvent, func()) {
	return m.hub.subscribe()
}

// PingResult reports an ICMP-over-N3 run.
type PingResult struct {
	Sent, Received int
	LastRTT        time.Duration
	ReplyFrom      string
}

// Ping sends count ICMP echoes from the UE through its N3 data path to dst
// and reports the results. Requires the UE to have an active session and a
// gNB N3 address (set at Register time) reachable from the UPF.
func (m *Manager) Ping(supi, dst string, count int) (*PingResult, error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	if !sess.Result.SessionActive {
		return nil, fmt.Errorf("UE %s has no active PDU session", supi)
	}
	if sess.gnbN3 == "" {
		return nil, fmt.Errorf("UE %s registered without a gNB N3 address; data path disabled", supi)
	}
	if dst == "" {
		dst = "8.8.8.8"
	}
	if count <= 0 {
		count = 3
	}
	// Open (or reuse) the data path; sends below go through sess.SendUplink —
	// never a tunnel snapshot — so a handover mid-ping follows the rebind
	// instead of writing the old gNB's socket with a stale UL TEID. The rx
	// lane is carried across rebinds, so the subscription below stays live.
	_, rx, err := sess.dataplane()
	if err != nil {
		return nil, err
	}

	ueIP := net.ParseIP(sess.Result.PDUAddress)
	dstIP := net.ParseIP(dst)
	if ueIP == nil || dstIP == nil {
		return nil, fmt.Errorf("invalid IP (ue %q dst %q)", sess.Result.PDUAddress, dst)
	}
	ring := rx.SubscribeICMP()
	defer rx.UnsubscribeICMP(ring)
	res := &PingResult{}
	for seq := 1; seq <= count; seq++ {
		req, err := datapath.BuildICMPEchoRequest(ueIP, dstIP, uint16(0xB000|seq), uint16(seq), []byte("orbit"))
		if err != nil {
			return nil, err
		}
		start := time.Now()
		if err := sess.SendUplink(req); err != nil {
			return nil, err
		}
		res.Sent++
		deadline := time.Now().Add(2 * time.Second)
		for remaining := 2 * time.Second; remaining > 0; remaining = time.Until(deadline) {
			f, err := ring.Read(remaining)
			if err != nil {
				break // timeout (or lane closed) → this echo is lost
			}
			if r, ok := datapath.MatchICMPEchoReply(f.Payload, uint16(0xB000|seq), uint16(seq)); ok {
				res.Received++
				// RTT against the demux reader's socket-read timestamp, not
				// dequeue time: the reply may have waited in the ring behind
				// other traffic (design §6 — arrival ts preserved).
				if !f.Arrival.IsZero() {
					res.LastRTT = f.Arrival.Sub(start)
				} else {
					res.LastRTT = time.Since(start)
				}
				res.ReplyFrom = r.From.String()
				break
			}
			// other ICMP traffic on the lane — keep waiting for our reply
		}
	}
	return res, nil
}

// DataStats returns a snapshot of the per-QFI uplink/downlink counters on a
// UE's N3 data path. A registered UE whose tunnel has not been opened yet
// (nothing has used the data path) reports no flows rather than an error.
func (m *Manager) DataStats(supi string) (map[uint8]datapath.QFIStatsSnapshot, error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	sess.dpMu.Lock()
	ue := sess.ue
	sess.dpMu.Unlock()
	if ue == nil {
		return map[uint8]datapath.QFIStatsSnapshot{}, nil
	}
	return ue.Stats(), nil
}

// ReleaseGNB force-closes the data paths of every session riding the given
// gNB N3 address — the gNB-removal path. A bare host matches every session
// on that N3 IP regardless of port; host:port matches only sessions whose
// N3 bind is exactly that endpoint (two gNBs sharing one N3 IP on distinct
// ports stay distinguishable). Each session's view is unregistered from the
// shared Demux, so its lanes get a closed signal: blocked downlink consumers
// wake with net.ErrClosed, running app sessions' media engines terminate
// with an error that surfaces in their report (never a silent blackhole),
// and the next SendUplink fails. When the last session releases the shared
// tunnel its socket closes. Control-plane state is untouched: sessions stay
// registered, and a later data-path use lazily re-opens against the
// session's (possibly new) gNB N3 address.
//
// No RPC/CLI reaches this yet: it is the engine seam for operational gNB
// removal (and its tests) until the API grows a gNB-lifecycle surface.
func (m *Manager) ReleaseGNB(gnbN3 string) int {
	host, port := gnbN3, ""
	if h, p, err := net.SplitHostPort(gnbN3); err == nil {
		host, port = h, p
	}
	m.mu.Lock()
	var victims []*Session
	for _, s := range m.sessions {
		if s.gnbN3 != host {
			continue
		}
		if port != "" && s.localN3() != net.JoinHostPort(host, port) {
			continue
		}
		victims = append(victims, s)
	}
	m.mu.Unlock()
	for _, s := range victims {
		s.closeDataPath()
		m.log.Warn("gNB released with a live session; N3 data path closed",
			"supi", s.SUPI, "gnb_n3", gnbN3)
	}
	return len(victims)
}

// State reports the current registration state of the session (snapshot).
//
// Registration and mobility are orthogonal axes: a UE that has handed over is
// still SESSION_ACTIVE, so a handover never changes this value. Use Mobility
// for the mobility phase.
func (s *Session) State() string {
	s.stMu.RLock()
	defer s.stMu.RUnlock()
	return s.state
}

// ServingGNB reports the cell currently serving the UE (snapshot). It follows
// the UE across handovers, so it answers "where is this UE now?".
func (s *Session) ServingGNB() gnb.Config {
	s.stMu.RLock()
	defer s.stMu.RUnlock()
	return s.gnbCfg
}

// Mobility reports the last mobility phase the UE reached and when. The phase
// is empty, with a zero time, until the UE first moves.
//
// A failed handover is visible here as StateHandoverFailed while State still
// reports SESSION_ACTIVE — the combination a polling client needs in order to
// tell a UE with a broken path from a healthy one.
func (s *Session) Mobility() (phase string, at time.Time) {
	s.stMu.RLock()
	defer s.stMu.RUnlock()
	return s.mobState, s.mobAt
}

// setState records the registration state.
func (s *Session) setState(state string) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	s.state = state
}

// setServingGNB moves the session to a new serving cell.
func (s *Session) setServingGNB(cfg gnb.Config) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	s.gnbCfg = cfg
}

// setMobility records a mobility phase and its instant.
func (s *Session) setMobility(phase string, at time.Time) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	s.mobState, s.mobAt = phase, at
}

// tunnel lazily creates (and caches) the session's uplink/stats view of its
// gNB's shared N3 tunnel.
func (s *Session) tunnel() (*datapath.UETunnel, error) {
	t, _, err := s.dataplane()
	return t, err
}

// dataplane lazily creates the session's N3 data path on the per-gNB shared
// tunnel: the first session on a gNB N3 address binds the ONE socket (pool
// acquire), later sessions share it — no second 2152 bind, ever. The
// session's view stamps its UL TEID/QFI on uplink; its downlink lane is
// registered on the shared Demux under its DL TEID. Returns the uplink view
// and the downlink lane.
func (s *Session) dataplane() (*datapath.UETunnel, *datapath.UERx, error) {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	if s.ue != nil {
		return s.ue, s.rx, nil
	}
	ref, ue, err := s.openViewLocked(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	s.n3ref, s.ue, s.rx, s.rxTEID = ref, ue, ue.Lane(), s.Result.DLTEID
	return s.ue, s.rx, nil
}

// localN3 is the gNB N3 bind address for the session's CURRENT gNB — the
// shared-tunnel pool key.
func (s *Session) localN3() string {
	port := s.n3Port
	if port == "" {
		port = "2152"
	}
	return net.JoinHostPort(s.gnbN3, port)
}

// upfN3 is the UPF N3 endpoint (bare IP normally; host:port respected).
func (s *Session) upfN3() string {
	upfN3 := s.Result.UPFAddress
	if _, _, err := net.SplitHostPort(upfN3); err != nil {
		upfN3 = net.JoinHostPort(upfN3, "2152")
	}
	return upfN3
}

// openViewLocked acquires the shared tunnel for the session's CURRENT gNB N3
// address and registers this UE's view on it, optionally carrying a live
// lane and counters (the inter-gNB handover move). Callers hold dpMu; on
// error nothing is retained (the acquired ref is released).
func (s *Session) openViewLocked(lane *datapath.UERx, stats *datapath.UEStats) (*sharedN3, *datapath.UETunnel, error) {
	if s.n3 == nil {
		return nil, nil, fmt.Errorf("UE %s has no N3 tunnel pool (session is not manager-owned)", s.SUPI)
	}
	ref, err := s.n3.acquire(s.localN3(), s.upfN3())
	if err != nil {
		return nil, nil, fmt.Errorf("open N3 data path: %w", err)
	}
	ue, err := ref.st.Register(datapath.UETunnelConfig{
		ULTEID: s.Result.UPFTEID,
		DLTEID: s.Result.DLTEID,
		QFI:    s.Result.QFI,
		UPFN3:  s.upfN3(),
		Lane:   lane,
		Stats:  stats,
	})
	if err != nil {
		s.n3.release(ref)
		return nil, nil, fmt.Errorf("open N3 data path: %w", err)
	}
	return ref, ue, nil
}

// teardownLocked releases the session's view (closing its lane, waking
// downlink consumers with net.ErrClosed) and its pool reference — the shared
// socket closes only when the last session on the gNB lets go. Callers hold
// dpMu.
func (s *Session) teardownLocked() {
	if s.ue != nil {
		s.ue.Close()
	}
	if s.n3 != nil {
		s.n3.release(s.n3ref)
	}
	s.n3ref, s.ue, s.rx, s.rxTEID = nil, nil, nil, 0
}

// closeDataPath tears down the lazily-created N3 data path (this session's
// view and lane, plus the shared socket if this was its last user) so the
// next use re-opens against the session's current gNB N3 address and TEIDs
// — used on deregistration and gNB removal. Handovers use rebindDataPath
// instead: closing would kill the rings live app sessions consume,
// permanently (loom's dgram receive loop exits on net.ErrClosed).
func (s *Session) closeDataPath() {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	s.teardownLocked()
}

// dataPathMove is a handover's data-path retarget: the new gNB N3 address
// and DL TEID, plus (when the core reallocated the UPF's UL F-TEID at path
// switch — TS 38.413 allows it) the new uplink endpoint. Zero values keep
// the current setting.
type dataPathMove struct {
	gnbN3   string // new gNB N3 address ("" = unchanged)
	dlTEID  uint32 // new gNB downlink TEID (0 = unchanged)
	upfTEID uint32 // new UPF uplink TEID (0 = unchanged)
	upfN3   string // new UPF N3 address ("" = unchanged)
}

// retargetDataPath applies a handover's data-path move: it updates the
// session's data-path identity (gnbN3, Result's TEIDs/UPF address) and
// rebinds any open path, all under dpMu — the same lock dataplane() and
// SendUplink readers take — so a concurrent Ping/Traffic/app-session can
// never observe a torn pair (e.g. the new DL TEID with the old gNB address,
// which would register the new TEID's lane on the OLD gNB's demux).
func (s *Session) retargetDataPath(mv dataPathMove) error {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	if mv.gnbN3 != "" {
		s.gnbN3 = mv.gnbN3
	}
	if mv.dlTEID != 0 {
		s.Result.DLTEID = mv.dlTEID
	}
	if mv.upfTEID != 0 {
		s.Result.UPFTEID = mv.upfTEID
	}
	if mv.upfN3 != "" {
		s.Result.UPFAddress = mv.upfN3
	}
	return s.rebindLocked()
}

// rebindDataPath moves an OPEN data path onto the session's current gNB N3
// address and TEIDs (already stored in s.gnbN3/s.Result by retargetDataPath,
// the production entry point; tests that stage the fields directly call this).
func (s *Session) rebindDataPath() error {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	return s.rebindLocked()
}

// rebindLocked is the data-path swap the consumers never notice (design §6).
// Intra-gNB (same N3 address) it retargets the uplink (UL TEID / UPF may
// have moved at path switch) and atomically swaps the DL TEID on the shared
// Demux (Demux.Rebind) — same socket, same view, media rings untouched.
// Inter-gNB, the live UERx lane — the media rings, ICMP subscriptions, and
// End-Marker callback of any running app session or probe — is detached
// from the source tunnel's demux and attached to the target gNB's shared
// tunnel under the new TEID (per-UE counters carried), so downlink
// consumers see only a gap (the handover's real media gap), never a closed
// ring; uplink continuity comes from Session.SendUplink resolving the
// current view per packet. The source tunnel ref is released after the
// End-Marker grace window, NOT closed — other UEs on that gNB keep their
// data paths, and even when the mover was the LAST UE the source socket
// outlives the move long enough for the UPF's post-path-switch End Marker
// (TS 29.281 §7.3) to reach the tombstoned lane. With no data path open it
// is a no-op: the next use lazily opens against the new target. On a
// bind/registration failure it degrades to closeDataPath semantics
// (consumers see closed lanes, never a silent blackhole) and returns the
// error. Callers hold dpMu.
func (s *Session) rebindLocked() error {
	if s.ue == nil {
		return nil // nothing open; lazy re-open handles the move
	}
	if s.n3ref != nil && s.n3ref.key == s.localN3() {
		// Intra-gNB: retarget uplink, then the atomic DL TEID swap.
		if err := s.ue.SetUplink(s.Result.UPFTEID, s.upfN3()); err != nil {
			s.teardownLocked()
			return err
		}
		if s.rxTEID == s.Result.DLTEID {
			return nil
		}
		if err := s.ue.Rebind(s.Result.DLTEID); err != nil {
			// Target TEID claimed (another UE on this gNB): fail like the
			// old teardown — consumers see closed lanes, not a stale path.
			s.teardownLocked()
			return err
		}
		s.rxTEID = s.Result.DLTEID
		return nil
	}
	// Inter-gNB: carry the live lane and counters to the target tunnel.
	lane, stats := s.ue.Detach()
	ref, ue, err := s.openViewLocked(lane, stats)
	if err != nil {
		// Cannot move: close the carried lane so its consumers wake, then
		// tear down like the old path did.
		lane.Close()
		s.teardownLocked()
		return err
	}
	old := s.n3ref
	s.n3ref, s.ue, s.rx, s.rxTEID = ref, ue, lane, s.Result.DLTEID
	if s.n3 != nil {
		// Grace-release: hold the source socket through the End-Marker
		// window so the tombstoned TEID's marker still lands even when this
		// was the source gNB's last UE.
		s.n3.releaseAfterGrace(old)
	}
	return nil
}

func stateFromResult(r *AttachResult) string {
	if r.SessionActive {
		return StateSessionActive
	}
	if r.Registered {
		return StateRegistered
	}
	return StateRegistering
}

// deregister sends a UE-originating switch-off Deregistration Request under
// the session's security context.
func (s *Session) deregister(ctx context.Context) error {
	if s.sec == nil {
		return fmt.Errorf("no security context")
	}
	if len(s.guti) == 0 {
		return fmt.Errorf("no 5G-GUTI (AMF assigned none in Registration Accept)")
	}
	msg, err := ueBuildDeregistration(s.guti)
	if err != nil {
		return err
	}
	wrapped, err := s.sec.EncodeSecure(msg, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return fmt.Errorf("wrap Deregistration Request: %w", err)
	}
	pdu, err := gnb.BuildUplinkNASTransport(s.gnbCfg, s.amfID, s.ranID, wrapped)
	if err != nil {
		return err
	}
	return gnb.SendPDU(s.conn, ueStream, pdu)
}
