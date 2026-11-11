package conformance

import (
	"fmt"
	"sort"
	"strings"
)

// Procedure names a procedure that checks depend on completing (ADR-0011
// rule 5). The list is fixed: exactly one registered check owns each
// procedure another check needs, and a dependent whose prerequisite the
// core does not complete reports BLOCKED, naming the procedure and its
// owner.
type Procedure string

const (
	// ProcNGSetup is an NG Setup the AMF accepts.
	ProcNGSetup Procedure = "NG Setup"
	// ProcRegistration is an initial registration, through to the
	// Registration Accept.
	ProcRegistration Procedure = "registration"
	// ProcPDUSession is a UE-requested PDU session establishment, through
	// to the N2 resource setup.
	ProcPDUSession Procedure = "PDU session establishment"
	// ProcDeregistration is a UE-initiated deregistration the AMF accepts.
	ProcDeregistration Procedure = "deregistration"
	// ProcANRelease is an AN release: the AMF's UE CONTEXT RELEASE COMMAND
	// for a UE CONTEXT RELEASE REQUEST.
	ProcANRelease Procedure = "AN release"
	// ProcServiceRequest is a Service Request the AMF accepts.
	ProcServiceRequest Procedure = "Service Request"
	// ProcXnPathSwitch is an Xn handover's path switch: the AMF's PATH
	// SWITCH REQUEST ACKNOWLEDGE.
	ProcXnPathSwitch Procedure = "Xn path switch"
	// ProcN2HandoverPreparation is an N2 handover's preparation, through to
	// the HANDOVER COMMAND.
	ProcN2HandoverPreparation Procedure = "N2 handover preparation"
	// ProcGTPUEcho is the UPF answering a GTP-U Echo Request on N3.
	ProcGTPUEcho Procedure = "GTP-U Echo"
)

// procedures is the fixed list.
var procedures = []Procedure{
	ProcNGSetup, ProcRegistration, ProcPDUSession, ProcDeregistration, ProcANRelease,
	ProcServiceRequest, ProcXnPathSwitch, ProcN2HandoverPreparation, ProcGTPUEcho,
}

// Procedures returns the fixed list of procedure names.
func Procedures() []Procedure { return append([]Procedure(nil), procedures...) }

// valid reports whether p is on the fixed list.
func (p Procedure) valid() bool {
	for _, x := range procedures {
		if p == x {
			return true
		}
	}
	return false
}

// article is the procedure as it reads in a sentence: "an NG Setup", "a
// registration".
func (p Procedure) article() string {
	switch p {
	case ProcNGSetup, ProcXnPathSwitch, ProcN2HandoverPreparation, ProcANRelease:
		return "an " + string(p)
	}
	return "a " + string(p)
}

// Owner is a check that owns the question whether a procedure completes.
// Its class follows from what the specification says of the network
// completing the procedure, like any check's; its outcomes that report the
// procedure as not completing carry Outcome.NotCompleted.
type Owner interface {
	// Owns returns the procedures the check owns.
	Owns() []Procedure
}

// OwnsOf returns the procedures t owns, none for a check that owns none.
func OwnsOf(t Test) []Procedure {
	if o, ok := t.(Owner); ok {
		return o.Owns()
	}
	return nil
}

// owns reports whether t owns p.
func owns(t Test, p Procedure) bool {
	for _, x := range OwnsOf(t) {
		if x == p {
			return true
		}
	}
	return false
}

// OwnerOf returns the ID of the registered check that owns p, "" when none
// does.
func (r *Registry) OwnerOf(p Procedure) string {
	for _, t := range r.tests {
		if owns(t, p) {
			return t.ID()
		}
	}
	return ""
}

// owners maps each owned procedure to the ID of the check that owns it.
func (r *Registry) owners() map[Procedure]string {
	out := map[Procedure]string{}
	for _, t := range r.tests {
		for _, p := range OwnsOf(t) {
			out[p] = t.ID()
		}
	}
	return out
}

// Validate checks the registry as a whole (ADR-0011 rule 5): every
// procedure a check owns or needs is on the fixed list, no procedure has
// two owners, no check needs a procedure it owns, and every needed
// procedure has a registered owner. NewRegistry runs it over the built-in
// checks, so a renamed or removed owner fails at start-up.
func (r *Registry) Validate() error {
	owner := map[Procedure]string{}
	for _, t := range r.tests {
		for _, p := range OwnsOf(t) {
			if !p.valid() {
				return fmt.Errorf("%s: owns %q, which is not a procedure of the fixed list", t.ID(), p)
			}
			if other, dup := owner[p]; dup {
				return fmt.Errorf("procedure %q has two owners: %s and %s", p, other, t.ID())
			}
			owner[p] = t.ID()
		}
	}
	var missing []string
	for _, t := range r.tests {
		for _, p := range NeedsOf(t).Procedures {
			switch {
			case !p.valid():
				return fmt.Errorf("%s: needs %q, which is not a procedure of the fixed list", t.ID(), p)
			case owns(t, p):
				return fmt.Errorf("%s: needs %q, which it owns; an owner's own attempt is its verdict, never BLOCKED", t.ID(), p)
			case owner[p] == "":
				missing = append(missing, fmt.Sprintf("%s needs %q", t.ID(), p))
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("no registered check owns a needed procedure: %s", strings.Join(missing, "; "))
	}
	return nil
}
