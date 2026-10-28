package gnb

import (
	"testing"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// Both release messages encode, decode back as the procedure they name, and
// carry the NGAP ID pair they were built with.
func TestUEContextReleaseBuilders(t *testing.T) {
	tests := []struct {
		name     string
		pdu      ngapType.NGAPPDU
		present  int
		ids      func(*ngapType.NGAPPDU) (amf, ran int64)
		wantCode int64
	}{
		{name: "request", pdu: BuildUEContextReleaseRequest(7, 9), present: ngapType.NGAPPDUPresentInitiatingMessage,
			wantCode: ngapType.ProcedureCodeUEContextReleaseRequest,
			ids: func(p *ngapType.NGAPPDU) (amf, ran int64) {
				for _, ie := range p.InitiatingMessage.Value.UEContextReleaseRequest.ProtocolIEs.List {
					switch ie.Id.Value {
					case ngapType.ProtocolIEIDAMFUENGAPID:
						amf = ie.Value.AMFUENGAPID.Value
					case ngapType.ProtocolIEIDRANUENGAPID:
						ran = ie.Value.RANUENGAPID.Value
					}
				}
				return amf, ran
			}},
		{name: "complete", pdu: BuildUEContextReleaseComplete(7, 9), present: ngapType.NGAPPDUPresentSuccessfulOutcome,
			wantCode: ngapType.ProcedureCodeUEContextRelease,
			ids: func(p *ngapType.NGAPPDU) (amf, ran int64) {
				for _, ie := range p.SuccessfulOutcome.Value.UEContextReleaseComplete.ProtocolIEs.List {
					switch ie.Id.Value {
					case ngapType.ProtocolIEIDAMFUENGAPID:
						amf = ie.Value.AMFUENGAPID.Value
					case ngapType.ProtocolIEIDRANUENGAPID:
						ran = ie.Value.RANUENGAPID.Value
					}
				}
				return amf, ran
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ngap.Encode(tc.pdu)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := ngap.Decode(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Present != tc.present {
				t.Fatalf("present = %d, want %d", got.Present, tc.present)
			}
			var code int64
			if got.InitiatingMessage != nil {
				code = got.InitiatingMessage.ProcedureCode.Value
			} else {
				code = got.SuccessfulOutcome.ProcedureCode.Value
			}
			if code != tc.wantCode {
				t.Errorf("procedure = %d, want %d", code, tc.wantCode)
			}
			if amf, ran := tc.ids(got); amf != 7 || ran != 9 {
				t.Errorf("IDs = %d/%d, want 7/9", amf, ran)
			}
		})
	}
}

// releaseCommand is a UE CONTEXT RELEASE COMMAND naming the UE by ids.
func releaseCommand(ids *ngapType.UENGAPIDs) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseCommand
	cmd := new(ngapType.UEContextReleaseCommand)
	pdu.InitiatingMessage.Value.UEContextReleaseCommand = cmd
	ie := ngapType.UEContextReleaseCommandIEs{}
	ie.Id.Value = ngapType.ProtocolIEIDUENGAPIDs
	ie.Criticality.Value = ngapType.CriticalityPresentReject
	ie.Value.Present = ngapType.UEContextReleaseCommandIEsPresentUENGAPIDs
	ie.Value.UENGAPIDs = ids
	cause := ngapType.UEContextReleaseCommandIEs{}
	cause.Id.Value = ngapType.ProtocolIEIDCause
	cause.Criticality.Value = ngapType.CriticalityPresentIgnore
	cause.Value.Present = ngapType.UEContextReleaseCommandIEsPresentCause
	cause.Value.Cause = &ngapType.Cause{Present: ngapType.CausePresentNas, Nas: &ngapType.CauseNas{Value: ngapType.CauseNasPresentDeregister}}
	cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ie, cause)
	return pdu
}

// A UE CONTEXT RELEASE COMMAND reaches the UE it names on a shared
// association: by the RAN ID of its NGAP ID pair. One naming the UE by its
// AMF ID alone has no RAN ID and is not routed.
func TestRANUENGAPIDOfReleaseCommand(t *testing.T) {
	tests := []struct {
		name   string
		ids    *ngapType.UENGAPIDs
		wantID int64
		wantOK bool
	}{
		{name: "the ID pair", ids: &ngapType.UENGAPIDs{Present: ngapType.UENGAPIDsPresentUENGAPIDPair,
			UENGAPIDPair: &ngapType.UENGAPIDPair{AMFUENGAPID: ngapType.AMFUENGAPID{Value: 7}, RANUENGAPID: ngapType.RANUENGAPID{Value: 9}}},
			wantID: 9, wantOK: true},
		{name: "the AMF ID alone", ids: &ngapType.UENGAPIDs{Present: ngapType.UENGAPIDsPresentAMFUENGAPID,
			AMFUENGAPID: &ngapType.AMFUENGAPID{Value: 7}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ngap.Encode(releaseCommand(tc.ids))
			if err != nil {
				t.Fatal(err)
			}
			pdu, err := ngap.Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			id, ok := ranUENGAPIDOf(pdu)
			if id != tc.wantID || ok != tc.wantOK {
				t.Errorf("ranUENGAPIDOf = %d, %v; want %d, %v", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
