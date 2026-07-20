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

	gnbCfg gnb.Config
	conn   gnb.Transport
	sec    *nas.SecurityContext
	amfID  int64
	ranID  int64
	guti   []byte
	state  string
	gnbN3  string

	// N3 data path, created lazily on first use. The Demux owns the socket's
	// downlink read path (design §6): every downlink consumer (ping, latency
	// probe, media lanes) subscribes on rx; nothing calls ReadDownlink.
	dpMu     sync.Mutex // guards lazy create/teardown of the fields below
	dataPath *datapath.Tunnel
	demux    *datapath.Demux
	rx       *datapath.UERx // this UE's downlink lane (keyed by DL TEID)
	rxTEID   uint32         // the DL TEID rx is currently registered under
	n3Port   string         // local N3 port; "" = "2152" (tests bind ephemeral)
}

// SendUplink sends one inner IP packet up the session's CURRENT tunnel,
// following handover rebinds — long-lived uplink consumers (app sessions)
// hold the Session, never a tunnel snapshot, so a data path replaced by
// rebindDataPath keeps carrying their media. *Session satisfies
// loomgtp.Uplink.
func (s *Session) SendUplink(innerIP []byte) error {
	s.dpMu.Lock()
	t := s.dataPath
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
	sess.state = stateFromResult(sess.Result)
	sess.gnbN3 = ueCfg.GNBN3Addr

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
	tun, rx, err := sess.dataplane()
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
		if err := tun.SendUplink(req); err != nil {
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
	tun := sess.dataPath
	sess.dpMu.Unlock()
	if tun == nil {
		return map[uint8]datapath.QFIStatsSnapshot{}, nil
	}
	return tun.Stats(), nil
}

// State reports the current lifecycle state of the session (snapshot).
func (s *Session) State() string { return s.state }

// tunnel lazily creates (and caches) the N3 tunnel for this session.
func (s *Session) tunnel() (*datapath.Tunnel, error) {
	t, _, err := s.dataplane()
	return t, err
}

// dataplane lazily creates the session's N3 data path: the tunnel, its Demux
// (which owns the downlink read path — no new 2152 bind, the demux layers on
// the tunnel's socket), and this UE's downlink lane registered under its DL
// TEID. Returns the uplink (tunnel) and the downlink lane.
func (s *Session) dataplane() (*datapath.Tunnel, *datapath.UERx, error) {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	if s.dataPath != nil {
		return s.dataPath, s.rx, nil
	}
	t, err := s.openTunnelLocked()
	if err != nil {
		return nil, nil, err
	}
	s.dataPath = t
	s.demux = t.Demux()
	s.rx = s.demux.Register(s.Result.DLTEID)
	s.rxTEID = s.Result.DLTEID
	return t, s.rx, nil
}

// openTunnelLocked binds a tunnel against the session's CURRENT gNB N3
// address and TEIDs. Callers hold dpMu.
func (s *Session) openTunnelLocked() (*datapath.Tunnel, error) {
	port := s.n3Port
	if port == "" {
		port = "2152"
	}
	upfN3 := s.Result.UPFAddress // bare IP normally; host:port respected
	if _, _, err := net.SplitHostPort(upfN3); err != nil {
		upfN3 = net.JoinHostPort(upfN3, "2152")
	}
	t, err := datapath.NewTunnel(datapath.Config{
		LocalN3: net.JoinHostPort(s.gnbN3, port),
		UPFN3:   upfN3,
		ULTEID:  s.Result.UPFTEID,
		DLTEID:  s.Result.DLTEID,
		QFI:     s.Result.QFI,
	})
	if err != nil {
		return nil, fmt.Errorf("open N3 data path: %w", err)
	}
	return t, nil
}

// closeDataPath tears down the lazily-created N3 data path (tunnel, demux,
// downlink lane) so the next use re-opens it against the session's current
// gNB N3 address and TEIDs — used on deregistration. Handovers use
// rebindDataPath instead: closing would kill the rings live app sessions
// consume, permanently (loom's dgram receive loop exits on net.ErrClosed).
func (s *Session) closeDataPath() {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	if s.dataPath != nil {
		s.dataPath.Close() // stops the demux reader
	}
	if s.demux != nil {
		s.demux.Close() // waits for the reader to exit
	}
	s.dataPath, s.demux, s.rx, s.rxTEID = nil, nil, nil, 0
}

// rebindDataPath moves an OPEN data path onto the session's current gNB N3
// address and DL TEID after a handover (design §6: the data-path swap the
// consumers never notice). The live UERx lane — the media rings, ICMP
// subscriptions, and End-Marker callback of any running app session or probe
// — is detached from the old demux BEFORE that demux dies, then attached to
// the new tunnel's demux under the new TEID, so downlink consumers see only
// a gap (the handover's real media gap), never a closed ring; uplink
// continuity comes from Session.SendUplink resolving the current tunnel per
// packet. With no data path open it is a no-op: the next use lazily opens
// against the new target. On a bind failure it degrades to closeDataPath
// semantics and returns the error.
func (s *Session) rebindDataPath() error {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	if s.dataPath == nil {
		return nil // nothing open; lazy re-open handles the move
	}
	t, err := s.openTunnelLocked()
	if err != nil {
		// Cannot bind the target: fail like the old teardown did (consumers
		// see closed lanes) rather than keep media on the stale path.
		s.dataPath.Close()
		s.demux.Close()
		s.dataPath, s.demux, s.rx, s.rxTEID = nil, nil, nil, 0
		return err
	}
	// Carry the live lane across. Detach first so the old demux's teardown
	// cannot close its rings; the old reader exits (socket close) before the
	// new demux dispatches, so the lane never has two writers.
	rx, ok := s.demux.Detach(s.rxTEID)
	s.dataPath.Close()
	s.demux.Close()
	s.dataPath = t
	s.demux = t.Demux()
	if !ok || rx == nil {
		rx = s.demux.Register(s.Result.DLTEID)
	} else if err := s.demux.Attach(s.Result.DLTEID, rx); err != nil {
		// Unreachable on a fresh demux; keep the lane alive regardless.
		return err
	}
	s.rx = rx
	s.rxTEID = s.Result.DLTEID
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
