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

// leases is what one run holds, canonicalised.
type leases struct {
	supis []SUPIBlock
	gnbs  map[GNBKey]bool
	n2    map[string]bool
}

// leaseTable holds every active run's leases. It is guarded by the
// registry's lock: a run's leases are taken in the same critical section
// that registers it, so two StartRuns cannot both win a resource. Needs are
// canonicalised (canonical, which may resolve names) BEFORE that section,
// so a slow resolver never holds the lock.
type leaseTable struct {
	// resolveAMF canonicalises an AMF address to IP:port, so a name and
	// its address are one key. Tests replace it.
	resolveAMF func(ctx context.Context, amf string) string
	held       map[string]*leases // run id → its leases
}

func newLeaseTable() *leaseTable {
	return &leaseTable{resolveAMF: resolveAMF, held: map[string]*leases{}}
}

// amfResolveTimeout bounds resolving one AMF name at StartRun.
const amfResolveTimeout = 2 * time.Second

// resolveAMF is an AMF's IP:port, or the address as written (lower-cased)
// when it does not resolve within ctx.
func resolveAMF(ctx context.Context, amf string) string {
	host, port, err := net.SplitHostPort(amf)
	if err != nil {
		return strings.ToLower(amf)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return strings.ToLower(amf)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(a.Unmap().WithZone(""), uint16(p)).String()
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return strings.ToLower(amf)
	}
	// The first address is the one a dial would try first.
	return netip.AddrPortFrom(addrs[0].Unmap(), uint16(p)).String()
}

// n2Key is an N2 bind address's IP, canonicalised, or "" for none: an empty
// or unspecified bind lets the kernel choose, which leases nothing.
func n2Key(bind string) string {
	host := bind
	if h, _, err := net.SplitHostPort(bind); err == nil {
		host = h
	}
	if host == "" {
		return ""
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return strings.ToLower(host)
	}
	if a.IsUnspecified() {
		return ""
	}
	return a.Unmap().WithZone("").String()
}

// canonical turns a run's needs into its leases, deduplicated (a load run
// may share one N2 address across its gNBs), resolving each distinct AMF
// once within amfResolveTimeout. It takes no lock: call it before the
// registry's critical section.
func (t *leaseTable) canonical(n Needs) *leases {
	l := &leases{supis: n.SUPIs, gnbs: map[GNBKey]bool{}, n2: map[string]bool{}}
	resolved := map[string]string{}
	for _, g := range n.GNBs {
		amf, ok := resolved[g.AMF]
		if !ok {
			ctx, cancel := context.WithTimeout(context.Background(), amfResolveTimeout)
			amf = t.resolveAMF(ctx, g.AMF)
			cancel()
			resolved[g.AMF] = amf
		}
		g.AMF = amf
		l.gnbs[g] = true
	}
	for _, b := range n.N2IPs {
		if k := n2Key(b); k != "" {
			l.n2[k] = true
		}
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

// conflict reports the first resource want shares with have (held by h).
func conflict(want, have *leases, h string) error {
	for _, a := range want.supis {
		for _, b := range have.supis {
			if a.PLMN == b.PLMN && a.First <= b.Last && b.First <= a.Last {
				lo, hi := max(a.First, b.First), min(a.Last, b.Last)
				return &ErrLeaseConflict{Resource: "SUPIs", Value: supiRange(lo, hi) + " (PLMN " + a.PLMN + ")", Holder: h}
			}
		}
	}
	keys := make([]GNBKey, 0, len(want.gnbs))
	for k := range want.gnbs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	for _, k := range keys {
		if have.gnbs[k] {
			return &ErrLeaseConflict{Resource: "gNB ID", Value: fmt.Sprintf("%d on AMF %s (PLMN %s)", k.ID, k.AMF, k.PLMN), Holder: h}
		}
	}
	ips := make([]string, 0, len(want.n2))
	for ip := range want.n2 {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	for _, ip := range ips {
		if have.n2[ip] {
			return &ErrLeaseConflict{Resource: "N2 source IP", Value: ip, Holder: h}
		}
	}
	return nil
}

func supiRange(lo, hi uint64) string {
	if lo == hi {
		return fmt.Sprintf("%015d", lo)
	}
	return fmt.Sprintf("%015d-%015d", lo, hi)
}
