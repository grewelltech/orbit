package ue

import (
	"encoding/hex"
	"testing"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/nas"
)

func TestBuildRequestedNSSAI(t *testing.T) {
	cases := []struct {
		slices []SNSSAI
		want   string // hex of buffer, "" means nil IE
	}{
		{nil, ""},
		{[]SNSSAI{{SST: 1, SD: "010203"}}, "0401010203"},
		{[]SNSSAI{{SST: 1}}, "0101"},
		{[]SNSSAI{{SST: 1, SD: "010203"}, {SST: 2}}, "04010102030102"},
	}
	for _, c := range cases {
		got, err := BuildRequestedNSSAI(c.slices)
		if err != nil {
			t.Fatalf("%v: %v", c.slices, err)
		}
		if c.want == "" {
			if got != nil {
				t.Errorf("%v: expected nil IE, got %x", c.slices, got.Buffer)
			}
			continue
		}
		if hex.EncodeToString(got.Buffer) != c.want {
			t.Errorf("%v: buffer %x, want %s", c.slices, got.Buffer, c.want)
		}
		if int(got.Len) != len(got.Buffer) {
			t.Errorf("%v: len %d != buffer %d", c.slices, got.Len, len(got.Buffer))
		}
	}
}

// TestRegistrationRequestCarriesNSSAI confirms the Requested NSSAI survives a
// full Registration Request encode/decode through free5gc.
func TestRegistrationRequestCarriesNSSAI(t *testing.T) {
	id, err := ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	suci, err := id.EncodeNullSUCI()
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequestedNSSAI([]SNSSAI{{SST: 1, SD: "010203"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := BuildRegistrationRequest(suci, SecurityCapability(), req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := nas.DecodePlain(raw)
	if err != nil {
		t.Fatal(err)
	}
	rr := m.RegistrationRequest
	if rr == nil || rr.RequestedNSSAI == nil {
		t.Fatal("Registration Request lost the Requested NSSAI")
	}
	if hex.EncodeToString(rr.RequestedNSSAI.Buffer) != "0401010203" {
		t.Errorf("decoded Requested NSSAI = %x", rr.RequestedNSSAI.Buffer)
	}
}

// The establishment request carries the PTI it was given; without one it
// carries the PDU session ID.
func TestBuildPDUSessionEstablishmentRequestPTI(t *testing.T) {
	for _, tc := range []struct{ pti, want uint8 }{{0, 3}, {1, 1}, {254, 254}} {
		sm, err := BuildPDUSessionEstablishmentRequest(PDUSessionParams{PDUSessionID: 3, SST: 1, DNN: "internet", PTI: tc.pti})
		if err != nil {
			t.Fatal(err)
		}
		if len(sm) < 4 || sm[1] != 3 || sm[2] != tc.want || sm[3] != f5nas.MsgTypePDUSessionEstablishmentRequest {
			t.Errorf("PTI %d: request header %x, want PTI %d", tc.pti, sm, tc.want)
		}
	}
}

// The release request decodes back to its session and transaction, and its
// UL NAS Transport names the session with no request type, DNN or S-NSSAI.
func TestBuildPDUSessionReleaseRequest(t *testing.T) {
	tests := []struct{ psi, pti uint8 }{{1, 1}, {15, 254}}
	for _, tc := range tests {
		sm, err := BuildPDUSessionReleaseRequest(tc.psi, tc.pti)
		if err != nil {
			t.Fatal(err)
		}
		m := f5nas.NewMessage()
		buf := append([]byte(nil), sm...)
		if err := m.PlainNasDecode(&buf); err != nil {
			t.Fatal(err)
		}
		rel := m.PDUSessionReleaseRequest
		if m.GsmMessage == nil || rel == nil || rel.GetPDUSessionID() != tc.psi || rel.GetPTI() != tc.pti {
			t.Errorf("release request %+v, want session %d PTI %d", rel, tc.psi, tc.pti)
		}
		tr, err := BuildULNASTransportForSM(sm, tc.psi)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := tr.PlainNasEncode()
		if err != nil {
			t.Fatal(err)
		}
		dec, err := nas.DecodePlain(raw)
		if err != nil {
			t.Fatal(err)
		}
		ul := dec.ULNASTransport
		if ul == nil || ul.PduSessionID2Value == nil || ul.GetPduSessionID2Value() != tc.psi ||
			ul.RequestType != nil || ul.DNN != nil || ul.SNSSAI != nil {
			t.Errorf("UL NAS Transport %+v, want session %d and no request type, DNN or S-NSSAI", ul, tc.psi)
		}
		if got := ul.GetPayloadContainerContents(); hex.EncodeToString(got) != hex.EncodeToString(sm) {
			t.Errorf("payload %x, want %x", got, sm)
		}
	}
	if _, err := BuildULNASTransportForSM(nil, 1); err == nil {
		t.Error("an empty 5GSM payload was accepted")
	}
}

// WithMobilityRegistration turns the request into a mobility registration
// update under key set ngKSI, naming the UE by the identity given; an ngKSI
// that names no key set is refused.
func TestWithMobilityRegistration(t *testing.T) {
	guti := []byte{0xf2, 0x02, 0xf8, 0x39, 0xca, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07}
	tests := []struct {
		name    string
		ngKSI   uint8
		wantErr bool
	}{
		{name: "key set 0", ngKSI: 0},
		{name: "key set 1", ngKSI: 1},
		{name: "key set 6", ngKSI: 6},
		{name: "no key available", ngKSI: 7, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := BuildRegistrationRequest(guti, SecurityCapability(), nil, WithMobilityRegistration(tc.ngKSI))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			m, err := nas.DecodePlain(raw)
			if err != nil {
				t.Fatal(err)
			}
			rr := m.RegistrationRequest
			if rr == nil {
				t.Fatal("not a Registration Request")
			}
			k := rr.NgksiAndRegistrationType5GS
			if k.GetRegistrationType5GS() != 2 || k.GetNasKeySetIdentifiler() != tc.ngKSI || k.GetFOR() != 0 || k.GetTSC() != 0 {
				t.Errorf("type %d ngKSI %d FOR %d TSC %d, want mobility updating (2), ngKSI %d, no follow-on, native",
					k.GetRegistrationType5GS(), k.GetNasKeySetIdentifiler(), k.GetFOR(), k.GetTSC(), tc.ngKSI)
			}
			if hex.EncodeToString(rr.MobileIdentity5GS.Buffer) != hex.EncodeToString(guti) {
				t.Errorf("identity %x, want the 5G-GUTI %x", rr.MobileIdentity5GS.Buffer, guti)
			}
		})
	}
}

// The PDU Session Establishment Request carries an allocated PTI, the PDU
// session ID, never 0 (TS 24.501 6.4.1.2); a PDU session ID outside 1 to 15
// is refused.
func TestPDUSessionEstablishmentRequestPTI(t *testing.T) {
	tests := []struct {
		name    string
		id      uint8
		wantErr bool
	}{
		{name: "session 1", id: 1},
		{name: "session 5", id: 5},
		{name: "session 15", id: 15},
		{name: "session 0", id: 0, wantErr: true},
		{name: "session 16", id: 16, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := BuildPDUSessionEstablishmentRequest(PDUSessionParams{PDUSessionID: tc.id, SST: 1, DNN: "internet"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("built a request for PDU session %d", tc.id)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m, err := nas.DecodePlain(raw)
			if err != nil {
				t.Fatal(err)
			}
			req := m.PDUSessionEstablishmentRequest
			if req == nil || req.GetPDUSessionID() != tc.id || req.GetPTI() != tc.id {
				t.Errorf("request = %+v, want PDU session ID and PTI %d", req, tc.id)
			}
		})
	}
}
