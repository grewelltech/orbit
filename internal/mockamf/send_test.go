package mockamf

import (
	"context"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"
)

// A downlink PDU that does not encode is dropped and counted, not returned
// as an error, so the serve loop keeps the association: the next good PDU on
// the same pipe still arrives.
func TestSendDropsAnUnencodablePDUAndKeepsTheAssociation(t *testing.T) {
	a, err := New(Config{Ki: make([]byte, 16), OPc: make([]byte, 16), SQN: make([]byte, 6), AMF: make([]byte, 2), MCC: "208", MNC: "93", SST: 1})
	if err != nil {
		t.Fatal(err)
	}
	gnbSide, amfSide := newPipePair()
	defer func() { _ = gnbSide.Close() }()
	defer func() { _ = amfSide.Close() }()

	// An empty switched list violates SIZE(1..): this is the PDU that used
	// to end the association.
	bad, err := buildPathSwitchRequestAcknowledge(1, 2, nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.send(amfSide, bad); err != nil {
		t.Fatalf("send of an unencodable PDU returned %v, want nil", err)
	}
	if a.EncodeErrors() != 1 {
		t.Errorf("EncodeErrors = %d, want 1", a.EncodeErrors())
	}
	if err := a.send(amfSide, buildPathSwitchRequestFailure(1, 2, []uint8{1})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := gnbSide.ReadMsg(make([]byte, 4096))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read after the dropped PDU: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the good PDU after the dropped one never arrived")
	}
}

// The failure encodes with and without a released list.
func TestPathSwitchRequestFailureEncodes(t *testing.T) {
	for _, sessions := range [][]uint8{nil, {1}, {1, 5}} {
		pdu := buildPathSwitchRequestFailure(7, 3, sessions)
		if _, err := encode(pdu); err != nil {
			t.Errorf("encode failure with sessions %v: %v", sessions, err)
		}
		if pdu.Present != ngapType.NGAPPDUPresentUnsuccessfulOutcome {
			t.Errorf("present = %d", pdu.Present)
		}
	}
}
