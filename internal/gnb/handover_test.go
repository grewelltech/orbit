package gnb

import (
	"encoding/binary"
	"testing"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapConvert"
	"github.com/free5gc/ngap/ngapType"
)

// TestHandoverAckTransferRoundTrip confirms the target's downlink tunnel is
// well-formed in the HandoverRequestAcknowledge transfer: it decodes back to
// the address and TEID we put in. If this passes but a live handover does not
// switch the downlink, the gap is in the core's N4 path switch, not our
// encoding.
func TestHandoverAckTransferRoundTrip(t *testing.T) {
	pdu, err := BuildHandoverRequestAcknowledge(42, 1, []AdmittedSession{
		{PDUSessionID: 1, GNBTunnel: GNBTunnel{Address: "172.17.50.13", TEID: 200}, QFIs: []int64{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Find the admitted list and decode the first item's transfer.
	var transfer aper.OctetString
	for _, ie := range pdu.SuccessfulOutcome.Value.HandoverRequestAcknowledge.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDPDUSessionResourceAdmittedList {
			transfer = ie.Value.PDUSessionResourceAdmittedList.List[0].HandoverRequestAcknowledgeTransfer
		}
	}
	if transfer == nil {
		t.Fatal("no admitted PDU session in the ack")
	}

	var got ngapType.HandoverRequestAcknowledgeTransfer
	if err := aper.UnmarshalWithParams(transfer, &got, "valueExt"); err != nil {
		t.Fatalf("decode ack transfer: %v", err)
	}
	gt := got.DLNGUUPTNLInformation.GTPTunnel
	if gt == nil {
		t.Fatal("no DL GTP tunnel in the decoded transfer")
	}
	if teid := binary.BigEndian.Uint32(gt.GTPTEID.Value); teid != 200 {
		t.Errorf("decoded DL TEID = %d, want 200", teid)
	}
	if v4, _ := ngapConvert.IPAddressToString(gt.TransportLayerAddress); v4 != "172.17.50.13" {
		t.Errorf("decoded DL address = %q, want 172.17.50.13", v4)
	}
	if len(got.QosFlowSetupResponseList.List) != 1 || got.QosFlowSetupResponseList.List[0].QosFlowIdentifier.Value != 1 {
		t.Error("QoS flow list did not round-trip")
	}
}
