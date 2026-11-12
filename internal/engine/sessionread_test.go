package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// chanTransport is a gnb.Transport fed from a channel.
type chanTransport struct {
	in     chan []byte
	closed chan struct{}
}

func newChanTransport() *chanTransport {
	return &chanTransport{in: make(chan []byte, 4), closed: make(chan struct{})}
}

func (c *chanTransport) WriteNGAP(uint16, []byte) error { return nil }
func (c *chanTransport) ReadMsg([]byte) ([]byte, uint16, uint32, error) {
	select {
	case b := <-c.in:
		return b, 1, sctp.PPIDNGAP, nil
	case <-c.closed:
		return nil, 0, 0, errors.New("closed")
	}
}
func (c *chanTransport) Close() error { close(c.closed); return nil }

func (c *chanTransport) push(t *testing.T, pdu ngapType.NGAPPDU) {
	t.Helper()
	b, err := ngap.Encode(pdu)
	if err != nil {
		t.Fatal(err)
	}
	c.in <- b
}

// readPDU hands a PDU that arrives after one wait ran out to the next wait
// on the same association, in order, and never to a wait on an association
// the session has moved to.
func TestSessionReadPDUKeepsAnAbandonedReadPerAssociation(t *testing.T) {
	conn := newChanTransport()
	s := &Session{SUPI: "208930100000001", conn: conn}
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	long := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		return ctx
	}

	if _, err := s.readPDU(short()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readPDU on a silent association = %v, want a deadline error", err)
	}
	conn.push(t, gnb.BuildUEContextReleaseComplete(1, 2))
	conn.push(t, gnb.BuildUEContextReleaseRequest(3, 4))
	first, err := s.readPDU(long())
	if err != nil || first.Present != ngapType.NGAPPDUPresentSuccessfulOutcome {
		t.Fatalf("first PDU = %+v, %v; want the one the abandoned read took", first, err)
	}
	second, err := s.readPDU(long())
	if err != nil || second.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		t.Fatalf("second PDU = %+v, %v; want the next one in order", second, err)
	}

	// A read left on the old association is not delivered on the new one.
	if _, err := s.readPDU(short()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readPDU = %v, want a deadline error", err)
	}
	moved := newChanTransport()
	s.conn = moved
	conn.push(t, gnb.BuildUEContextReleaseComplete(5, 6))
	moved.push(t, gnb.BuildUEContextReleaseRequest(7, 8))
	got, err := s.readPDU(long())
	if err != nil || got.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		t.Fatalf("PDU after the move = %+v, %v; want the new association's", got, err)
	}

	// A closed association ends the read with its error.
	_ = moved.Close()
	if _, err := s.readPDU(long()); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readPDU on a closed association = %v, want its read error", err)
	}
	_ = conn.Close()
}
