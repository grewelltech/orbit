package gnb

import (
	"bytes"
	"testing"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// modifyRequestPDU is a PDU SESSION RESOURCE MODIFY REQUEST for UE (7, 9)
// listing ids, the first item carrying nasPDU when it is non-nil.
func modifyRequestPDU(ids []int64, nasPDU []byte) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePDUSessionResourceModify
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPDUSessionResourceModifyRequest
	req := new(ngapType.PDUSessionResourceModifyRequest)
	pdu.InitiatingMessage.Value.PDUSessionResourceModifyRequest = req
	add := func(id int64, fill func(*ngapType.PDUSessionResourceModifyRequestIEs)) {
		ie := ngapType.PDUSessionResourceModifyRequestIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		fill(&ie)
		req.ProtocolIEs.List = append(req.ProtocolIEs.List, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, func(ie *ngapType.PDUSessionResourceModifyRequestIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceModifyRequestIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: 7}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, func(ie *ngapType.PDUSessionResourceModifyRequestIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceModifyRequestIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: 9}
	})
	add(ngapType.ProtocolIEIDPDUSessionResourceModifyListModReq, func(ie *ngapType.PDUSessionResourceModifyRequestIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceModifyRequestIEsPresentPDUSessionResourceModifyListModReq
		ie.Value.PDUSessionResourceModifyListModReq = new(ngapType.PDUSessionResourceModifyListModReq)
		for i, id := range ids {
			item := ngapType.PDUSessionResourceModifyItemModReq{}
			item.PDUSessionID.Value = id
			item.PDUSessionResourceModifyRequestTransfer = []byte{0x00, 0x00}
			if i == 0 && nasPDU != nil {
				item.NASPDU = &ngapType.NASPDU{Value: nasPDU}
			}
			ie.Value.PDUSessionResourceModifyListModReq.List = append(ie.Value.PDUSessionResourceModifyListModReq.List, item)
		}
	})
	return pdu
}

// A PDU SESSION RESOURCE MODIFY REQUEST is read after an APER round trip,
// with and without a NAS PDU; other PDUs are refused.
func TestParsePDUSessionResourceModifyRequest(t *testing.T) {
	tests := []struct {
		name   string
		ids    []int64
		nasPDU []byte
	}{
		{name: "one session with a NAS PDU", ids: []int64{2}, nasPDU: []byte{0x7e, 0x02, 1, 2, 3, 4, 5}},
		{name: "two sessions, no NAS PDU", ids: []int64{2, 5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ngap.Encode(modifyRequestPDU(tc.ids, tc.nasPDU))
			if err != nil {
				t.Fatal(err)
			}
			pdu, err := ngap.Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParsePDUSessionResourceModifyRequest(pdu)
			if err != nil {
				t.Fatal(err)
			}
			if got.AMFUENGAPID != 7 || got.RANUENGAPID != 9 || len(got.PDUSessionIDs) != len(tc.ids) {
				t.Fatalf("parsed %+v, want UE (7, 9) and sessions %v", got, tc.ids)
			}
			for i, id := range tc.ids {
				if got.PDUSessionIDs[i] != id {
					t.Errorf("session %d = %d, want %d", i, got.PDUSessionIDs[i], id)
				}
			}
			wantNAS := 0
			if tc.nasPDU != nil {
				wantNAS = 1
			}
			if len(got.NASPDUs) != wantNAS || (wantNAS == 1 && !bytes.Equal(got.NASPDUs[0], tc.nasPDU)) {
				t.Errorf("NAS PDUs %x, want %x", got.NASPDUs, tc.nasPDU)
			}
		})
	}
	for name, pdu := range map[string]*ngapType.NGAPPDU{
		"nil":             nil,
		"another message": func() *ngapType.NGAPPDU { p := BuildUEContextReleaseComplete(7, 9); return &p }(),
		"no value": {Present: ngapType.NGAPPDUPresentInitiatingMessage, InitiatingMessage: &ngapType.InitiatingMessage{
			Value: ngapType.InitiatingMessageValue{Present: ngapType.InitiatingMessagePresentPDUSessionResourceModifyRequest}}},
	} {
		if got, err := ParsePDUSessionResourceModifyRequest(pdu); err == nil {
			t.Errorf("%s: parsed %+v, want an error", name, got)
		}
	}
}
