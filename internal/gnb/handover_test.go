package gnb

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapConvert"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/coreprofile"
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
	}, coreprofile.Quirks{})
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

// TestHandoverAckTransferSDCoreQuirk locks in the SD-Core compatibility quirk:
// with HandoverAckForwardingMandatory, the per-session transfer must encode to
// the exact bytes omec's ngap v2.1.0 produces/accepts (dLForwardingUP-
// TNLInformation present). This golden value was verified byte-identical to
// omec's own encoder and decodable by omec's v2.1.0 decoder. See §5(i) /
// docs/interop/sdcore.md.
func TestHandoverAckTransferSDCoreQuirk(t *testing.T) {
	// tunnel 172.17.50.13 / TEID 200, QFI 1
	const golden = "000f80ac11320d000000c801f0ac11320d000000c80001"
	got, err := encodeHandoverAckTransfer(GNBTunnel{Address: "172.17.50.13", TEID: 200}, []int64{1}, true)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != golden {
		t.Errorf("sdcore quirk transfer = %s, want %s", hex.EncodeToString(got), golden)
	}
	// The strict (conformant) encoding must remain the canonical, shorter form.
	strict, err := encodeHandoverAckTransfer(GNBTunnel{Address: "172.17.50.13", TEID: 200}, []int64{1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(strict) != "0007c0ac11320d000000c80001" {
		t.Errorf("strict transfer changed: %s", hex.EncodeToString(strict))
	}
}
