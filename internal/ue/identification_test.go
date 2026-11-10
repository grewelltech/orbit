package ue

import (
	"bytes"
	"testing"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

func TestEquipmentIdentity(t *testing.T) {
	tests := []struct {
		name       string
		id         Identity
		wantIMEISV string
		wantIMEI   string
		wantErr    bool
	}{
		{name: "derived from a 15-digit SUPI", id: Identity{SUPI: "208930100007500"},
			wantIMEISV: "0010010000750000", wantIMEI: "001001000075009"},
		{name: "the next SUPI differs", id: Identity{SUPI: "208930100007501"},
			wantIMEISV: "0010010000750100", wantIMEI: "001001000075017"},
		{name: "short SUPI is zero-padded", id: Identity{SUPI: "00101"},
			wantIMEISV: "0010000000010100", wantIMEI: "001000000001015"},
		{name: "configured", id: Identity{SUPI: "208930100007500", IMEISV: "4901542032375186"},
			wantIMEISV: "4901542032375186", wantIMEI: "490154203237518"},
		{name: "configured, too short", id: Identity{SUPI: "208930100007500", IMEISV: "490154203237518"}, wantErr: true},
		{name: "configured, not digits", id: Identity{SUPI: "208930100007500", IMEISV: "49015420323751AB"}, wantErr: true},
		{name: "no SUPI", id: Identity{}, wantErr: true},
		{name: "SUPI not digits", id: Identity{SUPI: "imsi-20893"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sv, err := tc.id.EquipmentIMEISV()
			imei, err2 := tc.id.EquipmentIMEI()
			if tc.wantErr {
				if err == nil || err2 == nil {
					t.Fatalf("IMEISV %q (%v), IMEI %q (%v): want errors", sv, err, imei, err2)
				}
				return
			}
			if err != nil || err2 != nil {
				t.Fatal(err, err2)
			}
			if sv != tc.wantIMEISV || imei != tc.wantIMEI {
				t.Errorf("IMEISV %s, IMEI %s; want %s, %s", sv, imei, tc.wantIMEISV, tc.wantIMEI)
			}
		})
	}
}

// Each type of identity an Identity Request can name gets its 5GS mobile
// identity value (TS 24.501 9.11.3.4); one the UE cannot give is "No
// identity".
func TestMobileIdentity(t *testing.T) {
	id, err := ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	suci, err := id.EncodeNullSUCI()
	if err != nil {
		t.Fatal(err)
	}
	guti := []byte{0xf2, 0x02, 0xf8, 0x39, 0xca, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x01}
	none := []byte{0x00}
	tests := []struct {
		name   string
		idType uint8
		guti   []byte
		want   []byte
	}{
		{name: "SUCI", idType: IdentityTypeSUCI, want: suci},
		// 0010010000750000: digit 1 with type 5 and the even indication,
		// then digit pairs low nibble first, and the end mark.
		{name: "IMEISV", idType: IdentityTypeIMEISV, want: []byte{0x05, 0x10, 0x00, 0x01, 0x00, 0x70, 0x05, 0x00, 0xf0}},
		// 001001000075009: odd indication set, no end mark.
		{name: "IMEI", idType: IdentityTypeIMEI, want: []byte{0x0b, 0x10, 0x00, 0x01, 0x00, 0x70, 0x05, 0x90}},
		{name: "5G-GUTI held", idType: IdentityTypeGUTI, guti: guti, want: guti},
		{name: "5G-GUTI not held", idType: IdentityTypeGUTI, want: none},
		{name: "5G-GUTI of a wrong length", idType: IdentityTypeGUTI, guti: guti[:5], want: none},
		{name: "5G-S-TMSI from the 5G-GUTI", idType: IdentityTypeSTMSI, guti: guti, want: []byte{0xf4, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x01}},
		{name: "5G-S-TMSI without a 5G-GUTI", idType: IdentityTypeSTMSI, want: none},
		{name: "No identity", idType: IdentityTypeNone, want: none},
		{name: "MAC address", idType: 6, want: none},
		{name: "EUI-64", idType: 7, want: none},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := id.MobileIdentity(tc.idType, tc.guti)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("= %x, want %x", got, tc.want)
			}
			// The response round-trips through the codec, and the request
			// that asks for it parses back to the type.
			resp, err := BuildIdentityResponse(got)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := resp.PlainNasEncode()
			if err != nil {
				t.Fatal(err)
			}
			m, err := nas.DecodePlain(raw)
			if err != nil {
				t.Fatal(err)
			}
			if r := m.IdentityResponse; r == nil || !bytes.Equal(r.Buffer[:r.GetLen()], tc.want) {
				t.Errorf("decoded Identity Response = %+v, want %x", r, tc.want)
			}
			req := f5nas.NewMessage()
			req.GmmMessage = f5nas.NewGmmMessage()
			req.IdentityRequest = nasMessage.NewIdentityRequest(f5nas.MsgTypeIdentityRequest)
			req.IdentityRequest.SetTypeOfIdentity(tc.idType)
			if got, err := ParseIdentityRequest(req); err != nil || got != tc.idType {
				t.Errorf("ParseIdentityRequest = %d, %v; want %d", got, err, tc.idType)
			}
		})
	}
	if _, err := (Identity{SUPI: "208930100007500", IMEISV: "12"}).MobileIdentity(IdentityTypeIMEISV, nil); err == nil {
		t.Error("an invalid configured IMEISV was encoded")
	}
	if _, err := BuildIdentityResponse(nil); err == nil {
		t.Error("built an Identity Response without an identity")
	}
	if _, err := ParseIdentityRequest(f5nas.NewMessage()); err == nil {
		t.Error("parsed a message that is not an Identity Request")
	}
}

// The Security Mode Complete carries the IMEISV when asked to, together
// with the complete Registration Request in the NAS message container
// (TS 24.501 5.4.2.3), and the request is read from the Security Mode
// Command's IMEISV request IE.
func TestSecurityModeCompleteIMEISV(t *testing.T) {
	complete, err := BuildRegistrationRequest(buildSUCI(t), SecurityCapability(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		opts    []SecurityModeCompleteOption
		want    []byte // the IMEISV IE's value, nil for no IE
		wantErr bool
	}{
		{name: "not requested"},
		{name: "requested", opts: []SecurityModeCompleteOption{WithIMEISV("4370816125816151")},
			want: []byte{0x45, 0x73, 0x80, 0x61, 0x21, 0x85, 0x61, 0x51, 0xf1}},
		{name: "15 digits", opts: []SecurityModeCompleteOption{WithIMEISV("437081612581615")}, wantErr: true},
		{name: "not digits", opts: []SecurityModeCompleteOption{WithIMEISV("43708161258161xx")}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := BuildSecurityModeComplete(complete, tc.opts...)
			if tc.wantErr {
				if err == nil {
					t.Fatal("built a Security Mode Complete with an invalid IMEISV")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := m.PlainNasEncode()
			if err != nil {
				t.Fatal(err)
			}
			got, err := nas.DecodePlain(raw)
			if err != nil {
				t.Fatal(err)
			}
			smc := got.SecurityModeComplete
			if c := smc.NASMessageContainer; c == nil || !bytes.Equal(c.Buffer[:c.GetLen()], complete) {
				t.Errorf("NAS message container = %+v, want the complete request", c)
			}
			switch {
			case tc.want == nil && smc.IMEISV != nil:
				t.Errorf("IMEISV IE %x present, want none", smc.IMEISV.Octet)
			case tc.want != nil && (smc.IMEISV == nil || !bytes.Equal(smc.IMEISV.Octet[:smc.IMEISV.GetLen()], tc.want)):
				t.Errorf("IMEISV IE = %+v, want %x", smc.IMEISV, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		ie   *nasType.IMEISVRequest
		want bool
	}{
		{name: "no IMEISV request IE"},
		{name: "IMEISV not requested", ie: &nasType.IMEISVRequest{Octet: 0xe0}},
		{name: "IMEISV requested", ie: &nasType.IMEISVRequest{Octet: 0xe1}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := f5nas.NewMessage()
			m.GmmMessage = f5nas.NewGmmMessage()
			m.SecurityModeCommand = nasMessage.NewSecurityModeCommand(f5nas.MsgTypeSecurityModeCommand)
			m.IMEISVRequest = tc.ie
			sel, err := ParseSecurityModeCommand(m)
			if err != nil {
				t.Fatal(err)
			}
			if sel.IMEISVRequested != tc.want {
				t.Errorf("IMEISVRequested = %t, want %t", sel.IMEISVRequested, tc.want)
			}
		})
	}
}
