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

// Every gNB N3 tunnel encoder refuses a non-IPv4 address with an error,
// never the NGAP converter's panic (#156): the PDU session setup response,
// the path switch transfer and both handover acknowledge transfers.
func TestTunnelEncodersRefuseIPv6(t *testing.T) {
	encoders := map[string]func(addr string) error{
		"PDU session setup response": func(addr string) error {
			_, err := BuildPDUSessionResourceSetupResponse(1, 1, []PDUSessionResource{{PDUSessionID: 1, QFIs: []int64{1}}}, addr,
				func(int64) uint32 { return 7 })
			return err
		},
		"path switch transfer": func(addr string) error {
			_, err := encodePathSwitchTransfer(GNBTunnel{Address: addr, TEID: 7}, nil)
			return err
		},
		"handover acknowledge transfer": func(addr string) error {
			_, err := encodeHandoverAckTransfer(GNBTunnel{Address: addr, TEID: 7}, nil, false)
			return err
		},
		"handover acknowledge transfer, forwarding mandatory": func(addr string) error {
			_, err := encodeHandoverAckTransfer(GNBTunnel{Address: addr, TEID: 7}, nil, true)
			return err
		},
	}
	tests := []struct {
		addr    string
		wantErr error // nil: encodes; errAny: any error
	}{
		{addr: "10.0.0.5"},
		{addr: "::ffff:10.0.0.5"},
		{addr: "::1", wantErr: ErrIPv6N3},
		{addr: "fe80::1", wantErr: ErrIPv6N3},
		{addr: "2001:db8::5", wantErr: ErrIPv6N3},
		{addr: "not-an-ip", wantErr: errAny},
		{addr: "", wantErr: errAny},
	}
	for name, enc := range encoders {
		for _, tc := range tests {
			t.Run(name+"/"+tc.addr, func(t *testing.T) {
				err := enc(tc.addr)
				switch {
				case tc.wantErr == nil && err != nil:
					t.Errorf("err = %v, want none", err)
				case tc.wantErr == errAny && err == nil:
					t.Error("encoded an address that is not an IP")
				case tc.wantErr == ErrIPv6N3 && !errors.Is(err, ErrIPv6N3):
					t.Errorf("err = %v, want ErrIPv6N3", err)
				}
			})
		}
	}
}

// errAny marks a row that wants some error.
var errAny = errors.New("any error")
