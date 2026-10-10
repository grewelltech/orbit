package sctp

import (
	"strings"
	"testing"
)

func TestNormalizeLocal(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "empty means kernel default", in: "", want: ""},
		{name: "bare ipv4 gets ephemeral port", in: "10.0.0.1", want: "10.0.0.1:0"},
		{name: "ipv4 with zero port unchanged", in: "10.0.0.1:0", want: "10.0.0.1:0"},
		{name: "ipv4 with explicit port unchanged", in: "10.0.0.1:38412", want: "10.0.0.1:38412"},
		{name: "bare ipv6 is bracketed", in: "::1", want: "[::1]:0"},
		{name: "bracketed ipv6 with port unchanged", in: "[::1]:0", want: "[::1]:0"},
		{name: "bracketed ipv6 with explicit port unchanged", in: "[fd00::21]:38412", want: "[fd00::21]:38412"},
		{name: "hostname rejected", in: "not-an-ip", wantErr: true},
		{name: "hostname with port rejected", in: "localhost:0", wantErr: true},
		{name: "trailing colon rejected", in: "10.0.0.1:", wantErr: true},
		{name: "port only rejected", in: ":38412", wantErr: true},
		{name: "bracketed ipv6 without port rejected", in: "[::1]", wantErr: true},
		{name: "garbage rejected", in: "10.0.0.1:0:0", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeLocal(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeLocal(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeLocal(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeLocal(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A malformed local address fails before any socket is touched, with the
// normalisation error rather than a resolver or bind error.
func TestDialRejectsBadLocal(t *testing.T) {
	_, err := Dial("not-an-ip", "127.0.0.1:38412")
	if err == nil {
		t.Fatal("Dial with a bad local address succeeded")
	}
	if want := `invalid local address "not-an-ip"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}
}
