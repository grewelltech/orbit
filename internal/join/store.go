// Package join issues the join tokens an app host presents to set up an N6
// responder (ADR-0009), and renders the script a token unlocks. The server
// keeps only a token's hash, with the addresses its script needs, until the
// token expires (at most an hour) or is revoked.
package join

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/target"
)

// Token lifetimes: the default, and the most a token may have.
const (
	DefaultTTL = 10 * time.Minute
	MaxTTL     = time.Hour
)

// Prefix starts every token, so one that leaks into a log or a paste can be
// found, and refused, by a grep.
const Prefix = "oj1_"

// tokenBytes is a token's entropy: 128 bits.
const tokenBytes = 16

// Spec is what a join script needs from the target: the UE pools to route
// back through the UPF, the UPF's N6 address (the next hop) and subnet (the
// firewall's source), and the address the app host reaches the server at.
type Spec struct {
	UEPools    []string
	UPFN6      string
	N6Subnet   string
	Management string // host:port
}

// Validate refuses a spec a script could not use, or that could carry
// anything but addresses: every pool a CIDR, the UPF's N6 an IP, the subnet
// a CIDR, the management address host:port.
func (s Spec) Validate() error {
	if len(s.UEPools) == 0 {
		return errors.New("ue_pools: give at least one UE pool CIDR (core.ue_pools, or --ue-pool)")
	}
	for _, p := range s.UEPools {
		if pr, err := netip.ParsePrefix(p); err != nil || pr.Addr().Zone() != "" {
			return fmt.Errorf("ue_pools: %q is not a CIDR", p)
		}
	}
	// No address may carry an IPv6 zone: a zone is free text (netip takes
	// any), and none of these is link-local to the app host.
	if a, err := netip.ParseAddr(s.UPFN6); err != nil || a.Zone() != "" {
		return fmt.Errorf("upf_n6: %q is not an IP address without a zone (core.upf_n6, or --upf-n6)", s.UPFN6)
	}
	if pr, err := netip.ParsePrefix(s.N6Subnet); err != nil || pr.Addr().Zone() != "" {
		return fmt.Errorf("n6_subnet: %q is not a CIDR (core.n6_subnet, or --n6-subnet)", s.N6Subnet)
	}
	host, port, err := net.SplitHostPort(s.Management)
	if err != nil || host == "" {
		return fmt.Errorf("management_address: %q is not host:port", s.Management)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return fmt.Errorf("management_address: %q has an IPv6 zone", host)
		}
	} else if !validHost(host) {
		return fmt.Errorf("management_address: %q is not an IP address or host name", host)
	}
	if p, err := netip.ParseAddrPort("0.0.0.0:" + port); err != nil || p.Port() == 0 {
		return fmt.Errorf("management_address: %q has no valid port", s.Management)
	}
	return nil
}

// validHost reports whether s is a DNS host name: dot-separated labels of
// letters, digits and inner hyphens.
func validHost(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if !hostChar(c) {
				return false
			}
		}
	}
	return true
}

// hostChar reports whether c may appear in a host name label.
func hostChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// Token is a pending token as the server keeps it: its ID, never its
// secret (only the secret's hash).
type Token struct {
	ID      string
	Name    string // "" = the app host's hostname, at registration
	Created time.Time
	Expires time.Time
	Spec    Spec
	hash    [sha256.Size]byte
	// burned is set by the first registration (Burn): burnedBy is the hash
	// of the credential that registered, responder its ID. A burned token
	// still serves its script until it expires, so a rerun can retry, but
	// registers nothing but that same credential.
	burned    bool
	burnedBy  [sha256.Size]byte
	responder string
}

// Store holds the pending tokens, in memory: a restart revokes them all.
type Store struct {
	now func() time.Time

	mu     sync.Mutex
	tokens map[[sha256.Size]byte]*Token
}

// Option configures a Store.
type Option func(*Store)

// WithClock replaces the store's clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// NewStore returns an empty store.
func NewStore(opts ...Option) *Store {
	s := &Store{now: time.Now, tokens: map[[sha256.Size]byte]*Token{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Errors Create refuses with.
var (
	ErrTTL       = fmt.Errorf("ttl: at most %s", MaxTTL)
	ErrName      = errors.New("name: a letter or digit, then up to 62 letters, digits, '-' or '_'")
	ErrNameTaken = errors.New("name taken by a pending join token")
	ErrNotFound  = errors.New("no pending join token with that ID")
)

// Create issues a token for spec, valid for ttl (0 = DefaultTTL), and
// returns it with its secret, which the store does not keep.
func (s *Store) Create(name string, ttl time.Duration, spec Spec) (Token, string, error) {
	switch {
	case ttl == 0:
		ttl = DefaultTTL
	case ttl < 0 || ttl > MaxTTL:
		return Token{}, "", ErrTTL
	}
	if name != "" && !target.ValidName(name) {
		return Token{}, "", ErrName
	}
	if err := spec.Validate(); err != nil {
		return Token{}, "", err
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, "", err
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return Token{}, "", err
	}
	secret := Prefix + base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	t := &Token{ID: "jt-" + hex.EncodeToString(id), Name: name, Created: now, Expires: now.Add(ttl),
		Spec: spec, hash: sha256.Sum256([]byte(secret))}
	t.Spec.UEPools = append([]string(nil), spec.UEPools...)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(now)
	if name != "" {
		// Only a pending token holds its name. A burned one has registered
		// its responder: whether that name is free is the registry's rule,
		// at registration (a re-join, or --fresh, replaces it there).
		for _, o := range s.tokens {
			if o.Name == name && !o.burned {
				return Token{}, "", fmt.Errorf("%w: token %s (expires in %s) already has that name; orbit join revoke %s",
					ErrNameTaken, o.ID, o.Expires.Sub(now).Round(time.Second), o.ID)
			}
		}
	}
	s.tokens[t.hash] = t
	return *t, secret, nil
}

// Lookup is the token whose secret this is, pending or burned; ok is false
// for an unknown, expired or revoked one alike. Looking a token up does not
// use it up.
func (s *Store) Lookup(secret string) (Token, bool) {
	if !strings.HasPrefix(secret, Prefix) {
		return Token{}, false
	}
	h := sha256.Sum256([]byte(secret))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweep(now)
	t, ok := s.tokens[h]
	// The map found it by hash; the compare keeps the answer constant-time
	// in the secret for any later change of lookup.
	if !ok || subtle.ConstantTimeCompare(t.hash[:], h[:]) != 1 {
		return Token{}, false
	}
	return *t, true
}

// List is the pending tokens, oldest first: a burned one is not pending.
func (s *Store) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(s.now())
	out := make([]Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		if !t.burned {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Revoke cancels the pending token with this ID. A token a registration
// burned is ErrBurned and left as it is: its record is what tells that
// registration's retry (the same credential) from another one.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(s.now())
	for h, t := range s.tokens {
		if t.ID == id {
			if t.burned {
				return ErrBurned
			}
			delete(s.tokens, h)
			return nil
		}
	}
	return ErrNotFound
}

// Burned reports whether t was burned, and by which credential hash and
// responder.
func (t Token) Burned() (burned bool, by [sha256.Size]byte, responder string) {
	return t.burned, t.burnedBy, t.responder
}

// ErrBurned refuses to burn a token a registration already used.
var ErrBurned = errors.New("join token already used")

// Burn marks the token as used by the registration of credential hash by
// for responder: atomically, so of two registrations racing with one token
// exactly one burns it. A token that is unknown, expired or revoked is
// ErrNotFound; one already burned is ErrBurned.
func (s *Store) Burn(secret string, by [sha256.Size]byte, responder string) error {
	if !strings.HasPrefix(secret, Prefix) {
		return ErrNotFound
	}
	h := sha256.Sum256([]byte(secret))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(s.now())
	t, ok := s.tokens[h]
	if !ok {
		return ErrNotFound
	}
	if t.burned {
		return ErrBurned
	}
	t.burned, t.burnedBy, t.responder = true, by, responder
	return nil
}

// sweep drops expired tokens; the caller holds mu.
func (s *Store) sweep(now time.Time) {
	for h, t := range s.tokens {
		if !now.Before(t.Expires) {
			delete(s.tokens, h)
		}
	}
}
