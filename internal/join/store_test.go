package join

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

var testSpec = Spec{UEPools: []string{"10.250.0.0/16"}, UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24", Management: "192.0.2.10:8412"}

// clock is a settable clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestStoreCreate(t *testing.T) {
	tests := []struct {
		name    string
		tok     string
		ttl     time.Duration
		spec    Spec
		wantTTL time.Duration
		wantErr string
	}{
		{name: "default ttl", spec: testSpec, wantTTL: DefaultTTL},
		{name: "named, an hour", tok: "app1", ttl: time.Hour, spec: testSpec, wantTTL: time.Hour},
		{name: "past the cap", ttl: time.Hour + time.Second, spec: testSpec, wantErr: "at most 1h"},
		{name: "negative", ttl: -time.Second, spec: testSpec, wantErr: "at most 1h"},
		{name: "a bad name", tok: "app 1", spec: testSpec, wantErr: "name:"},
		{name: "a name with a quote", tok: "a'b", spec: testSpec, wantErr: "name:"},
		{name: "no pools", spec: Spec{UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24", Management: "h:1"}, wantErr: "ue_pools"},
		{name: "a pool that is an address", spec: func() Spec { s := testSpec; s.UEPools = []string{"10.0.0.1"}; return s }(), wantErr: "not a CIDR"},
		{name: "a pool with shell", spec: func() Spec { s := testSpec; s.UEPools = []string{"10.0.0.0/8;id"}; return s }(), wantErr: "not a CIDR"},
		{name: "upf n6 a name", spec: func() Spec { s := testSpec; s.UPFN6 = "upf"; return s }(), wantErr: "upf_n6"},
		{name: "subnet an address", spec: func() Spec { s := testSpec; s.N6Subnet = "198.51.100.1"; return s }(), wantErr: "n6_subnet"},
		{name: "management with no port", spec: func() Spec { s := testSpec; s.Management = "ran1"; return s }(), wantErr: "management_address"},
		{name: "management with shell", spec: func() Spec { s := testSpec; s.Management = "$(id):8412"; return s }(), wantErr: "management_address"},
		{name: "management port 0", spec: func() Spec { s := testSpec; s.Management = "ran1:0"; return s }(), wantErr: "no valid port"},
		{name: "management a host name", spec: func() Spec { s := testSpec; s.Management = "ran-1.lab:8412"; return s }(), wantTTL: DefaultTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Unix(1_800_000_000, 0)}
			s := NewStore(WithClock(c.now))
			tok, secret, err := s.Create(tc.tok, tc.ttl, tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tokenRE.MatchString(secret) || !strings.HasPrefix(tok.ID, "jt-") || strings.Contains(tok.ID, secret[len(Prefix):]) {
				t.Errorf("secret %q, id %q", secret, tok.ID)
			}
			if got := tok.Expires.Sub(tok.Created); got != tc.wantTTL {
				t.Errorf("ttl = %s, want %s", got, tc.wantTTL)
			}
		})
	}
}

// The store keeps a token's hash, never its secret; a lookup finds a
// pending token without using it up; expired, revoked and unknown tokens
// all simply miss.
func TestStoreLookup(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s := NewStore(WithClock(c.now))
	tok, secret, err := s.Create("app1", 10*time.Minute, testSpec)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range s.tokens {
		if kept.hash != sha256.Sum256([]byte(secret)) {
			t.Error("the store holds something other than the secret's hash")
		}
	}
	for i := 0; i < 2; i++ {
		if got, ok := s.Lookup(secret); !ok || got.ID != tok.ID {
			t.Fatalf("lookup %d = %v, %t", i, got, ok)
		}
	}
	for _, miss := range []string{"", Prefix, secret + "x", strings.TrimPrefix(secret, Prefix), tok.ID} {
		if _, ok := s.Lookup(miss); ok {
			t.Errorf("lookup %q found a token", miss)
		}
	}
	c.t = tok.Expires.Add(-time.Nanosecond)
	if _, ok := s.Lookup(secret); !ok {
		t.Error("a token was gone before it expired")
	}
	c.t = tok.Expires
	if _, ok := s.Lookup(secret); ok {
		t.Error("an expired token was found")
	}
	if len(s.List()) != 0 {
		t.Error("an expired token is listed")
	}
}

func TestStoreListRevoke(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s := NewStore(WithClock(c.now))
	a, sa, _ := s.Create("a", 0, testSpec)
	c.t = c.t.Add(time.Second)
	b, _, _ := s.Create("", 0, testSpec)
	if _, _, err := s.Create("a", 0, testSpec); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a second pending %q: %v", "a", err)
	}
	if l := s.List(); len(l) != 2 || l[0].ID != a.ID || l[1].ID != b.ID {
		t.Fatalf("list = %+v", l)
	}
	if err := s.Revoke(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(sa); ok {
		t.Error("a revoked token was found")
	}
	if err := s.Revoke(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice: %v", err)
	}
	if _, _, err := s.Create("a", 0, testSpec); err != nil {
		t.Errorf("a revoked token's name is not free: %v", err)
	}
}
