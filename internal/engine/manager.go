package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	conn   *sctp.Conn
	sec    *nas.SecurityContext
	amfID  int64
	ranID  int64
	guti   []byte
	state  string
}

// Manager owns registered UE sessions and the StateStream event hub. It is
// the engine surface the API server calls; nothing above it touches sockets.
type Manager struct {
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session

	hub *hub
}

// NewManager returns an empty session manager.
func NewManager(log *slog.Logger) *Manager {
	return &Manager{
		log:      log,
		sessions: make(map[string]*Session),
		hub:      newHub(),
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

// State reports the current lifecycle state of the session (snapshot).
func (s *Session) State() string { return s.state }

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
