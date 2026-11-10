package gnb

import (
	"strings"
	"testing"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"
)

// pathSwitchAck builds a PATH SWITCH REQUEST ACKNOWLEDGE whose Switched List
// carries transfers as given, one item per entry, session IDs from 1.
// withList false leaves the list out.
func pathSwitchAck(transfers []aper.OctetString, withList bool) *ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodePathSwitchRequest
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge
	ack := new(ngapType.PathSwitchRequestAcknowledge)
	pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge = ack
	if !withList {
		return &pdu
	}
	ie := ngapType.PathSwitchRequestAcknowledgeIEs{}
	ie.Id.Value = ngapType.ProtocolIEIDPDUSessionResourceSwitchedList
	ie.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentPDUSessionResourceSwitchedList
	ie.Value.PDUSessionResourceSwitchedList = new(ngapType.PDUSessionResourceSwitchedList)
	for i, tr := range transfers {
		item := ngapType.PDUSessionResourceSwitchedItem{}
		item.PDUSessionID.Value = int64(i + 1)
		item.PathSwitchRequestAcknowledgeTransfer = tr
		ie.Value.PDUSessionResourceSwitchedList.List = append(ie.Value.PDUSessionResourceSwitchedList.List, item)
	}
	ack.ProtocolIEs.List = append(ack.ProtocolIEs.List, ie)
	return &pdu
}

// ParsePathSwitchAcknowledgeItems keeps every item of the Switched List and
// says of each whether its transfer is there, decodes, and names a new
// uplink endpoint; a message without the list, or another PDU, has none.
func TestParsePathSwitchAcknowledgeItems(t *testing.T) {
	plain, err := aper.MarshalWithParams(ngapType.PathSwitchRequestAcknowledgeTransfer{}, "valueExt")
	if err != nil {
		t.Fatal(err)
	}
	up, err := gtpTunnelUP(GNBTunnel{Address: "10.0.0.9", TEID: 0x77})
	if err != nil {
		t.Fatal(err)
	}
	withUL, err := aper.MarshalWithParams(ngapType.PathSwitchRequestAcknowledgeTransfer{ULNGUUPTNLInformation: &up}, "valueExt")
	if err != nil {
		t.Fatal(err)
	}
	got := ParsePathSwitchAcknowledgeItems(pathSwitchAck([]aper.OctetString{plain, withUL, {}, {0xff}}, true))
	if !got.ListPresent || len(got.Items) != 4 {
		t.Fatalf("parsed %+v, want a list of 4 items", got)
	}
	want := []struct {
		transfer, ulTNL, decodeErr bool
	}{{true, false, false}, {true, true, false}, {false, false, false}, {false, false, true}}
	for i, w := range want {
		it := got.Items[i]
		if it.PDUSessionID != int64(i+1) || it.HasTransfer() != w.transfer || it.HasULTNL != w.ulTNL || (it.TransferErr != "") != w.decodeErr {
			t.Errorf("item %d = %+v, want transfer %v, UL TNL %v, decode error %v", i+1, it, w.transfer, w.ulTNL, w.decodeErr)
		}
	}
	if got.Items[2].TransferLen != 0 || got.Items[3].TransferLen != 1 {
		t.Errorf("transfer lengths %d and %d, want 0 and 1", got.Items[2].TransferLen, got.Items[3].TransferLen)
	}
	if got := ParsePathSwitchAcknowledgeItems(pathSwitchAck(nil, false)); got.ListPresent || len(got.Items) != 0 {
		t.Errorf("without the list: %+v", got)
	}
	var other ngapType.NGAPPDU
	other.Present = ngapType.NGAPPDUPresentInitiatingMessage
	other.InitiatingMessage = new(ngapType.InitiatingMessage)
	if got := ParsePathSwitchAcknowledgeItems(&other); got.ListPresent || len(got.Items) != 0 {
		t.Errorf("another PDU: %+v", got)
	}
}

// DescribePathSwitchAnswer names an Acknowledge, a Failure and any other
// PDU by its kind and procedure code.
func TestDescribePathSwitchAnswer(t *testing.T) {
	failure := ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentUnsuccessfulOutcome, UnsuccessfulOutcome: new(ngapType.UnsuccessfulOutcome)}
	failure.UnsuccessfulOutcome.Value.Present = ngapType.UnsuccessfulOutcomePresentPathSwitchRequestFailure
	initiating := ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentInitiatingMessage, InitiatingMessage: new(ngapType.InitiatingMessage)}
	initiating.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeErrorIndication
	successful := ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentSuccessfulOutcome, SuccessfulOutcome: new(ngapType.SuccessfulOutcome)}
	successful.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeNGReset
	unsuccessful := ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentUnsuccessfulOutcome, UnsuccessfulOutcome: new(ngapType.UnsuccessfulOutcome)}
	unsuccessful.UnsuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeNGSetup
	tests := []struct {
		name string
		pdu  *ngapType.NGAPPDU
		want string
	}{
		{"acknowledge", pathSwitchAck(nil, false), "PATH SWITCH REQUEST ACKNOWLEDGE"},
		{"failure", &failure, "PATH SWITCH REQUEST FAILURE"},
		{"initiating", &initiating, "an initiating message, procedure code"},
		{"successful", &successful, "a successful outcome, procedure code"},
		{"unsuccessful", &unsuccessful, "an unsuccessful outcome, procedure code"},
		{"empty", &ngapType.NGAPPDU{}, "an undecodable PDU"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DescribePathSwitchAnswer(tc.pdu); !strings.HasPrefix(got, tc.want) {
				t.Errorf("DescribePathSwitchAnswer = %q, want %q", got, tc.want)
			}
		})
	}
}
