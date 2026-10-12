package ue

import (
	"strings"
	"testing"
)

// NormalizeSUPI accepts the bare IMSI and the imsi- NAI form, 5 to 15 digits,
// and returns digits only; everything else is one error naming both forms.
func TestNormalizeSUPI(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "bare 15 digits", in: "208930100007500", want: "208930100007500"},
		{name: "nai 15 digits", in: "imsi-208930100007500", want: "208930100007500"},
		{name: "bare 5 digits (minimum)", in: "20893", want: "20893"},
		{name: "nai 5 digits", in: "imsi-20893", want: "20893"},
		{name: "16 digits rejected", in: "2089301000075001", wantErr: true},
		{name: "4 digits rejected", in: "2089", wantErr: true},
		{name: "upper-case prefix rejected", in: "IMSI-208930100007500", wantErr: true},
		{name: "other prefix rejected", in: "nai-208930100007500", wantErr: true},
		{name: "letter inside rejected", in: "imsi-20893010000750a", wantErr: true},
		{name: "prefix only rejected", in: "imsi-", wantErr: true},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "spaces rejected", in: " 208930100007500", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSUPI(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeSUPI(%q) = %q, want an error", tc.in, got)
				}
				if !strings.Contains(err.Error(), "want 5-15 digits or imsi-<digits>") || !strings.Contains(err.Error(), tc.in) {
					t.Errorf("error %q does not name the input and the accepted forms", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSUPI(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeSUPI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Both spellings parse to the same Identity, with a digits-only SUPI, and a
// malformed SUPI fails with the normalisation error before any other check.
func TestParseIdentityAcceptsBothSUPIForms(t *testing.T) {
	bare, err := ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	nai, err := ParseIdentity("imsi-208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	if bare != nai {
		t.Errorf("identities differ: bare %+v, nai %+v", bare, nai)
	}
	if nai.SUPI != "208930100007500" || nai.MSIN != "0100007500" {
		t.Errorf("nai identity = %+v, want digits-only SUPI and MSIN 0100007500", nai)
	}
	if _, err := ParseIdentity("IMSI-208930100007500", "208", "93", "0"); err == nil || !strings.Contains(err.Error(), "want 5-15 digits or imsi-<digits>") {
		t.Errorf("upper-case prefix: err = %v, want the normalisation error", err)
	}
}
