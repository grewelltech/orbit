package target

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// GateName is a gate's name in the gates list: derived from what it checks,
// the metric and the workload it scopes to ("" for the whole run), so a gate
// set in a run file and a --slo-* flag on the same metric are one gate and
// merge (the flag winning) instead of being two.
func GateName(workload, metric string) string {
	if workload == "" {
		return metric
	}
	return workload + "/" + metric
}

var (
	digits3    = regexp.MustCompile(`^[0-9]{3}$`)
	digits2or3 = regexp.MustCompile(`^[0-9]{2,3}$`)
	imsiDigits = regexp.MustCompile(`^[0-9]{6,15}$`)
	hex32      = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	hex6       = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)
)

// checks validates a resolved value's format by field pattern. Each
// returns the reason a value is bad, "" when it is fine. Which fields are
// required is the command's business, not the resolver's.
var checks = map[string]func(any) string{
	"core.plmn.mcc": func(v any) string { return match(v, digits3, "3 digits") },
	"core.plmn.mnc": func(v any) string { return match(v, digits2or3, "2 or 3 digits") },
	"core.tac": func(v any) string {
		if v.(uint64) > 0xFFFFFF {
			return "more than 24 bits"
		}
		return ""
	},
	"core.slices": func(v any) string {
		for _, s := range v.([]Slice) {
			if s.SST > 255 {
				return fmt.Sprintf("SST %d is more than 255", s.SST)
			}
			if s.SD != "" && !hex6.MatchString(s.SD) {
				return fmt.Sprintf("SD %q is not 6 hex digits", s.SD)
			}
		}
		return ""
	},
	"core.amf":                 hostPort,
	"core.upf_n3":              hostPort,
	"app.responders[].control": hostPort,
	"app.responders[].data_ip": func(v any) string { return ip(v.(string)) },
	"subscribers[].first_supi": supi,
	"subscribers[].ki":         func(v any) string { return match(v, hex32, "32 hex digits") },
	"subscribers[].opc":        func(v any) string { return match(v, hex32, "32 hex digits") },
	"ran.n2_source_ips":        func(v any) string { return eachAddr(v, true) },
	"ran.n3_ips":               func(v any) string { return eachAddr(v, false) },
	"ran.gnb_ids.bits": func(v any) string {
		if b := v.(uint64); b != 0 && (b < 22 || b > 32) {
			return "not between 22 and 32"
		}
		return ""
	},
}

func match(v any, re *regexp.Regexp, want string) string {
	if !re.MatchString(v.(string)) {
		return "is not " + want
	}
	return ""
}

func hostPort(v any) string {
	host, port, err := net.SplitHostPort(v.(string))
	if err != nil || host == "" {
		return "is not host:port"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "has no valid port"
	}
	return ""
}

func ip(s string) string {
	if _, err := netip.ParseAddr(s); err != nil {
		return "is not an IP address"
	}
	return ""
}

func supi(v any) string {
	s := strings.TrimPrefix(v.(string), "imsi-")
	if !imsiDigits.MatchString(s) {
		return "is not an IMSI (6 to 15 digits, optionally imsi-)"
	}
	return ""
}

// eachAddr checks an address list: IPs and ip:port (port 0 is ephemeral)
// and, for source pools, ranges (a-b) and CIDRs, which take no port.
func eachAddr(v any, pools bool) string {
	for _, a := range v.([]string) {
		ok := false
		if _, err := netip.ParseAddr(a); err == nil {
			ok = true
		} else if _, err := netip.ParseAddrPort(a); err == nil {
			ok = true
		} else if pools {
			if _, err := netip.ParsePrefix(a); err == nil {
				ok = true
			} else if lo, hi, found := strings.Cut(a, "-"); found {
				l, e1 := netip.ParseAddr(lo)
				h, e2 := netip.ParseAddr(hi)
				ok = e1 == nil && e2 == nil && l.Is4() == h.Is4() && !h.Less(l)
			}
		}
		if !ok {
			return fmt.Sprintf("entry %q is not an address", a)
		}
	}
	return ""
}

// validateValue checks one resolved field; the error names the path, the
// value (masked when secret) and the layer and source that set it.
func validateValue(path string, f field, v any, src Source) error {
	segs, _ := parsePath(path)
	check, ok := checks[pattern(segs)]
	if !ok {
		return nil
	}
	// An empty value is a layer clearing the field (--bind "" is the
	// route-selected source; --supi "" turns a target's UE off), not a
	// malformed one.
	if s, isStr := v.(string); isStr && s == "" {
		return nil
	}
	if l, isList := v.([]string); isList && len(l) == 0 {
		return nil
	}
	reason := check(v)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("%s: %s %s (%s)", path, errValue(f, v), reason, src)
}
