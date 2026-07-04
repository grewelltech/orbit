// Package coreprofile carries per-core compatibility quirks.
//
// ORBIT's codecs are strict and 3GPP/X.691-conformant by default. Real
// deployed cores are not always conformant, so a *named, opt-in* profile can
// enable targeted workarounds for a specific core's known non-conformance.
// Each quirk is documented (the defect, the core, the version, the upstream
// report), defaults off, and is reported when active — the set of quirks a
// core needs is a conformance scorecard, not hidden tuning. The codecs
// themselves are never bent to one core; quirks live only at the
// message-build boundary.
//
// See docs/interop/sdcore.md and docs/DESIGN.md §5(i).
package coreprofile

import "sort"

// Quirks is the set of opt-in compatibility workarounds. The zero value is
// strict 3GPP — no workarounds.
type Quirks struct {
	// HandoverAckForwardingMandatory works around SD-Core / omec-project
	// ngap v2.x generating HandoverRequestAcknowledgeTransfer with
	// dLForwardingUP-TNLInformation as a MANDATORY field — 3GPP TS 38.413
	// defines it OPTIONAL (omec dropped the `optional` tag). Its decoder
	// therefore rejects conformant encodings that omit the field with
	// "align Bit is not zero", so the SMF never learns the target N3 tunnel
	// and the downlink path never switches after an N2 handover.
	//
	// When set, ORBIT emits the transfer with dLForwardingUP-TNLInformation
	// present so omec's decoder accepts it. The bytes are non-conformant and
	// a strict core would reject them — hence opt-in. Verified byte-identical
	// to omec's own v2.1.0 encoder. Upstream: report to omec-project/ngap.
	HandoverAckForwardingMandatory bool
}

// Profile is a named set of quirks for a particular core.
type Profile struct {
	Name   string
	Quirks Quirks
}

// registry maps profile name to its quirks. strict-3gpp is the default and
// carries no quirks; a conformant core needs no profile.
var registry = map[string]Profile{
	"strict-3gpp": {Name: "strict-3gpp"},
	"sdcore":      {Name: "sdcore", Quirks: Quirks{HandoverAckForwardingMandatory: true}},
}

// Default is the strict, fully-conformant profile.
func Default() Profile { return registry["strict-3gpp"] }

// Get returns the named profile. Unknown names return the default and false.
func Get(name string) (Profile, bool) {
	if name == "" {
		return Default(), true
	}
	p, ok := registry[name]
	if !ok {
		return Default(), false
	}
	return p, true
}

// Names lists the registered profile names (sorted), for CLI help/validation.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Active lists the quirk names enabled in this profile — the conformance
// scorecard ORBIT reports for the core under test.
func (p Profile) Active() []string {
	var out []string
	if p.Quirks.HandoverAckForwardingMandatory {
		out = append(out, "HandoverAckForwardingMandatory")
	}
	return out
}
