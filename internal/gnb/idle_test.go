package gnb

import (
	"testing"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// The 5G-S-TMSI comes out of a 5G-GUTI's last six octets, survives the NGAP
// encoding, and anything that is not an 11-octet 5G-GUTI is refused.
func TestSTMSIFromGUTI(t *testing.T) {
	tests := []struct {
		name    string
		guti    []byte
		want    STMSI
		wantErr bool
	}{
		{name: "set 0x3f8 pointer 0", guti: []byte{0xf2, 0x02, 0xf8, 0x39, 0xca, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07},
			want: STMSI{AMFSetID: 0x3f8, AMFPointer: 0, TMSI: 7}},
		{name: "set 0x001 pointer 0x3f", guti: []byte{0xf2, 0x02, 0xf8, 0x39, 0x01, 0x00, 0x7f, 0xde, 0xad, 0xbe, 0xef},
			want: STMSI{AMFSetID: 0x001, AMFPointer: 0x3f, TMSI: 0xdeadbeef}},
		{name: "set 0x2aa pointer 0x15", guti: []byte{0xf2, 0x02, 0xf8, 0x39, 0x01, 0xaa, 0x95, 0x01, 0x02, 0x03, 0x04},
			want: STMSI{AMFSetID: 0x2aa, AMFPointer: 0x15, TMSI: 0x01020304}},
		{name: "too short", guti: []byte{0xf2, 0x02, 0xf8}, wantErr: true},
		{name: "a SUCI, not a GUTI", guti: []byte{0x01, 0x02, 0xf8, 0x39, 0xca, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07}, wantErr: true},
		{name: "nil", guti: nil, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := STMSIFromGUTI(tc.guti)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got != tc.want {
				t.Errorf("STMSI = %+v, want %+v", got, tc.want)
			}
			back, ok := stmsiFromNGAP(got.ngap())
			if !ok || back != got {
				t.Errorf("NGAP round trip = %+v (%v), want %+v", back, ok, got)
			}
		})
	}
	if _, ok := stmsiFromNGAP(&ngapType.FiveGSTMSI{}); ok {
		t.Error("an empty NGAP 5G-S-TMSI parsed")
	}
}

// An Initial UE Message carries the 5G-S-TMSI and the RRC establishment
// cause its options give, through the codec; without options it has
// neither IE change.
func TestBuildInitialUEMessageOptions(t *testing.T) {
	cfg := Config{ID: 1, Name: "g", MCC: "208", MNC: "93", TAC: 1}
	want := STMSI{AMFSetID: 0x3f8, AMFPointer: 2, TMSI: 0x0a0b0c0d}
	tests := []struct {
		name      string
		opts      []InitialUEOption
		wantCause aper.Enumerated
		wantSTMSI bool
	}{
		{"default", nil, ngapType.RRCEstablishmentCausePresentMoSignalling, false},
		{"paging answer", []InitialUEOption{WithSTMSI(want), WithMTAccess()}, ngapType.RRCEstablishmentCausePresentMtAccess, true},
		{"uplink data", []InitialUEOption{WithSTMSI(want), WithMOData()}, ngapType.RRCEstablishmentCausePresentMoData, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pdu, err := BuildInitialUEMessage(cfg, 9, []byte{0x7e, 0x00, 0x4c}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ngap.Encode(pdu)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := ngap.Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			cause := aper.Enumerated(255)
			var got STMSI
			has := false
			for _, ie := range dec.InitiatingMessage.Value.InitialUEMessage.ProtocolIEs.List {
				switch ie.Id.Value {
				case ngapType.ProtocolIEIDRRCEstablishmentCause:
					cause = ie.Value.RRCEstablishmentCause.Value
				case ngapType.ProtocolIEIDFiveGSTMSI:
					got, has = stmsiFromNGAP(ie.Value.FiveGSTMSI)
				}
			}
			if cause != tc.wantCause || has != tc.wantSTMSI || (has && got != want) {
				t.Errorf("cause %d, 5G-S-TMSI %+v (%v); want cause %d, 5G-S-TMSI present %v", cause, got, has, tc.wantCause, tc.wantSTMSI)
			}
		})
	}
}

// pagingPDU is a PAGING with the given identity and TAIs; a nil identity
// omits the IE.
func pagingPDU(id *STMSI, tais ...TAI) *ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = &ngapType.InitiatingMessage{}
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePaging
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPaging
	m := &ngapType.Paging{}
	pdu.InitiatingMessage.Value.Paging = m
	if id != nil {
		ie := ngapType.PagingIEs{}
		ie.Id.Value, ie.Value.Present = ngapType.ProtocolIEIDUEPagingIdentity, ngapType.PagingIEsPresentUEPagingIdentity
		ie.Value.UEPagingIdentity = &ngapType.UEPagingIdentity{Present: ngapType.UEPagingIdentityPresentFiveGSTMSI, FiveGSTMSI: id.ngap()}
		m.ProtocolIEs.List = append(m.ProtocolIEs.List, ie)
	}
	if len(tais) > 0 {
		ie := ngapType.PagingIEs{}
		ie.Id.Value, ie.Value.Present = ngapType.ProtocolIEIDTAIListForPaging, ngapType.PagingIEsPresentTAIListForPaging
		ie.Value.TAIListForPaging = &ngapType.TAIListForPaging{}
		for _, t := range tais {
			plmn, _ := ngap.EncodePLMN(t.MCC, t.MNC)
			item := ngapType.TAIListForPagingItem{}
			item.TAI.PLMNIdentity.Value = plmn[:]
			item.TAI.TAC.Value = []byte{byte(t.TAC >> 16), byte(t.TAC >> 8), byte(t.TAC)}
			ie.Value.TAIListForPaging.List = append(ie.Value.TAIListForPaging.List, item)
		}
		m.ProtocolIEs.List = append(m.ProtocolIEs.List, ie)
	}
	return &pdu
}

// ParsePaging reads the identity and every TAI of a PAGING through the
// codec, reports a missing identity, and refuses other PDUs.
func TestParsePaging(t *testing.T) {
	id := STMSI{AMFSetID: 0x3f8, AMFPointer: 1, TMSI: 42}
	tais := []TAI{{MCC: "208", MNC: "93", TAC: 1}, {MCC: "001", MNC: "01", TAC: 0x0a0b0c}}
	b, err := ngap.Encode(*pagingPDU(&id, tais...))
	if err != nil {
		t.Fatal(err)
	}
	dec, err := ngap.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ParsePaging(dec)
	if !ok || !p.HasSTMSI || p.STMSI != id || len(p.TAIs) != 2 || p.TAIs[0] != tais[0] || p.TAIs[1] != tais[1] {
		t.Errorf("ParsePaging = %+v (%v), want %s and %+v", p, ok, id, tais)
	}
	if p, ok := ParsePaging(pagingPDU(nil, tais[0])); !ok || p.HasSTMSI || len(p.TAIs) != 1 {
		t.Errorf("PAGING without an identity = %+v (%v)", p, ok)
	}
	release := BuildUEContextReleaseRequest(1, 1)
	for name, pdu := range map[string]*ngapType.NGAPPDU{"nil": nil, "another message": &release} {
		if _, ok := ParsePaging(pdu); ok {
			t.Errorf("%s parsed as PAGING", name)
		}
	}
}

// ReleaseCommandCause names a command's cause group and value and refuses
// other PDUs.
func TestReleaseCommandCause(t *testing.T) {
	cmd := func(c *ngapType.Cause) *ngapType.NGAPPDU {
		var pdu ngapType.NGAPPDU
		pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
		pdu.InitiatingMessage = &ngapType.InitiatingMessage{}
		pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseCommand
		m := &ngapType.UEContextReleaseCommand{}
		pdu.InitiatingMessage.Value.UEContextReleaseCommand = m
		if c != nil {
			ie := ngapType.UEContextReleaseCommandIEs{}
			ie.Id.Value, ie.Value.Cause = ngapType.ProtocolIEIDCause, c
			m.ProtocolIEs.List = append(m.ProtocolIEs.List, ie)
		}
		return &pdu
	}
	tests := []struct {
		name  string
		cause *ngapType.Cause
		want  string
	}{
		{"nas", &ngapType.Cause{Present: ngapType.CausePresentNas, Nas: &ngapType.CauseNas{Value: ngapType.CauseNasPresentNormalRelease}}, "nas/0"},
		{"radio network", &ngapType.Cause{Present: ngapType.CausePresentRadioNetwork, RadioNetwork: &ngapType.CauseRadioNetwork{Value: ngapType.CauseRadioNetworkPresentUserInactivity}}, "radio-network/20"},
		{"transport", &ngapType.Cause{Present: ngapType.CausePresentTransport, Transport: &ngapType.CauseTransport{Value: 1}}, "transport/1"},
		{"protocol", &ngapType.Cause{Present: ngapType.CausePresentProtocol, Protocol: &ngapType.CauseProtocol{Value: 2}}, "protocol/2"},
		{"misc", &ngapType.Cause{Present: ngapType.CausePresentMisc, Misc: &ngapType.CauseMisc{Value: 3}}, "misc/3"},
		{"no cause IE", nil, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := ReleaseCommandCause(cmd(tc.cause)); !ok || got != tc.want {
				t.Errorf("cause = %q (%v), want %q", got, ok, tc.want)
			}
		})
	}
	req := BuildUEContextReleaseRequest(1, 1)
	if _, ok := ReleaseCommandCause(&req); ok {
		t.Error("a UE CONTEXT RELEASE REQUEST read as a command")
	}
	if _, ok := ReleaseCommandCause(nil); ok {
		t.Error("nil read as a command")
	}
}

// An Initial Context Setup Response reports each set-up session's downlink
// tunnel; with no sessions it is the plain response. A request without the
// session list, or any other PDU, yields no sessions.
func TestInitialContextSetupSessions(t *testing.T) {
	res := []PDUSessionResource{{PDUSessionID: 1, QFIs: []int64{1}}, {PDUSessionID: 5, QFIs: []int64{1, 2}}}
	teids := map[int64]uint32{1: 0x101, 5: 0x105}
	pdu, err := BuildInitialContextSetupResponseSessions(3, 4, res, "10.0.0.7", func(id int64) uint32 { return teids[id] })
	if err != nil {
		t.Fatal(err)
	}
	b, err := ngap.Encode(pdu)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := ngap.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]uint32{}
	for _, ie := range dec.SuccessfulOutcome.Value.InitialContextSetupResponse.ProtocolIEs.List {
		if ie.Id.Value != ngapType.ProtocolIEIDPDUSessionResourceSetupListCxtRes {
			continue
		}
		for _, it := range ie.Value.PDUSessionResourceSetupListCxtRes.List {
			var tr ngapType.PDUSessionResourceSetupResponseTransfer
			if err := aper.UnmarshalWithParams(it.PDUSessionResourceSetupResponseTransfer, &tr, "valueExt"); err != nil {
				t.Fatal(err)
			}
			teid := tr.DLQosFlowPerTNLInformation.UPTransportLayerInformation.GTPTunnel.GTPTEID.Value
			got[it.PDUSessionID.Value] = uint32(teid[0])<<24 | uint32(teid[1])<<16 | uint32(teid[2])<<8 | uint32(teid[3])
		}
	}
	if len(got) != 2 || got[1] != 0x101 || got[5] != 0x105 {
		t.Errorf("response tunnels = %v, want %v", got, teids)
	}
	plain, err := BuildInitialContextSetupResponseSessions(3, 4, nil, "10.0.0.7", nil)
	if err != nil || len(plain.SuccessfulOutcome.Value.InitialContextSetupResponse.ProtocolIEs.List) != 2 {
		t.Errorf("response without sessions has %d IEs (%v), want the two IDs", len(plain.SuccessfulOutcome.Value.InitialContextSetupResponse.ProtocolIEs.List), err)
	}
	if _, err := BuildInitialContextSetupResponseSessions(3, 4, res, "not-an-ip", func(int64) uint32 { return 1 }); err == nil {
		t.Error("a response with an unusable gNB address built")
	}
	for name, p := range map[string]*ngapType.NGAPPDU{"nil": nil, "a response": &pdu} {
		if s, err := ParseInitialContextSetupSessions(p); err != nil || len(s) != 0 {
			t.Errorf("%s: sessions %v, err %v; want none", name, s, err)
		}
	}
}
