package join

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/target"
)

// Responder secrets: a credential (orc1_) the app host generates and keeps,
// of which the server stores only the hash, and the loom control token
// (orl1_) the server generates and presents to the responder's loom agent.
// Both are 128 random bits, base64url, so a leaked one is found by a grep.
const (
	CredentialPrefix = "orc1_"
	LoomTokenPrefix  = "orl1_"
)

// Heartbeat timing (ADR-0009 open question 5): a responder beats every
// HeartbeatInterval, with ±20% jitter, and counts as offline after
// OfflineAfter missed beats.
const (
	HeartbeatInterval = 10 * time.Second
	OfflineAfter      = 3
)

var secretRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// NewCredential returns a fresh responder credential.
func NewCredential() (string, error) { return newSecret(CredentialPrefix) }

func newSecret(prefix string) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// ValidCredential reports whether c has a responder credential's form.
func ValidCredential(c string) bool {
	return len(c) == len(CredentialPrefix)+22 && c[:len(CredentialPrefix)] == CredentialPrefix && secretRE.MatchString(c[len(CredentialPrefix):])
}

// Responder is a registered responder as the server keeps it.
type Responder struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	CredentialHash string    `json:"credential_sha256"` // hex
	LoomToken      string    `json:"loom_token"`
	Control        string    `json:"control"`
	DataIP         string    `json:"data_ip"`
	PortLow        uint32    `json:"data_port_low"`
	PortHigh       uint32    `json:"data_port_high"`
	Version        string    `json:"version"`
	Registered     time.Time `json:"registered"`
	RegisteredFrom string    `json:"registered_from"`
	JoinTokenID    string    `json:"join_token_id"`
}

// tombstone is a removed responder: its credential is told it was removed
// (and stops), rather than that it is unknown (and keeps trying).
type tombstone struct {
	ID             string    `json:"id"`
	CredentialHash string    `json:"credential_sha256"`
	Removed        time.Time `json:"removed"`
}

// registryFile is the registry on disk: version 1.
type registryFile struct {
	Version    int         `json:"version"`
	Responders []Responder `json:"responders"`
	Removed    []tombstone `json:"removed"`
}

// RegistryFileName is the registry's file in the server's state directory.
const RegistryFileName = "responders.json"

// entry is a responder with what the server sees of it while it runs; the
// latter is never persisted.
type entry struct {
	Responder
	lastSeen      time.Time
	lastSource    string
	loomListening bool
}

// Status is a responder as listed.
type Status struct {
	Responder
	State         string // StateOnline, StateOffline or StateNeverSeen
	LastSeen      time.Time
	LastSource    string
	LoomListening bool
}

// Responder states.
const (
	StateOnline    = "online"
	StateOffline   = "offline"
	StateNeverSeen = "never seen"
)

// Registry errors. ErrUnknownToken is the uniform answer to a join token
// that is unknown, expired, revoked, or used by another registration.
var (
	ErrUnknownToken     = errors.New("unknown or expired join token")
	ErrUnknownExisting  = errors.New("the existing credential is not one this server knows; remove /etc/orbit/responder.yaml on the app host to register it anew")
	ErrResponderRemoved = errors.New("responder removed")
	ErrUnknownResponder = errors.New("unknown responder")
	ErrNoResponder      = errors.New("no such registered responder")
)

// NameTakenError refuses a name another responder holds.
type NameTakenError struct {
	Name, From string
	At         time.Time
}

func (e *NameTakenError) Error() string {
	return fmt.Sprintf("responder name %s is taken (registered %s from %s); pass --name, or orbit responder remove %s",
		e.Name, e.At.UTC().Format(time.RFC3339), e.From, e.Name)
}

// OfflineError refuses a run a responder is offline for.
type OfflineError struct {
	Name       string
	LastSeen   time.Time // zero: not since the server started
	LastSource string
	Now        time.Time
}

func (e *OfflineError) Error() string {
	if e.LastSeen.IsZero() {
		return fmt.Sprintf("responder %s is offline (no heartbeat since the server started); check orbit-responder on the app host", e.Name)
	}
	return fmt.Sprintf("responder %s is offline (last heartbeat %s ago from %s); check orbit-responder on the app host",
		e.Name, e.Now.Sub(e.LastSeen).Round(time.Second), e.LastSource)
}

// Registry holds the registered responders: in memory, and in the state
// directory's RegistryFileName when there is one.
type Registry struct {
	path string // "" = memory only
	now  func() time.Time

	mu    sync.Mutex
	byID  map[string]*entry
	tombs map[string]tombstone // by credential hash

	// failSave, when it returns true, fails the next save (tests).
	failSave func() bool
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithRegistryClock replaces the registry's clock (tests).
func WithRegistryClock(now func() time.Time) RegistryOption {
	return func(r *Registry) { r.now = now }
}

// OpenRegistry loads the registry from dir's RegistryFileName ("" = memory
// only). A missing file is an empty registry; a file that cannot be read
// is an error, so a server never silently forgets its responders.
func OpenRegistry(dir string, opts ...RegistryOption) (*Registry, error) {
	r := &Registry{now: time.Now, byID: map[string]*entry{}, tombs: map[string]tombstone{}}
	for _, o := range opts {
		o(r)
	}
	if dir == "" {
		return r, nil
	}
	r.path = filepath.Join(dir, RegistryFileName)
	b, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var f registryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", r.path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: version %d, want 1", r.path, f.Version)
	}
	for _, p := range f.Responders {
		p := p
		r.byID[p.ID] = &entry{Responder: p}
	}
	for _, t := range f.Removed {
		r.tombs[t.CredentialHash] = t
	}
	return r, nil
}

// Persistent reports whether the registry survives a restart.
func (r *Registry) Persistent() bool { return r.path != "" }

// save writes the registry with byID and tombs atomically, 0600; the
// caller holds mu. Every mutation saves its new state first and only then
// makes it the registry's, so memory is never ahead of the disk.
func (r *Registry) save(byID map[string]*entry, tombs map[string]tombstone) error {
	if r.path == "" || r.failSave != nil && r.failSave() {
		if r.path != "" {
			return errors.New("injected save failure")
		}
		return nil
	}
	f := registryFile{Version: 1, Responders: []Responder{}, Removed: []tombstone{}}
	for _, e := range byID {
		f.Responders = append(f.Responders, e.Responder)
	}
	sort.Slice(f.Responders, func(i, j int) bool { return f.Responders[i].ID < f.Responders[j].ID })
	for _, t := range tombs {
		f.Removed = append(f.Removed, t)
	}
	sort.Slice(f.Removed, func(i, j int) bool { return f.Removed[i].ID < f.Removed[j].ID })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".responders-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, r.path)
}

func credHash(c string) [sha256.Size]byte { return sha256.Sum256([]byte(c)) }

func hexHash(h [sha256.Size]byte) string { return hex.EncodeToString(h[:]) }

// byHash is the entry whose credential hashes to h; the caller holds mu.
func (r *Registry) byHash(h [sha256.Size]byte) *entry {
	want := hexHash(h)
	var found *entry
	for _, e := range r.byID {
		if subtle.ConstantTimeCompare([]byte(e.CredentialHash), []byte(want)) == 1 {
			found = e
		}
	}
	return found
}

// RegisterRequest is a registration as the server receives it.
type RegisterRequest struct {
	JoinToken          string
	Credential         string
	ExistingCredential string // "" = a new responder
	Name               string
	Control            string
	DataIP             string
	PortLow, PortHigh  uint32
	Version            string
	Source             string // the peer's address
}

// validate checks a request's shape; its errors name the field.
func (q RegisterRequest) validate() error {
	if !ValidCredential(q.Credential) {
		return errors.New("credential: not a responder credential")
	}
	if q.ExistingCredential != "" && !ValidCredential(q.ExistingCredential) {
		return errors.New("existing_credential: not a responder credential")
	}
	if q.ExistingCredential == q.Credential {
		return errors.New("credential: a re-registration needs a new credential")
	}
	if !target.ValidName(q.Name) {
		return ErrName
	}
	host, port, err := net.SplitHostPort(q.Control)
	if err != nil {
		return fmt.Errorf("control_address: %q is not host:port", q.Control)
	}
	if a, err := netip.ParseAddr(host); err != nil || a.Zone() != "" {
		return fmt.Errorf("control_address: %q is not an IP address", host)
	}
	if p, err := netip.ParseAddrPort("0.0.0.0:" + port); err != nil || p.Port() == 0 {
		return fmt.Errorf("control_address: %q has no valid port", q.Control)
	}
	if a, err := netip.ParseAddr(q.DataIP); err != nil || a.Zone() != "" {
		return fmt.Errorf("data_ip: %q is not an IP address", q.DataIP)
	}
	if q.PortLow == 0 || q.PortHigh > 65535 || q.PortLow > q.PortHigh {
		return fmt.Errorf("data ports: %d-%d is not a port range", q.PortLow, q.PortHigh)
	}
	return nil
}

// Register registers a responder with a join token from store (ADR-0009
// §3). In order, under one lock:
//
//  1. A token already burned by this same credential is a retry whose
//     answer was lost: the same responder comes back, changed in nothing.
//  2. The token must be pending, else ErrUnknownToken: unknown, expired,
//     revoked and burned by another credential alike.
//  3. A re-registration names the responder by its existing credential;
//     one this server does not hold is ErrUnknownExisting.
//  4. The name must be free, or this responder's own: a NameTakenError
//     otherwise. All of these refuse before the token is burned.
//  5. The token is burned, and the responder stored with a fresh loom
//     token (a re-registration rotates both secrets and keeps its ID).
func (r *Registry) Register(store *Store, q RegisterRequest) (Responder, error) {
	if err := q.validate(); err != nil {
		return Responder{}, err
	}
	h := credHash(q.Credential)
	r.mu.Lock()
	defer r.mu.Unlock()
	tok, ok := store.Lookup(q.JoinToken)
	if !ok {
		return Responder{}, ErrUnknownToken
	}
	if burned, by, id := tok.Burned(); burned {
		if subtle.ConstantTimeCompare(by[:], h[:]) == 1 {
			if e := r.byID[id]; e != nil && e.CredentialHash == hexHash(h) {
				return e.Responder, nil
			}
		}
		return Responder{}, ErrUnknownToken
	}
	var cur *entry
	if q.ExistingCredential != "" {
		if cur = r.byHash(credHash(q.ExistingCredential)); cur == nil {
			return Responder{}, ErrUnknownExisting
		}
	}
	for _, e := range r.byID {
		if e.Name == q.Name && e != cur {
			return Responder{}, &NameTakenError{Name: q.Name, From: e.RegisteredFrom, At: e.Registered}
		}
	}
	if r.byHash(h) != nil {
		// A fresh credential that is already some responder's: never.
		return Responder{}, errors.New("credential: already in use; generate a new one")
	}
	loom, err := newSecret(LoomTokenPrefix)
	if err != nil {
		return Responder{}, err
	}
	id := ""
	if cur != nil {
		id = cur.ID
	} else {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err != nil {
			return Responder{}, err
		}
		id = "rs-" + hex.EncodeToString(raw)
	}
	now := r.now()
	e := &entry{Responder: Responder{ID: id, Name: q.Name, CredentialHash: hexHash(h), LoomToken: loom,
		Control: q.Control, DataIP: q.DataIP, PortLow: q.PortLow, PortHigh: q.PortHigh, Version: q.Version,
		Registered: now, RegisteredFrom: q.Source, JoinTokenID: tok.ID}}
	if cur != nil {
		e.lastSeen, e.lastSource, e.loomListening = cur.lastSeen, cur.lastSource, cur.loomListening
	}
	// Persist, then publish: the new state on disk first; the token burned
	// and the entry in memory only once it is there. A save that fails
	// changes nothing, so a retry starts clean.
	next := make(map[string]*entry, len(r.byID)+1)
	for k, v := range r.byID {
		next[k] = v
	}
	next[id] = e
	if err := r.save(next, r.tombs); err != nil {
		return Responder{}, fmt.Errorf("the responder registry did not persist: %w", err)
	}
	if err := store.Burn(q.JoinToken, h, id); err != nil {
		// Revoked or expired since the lookup: put the disk back as it was.
		if err := r.save(r.byID, r.tombs); err != nil {
			return Responder{}, fmt.Errorf("the responder registry did not persist: %w", err)
		}
		return Responder{}, ErrUnknownToken
	}
	r.byID = next
	return e.Responder, nil
}

// Heartbeat records a heartbeat from the responder holding credential, from
// source; a probe records nothing. It returns the responder's ID and the
// heartbeat before this one (zero: none since the server started). A
// removed responder's credential is ErrResponderRemoved; any other unknown
// one ErrUnknownResponder.
func (r *Registry) Heartbeat(credential, source, version string, loomListening, probe bool) (string, time.Time, error) {
	if !ValidCredential(credential) {
		return "", time.Time{}, ErrUnknownResponder
	}
	h := credHash(credential)
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.byHash(h)
	if e == nil {
		if _, ok := r.tombs[hexHash(h)]; ok {
			return "", time.Time{}, ErrResponderRemoved
		}
		return "", time.Time{}, ErrUnknownResponder
	}
	prev := e.lastSeen
	if !probe {
		e.lastSeen, e.lastSource, e.loomListening = r.now(), source, loomListening
		if version != "" {
			e.Version = version
		}
	}
	return e.ID, prev, nil
}

// state is e's state at now; the caller holds mu.
func state(e *entry, now time.Time) string {
	switch {
	case e.lastSeen.IsZero():
		return StateNeverSeen
	case now.Sub(e.lastSeen) <= OfflineAfter*HeartbeatInterval:
		return StateOnline
	default:
		return StateOffline
	}
}

// List is every registered responder, by name.
func (r *Registry) List() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]Status, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, Status{Responder: e.Responder, State: state(e, now), LastSeen: e.lastSeen,
			LastSource: e.lastSource, LoomListening: e.loomListening})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove revokes the responder with this name or ID, leaving a tombstone
// for its credential.
func (r *Registry) Remove(nameOrID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.byID {
		if e.Name == nameOrID || id == nameOrID {
			// Persist, then publish: a save that fails leaves the responder
			// registered and its credential working.
			byID := make(map[string]*entry, len(r.byID))
			for k, v := range r.byID {
				if k != id {
					byID[k] = v
				}
			}
			tombs := make(map[string]tombstone, len(r.tombs)+1)
			for k, v := range r.tombs {
				tombs[k] = v
			}
			tombs[e.CredentialHash] = tombstone{ID: id, CredentialHash: e.CredentialHash, Removed: r.now()}
			if err := r.save(byID, tombs); err != nil {
				return "", fmt.Errorf("the responder registry did not persist: %w", err)
			}
			r.byID, r.tombs = byID, tombs
			return id, nil
		}
	}
	return "", ErrNoResponder
}

// Resolve is the online responder named name, for a run: ErrNoResponder
// when none is registered by that name, an *OfflineError when it is not
// online.
func (r *Registry) Resolve(name string) (Responder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, e := range r.byID {
		if e.Name != name {
			continue
		}
		if state(e, now) != StateOnline {
			return Responder{}, &OfflineError{Name: name, LastSeen: e.lastSeen, LastSource: e.lastSource, Now: now}
		}
		return e.Responder, nil
	}
	return Responder{}, ErrNoResponder
}
