package engine

import (
	"context"
	"fmt"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

// pendingRead is one read of an association, running on its own goroutine
// until the association delivers a PDU or closes.
type pendingRead struct {
	conn gnb.Transport
	ch   chan pduResult
}

// readPDU returns the next downlink PDU of the session's association, or
// ctx's error once ctx is done. A read cannot be interrupted, so one that
// ctx gave up on keeps running, and the next readPDU on the same
// association takes its PDU: a wait that runs out loses nothing to a read
// left behind. A read left on an association the session has since moved
// off is dropped.
//
// One caller at a time: the procedures that wait on the association with it
// hold the session's procedure lock.
func (s *Session) readPDU(ctx context.Context) (*ngapType.NGAPPDU, error) {
	s.readMu.Lock()
	p := s.pending
	if p == nil || p.conn != s.conn {
		p = &pendingRead{conn: s.conn, ch: make(chan pduResult, 1)}
		s.pending = p
		go func() {
			pdu, err := gnb.ReadPDU(context.Background(), p.conn)
			p.ch <- pduResult{pdu, err}
		}()
	}
	s.readMu.Unlock()
	select {
	case r := <-p.ch:
		s.readMu.Lock()
		if s.pending == p {
			s.pending = nil
		}
		s.readMu.Unlock()
		return r.pdu, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for NGAP PDU: %w", ctx.Err())
	}
}
