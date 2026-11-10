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

// The establishment request carries the PTI it was given, 0 by default.
func TestBuildPDUSessionEstablishmentRequestPTI(t *testing.T) {
	for _, pti := range []uint8{0, 1, 254} {
		sm, err := BuildPDUSessionEstablishmentRequest(PDUSessionParams{PDUSessionID: 3, SST: 1, DNN: "internet", PTI: pti})
		if err != nil {
			t.Fatal(err)
		}
		if len(sm) < 4 || sm[1] != 3 || sm[2] != pti || sm[3] != f5nas.MsgTypePDUSessionEstablishmentRequest {
			t.Errorf("PTI %d: request header %x", pti, sm)
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
