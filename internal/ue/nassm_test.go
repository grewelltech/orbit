package ue

import (
	"encoding/hex"
	"testing"

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
