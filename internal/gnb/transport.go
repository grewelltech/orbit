package gnb

import (
	"context"
	"fmt"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// SendPDU encodes and sends an NGAP PDU on the UE-associated stream.
// Non-UE-associated procedures (NG Setup) use stream 0; UE-associated
// signalling uses a nonzero stream (TS 38.412 §7). Phase 1a multiplexes a
// single UE, so a fixed stream is sufficient.
func SendPDU(conn *sctp.Conn, stream uint16, pdu ngapType.NGAPPDU) error {
	b, err := ngap.Encode(pdu)
	if err != nil {
		return err
	}
	return conn.WriteNGAP(stream, b)
}

// ReadPDU blocks for one NGAP PDU from the association, honouring ctx for
// cancellation/timeout. It verifies the SCTP PPID is NGAP (accepting the
// byte-reversed form the omec AMF emits; see internal/sctp) before decoding.
func ReadPDU(ctx context.Context, conn *sctp.Conn) (*ngapType.NGAPPDU, error) {
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
