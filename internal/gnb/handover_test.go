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
// the address and TEID we put in.
//
// Note: these bytes round-trip under free5gc/aper but SD-Core's SMF (omec's
// aper fork) rejects them with "align Bit is not zero". An independent oracle
// (pycrate's spec-derived NGAP ASN.1 + canonical APER) decodes them correctly
// and re-encodes to the identical bytes — so ORBIT's encoding IS X.691-
// conformant and SD-Core's decoder is the non-conformant party. This is why a
// live N2 handover completes on the control plane but the UPF downlink does not
// switch. The workaround belongs in an opt-in CoreProfile quirk, not here — the
// codec stays conformant. See docs/DESIGN.md D-4 and §5(i).
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
