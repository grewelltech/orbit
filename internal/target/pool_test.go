package target

import (
	"reflect"
	"strings"
	"testing"
)

// ExpandPool walks a pool in order and hands out the first n addresses:
// ranges address by address, IPv4 CIDRs by host address, IP and ip:port
// entries as they are.
func TestExpandPool(t *testing.T) {
	tests := []struct {
		name    string
		pool    []string
		n       int
		want    []string
		wantErr string
	}{
		{name: "range", pool: []string{"127.0.0.2-127.0.0.9"}, n: 3, want: []string{"127.0.0.2", "127.0.0.3", "127.0.0.4"}},
		{name: "range shorter than n", pool: []string{"127.0.0.2-127.0.0.4"}, n: 5, want: []string{"127.0.0.2", "127.0.0.3", "127.0.0.4"}},
		{name: "cidr /29: hosts only", pool: []string{"192.0.2.0/29"}, n: 10,
			want: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4", "192.0.2.5", "192.0.2.6"}},
		{name: "cidr /30: hosts only", pool: []string{"192.0.2.4/30"}, n: 10, want: []string{"192.0.2.5", "192.0.2.6"}},
		{name: "cidr /31: both", pool: []string{"192.0.2.4/31"}, n: 10, want: []string{"192.0.2.4", "192.0.2.5"}},
		{name: "cidr /32: the one", pool: []string{"192.0.2.9/32"}, n: 10, want: []string{"192.0.2.9"}},
		{name: "cidr unmasked", pool: []string{"192.0.2.7/29"}, n: 1, want: []string{"192.0.2.1"}},
		{name: "ipv6 cidr: every address", pool: []string{"2001:db8::/126"}, n: 10, want: []string{"2001:db8::", "2001:db8::1", "2001:db8::2", "2001:db8::3"}},
		{name: "mixed list in order", pool: []string{"192.0.2.50", "192.0.2.60:0", "192.0.2.70-192.0.2.71", "198.51.100.0/30"}, n: 10,
			want: []string{"192.0.2.50", "192.0.2.60:0", "192.0.2.70", "192.0.2.71", "198.51.100.1", "198.51.100.2"}},
		{name: "first n of a mixed list", pool: []string{"192.0.2.50", "192.0.2.70-192.0.2.79"}, n: 2, want: []string{"192.0.2.50", "192.0.2.70"}},
		{name: "a /8 stops at n", pool: []string{"10.0.0.0/8"}, n: 2, want: []string{"10.0.0.1", "10.0.0.2"}},
		{name: "cap", pool: []string{"10.0.0.0/8"}, n: MaxPoolExpand + 1, wantErr: "65537 addresses is more than a pool expands to (65536)"},
		{name: "at the cap", pool: []string{"10.0.0.0/8"}, n: MaxPoolExpand},
		{name: "host names pass unresolved", pool: []string{"lab-n2", "lab-n2b:0", "192.0.2.5"}, n: 3, want: []string{"lab-n2", "lab-n2b:0", "192.0.2.5"}},
		{name: "bad entry", pool: []string{"bad_host!"}, n: 1, wantErr: `entry "bad_host!" is not an address`},
		{name: "bad IP is not a host name", pool: []string{"192.0.2.300"}, n: 1, wantErr: `entry "192.0.2.300" is not an address`},
		{name: "range with mismatched zones", pool: []string{"fe80::1-fe80::9%eth1"}, n: 1, wantErr: "is not an address range"},
		{name: "range zoned at the low end only", pool: []string{"fe80::1%eth0-fe80::9"}, n: 1, wantErr: "is not an address range"},
		{name: "range zoned at both ends", pool: []string{"fe80::1%eth0-fe80::3%eth0"}, n: 5, want: []string{"fe80::1%eth0", "fe80::2%eth0", "fe80::3%eth0"}},
		{name: "a zone with a hyphen is one address", pool: []string{"fe80::1%br-lan"}, n: 5, want: []string{"fe80::1%br-lan"}},
		{name: "backwards range", pool: []string{"192.0.2.9-192.0.2.1"}, n: 1, wantErr: "is not an address range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandPool(tc.pool, tc.n)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "at the cap" {
				if len(got) != MaxPoolExpand {
					t.Fatalf("%d addresses at the cap", len(got))
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}
