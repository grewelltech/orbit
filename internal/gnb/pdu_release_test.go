package gnb

import (
	"bytes"
	"testing"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// releaseCommandPDU is a PDU SESSION RESOURCE RELEASE COMMAND for UE (7, 9)
// listing ids, with nasPDU when it is non-nil.
func releaseCommandPDU(t *testing.T, ids []int64, nasPDU []byte) ngapType.NGAPPDU {
	t.Helper()
	transfer, err := aper.MarshalWithParams(ngapType.PDUSessionResourceReleaseCommandTransfer{
		Cause: ngapType.Cause{Present: ngapType.CausePresentNas, Nas: &ngapType.CauseNas{Value: ngapType.CauseNasPresentNormalRelease}},
	}, "valueExt")
	if err != nil {
		t.Fatal(err)
	}
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePDUSessionResourceRelease
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPDUSessionResourceReleaseCommand
	cmd := new(ngapType.PDUSessionResourceReleaseCommand)
	pdu.InitiatingMessage.Value.PDUSessionResourceReleaseCommand = cmd
	add := func(id int64, fill func(*ngapType.PDUSessionResourceReleaseCommandIEs)) {
		ie := ngapType.PDUSessionResourceReleaseCommandIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		fill(&ie)
		cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: 7}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: 9}
	})
	if nasPDU != nil {
		add(ngapType.ProtocolIEIDNASPDU, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
			ie.Criticality.Value = ngapType.CriticalityPresentIgnore
			ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentNASPDU
			ie.Value.NASPDU = &ngapType.NASPDU{Value: nasPDU}
		})
	}
	add(ngapType.ProtocolIEIDPDUSessionResourceToReleaseListRelCmd, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentPDUSessionResourceToReleaseListRelCmd
		ie.Value.PDUSessionResourceToReleaseListRelCmd = new(ngapType.PDUSessionResourceToReleaseListRelCmd)
		for _, id := range ids {
			item := ngapType.PDUSessionResourceToReleaseItemRelCmd{}
			item.PDUSessionID.Value = id
			item.PDUSessionResourceReleaseCommandTransfer = transfer
			ie.Value.PDUSessionResourceToReleaseListRelCmd.List = append(ie.Value.PDUSessionResourceToReleaseListRelCmd.List, item)
		}
	})
	return pdu
}

// A PDU SESSION RESOURCE RELEASE COMMAND is read after an APER round trip,
// with and without its NAS PDU; other PDUs are refused.
func TestParsePDUSessionResourceReleaseCommand(t *testing.T) {
	tests := []struct {
		name    string
		ids     []int64
		nasPDU  []byte
		wantNAS bool
	}{
		{name: "one session with the NAS PDU", ids: []int64{1}, nasPDU: []byte{0x7e, 0x02, 1, 2, 3, 4, 5}, wantNAS: true},
		{name: "two sessions, no NAS PDU", ids: []int64{1, 5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ngap.Encode(releaseCommandPDU(t, tc.ids, tc.nasPDU))
			if err != nil {
				t.Fatal(err)
			}
			pdu, err := ngap.Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParsePDUSessionResourceReleaseCommand(pdu)
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
			if (got.NASPDU != nil) != tc.wantNAS || !bytes.Equal(got.NASPDU, tc.nasPDU) {
				t.Errorf("NAS PDU %x, want %x", got.NASPDU, tc.nasPDU)
			}
		})
	}
	for name, pdu := range map[string]*ngapType.NGAPPDU{
		"nil":             nil,
		"another message": func() *ngapType.NGAPPDU { p := BuildUEContextReleaseComplete(7, 9); return &p }(),
		"no value": {Present: ngapType.NGAPPDUPresentInitiatingMessage, InitiatingMessage: &ngapType.InitiatingMessage{
			Value: ngapType.InitiatingMessageValue{Present: ngapType.InitiatingMessagePresentPDUSessionResourceReleaseCommand}}},
	} {
		if got, err := ParsePDUSessionResourceReleaseCommand(pdu); err == nil {
			t.Errorf("%s: parsed %+v, want an error", name, got)
		}
	}
}

// The response encodes, and names the UE and each released session with a
// transfer that decodes.
func TestBuildPDUSessionResourceReleaseResponse(t *testing.T) {
	for _, ids := range [][]int64{{1}, {1, 5}} {
		pdu, err := BuildPDUSessionResourceReleaseResponse(7, 9, ids)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ngap.Encode(pdu)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := ngap.Decode(b)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Present != ngapType.NGAPPDUPresentSuccessfulOutcome || got.SuccessfulOutcome.ProcedureCode.Value != ngapType.ProcedureCodePDUSessionResourceRelease ||
			got.SuccessfulOutcome.Value.PDUSessionResourceReleaseResponse == nil {
			t.Fatalf("decoded %+v, want a PDU Session Resource Release Response", got)
		}
		var amfID, ranID int64
		var released []int64
		for _, ie := range got.SuccessfulOutcome.Value.PDUSessionResourceReleaseResponse.ProtocolIEs.List {
			switch ie.Id.Value {
			case ngapType.ProtocolIEIDAMFUENGAPID:
				amfID = ie.Value.AMFUENGAPID.Value
			case ngapType.ProtocolIEIDRANUENGAPID:
				ranID = ie.Value.RANUENGAPID.Value
			case ngapType.ProtocolIEIDPDUSessionResourceReleasedListRelRes:
				for _, item := range ie.Value.PDUSessionResourceReleasedListRelRes.List {
					released = append(released, item.PDUSessionID.Value)
					var tr ngapType.PDUSessionResourceReleaseResponseTransfer
					if err := aper.UnmarshalWithParams(item.PDUSessionResourceReleaseResponseTransfer, &tr, "valueExt"); err != nil {
						t.Errorf("session %d: transfer %x does not decode: %v", item.PDUSessionID.Value, item.PDUSessionResourceReleaseResponseTransfer, err)
					}
				}
			}
		}
		if amfID != 7 || ranID != 9 || len(released) != len(ids) {
			t.Errorf("response names UE (%d, %d) and sessions %v, want (7, 9) and %v", amfID, ranID, released, ids)
		}
	}
}
