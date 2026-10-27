package target

import (
	"fmt"
	"net/netip"
	"strings"
)

// MaxPoolExpand bounds how many addresses ExpandPool hands out, so a pool
// such as 10.0.0.0/8 cannot exhaust memory.
const MaxPoolExpand = 65536

// ExpandPool expands an address pool (ran.n2_source_ips, ran.n3_ips) in
// order into individual addresses and returns the first n, or every
// address when the pool has fewer. An IP or ip:port entry is one address
// and keeps its port; a range (a-b) gives every address from a to b; an
// IPv4 CIDR gives its host addresses (no network or broadcast address for
// /30 and wider, both addresses for /31 and /32); an IPv6 CIDR gives every
// address. n above MaxPoolExpand is an error. Until leasing allocates from
// the pool, a caller that needs one address per gNB takes the first n.
func ExpandPool(pool []string, n int) ([]string, error) {
	if n > MaxPoolExpand {
		return nil, fmt.Errorf("%d addresses is more than a pool expands to (%d)", n, MaxPoolExpand)
	}
	var out []string
	for _, entry := range pool {
		if len(out) >= n {
			break
		}
		if _, err := netip.ParseAddr(entry); err == nil {
			out = append(out, entry)
			continue
		}
		if _, err := netip.ParseAddrPort(entry); err == nil {
			out = append(out, entry)
			continue
		}
		lo, hi, err := poolBounds(entry)
		if err != nil {
			return nil, err
		}
		for a := lo; len(out) < n; a = a.Next() {
			out = append(out, a.String())
			if a == hi {
				break
			}
		}
	}
	return out, nil
}

// PoolFirst is the first address of a pool entry: the entry itself for an
// IP or ip:port, a range's low end, a CIDR's first host address. narrowed
// reports whether the entry was a range or CIDR.
func PoolFirst(entry string) (addr netip.Addr, port uint16, narrowed bool, err error) {
	if a, err := netip.ParseAddr(entry); err == nil {
		return a, 0, false, nil
	}
	if ap, err := netip.ParseAddrPort(entry); err == nil {
		return ap.Addr(), ap.Port(), false, nil
	}
	lo, _, err := poolBounds(entry)
	if err != nil {
		return netip.Addr{}, 0, false, err
	}
	return lo, 0, true, nil
}

// poolBounds is the first and last usable address of a range or CIDR.
func poolBounds(entry string) (netip.Addr, netip.Addr, error) {
	if lo, hi, found := strings.Cut(entry, "-"); found {
		l, e1 := netip.ParseAddr(lo)
		h, e2 := netip.ParseAddr(hi)
		if e1 != nil || e2 != nil || l.Is4() != h.Is4() || h.Less(l) {
			return netip.Addr{}, netip.Addr{}, fmt.Errorf("entry %q is not an address range", entry)
		}
		return l, h, nil
	}
	p, err := netip.ParsePrefix(entry)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("entry %q is not an address", entry)
	}
	p = p.Masked()
	first, last := p.Addr(), lastAddr(p)
	if first.Is4() && p.Bits() <= 30 {
		first, last = first.Next(), last.Prev()
	}
	return first, last, nil
}

// lastAddr is the last address in p.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := range b {
		bit := i * 8
		switch {
		case bit >= p.Bits():
			b[i] = 0xff
		case bit+8 > p.Bits():
			b[i] |= 0xff >> uint(p.Bits()-bit)
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}
