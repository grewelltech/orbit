package ue

import (
	"encoding/hex"
	"testing"

	"github.com/free5gc/nas/nasConvert"
)

func TestEncodeNullSUCIGolden(t *testing.T) {
	id, err := ParseIdentity("imsi-208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	got, err := id.EncodeNullSUCI()
	if err != nil {
		t.Fatal(err)
	}
	// Golden bytes: 01 (SUCI/IMSI) | 02 f8 39 (PLMN 208/93) |
	// f0 ff (RID 0) | 00 (null) | 00 (key id) | 10 00 00 57 00 (MSIN).
	want := "01" + "02f839" + "f0ff" + "00" + "00" + "1000005700"
	if hex.EncodeToString(got) != want {
		t.Errorf("SUCI bytes:\n got  %s\n want %s", hex.EncodeToString(got), want)
	}
}

// TestNullSUCIRoundTrip is the §5f verification method: encode UE-side, then
// decode with free5gc's network-side decoder and confirm the original
// identity is recovered. For the null scheme this is a plain round trip; the
// same harness extends to decrypt-and-compare for ciphered Profile A/B.
func TestNullSUCIRoundTrip(t *testing.T) {
	cases := []struct {
		supi, mcc, mnc, rid string
		wantSuci            string
	}{
		{"208930100007500", "208", "93", "0", "suci-0-208-93-0-0-0-0100007500"},
		{"208930100007599", "208", "93", "0", "suci-0-208-93-0-0-0-0100007599"},
		{"imsi-208930100007512", "208", "93", "0", "suci-0-208-93-0-0-0-0100007512"},
	}
	for _, c := range cases {
		id, err := ParseIdentity(c.supi, c.mcc, c.mnc, c.rid)
		if err != nil {
			t.Fatalf("%s: %v", c.supi, err)
		}
		buf, err := id.EncodeNullSUCI()
		if err != nil {
			t.Fatalf("%s: %v", c.supi, err)
		}
		suci, plmn, err := nasConvert.SuciToStringWithError(buf)
		if err != nil {
			t.Fatalf("%s: free5gc decode failed: %v", c.supi, err)
		}
		if suci != c.wantSuci {
			t.Errorf("%s: decoded suci = %q, want %q", c.supi, suci, c.wantSuci)
		}
		if plmn != c.mcc+c.mnc {
			t.Errorf("%s: decoded plmn = %q, want %q", c.supi, plmn, c.mcc+c.mnc)
		}
	}
}

func TestParseIdentityValidation(t *testing.T) {
	bad := []struct{ supi, mcc, mnc, rid string }{
		{"208930100007500", "20", "93", "0"},      // short mcc
		{"208930100007500", "208", "9", "0"},      // short mnc
		{"999930100007500", "208", "93", "0"},     // supi prefix mismatch
		{"20893010000750x", "208", "93", "0"},     // non-digit supi
		{"208930100007500", "208", "93", "12345"}, // rid too long
	}
	for _, c := range bad {
		if _, err := ParseIdentity(c.supi, c.mcc, c.mnc, c.rid); err == nil {
			t.Errorf("ParseIdentity(%q,%q,%q,%q): expected error", c.supi, c.mcc, c.mnc, c.rid)
		}
	}
}
