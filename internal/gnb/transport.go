package gnb

import (
	"context"
	"fmt"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Transport is the NGAP-carrying association ORBIT reads and writes. Both
// the real SCTP association (internal/sctp.Conn) and an in-process pipe (for
// the mock AMF / D-6 sim-capacity harness) satisfy it, so the engine drives
// either without change. The method set matches sctp.Conn exactly.
type Transport interface {
	WriteNGAP(stream uint16, pdu []byte) error
	ReadMsg(buf []byte) (payload []byte, stream uint16, ppid uint32, err error)
	Close() error
}

// SendPDU encodes and sends an NGAP PDU on the UE-associated stream.
// Non-UE-associated procedures (NG Setup) use stream 0; UE-associated
// signalling uses a nonzero stream (TS 38.412 §7). Phase 1a multiplexes a
// single UE, so a fixed stream is sufficient.
func SendPDU(conn Transport, stream uint16, pdu ngapType.NGAPPDU) error {
	b, err := ngap.Encode(pdu)
	if err != nil {
		return err
	}
	return conn.WriteNGAP(stream, b)
}

// ReadPDU blocks for one NGAP PDU from the association, honouring ctx for
// cancellation/timeout. It verifies the SCTP PPID is NGAP (accepting the
// byte-reversed form the omec AMF emits; see internal/sctp) before decoding.
func ReadPDU(ctx context.Context, conn Transport) (*ngapType.NGAPPDU, error) {
	type result struct {
		pdu *ngapType.NGAPPDU
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 65536)
		payload, _, ppid, err := conn.ReadMsg(buf)
		if err != nil {
			ch <- result{err: err}
			return
		}
		if ppid != sctp.PPIDNGAP && ppid != sctp.PPIDNGAPSwapped {
			ch <- result{err: fmt.Errorf("non-NGAP PPID %d", ppid)}
			return
		}
		pdu, err := ngap.Decode(payload)
		ch <- result{pdu: pdu, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for NGAP PDU: %w", ctx.Err())
	case r := <-ch:
		return r.pdu, r.err
	}
}
