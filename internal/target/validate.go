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
// the server's metric (attach.success_ratio, registration.p99_ms) and the
// workload it scopes to ("" for the whole run), so a gate set in a run file
// and a --slo-* or --gate flag on the same metric are one gate and merge
// (the flag winning) instead of being two. A path selects a list element
// by name between dots, so the metric's dots are underscores in the name:
// attach_success_ratio, pop/registration_p99_ms.
func GateName(workload, metric string) string {
	name := strings.ReplaceAll(metric, ".", "_")
	if workload == "" {
		return name
	}
	return workload + "/" + name
}

// The metrics the --slo-* flags gate on, as the server names them.
const (
	MetricSuccessRatio = "attach.success_ratio"
	MetricRegP99       = "registration.p99_ms"
	MetricAttachP99    = "attach.p99_ms"
)

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
	"core.slices[].sst": func(v any) string {
		if n := v.(uint64); n > 255 {
			return fmt.Sprintf("SST %d is more than 255", n)
		}
		return ""
	},
	"core.slices[].sd":         func(v any) string { return match(v, hex6, "6 hex digits") },
	"core.amf":                 hostPort,
	"core.upf_n3":              hostPort,
	"app.responders[].control": hostPort,
	"app.responders[].data_ip": func(v any) string { return ipOrHost(v.(string)) },
	"subscribers[].first_supi": supi,
	"subscribers[].ki":         func(v any) string { return match(v, hex32, "32 hex digits") },
	"subscribers[].opc":        func(v any) string { return match(v, hex32, "32 hex digits") },
	"ran.n2_source_ips":        func(v any) string { return eachAddr(v) },
	"ran.n3_ips":               func(v any) string { return eachAddr(v) },
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

func supi(v any) string {
	s := strings.TrimPrefix(v.(string), "imsi-")
	if !imsiDigits.MatchString(s) {
		return "is not an IMSI (6 to 15 digits, optionally imsi-)"
	}
	return ""
}

// eachAddr checks an address pool (ran.n2_source_ips, ran.n3_ips): IPs and
// host names, either with an optional :port (port 0 is ephemeral), and
// ranges (a-b) and CIDRs, which are IP-only and take no port. Host names
// pass unresolved, as the commands pass them (the server resolves them at
// the dial; S4 when it leases): the resolver never refuses a value a
// command accepts.
func eachAddr(v any) string {
	for _, a := range v.([]string) {
		if !addrEntry(a, true) {
			return fmt.Sprintf("entry %q is not an address", a)
		}
	}
	return ""
}

func addrEntry(a string, pools bool) bool {
	if _, _, isRange := splitRange(a); isRange {
		_, _, err := poolBounds(a)
		return pools && err == nil
	}
	if _, err := parseIP(a); err == nil {
		return true
	}
	if lo, _, found := strings.Cut(a, "-"); found {
		if _, err := netip.ParseAddr(lo); err == nil {
			return false // meant as a range (10.0.0.1-10.0.0.x), and not a valid one
		}
	}
	if _, err := netip.ParseAddrPort(a); err == nil {
		return true
	}
	if pools {
		if _, err := netip.ParsePrefix(a); err == nil {
			return true
		}
	}
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		host, port = a, ""
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return false
		}
	}
	return isHostName(host)
}

// hostLabel is one DNS label (RFC 1123: letters, digits, hyphens inside).
var hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// isHostName reports whether s is a host name: dot-separated labels whose
// last is not all digits, so 192.0.2.300 is a bad IP, not a name.
func isHostName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(s, "."), ".")
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return false
		}
	}
	last := labels[len(labels)-1]
	return strings.Trim(last, "0123456789") != ""
}

// ipOrHost checks an IP or a host name with no port.
func ipOrHost(s string) string {
	if _, err := parseIP(s); err == nil || isHostName(s) {
		return ""
	}
	return "is not an IP address or host name"
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
