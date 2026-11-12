package engine

import (
	"context"
	"fmt"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

// pduRead is the outcome of one read of an association.
type pduRead struct {
	pdu *ngapType.NGAPPDU
	err error
}

// readPDU reads the next downlink PDU of the session's association,
// honouring ctx. Over a gNB lane (gnb.UETransport) the read is direct: a
// wait that ends on ctx leaves nothing in flight, and the next PDU stays
// queued on the lane. Over any other transport a read that ctx gives up on
// stays in flight and is kept: the next readPDU resumes it, so the PDU it
// eventually returns is not lost to a reader nobody waits for (as it is
// after gnb.ReadPDU times out). A kept read of an association the session
// has since left is abandoned. Callers hold the procedure lock, or own the
// session outright.
func (s *Session) readPDU(ctx context.Context) (*ngapType.NGAPPDU, error) {
	if _, lane := s.conn.(*gnb.UETransport); lane {
		s.held, s.heldConn = nil, nil
		return gnb.ReadPDU(ctx, s.conn)
	}
	ch := s.held
	if ch == nil || s.heldConn != s.conn {
		ch = make(chan pduRead, 1)
		conn := s.conn
		go func() {
			pdu, err := gnb.ReadPDU(context.Background(), conn)
			ch <- pduRead{pdu, err}
		}()
	}
	s.held, s.heldConn = nil, nil
	select {
	case r := <-ch:
		return r.pdu, r.err
	case <-ctx.Done():
		s.held, s.heldConn = ch, s.conn
		return nil, fmt.Errorf("waiting for NGAP PDU: %w", ctx.Err())
	}
}

// dropHeld discards the PDU of a kept read that has already finished: it
// arrived before whatever the caller is about to send, so it cannot be the
// answer. A kept read still in flight stays kept.
func (s *Session) dropHeld() {
	if s.held == nil {
		return
	}
	select {
	case <-s.held:
		s.held, s.heldConn = nil, nil
	default:
	}
}
