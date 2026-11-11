package gnb

import (
	"slices"
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

// releaseCommandWith builds a UE CONTEXT RELEASE COMMAND naming the UE by ids,
// with cause when it is not nil.
func releaseCommandWith(ids *ngapType.UENGAPIDs, cause *ngapType.Cause) *ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseCommand
	cmd := new(ngapType.UEContextReleaseCommand)
	pdu.InitiatingMessage.Value.UEContextReleaseCommand = cmd
	if ids != nil {
		ie := ngapType.UEContextReleaseCommandIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDUENGAPIDs
		ie.Value.Present = ngapType.UEContextReleaseCommandIEsPresentUENGAPIDs
		ie.Value.UENGAPIDs = ids
		cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ie)
	}
	if cause != nil {
		ie := ngapType.UEContextReleaseCommandIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDCause
		ie.Value.Present = ngapType.UEContextReleaseCommandIEsPresentCause
		ie.Value.Cause = cause
		cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ie)
	}
	return &pdu
}

// ParseUEContextReleaseCommand reads the ID pair or the AMF ID alone, and
// the cause; another PDU is not a command.
func TestParseUEContextReleaseCommand(t *testing.T) {
	pair := &ngapType.UENGAPIDs{Present: ngapType.UENGAPIDsPresentUENGAPIDPair, UENGAPIDPair: &ngapType.UENGAPIDPair{
		AMFUENGAPID: ngapType.AMFUENGAPID{Value: 7}, RANUENGAPID: ngapType.RANUENGAPID{Value: 9}}}
	amfOnly := &ngapType.UENGAPIDs{Present: ngapType.UENGAPIDsPresentAMFUENGAPID, AMFUENGAPID: &ngapType.AMFUENGAPID{Value: 7}}
	nasCause := &ngapType.Cause{Present: ngapType.CausePresentNas, Nas: &ngapType.CauseNas{Value: ngapType.CauseNasPresentNormalRelease}}
	complete := BuildUEContextReleaseComplete(7, 9)
	tests := []struct {
		name   string
		pdu    *ngapType.NGAPPDU
		wantOK bool
		want   UEContextReleaseCommandInfo
	}{
		{name: "ID pair and cause", pdu: releaseCommandWith(pair, nasCause), wantOK: true,
			want: UEContextReleaseCommandInfo{AMFUENGAPID: 7, RANUENGAPID: 9, HasPair: true, Cause: "nas(0)"}},
		{name: "AMF ID alone", pdu: releaseCommandWith(amfOnly, nasCause), wantOK: true,
			want: UEContextReleaseCommandInfo{AMFUENGAPID: 7, Cause: "nas(0)"}},
		{name: "no cause, no IDs", pdu: releaseCommandWith(nil, nil), wantOK: true},
		{name: "an empty IDs choice", pdu: releaseCommandWith(&ngapType.UENGAPIDs{}, nil), wantOK: true},
		{name: "release complete", pdu: &complete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseUEContextReleaseCommand(tc.pdu)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ParseUEContextReleaseCommand = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// BuildInitialUEMessageFromIdle encodes and carries the NAS PDU, the new
// RAN-UE-NGAP-ID and, from a 5G-GUTI, the 5G-S-TMSI (AMF Set ID, AMF
// Pointer, 5G-TMSI); it requests no UE context. Without a 5G-GUTI it
// carries no 5G-S-TMSI; a gNB identity that does not encode is an error.
func TestBuildInitialUEMessageFromIdle(t *testing.T) {
	cfg := Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []SNSSAI{{SST: 1, SD: "010203"}}}
	// AMF Region 0xca, AMF Set 0x3fb (10 bits), AMF Pointer 0x3e (6 bits).
	guti := []byte{0xf2, 0x02, 0xf8, 0x39, 0xca, 0xfe, 0xfe, 0x00, 0x00, 0x00, 0x2a}
	tests := []struct {
		name     string
		guti     []byte
		wantTMSI bool
	}{
		{name: "with a 5G-GUTI", guti: guti, wantTMSI: true},
		{name: "no identity", guti: nil},
		{name: "a SUCI is not a 5G-GUTI", guti: append([]byte{0x01}, guti[1:]...)},
		{name: "a truncated 5G-GUTI", guti: guti[:10]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := BuildInitialUEMessageFromIdle(cfg, 5, []byte{0x7e, 0x01, 0xaa}, tc.guti)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ngap.Encode(msg)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			pdu, err := ngap.Decode(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage || pdu.InitiatingMessage.Value.InitialUEMessage == nil {
				t.Fatalf("decoded PDU is not an Initial UE Message")
			}
			var ran int64
			var nasPDU []byte
			var tmsi *ngapType.FiveGSTMSI
			for _, ie := range pdu.InitiatingMessage.Value.InitialUEMessage.ProtocolIEs.List {
				switch ie.Id.Value {
				case ngapType.ProtocolIEIDRANUENGAPID:
					ran = ie.Value.RANUENGAPID.Value
				case ngapType.ProtocolIEIDNASPDU:
					nasPDU = ie.Value.NASPDU.Value
				case ngapType.ProtocolIEIDFiveGSTMSI:
					tmsi = ie.Value.FiveGSTMSI
				case ngapType.ProtocolIEIDUEContextRequest:
					t.Error("the message requests a UE context")
				}
			}
			if ran != 5 || string(nasPDU) != "\x7e\x01\xaa" {
				t.Errorf("RAN-UE-NGAP-ID %d, NAS PDU % x; want 5 and 7e 01 aa", ran, nasPDU)
			}
			if (tmsi != nil) != tc.wantTMSI {
				t.Fatalf("5G-S-TMSI present %v, want %v", tmsi != nil, tc.wantTMSI)
			}
			if tmsi == nil {
				return
			}
			set, ptr := tmsi.AMFSetID.Value, tmsi.AMFPointer.Value
			if set.BitLength != 10 || set.Bytes[0] != 0xfe || set.Bytes[1]&0xc0 != 0xc0 {
				t.Errorf("AMF Set ID % x (%d bits), want fe c0 (10 bits)", set.Bytes, set.BitLength)
			}
			if ptr.BitLength != 6 || ptr.Bytes[0] != 0xf8 {
				t.Errorf("AMF Pointer % x (%d bits), want f8 (6 bits)", ptr.Bytes, ptr.BitLength)
			}
			if string(tmsi.FiveGTMSI.Value) != "\x00\x00\x00\x2a" {
				t.Errorf("5G-TMSI % x, want 00 00 00 2a", tmsi.FiveGTMSI.Value)
			}
		})
	}
	bad := cfg
	bad.MCC = "20"
	if _, err := BuildInitialUEMessageFromIdle(bad, 5, []byte{0x7e}, guti); err == nil {
		t.Error("a gNB with an invalid PLMN built an Initial UE Message")
	}
}

// The PDU sessions with an active N3 user plane go out as the PDU Session
// Resource List of both release messages, and the IE is left out when there
// are none.
func TestUEContextReleaseSessionLists(t *testing.T) {
	requestList := func(p *ngapType.NGAPPDU) (ids []int64, present bool) {
		for _, ie := range p.InitiatingMessage.Value.UEContextReleaseRequest.ProtocolIEs.List {
			if ie.Id.Value == ngapType.ProtocolIEIDPDUSessionResourceListCxtRelReq {
				present = true
				for _, it := range ie.Value.PDUSessionResourceListCxtRelReq.List {
					ids = append(ids, it.PDUSessionID.Value)
				}
			}
		}
		return ids, present
	}
	completeList := func(p *ngapType.NGAPPDU) (ids []int64, present bool) {
		for _, ie := range p.SuccessfulOutcome.Value.UEContextReleaseComplete.ProtocolIEs.List {
			if ie.Id.Value == ngapType.ProtocolIEIDPDUSessionResourceListCxtRelCpl {
				present = true
				for _, it := range ie.Value.PDUSessionResourceListCxtRelCpl.List {
					ids = append(ids, it.PDUSessionID.Value)
				}
			}
		}
		return ids, present
	}
	for _, tc := range []struct {
		name     string
		sessions []int64
	}{
		{name: "no session"},
		{name: "one session", sessions: []int64{1}},
		{name: "two sessions", sessions: []int64{1, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, m := range []struct {
				what string
				pdu  ngapType.NGAPPDU
				list func(*ngapType.NGAPPDU) ([]int64, bool)
			}{
				{"request", BuildUEContextReleaseRequest(7, 9, tc.sessions...), requestList},
				{"complete", BuildUEContextReleaseComplete(7, 9, tc.sessions...), completeList},
			} {
				b, err := ngap.Encode(m.pdu)
				if err != nil {
					t.Fatalf("%s: encode: %v", m.what, err)
				}
				got, err := ngap.Decode(b)
				if err != nil {
					t.Fatalf("%s: decode: %v", m.what, err)
				}
				ids, present := m.list(got)
				if present != (len(tc.sessions) > 0) || !slices.Equal(ids, tc.sessions) {
					t.Errorf("%s: list present %v with %v, want %v", m.what, present, ids, tc.sessions)
				}
			}
		})
	}
}
