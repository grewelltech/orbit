package gnb

import (
	"errors"
	"testing"
)

// CanonicalN3 accepts IPv4 and IPv4-mapped forms (canonicalised), passes ""
// through, and refuses IPv6, hostnames, host:port and garbage.
func TestCanonicalN3(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
		ipv6    bool
	}{
		{in: "", want: ""},
		{in: "10.0.0.5", want: "10.0.0.5"},
		{in: "::ffff:10.0.0.5", want: "10.0.0.5"},
		{in: "0:0:0:0:0:ffff:a00:5", want: "10.0.0.5"},
		{in: "0:0:0:0:0:0:0:1", wantErr: true, ipv6: true},
		{in: "::1", wantErr: true, ipv6: true},
		{in: "2001:db8::5", wantErr: true, ipv6: true},
		{in: "10.0.0.5:2152", wantErr: true},
		{in: "gnb.example", wantErr: true},
		{in: "10.0.0", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := CanonicalN3(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CanonicalN3(%q) = %q, want an error", tc.in, got)
				}
				if errors.Is(err, ErrIPv6N3) != tc.ipv6 {
					t.Errorf("CanonicalN3(%q) error %v: IPv6 refusal %v, want %v", tc.in, err, errors.Is(err, ErrIPv6N3), tc.ipv6)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("CanonicalN3(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}
