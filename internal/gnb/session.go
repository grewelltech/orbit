package gnb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Session multiplexes many UEs over one gNB↔AMF association. A real gNB uses
// a single N2 SCTP association for its whole UE population; ORBIT does the
// same (one Session per gNB). NG Setup runs once; a single read loop then
// owns the association's reads and dispatches every downlink PDU, and each
// UE drives its procedures through a per-UE Transport (a lane) backed by the
// shared association. Works over any gnb.Transport (real SCTP or the
// in-process pipe), per D-6's decision to test the actor model both ways.
//
// Dispatch rules:
//   - Routing: a UE-associated PDU goes to the lane of its RAN-UE-NGAP-ID;
//     one that carries no RAN-UE-NGAP-ID goes by its AMF-UE-NGAP-ID, which
//     the loop learns from the PDUs it has routed by RAN ID. A non-UE-
//     associated PDU goes to the handler set with SetNonUEHandler. Whatever
//     is left goes to the default lane of a dedicated association
//     (Dedicated), where it went before the loop existed, and is otherwise
//     dropped.
//   - Backpressure: lanes are bounded and never blocked on. A PDU whose lane
//     is full, or which has no consumer, is dropped, counted (Dropped) and
//     logged at debug level, so one slow consumer cannot stall the
//     association.
//   - Shutdown: when the association closes, every lane's reader gets
//     ErrAssociationClosed (wrapping the cause) once it has drained what was
//     already queued, and nothing reads the association after that.
//   - Ordering: one goroutine reads and FIFO lanes carry, so the PDUs of one
//     UE arrive in the order the AMF sent them.
type Session struct {
	cfg     Config
	tr      Transport
	streams uint16
	log     *slog.Logger

	ranSeq  atomic.Int64
	dropped atomic.Int64

	mu      sync.Mutex
	inboxes map[int64]chan []byte
	amfIdx  map[int64]int64         // AMF-UE-NGAP-ID → RAN-UE-NGAP-ID, as learned
	amfOf   map[int64]int64         // the reverse, so a closing lane forgets its pairing
	def     chan []byte             // a dedicated association's lane; nil when shared
	nonUE   func(*ngapType.NGAPPDU) // non-UE-associated PDUs; nil drops them
	cause   error                   // why the association closed

	closed chan struct{}
	once   sync.Once
}

// ErrAssociationClosed is the error a lane's reads return once the
// association has closed and the lane is drained; it wraps the cause.
var ErrAssociationClosed = errors.New("N2 association closed")

// laneCapacity bounds each lane. A UE's procedures exchange a handful of
// PDUs at a time, so a full lane means its consumer has stopped reading.
const laneCapacity = 64

// A lane carries each downlink message's payload as read, decoded by the
// lane's reader, so a dedicated association reports an undecodable PDU to
// its procedure as a direct read did. A message with a PPID other than NGAP
// travels as a nil payload and reads back as a non-NGAP PPID error.

// Dial establishes a gNB session over an existing association and performs
// NG Setup. tr is the shared association (e.g. sctp.Dial result). On success
// a background read loop is running.
func Dial(ctx context.Context, tr Transport, cfg Config) (*Session, error) {
	ng, err := NGSetup(ctx, tr, cfg)
	if err != nil {
		return nil, err
	}
	if !ng.Accepted {
		return nil, fmt.Errorf("NG Setup rejected: %s", ng.Cause)
	}
	s := newSession(tr, cfg)
	go s.readLoop()
	return s, nil
}

// newSession builds the session of an association whose NG Setup is done.
// The caller starts its read loop once every destination the loop must route
// to is registered, so no PDU already waiting on the association is dropped.
func newSession(tr Transport, cfg Config) *Session {
	// Spread UEs across the streams the association actually negotiated;
	// the peer may grant fewer than requested. Transports without streams
	// (the in-process pipe) report 1 via the default.
	streams := uint16(1)
	if sc, ok := tr.(interface{ OutStreams() uint16 }); ok {
		if n := sc.OutStreams(); n > 0 {
			streams = n
		}
	}
	s := &Session{
		cfg: cfg, tr: tr, streams: streams, log: slog.New(slog.DiscardHandler),
		inboxes: make(map[int64]chan []byte), amfIdx: make(map[int64]int64), amfOf: make(map[int64]int64), closed: make(chan struct{}),
	}
	return s
}

// Dedicated wraps an association that carries one UE, after its NG Setup,
// and returns that UE's lane: the default lane, so every PDU no other lane
// claims reaches it, as a direct read of the association would. Writes keep
// the caller's stream, and closing the lane closes the association.
func Dedicated(tr Transport, cfg Config, ranID int64) *UETransport {
	s := newSession(tr, cfg)
	t := s.lane(ranID, 0)
	t.owned, t.passStream = true, true
	s.mu.Lock()
	s.def = t.inbox
	s.mu.Unlock()
	go s.readLoop()
	return t
}

// SetLogger sets where the read loop logs dropped PDUs (debug level).
func (s *Session) SetLogger(l *slog.Logger) {
	if l != nil {
		s.mu.Lock()
		s.log = l
		s.mu.Unlock()
	}
}

// SetNonUEHandler sets the handler for non-UE-associated PDUs. It runs on
// the read loop and must not block; nil drops them (counted).
func (s *Session) SetNonUEHandler(h func(*ngapType.NGAPPDU)) {
	s.mu.Lock()
	s.nonUE = h
	s.mu.Unlock()
}

// Dropped reports how many downlink PDUs the loop has dropped: no lane or
// handler for them, a full lane, or one it could not decode on a shared
// association.
func (s *Session) Dropped() int64 { return s.dropped.Load() }

// Config returns the gNB configuration this session was set up with.
func (s *Session) Config() Config { return s.cfg }

// UETransport is a per-UE view of the shared association: writes go out on
// the UE's assigned SCTP stream; reads come from the UE's lane.
type UETransport struct {
	s          *Session
	ranID      int64
	stream     uint16
	inbox      chan []byte
	owned      bool // Close closes the association too (Dedicated)
	passStream bool // writes keep the caller's stream (Dedicated)
}

// NewUE assigns a fresh RAN-UE-NGAP-ID and returns a Transport plus that ID
// (pass it as UEConfig.RANUENGAPID so the Initial UE Message matches the
// demux key). UEs are spread across the association's SCTP streams to avoid
// head-of-line blocking; demux is by NGAP ID, so sharing a stream is fine.
func (s *Session) NewUE() (*UETransport, int64) {
	ranID := s.ranSeq.Add(1)
	stream := uint16(1)
	if s.streams > 1 {
		stream = uint16(1 + (ranID-1)%int64(s.streams-1))
	}
	return s.lane(ranID, stream), ranID
}

// lane registers a lane for ranID.
func (s *Session) lane(ranID int64, stream uint16) *UETransport {
	ch := make(chan []byte, laneCapacity)
	s.mu.Lock()
	s.inboxes[ranID] = ch
	s.mu.Unlock()
	return &UETransport{s: s, ranID: ranID, stream: stream, inbox: ch}
}

func (t *UETransport) WriteNGAP(stream uint16, pdu []byte) error {
	if !t.passStream {
		stream = t.stream
	}
	return t.s.tr.WriteNGAP(stream, pdu)
}

// ReadMsg returns the lane's next message, blocking until one is queued or
// the association closes (ErrAssociationClosed, after the queue drains).
func (t *UETransport) ReadMsg(_ []byte) (payload []byte, stream uint16, ppid uint32, err error) {
	b, err := t.next(nil)
	if err != nil {
		return nil, 0, 0, err
	}
	if b == nil {
		return nil, t.stream, 0, nil // a non-NGAP PPID
	}
	return b, t.stream, sctp.PPIDNGAP, nil
}

// ReadPDU is gnb.ReadPDU for a lane: it waits for the lane's next message
// or ctx, and a wait that ends on ctx leaves no read behind, so the next
// PDU is not lost.
func (t *UETransport) ReadPDU(ctx context.Context) (*ngapType.NGAPPDU, error) {
	b, err := t.next(ctx.Done())
	if err != nil {
		if errors.Is(err, errLaneWait) {
			return nil, fmt.Errorf("waiting for NGAP PDU: %w", ctx.Err())
		}
		return nil, err
	}
	if b == nil {
		return nil, errors.New("non-NGAP PPID")
	}
	return ngap.Decode(b)
}

// errLaneWait marks a lane wait ended by its caller.
var errLaneWait = errors.New("lane wait ended")

// next takes the lane's next frame: a queued one first, then whichever of
// a new frame, done or the association's close comes first.
func (t *UETransport) next(done <-chan struct{}) ([]byte, error) {
	select {
	case b := <-t.inbox:
		return b, nil
	default:
	}
	select {
	case b := <-t.inbox:
		return b, nil
	case <-done:
		return nil, errLaneWait
	case <-t.s.closed:
		select {
		case b := <-t.inbox: // queued before the close
			return b, nil
		default:
		}
		return nil, t.s.closeErr()
	}
}

// Drain drops the PDUs waiting in the UE's inbox, without blocking, and
// returns how many: what arrived before a request cannot be its answer.
func (t *UETransport) Drain() int {
	n := 0
	for {
		select {
		case <-t.inbox:
			n++
		default:
			return n
		}
	}
}

// LocalAddr is the association's local address, when it has one.
func (t *UETransport) LocalAddr() net.Addr {
	if la, ok := t.s.tr.(interface{ LocalAddr() net.Addr }); ok {
		return la.LocalAddr()
	}
	return nil
}

// Close unregisters the UE from the session demux. A dedicated lane closes
// the association too; a shared one leaves it up for the other UEs.
func (t *UETransport) Close() error {
	t.s.mu.Lock()
	if t.s.inboxes[t.ranID] == t.inbox {
		delete(t.s.inboxes, t.ranID)
		if amfID, ok := t.s.amfOf[t.ranID]; ok {
			delete(t.s.amfOf, t.ranID)
			if t.s.amfIdx[amfID] == t.ranID {
				delete(t.s.amfIdx, amfID)
			}
		}
	}
	t.s.mu.Unlock()
	if t.owned {
		return t.s.Close()
	}
	return nil
}

// NumUEs reports how many UE demux lanes are currently registered on the
// session. A steadily growing count across handovers indicates leaked handles.
func (s *Session) NumUEs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inboxes)
}

// Close tears down the session and its shared association, waking all UEs.
func (s *Session) Close() error {
	err := s.tr.Close()
	s.shutdown(fmt.Errorf("closed locally"))
	return err
}

// shutdown records why the association closed and wakes every lane.
func (s *Session) shutdown(cause error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.cause = cause
		s.mu.Unlock()
		close(s.closed)
	})
}

func (s *Session) closeErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Errorf("%w: %w", ErrAssociationClosed, s.cause)
}

func (s *Session) readLoop() {
	buf := make([]byte, 65536)
	for {
		payload, _, ppid, err := s.tr.ReadMsg(buf)
		if err != nil {
			s.shutdown(err)
			return
		}
		s.dispatch(append([]byte(nil), payload...), ppid)
	}
}

// dispatch routes one message by the rules on Session.
func (s *Session) dispatch(payload []byte, ppid uint32) {
	var pdu *ngapType.NGAPPDU
	if ppid == sctp.PPIDNGAP || ppid == sctp.PPIDNGAPSwapped {
		pdu, _ = ngap.Decode(payload)
	} else {
		payload = nil
	}
	s.mu.Lock()
	var ch chan []byte
	var handler func(*ngapType.NGAPPDU)
	if pdu == nil {
		// A bad PPID or an undecodable PDU: only a dedicated association's
		// procedure, which would have read it directly, gets it.
		ch = s.def
	} else {
		if ranID, ok := ranUENGAPIDOf(pdu); ok {
			ch = s.inboxes[ranID]
			if ch != nil {
				if amfID, ok := amfUENGAPIDOf(pdu); ok {
					s.learnLocked(amfID, ranID)
				}
			}
		} else if amfID, ok := amfUENGAPIDOf(pdu); ok {
			if ranID, ok := s.amfIdx[amfID]; ok {
				ch = s.inboxes[ranID]
			}
		} else if s.nonUE != nil {
			handler = s.nonUE
		}
		if ch == nil && handler == nil {
			ch = s.def
		}
	}
	log := s.log
	s.mu.Unlock()
	if handler != nil {
		handler(pdu)
		return
	}
	if ch != nil {
		select {
		case ch <- payload:
			return
		default:
		}
	}
	n := s.dropped.Add(1)
	log.Debug("gnb: dropped a downlink PDU", "gnb", s.cfg.Name, "lane_full", ch != nil, "dropped_total", n)
}

// learnLocked records that the AMF names ranID's UE amfID, replacing what
// either ID was paired with before. Callers hold mu.
func (s *Session) learnLocked(amfID, ranID int64) {
	if s.amfIdx[amfID] == ranID && s.amfOf[ranID] == amfID {
		return
	}
	if old, ok := s.amfOf[ranID]; ok && s.amfIdx[old] == ranID {
		delete(s.amfIdx, old)
	}
	if prev, ok := s.amfIdx[amfID]; ok && s.amfOf[prev] == amfID {
		delete(s.amfOf, prev)
	}
	s.amfIdx[amfID], s.amfOf[ranID] = ranID, amfID
}

// ranUENGAPIDOf extracts the RAN-UE-NGAP-ID from a downlink PDU (the demux
// key), covering the AMF→gNB messages an attach and session setup receive.
func ranUENGAPIDOf(pdu *ngapType.NGAPPDU) (int64, bool) {
	// UE-associated responses to a gNB-initiated procedure (e.g. an Xn
	// PathSwitch) come back as Successful/Unsuccessful outcomes; route them by
	// the RAN-UE-NGAP-ID the gNB sent so muxed callers receive their reply.
	if pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome {
		if pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge {
			return findRANUEID(pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List,
				func(ie ngapType.PathSwitchRequestAcknowledgeIEs) (int64, bool) {
					if ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID {
						return ie.Value.RANUENGAPID.Value, true
					}
					return 0, false
				})
		}
		return 0, false
	}
	if pdu.Present == ngapType.NGAPPDUPresentUnsuccessfulOutcome {
		if pdu.UnsuccessfulOutcome.Value.Present == ngapType.UnsuccessfulOutcomePresentPathSwitchRequestFailure {
			return findRANUEID(pdu.UnsuccessfulOutcome.Value.PathSwitchRequestFailure.ProtocolIEs.List,
				func(ie ngapType.PathSwitchRequestFailureIEs) (int64, bool) {
					if ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID {
						return ie.Value.RANUENGAPID.Value, true
					}
					return 0, false
				})
		}
		return 0, false
	}
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		return 0, false
	}
	im := pdu.InitiatingMessage
	switch im.Value.Present {
	case ngapType.InitiatingMessagePresentDownlinkNASTransport:
		return findRANUEID(im.Value.DownlinkNASTransport.ProtocolIEs.List, func(ie ngapType.DownlinkNASTransportIEs) (int64, bool) {
			if ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID {
				return ie.Value.RANUENGAPID.Value, true
			}
			return 0, false
		})
	case ngapType.InitiatingMessagePresentInitialContextSetupRequest:
		return findRANUEID(im.Value.InitialContextSetupRequest.ProtocolIEs.List, func(ie ngapType.InitialContextSetupRequestIEs) (int64, bool) {
			if ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID {
				return ie.Value.RANUENGAPID.Value, true
			}
			return 0, false
		})
	case ngapType.InitiatingMessagePresentPDUSessionResourceSetupRequest:
		return findRANUEID(im.Value.PDUSessionResourceSetupRequest.ProtocolIEs.List, func(ie ngapType.PDUSessionResourceSetupRequestIEs) (int64, bool) {
			if ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID {
				return ie.Value.RANUENGAPID.Value, true
			}
			return 0, false
		})
	case ngapType.InitiatingMessagePresentUEContextReleaseCommand:
		// The release names the UE by its NGAP ID pair; an AMF-only ID
		// (TS 38.413 9.3.3.18) has no RAN ID to route by and is dropped.
		return findRANUEID(im.Value.UEContextReleaseCommand.ProtocolIEs.List, func(ie ngapType.UEContextReleaseCommandIEs) (int64, bool) {
			if ie.Id.Value != ngapType.ProtocolIEIDUENGAPIDs || ie.Value.UENGAPIDs == nil ||
				ie.Value.UENGAPIDs.Present != ngapType.UENGAPIDsPresentUENGAPIDPair || ie.Value.UENGAPIDs.UENGAPIDPair == nil {
				return 0, false
			}
			return ie.Value.UENGAPIDs.UENGAPIDPair.RANUENGAPID.Value, true
		})
	default:
		return 0, false
	}
}

func findRANUEID[T any](list []T, get func(T) (int64, bool)) (int64, bool) {
	for _, ie := range list {
		if id, ok := get(ie); ok {
			return id, true
		}
	}
	return 0, false
}

// amfUENGAPIDOf extracts the AMF-UE-NGAP-ID from the downlink PDUs that
// identify their UE by it: the RAN-ID-carrying ones ranUENGAPIDOf routes
// (to learn the pairing) and the HANDOVER REQUEST, which reaches a target
// gNB before the UE has a RAN-UE-NGAP-ID there.
func amfUENGAPIDOf(pdu *ngapType.NGAPPDU) (int64, bool) {
	amf := func(ie *ngapType.AMFUENGAPID) (int64, bool) {
		if ie == nil {
			return 0, false
		}
		return ie.Value, true
	}
	switch pdu.Present {
	case ngapType.NGAPPDUPresentInitiatingMessage:
		im := pdu.InitiatingMessage
		switch im.Value.Present {
		case ngapType.InitiatingMessagePresentDownlinkNASTransport:
			return findRANUEID(im.Value.DownlinkNASTransport.ProtocolIEs.List, func(ie ngapType.DownlinkNASTransportIEs) (int64, bool) {
				return amf(ie.Value.AMFUENGAPID)
			})
		case ngapType.InitiatingMessagePresentInitialContextSetupRequest:
			return findRANUEID(im.Value.InitialContextSetupRequest.ProtocolIEs.List, func(ie ngapType.InitialContextSetupRequestIEs) (int64, bool) {
				return amf(ie.Value.AMFUENGAPID)
			})
		case ngapType.InitiatingMessagePresentPDUSessionResourceSetupRequest:
			return findRANUEID(im.Value.PDUSessionResourceSetupRequest.ProtocolIEs.List, func(ie ngapType.PDUSessionResourceSetupRequestIEs) (int64, bool) {
				return amf(ie.Value.AMFUENGAPID)
			})
		case ngapType.InitiatingMessagePresentHandoverRequest:
			return findRANUEID(im.Value.HandoverRequest.ProtocolIEs.List, func(ie ngapType.HandoverRequestIEs) (int64, bool) {
				return amf(ie.Value.AMFUENGAPID)
			})
		}
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		if so := pdu.SuccessfulOutcome; so.Value.Present == ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge {
			return findRANUEID(so.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List, func(ie ngapType.PathSwitchRequestAcknowledgeIEs) (int64, bool) {
				return amf(ie.Value.AMFUENGAPID)
			})
		}
	}
	return 0, false
}
