package gnb

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Session multiplexes many UEs over one gNB↔AMF association. A real gNB uses
// a single N2 SCTP association for its whole UE population; ORBIT does the
// same (one Session per gNB). NG Setup runs once; a single read loop then
// demultiplexes downlink PDUs to per-UE inboxes by RAN-UE-NGAP-ID, and each
// UE drives its attach through a per-UE Transport backed by the shared
// association. Works over any gnb.Transport (real SCTP or the in-process
// pipe), per D-6's decision to test the actor model both ways.
type Session struct {
	cfg     Config
	tr      Transport
	streams uint16

	ranSeq  atomic.Int64
	mu      sync.Mutex
	inboxes map[int64]chan []byte
	closed  chan struct{}
	once    sync.Once
}

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
	s := &Session{
		cfg: cfg, tr: tr, streams: sctp.DefaultNGAPStreams,
		inboxes: make(map[int64]chan []byte), closed: make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// UETransport is a per-UE view of the shared association: writes go out on
// the UE's assigned SCTP stream; reads come from the UE's demux inbox.
type UETransport struct {
	s      *Session
	ranID  int64
	stream uint16
	inbox  chan []byte
}

// NewUE assigns a fresh RAN-UE-NGAP-ID and returns a Transport plus that ID
// (pass it as UEConfig.RANUENGAPID so the Initial UE Message matches the
// demux key). UEs are spread across the association's SCTP streams to avoid
// head-of-line blocking; demux is by NGAP ID, so sharing a stream is fine.
func (s *Session) NewUE() (*UETransport, int64) {
	ranID := s.ranSeq.Add(1)
	ch := make(chan []byte, 16)
	s.mu.Lock()
	s.inboxes[ranID] = ch
	s.mu.Unlock()
	stream := uint16(1)
	if s.streams > 1 {
		stream = uint16(1 + (ranID-1)%int64(s.streams-1))
	}
	return &UETransport{s: s, ranID: ranID, stream: stream, inbox: ch}, ranID
}

func (t *UETransport) WriteNGAP(_ uint16, pdu []byte) error {
	return t.s.tr.WriteNGAP(t.stream, pdu)
}

func (t *UETransport) ReadMsg(_ []byte) (payload []byte, stream uint16, ppid uint32, err error) {
	select {
	case b := <-t.inbox:
		return b, t.stream, sctp.PPIDNGAP, nil
	case <-t.s.closed:
		return nil, 0, 0, fmt.Errorf("gnb session closed")
	}
}

// Close unregisters the UE from the session demux. It does not close the
// shared association.
func (t *UETransport) Close() error {
	t.s.mu.Lock()
	delete(t.s.inboxes, t.ranID)
	t.s.mu.Unlock()
	return nil
}

// Close tears down the session and its shared association, waking all UEs.
func (s *Session) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.tr.Close()
}

func (s *Session) readLoop() {
	buf := make([]byte, 65536)
	for {
		payload, _, ppid, err := s.tr.ReadMsg(buf)
		if err != nil {
			s.once.Do(func() { close(s.closed) })
			return
		}
		if ppid != sctp.PPIDNGAP && ppid != sctp.PPIDNGAPSwapped {
			continue
		}
		pdu, err := ngap.Decode(payload)
		if err != nil {
			continue
		}
		ranID, ok := ranUENGAPIDOf(pdu)
		if !ok {
			continue // non-UE-associated (nothing expected after NG Setup)
		}
		s.mu.Lock()
		ch := s.inboxes[ranID]
		s.mu.Unlock()
		if ch == nil {
			continue
		}
		msg := append([]byte(nil), payload...)
		select {
		case ch <- msg:
		case <-s.closed:
			return
		}
	}
}

// ranUENGAPIDOf extracts the RAN-UE-NGAP-ID from a downlink PDU (the demux
// key), covering the AMF→gNB messages an attach and session setup receive.
func ranUENGAPIDOf(pdu *ngapType.NGAPPDU) (int64, bool) {
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
