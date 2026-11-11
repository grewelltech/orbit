package gnb

import (
	"testing"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// BuildHandoverCancel encodes, and decodes to a HANDOVER CANCEL naming the
// UE's NGAP ID pair with cause radio network "handover cancelled".
func TestBuildHandoverCancel(t *testing.T) {
	b, err := ngap.Encode(BuildHandoverCancel(7, 9))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pdu, err := ngap.Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage ||
		pdu.InitiatingMessage.ProcedureCode.Value != ngapType.ProcedureCodeHandoverCancel ||
		pdu.InitiatingMessage.Value.HandoverCancel == nil {
		t.Fatalf("decoded PDU is not a HANDOVER CANCEL: present %d", pdu.Present)
	}
	var amf, ran int64
	var cause *ngapType.Cause
	for _, ie := range pdu.InitiatingMessage.Value.HandoverCancel.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			amf = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDRANUENGAPID:
			ran = ie.Value.RANUENGAPID.Value
		case ngapType.ProtocolIEIDCause:
			cause = ie.Value.Cause
		}
	}
	if amf != 7 || ran != 9 {
		t.Errorf("NGAP IDs (%d, %d), want (7, 9)", amf, ran)
	}
	if cause == nil || cause.RadioNetwork == nil || cause.RadioNetwork.Value != ngapType.CauseRadioNetworkPresentHandoverCancelled {
		t.Errorf("cause = %+v, want radio network handover-cancelled", cause)
	}
}

// IsHandoverCancelAcknowledge and IsUEContextReleaseCommand each recognise
// their PDU and nothing else.
func TestHandoverCancelPredicates(t *testing.T) {
	ack := ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentSuccessfulOutcome, SuccessfulOutcome: new(ngapType.SuccessfulOutcome)}
	ack.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentHandoverCancelAcknowledge
	release := ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentInitiatingMessage, InitiatingMessage: new(ngapType.InitiatingMessage)}
	release.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseCommand
	cancel := BuildHandoverCancel(1, 2)
	complete := BuildUEContextReleaseComplete(1, 2)
	tests := []struct {
		name            string
		pdu             *ngapType.NGAPPDU
		isAck, isReleas bool
	}{
		{"cancel acknowledge", &ack, true, false},
		{"release command", &release, false, true},
		{"handover cancel", &cancel, false, false},
		{"release complete", &complete, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHandoverCancelAcknowledge(tc.pdu); got != tc.isAck {
				t.Errorf("IsHandoverCancelAcknowledge = %v, want %v", got, tc.isAck)
			}
			if got := IsUEContextReleaseCommand(tc.pdu); got != tc.isReleas {
				t.Errorf("IsUEContextReleaseCommand = %v, want %v", got, tc.isReleas)
			}
		})
	}
}
