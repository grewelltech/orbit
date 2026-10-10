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
		{name: "zoned link-local ipv6 is bracketed", in: "fe80::1%eth0", want: "[fe80::1%eth0]:0"},
		{name: "zoned link-local ipv6 with port unchanged", in: "[fe80::1%eth0]:0", want: "[fe80::1%eth0]:0"},
		{name: "bare hostname gets ephemeral port", in: "localhost", want: "localhost:0"},
		{name: "hostname with port unchanged", in: "localhost:0", want: "localhost:0"},
		{name: "unresolvable hostname is still a hostname here", in: "not-an-ip", want: "not-an-ip:0"},
		{name: "out-of-range port passes normalisation (resolve rejects it)", in: "10.0.0.1:70000", want: "10.0.0.1:70000"},
		{name: "trailing colon rejected", in: "10.0.0.1:", wantErr: true},
		{name: "port only rejected", in: ":38412", wantErr: true},
		{name: "bracketed ipv6 without port rejected", in: "[::1]", wantErr: true},
		{name: "too many colons rejected", in: "10.0.0.1:0:0", wantErr: true},
		{name: "unbracketed ipv6 with port rejected", in: "::1:38412:0", wantErr: true},
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
// normalisation error rather than a resolver or bind error — and before the
// remote is looked at, so a bad --bind is reported even alongside a bad AMF.
func TestDialRejectsBadLocal(t *testing.T) {
	_, err := Dial("10.0.0.1:0:0", "not-an-amf")
	if err == nil {
		t.Fatal("Dial with a bad local address succeeded")
	}
	if want := `invalid local address "10.0.0.1:0:0"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}
}

// The port range is enforced by resolve, as it always was: 70000 passes
// normalisation and is rejected before any socket is opened.
func TestDialRejectsOutOfRangeLocalPort(t *testing.T) {
	_, err := Dial("10.0.0.1:70000", "127.0.0.1:38412")
	if err == nil {
		t.Fatal("Dial with port 70000 succeeded")
	}
	if !strings.Contains(err.Error(), `resolve local "10.0.0.1:70000"`) {
		t.Errorf("error %q does not name the local address", err)
	}
}

// A local address of one IP family paired with an AMF of the other is
// refused before any socket is opened, naming both, instead of sitting
// through the kernel's INIT retransmits for minutes.
func TestDialRejectsFamilyMismatch(t *testing.T) {
	tests := []struct {
		name, laddr, raddr string
	}{
		{name: "ipv6 local to ipv4 amf", laddr: "::1", raddr: "127.0.0.1:38412"},
		{name: "bracketed ipv6 local to ipv4 amf", laddr: "[::1]:0", raddr: "127.0.0.1:38412"},
		{name: "ipv4 local to ipv6 amf", laddr: "127.0.0.1", raddr: "[::1]:38412"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Dial(tc.laddr, tc.raddr)
			if err == nil {
				t.Fatal("mismatched families dialled")
			}
			if !strings.Contains(err.Error(), "address family mismatch") {
				t.Errorf("err = %v, want a family-mismatch error", err)
			}
		})
	}
	// Matching families are not refused here (the dial itself may still fail).
	if _, err := Dial("::1", "[::1]:1"); err != nil && strings.Contains(err.Error(), "address family mismatch") {
		t.Errorf("same-family dial refused as a mismatch: %v", err)
	}
}

// A dial that fails after the local address was given names that address,
// so "cannot assign requested address" points at the bind, not the AMF.
// Whatever this host's SCTP support (refused, or no kernel module), the
// error carries the normalised source.
func TestDialErrorNamesLocal(t *testing.T) {
	_, err := Dial("127.0.0.1", "127.0.0.1:1")
	if err == nil {
		t.Skip("an SCTP listener answered on 127.0.0.1:1")
	}
	if !strings.Contains(err.Error(), `from "127.0.0.1:0"`) {
		t.Errorf("error %q does not name the local address", err)
	}
}
