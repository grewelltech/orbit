package gnb

import (
	"errors"
	"fmt"
	"net"

	"github.com/free5gc/ngap/ngapConvert"
	"github.com/free5gc/ngap/ngapType"
)

// ErrIPv6N3 refuses an IPv6 gNB N3 address: the gNB encodes its N3 tunnel
// address as IPv4 only (#156).
var ErrIPv6N3 = errors.New("gNB N3 address must be IPv4 (IPv4-mapped accepted); IPv6 N3 is not supported yet (#156)")

// CanonicalN3 validates a gNB N3 address and returns it in canonical IPv4
// form ("::ffff:10.0.0.5" becomes "10.0.0.5"). It is the one validator every
// input path uses (CLI, API, scenarios, the conformance harness), because an
// address the NGAP encoder cannot represent would otherwise fail deep in PDU
// session setup. "" is returned unchanged: whether an N3 address is required
// is the caller's rule.
func CanonicalN3(addr string) (string, error) {
	if addr == "" {
		return "", nil
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("gNB N3 address %q is not an IP address", addr)
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("%w: got %q", ErrIPv6N3, addr)
	}
	return v4.String(), nil
}

// tunnelAddress encodes a gNB N3 tunnel address for NGAP. Only IPv4 (or
// IPv4-mapped) is encodable: an IPv6 address is ErrIPv6N3, where free5gc's
// converter would panic on it.
func tunnelAddress(addr string) (ngapType.TransportLayerAddress, error) {
	v4, err := CanonicalN3(addr)
	if err != nil {
		return ngapType.TransportLayerAddress{}, err
	}
	if v4 == "" {
		return ngapType.TransportLayerAddress{}, errors.New("gNB N3 tunnel address is empty")
	}
	return ngapConvert.IPAddressToNgap(v4, ""), nil
}
