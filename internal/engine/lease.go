package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/ue"
)

// Needs are the resources a run leases while it is active (ADR-0008 §5).
// Two runs conflict only when their needs overlap; a run whose needs are
// empty conflicts with nothing.
type Needs struct {
	// SUPIs are blocks of subscribers, unique per PLMN.
	SUPIs []SUPIBlock
	// GNBs are gNB IDs, unique per (AMF, PLMN).
	GNBs []GNBKey
	// N2IPs are N2 source addresses, held exclusively. N3 addresses are not
	// leased: TEIDs keep sessions apart.
	N2IPs []string
}

// SUPIBlock is the SUPIs First..Last (inclusive, as 15-digit numbers) in a
// PLMN ("mcc/mnc").
type SUPIBlock struct {
	PLMN        string
	First, Last uint64
}

// GNBKey is one gNB ID at an AMF (host:port, as written; the lease table
// resolves it) in a PLMN ("mcc/mnc").
type GNBKey struct {
	AMF, PLMN string
	ID        uint32
}

// maxSUPI is the largest 15-digit SUPI, where an unbounded soak's block ends.
const maxSUPI uint64 = 999_999_999_999_999

func plmnKey(mcc, mnc string) string { return mcc + "/" + mnc }

// SUPISpan is the block of SUPIs the load run generates: Count SUPIs from
// BaseIMSI, which a soak with a Count cycles through; a soak without one
// keeps climbing, so its block runs to the end of the 15-digit space. ok is
// false when the run generates none.
func (s LoadSpec) SUPISpan() (first, last uint64, ok bool, err error) {
	base, err := ue.NormalizeSUPI(s.BaseIMSI)
	if err != nil {
		return 0, 0, false, fmt.Errorf("base IMSI: %w", err)
	}
	first, err = strconv.ParseUint(base, 10, 64)
	if err != nil {
		return 0, 0, false, fmt.Errorf("base IMSI %q: %w", base, err)
	}
	switch {
	case s.Count > 0:
		return first, first + uint64(s.Count) - 1, true, nil
	case s.Duration > 0:
		return first, maxSUPI, true, nil
	}
	return 0, 0, false, nil
}

// LoadNeeds are the leases a load run takes: its SUPI span, each gNB's ID,
// and each distinct N2 source address.
func LoadNeeds(s LoadSpec) (Needs, error) {
	var n Needs
	first, last, ok, err := s.SUPISpan()
	if err != nil {
		return Needs{}, err
	}
	if ok {
		n.SUPIs = []SUPIBlock{{PLMN: plmnKey(s.MCC, s.MNC), First: first, Last: last}}
	}
	for _, g := range s.GNBs {
		n.GNBs = append(n.GNBs, GNBKey{AMF: g.AMFAddr, PLMN: plmnKey(g.Config.MCC, g.Config.MNC), ID: g.Config.ID})
		n.N2IPs = append(n.N2IPs, g.BindAddr)
	}
	return n, nil
}

// FleetNeeds are the leases a fleet run takes: Count SUPIs from BaseIMSI,
// each gNB's ID, and each distinct N2 source address.
func FleetNeeds(s FleetRunSpec) (Needs, error) {
	var n Needs
	if s.Count > 0 {
		base, err := ue.NormalizeSUPI(s.BaseIMSI)
		if err != nil {
			return Needs{}, fmt.Errorf("base IMSI: %w", err)
		}
		first, err := strconv.ParseUint(base, 10, 64)
		if err != nil {
			return Needs{}, fmt.Errorf("base IMSI %q: %w", base, err)
		}
		n.SUPIs = []SUPIBlock{{PLMN: plmnKey(s.MCC, s.MNC), First: first, Last: first + uint64(s.Count) - 1}}
	}
	for _, g := range s.GNBs {
		n.GNBs = append(n.GNBs, GNBKey{AMF: s.AMFAddr, PLMN: plmnKey(g.Config.MCC, g.Config.MNC), ID: g.Config.ID})
		n.N2IPs = append(n.N2IPs, g.BindAddr)
	}
	return n, nil
}

// ErrLeaseConflict is a run refused because another active run holds one
// of the resources it needs.
type ErrLeaseConflict struct {
	Resource string // "SUPIs", "gNB ID", "N2 source IP"
	Value    string // what is held, e.g. "7 on AMF 10.0.0.1:38412 (PLMN 001/01)"
	Holder   string // the run holding it
}

func (e *ErrLeaseConflict) Error() string {
	return fmt.Sprintf("%s %s is held by %s", e.Resource, e.Value, e.Holder)
}

// leases is what one run holds. A gNB ID's AMF and an N2 source are kept as
// identity sets (the name as written plus every address it resolved to), so
// a name and its address are one resource however the resolver answers.
type leases struct {
	supis []SUPIBlock
	gnbs  []gnbLease
	n2    []identity
}

// gnbLease is one gNB ID at an AMF, the AMF by its identity set.
type gnbLease struct {
	plmn    string
	id      uint32
	amf     identity
	written string // the AMF as the run gave it, for messages
}

// identity is the set of keys one resource is known by.
type identity map[string]bool

func (a identity) intersects(b identity) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// key is a stable string for the set, to deduplicate within a run.
func (a identity) key() string {
	ks := make([]string, 0, len(a))
	for k := range a {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// leaseTable holds every active run's leases. It is guarded by the
// registry's lock: a run's leases are taken in the same critical section
// that registers it, so two StartRuns cannot both win a resource. Needs are
// canonicalised (canonical, which may resolve names) BEFORE that section,
// so a slow resolver never holds the lock.
type leaseTable struct {
	// resolveHost returns the addresses a host name resolves to (none on
	// failure or timeout). Tests replace it.
	resolveHost func(ctx context.Context, host string) []netip.Addr
	held        map[string]*leases // run id → its leases
}

func newLeaseTable() *leaseTable {
	return &leaseTable{resolveHost: resolveHost, held: map[string]*leases{}}
}

// resolveTimeout bounds resolving one name at StartRun.
const resolveTimeout = 2 * time.Second

// resolveHost resolves a name through the system resolver.
func resolveHost(ctx context.Context, host string) []netip.Addr {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	return addrs
}

// resolver resolves each distinct name once per run, within resolveTimeout.
type resolver struct {
	t    *leaseTable
	seen map[string][]netip.Addr
}

func (r *resolver) lookup(host string) []netip.Addr {
	host = strings.ToLower(host) // names are case-insensitive
	if a, ok := r.seen[host]; ok {
		return a
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	a := r.t.resolveHost(ctx, host)
	r.seen[host] = a
	return a
}

// amfIdentity is an AMF's identity: the address as written (lower-cased)
// and, for a name, every IP:port it resolves to. A literal IP is just its
// canonical IP:port.
func (r *resolver) amfIdentity(amf string) identity {
	host, port, err := net.SplitHostPort(amf)
	p, perr := strconv.ParseUint(port, 10, 16)
	if err != nil || perr != nil {
		return identity{strings.ToLower(amf): true}
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return identity{netip.AddrPortFrom(a.Unmap().WithZone(""), uint16(p)).String(): true}
	}
	id := identity{strings.ToLower(amf): true}
	for _, a := range r.lookup(host) {
		id[netip.AddrPortFrom(a.Unmap().WithZone(""), uint16(p)).String()] = true
	}
	return id
}

// n2Identity is an N2 bind's identity, or nil for none: an empty or
// unspecified bind lets the kernel choose, which leases nothing. A literal
// IP is its canonical form; a name is itself (lower-cased) and every IP it
// resolves to.
func (r *resolver) n2Identity(bind string) identity {
	host := bind
	if h, _, err := net.SplitHostPort(bind); err == nil {
		host = h
	}
	if host == "" {
		return nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.IsUnspecified() {
			return nil
		}
		return identity{a.Unmap().WithZone("").String(): true}
	}
	id := identity{strings.ToLower(host): true}
	for _, a := range r.lookup(host) {
		id[a.Unmap().WithZone("").String()] = true
	}
	return id
}

// canonical turns a run's needs into its leases, deduplicated (a load run
// may share one N2 address across its gNBs), resolving each distinct name
// once within resolveTimeout. It takes no lock: call it before the
// registry's critical section.
func (t *leaseTable) canonical(n Needs) *leases {
	r := &resolver{t: t, seen: map[string][]netip.Addr{}}
	l := &leases{supis: n.SUPIs}
	amfs := map[string]identity{}
	seenGNB := map[string]bool{}
	for _, g := range n.GNBs {
		id, ok := amfs[g.AMF]
		if !ok {
			id = r.amfIdentity(g.AMF)
			amfs[g.AMF] = id
		}
		k := fmt.Sprintf("%s|%d|%s", g.PLMN, g.ID, id.key())
		if seenGNB[k] {
			continue
		}
		seenGNB[k] = true
		l.gnbs = append(l.gnbs, gnbLease{plmn: g.PLMN, id: g.ID, amf: id, written: g.AMF})
	}
	seenN2 := map[string]bool{}
	for _, b := range n.N2IPs {
		id := r.n2Identity(b)
		if id == nil || seenN2[id.key()] {
			continue
		}
		seenN2[id.key()] = true
		l.n2 = append(l.n2, id)
	}
	return l
}

// acquire takes run id's leases (from canonical), or refuses with the
// first conflict, in a stable order (holders by id). Callers hold the
// registry's lock.
func (t *leaseTable) acquire(id string, want *leases) error {
	holders := make([]string, 0, len(t.held))
	for h := range t.held {
		holders = append(holders, h)
	}
	sort.Strings(holders)
	for _, h := range holders {
		if err := conflict(want, t.held[h], h); err != nil {
			return err
		}
	}
	t.held[id] = want
	return nil
}

// release drops run id's leases. It is idempotent.
func (t *leaseTable) release(id string) { delete(t.held, id) }

// conflict reports the first resource want shares with have (held by h):
// overlapping SUPI blocks in a PLMN, a gNB ID in a PLMN at an AMF whose
// identities intersect, or N2 sources whose identities intersect.
func conflict(want, have *leases, h string) error {
	for _, a := range want.supis {
		for _, b := range have.supis {
			if a.PLMN == b.PLMN && a.First <= b.Last && b.First <= a.Last {
				lo, hi := max(a.First, b.First), min(a.Last, b.Last)
				return &ErrLeaseConflict{Resource: "SUPIs", Value: supiRange(lo, hi) + " (PLMN " + a.PLMN + ")", Holder: h}
			}
		}
	}
	for _, a := range want.gnbs {
		for _, b := range have.gnbs {
			if a.plmn == b.plmn && a.id == b.id && a.amf.intersects(b.amf) {
				return &ErrLeaseConflict{Resource: "gNB ID", Value: fmt.Sprintf("%d on AMF %s (PLMN %s)", b.id, b.written, b.plmn), Holder: h}
			}
		}
	}
	for _, a := range want.n2 {
		for _, b := range have.n2 {
			if a.intersects(b) {
				return &ErrLeaseConflict{Resource: "N2 source IP", Value: firstKey(b), Holder: h}
			}
		}
	}
	return nil
}

// firstKey names an identity in a message: its address if it has one.
func firstKey(id identity) string {
	ks := make([]string, 0, len(id))
	for k := range id {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		_, ei := netip.ParseAddr(ks[i])
		_, ej := netip.ParseAddr(ks[j])
		if (ei == nil) != (ej == nil) {
			return ei == nil
		}
		return ks[i] < ks[j]
	})
	return ks[0]
}

func supiRange(lo, hi uint64) string {
	if lo == hi {
		return fmt.Sprintf("%015d", lo)
	}
	return fmt.Sprintf("%015d-%015d", lo, hi)
}

// LeaseInfo is one resource a run leases, for display and archiving.
type LeaseInfo struct {
	Resource  string // "supi_block", "gnb_id", "n2_ip"
	PLMN      string // SUPI block, gNB ID
	FirstSUPI string // SUPI block, 15 digits
	LastSUPI  string
	AMF       string // gNB ID: the AMF as the run gave it
	GNBID     uint32
	N2IP      string
}

// describe lists a run's leases, in a stable order.
func (l *leases) describe() []LeaseInfo {
	if l == nil {
		return nil
	}
	var out []LeaseInfo
	for _, b := range l.supis {
		out = append(out, LeaseInfo{Resource: "supi_block", PLMN: b.PLMN, FirstSUPI: fmt.Sprintf("%015d", b.First), LastSUPI: fmt.Sprintf("%015d", b.Last)})
	}
	for _, g := range l.gnbs {
		out = append(out, LeaseInfo{Resource: "gnb_id", PLMN: g.plmn, AMF: g.written, GNBID: g.id})
	}
	for _, n := range l.n2 {
		out = append(out, LeaseInfo{Resource: "n2_ip", N2IP: firstKey(n)})
	}
	return out
}
