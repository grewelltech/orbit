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
