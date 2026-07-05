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

	gnbCfg   gnb.Config
	conn     gnb.Transport
	sec      *nas.SecurityContext
	amfID    int64
	ranID    int64
	guti     []byte
	state    string
	gnbN3    string
	dataPath *datapath.Tunnel // lazily created for Ping
}

// Manager owns registered UE sessions and the StateStream event hub. It is
// the engine surface the API server calls; nothing above it touches sockets.
type Manager struct {
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session

	hub     *hub
	profile coreprofile.Profile // core-compatibility profile (default strict-3gpp)
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
	defer sess.conn.Close()
	if sess.dataPath != nil {
		sess.dataPath.Close()
	}

	if err := sess.deregister(ctx); err != nil {
		return err
	}
	m.hub.publish(StateEvent{SUPI: supi, State: StateDeregistered, Detail: "UE deregistered (switch-off)"})
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
	tun, err := sess.tunnel()
	if err != nil {
		return nil, err
	}

	ueIP := net.ParseIP(sess.Result.PDUAddress)
	dstIP := net.ParseIP(dst)
	if ueIP == nil || dstIP == nil {
		return nil, fmt.Errorf("invalid IP (ue %q dst %q)", sess.Result.PDUAddress, dst)
	}
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
		inner, err := tun.ReadDownlink(2 * time.Second)
		if err != nil {
			continue
		}
		if r, ok := datapath.MatchICMPEchoReply(inner, uint16(0xB000|seq), uint16(seq)); ok {
			res.Received++
			res.LastRTT = time.Since(start)
			res.ReplyFrom = r.From.String()
		}
	}
	return res, nil
}

// State reports the current lifecycle state of the session (snapshot).
func (s *Session) State() string { return s.state }

// tunnel lazily creates (and caches) the N3 tunnel for this session.
func (s *Session) tunnel() (*datapath.Tunnel, error) {
	if s.dataPath != nil {
		return s.dataPath, nil
	}
	t, err := datapath.NewTunnel(datapath.Config{
		LocalN3: net.JoinHostPort(s.gnbN3, "2152"),
		UPFN3:   net.JoinHostPort(s.Result.UPFAddress, "2152"),
		ULTEID:  s.Result.UPFTEID,
		DLTEID:  s.Result.DLTEID,
		QFI:     s.Result.QFI,
	})
	if err != nil {
		return nil, fmt.Errorf("open N3 data path: %w", err)
	}
	s.dataPath = t
	return t, nil
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
