package conformance

import "slices"

// CheckNeeds is what one check takes from the target while it runs, so a
// run can lease it before the suite starts (ADR-0008 §5, open question 6):
// how many UEs it registers at once, and the roles of the gNB IDs it
// presents (RunStream gives each role its ID; see gnbIDsOf).
type CheckNeeds struct {
	// UEs is the most subscribers the check registers at the same time.
	UEs int
	// Subscribers is how many subscribers it registers over its run, when
	// that is more than UEs: a check that registers several UEs one after
	// another gives each its own (UEHarness.UseSubscriber). Zero means UEs.
	Subscribers int
	// GNBRoles are the gNBs it presents: its own, a handover target, the
	// liveness probe.
	GNBRoles []GNBRole
	// RequiresIdleCore marks a check whose verdict a loaded core would
	// distort: under a concurrent population it SKIPs, IdleCoreReason
	// saying why, rather than run and blame the core for the load.
	RequiresIdleCore bool
	IdleCoreReason   string
	// SecondN2Source marks a check that holds a second association from
	// another N2 source address (Env.SecondBind) and SKIPs without one.
	SecondN2Source bool
	// Procedures are the procedures the check needs the core to complete
	// before its requirement can be tested (ADR-0011 rule 5). The check
	// reports BLOCKED, naming the procedure, when the core does not
	// complete one; the run's preflight is built from them.
	Procedures []Procedure
	// Inputs are the inputs the check SKIPs without, beyond the ones the
	// other fields imply: a UE for UEs, a second N2 source address for
	// SecondN2Source and an idle core for RequiresIdleCore.
	Inputs []Input
}

// inputs is every input the check SKIPs without: the declared ones and the
// ones its other needs imply.
func (n CheckNeeds) inputs() []Input {
	var out []Input
	add := func(in Input) {
		if !slices.Contains(out, in) {
			out = append(out, in)
		}
	}
	if n.UEs > 0 {
		add(InputUE)
	}
	if n.SecondN2Source {
		add(InputSecondN2)
	}
	if n.RequiresIdleCore {
		add(InputIdleCore)
	}
	for _, in := range n.Inputs {
		add(in)
	}
	return out
}

// needs reports whether the check declares procedure p as a need.
func (n CheckNeeds) needs(p Procedure) bool { return slices.Contains(n.Procedures, p) }

// GNBRole is a gNB a check presents.
type GNBRole int

const (
	// RoleOwn is the check's own gNB (Env.GNB).
	RoleOwn GNBRole = iota + 1
	// RoleHandover is a handover target (Env.TargetGNBID).
	RoleHandover
	// RoleLiveness is the liveness probe (Env.LivenessGNBID).
	RoleLiveness
)

// The legacy layout's offsets from a check's own ID, kept for a suite
// whose target does not bound its gNB IDs (the RunConformance wrapper's
// base 700 among them), so its runs present exactly the IDs they always
// have: the handover target at +0x8000, the liveness probe at +0x9000.
const (
	legacyTargetOffset   = 0x8000
	legacyLivenessOffset = 0x9000
)

// preflightGNBID is the gNB ID the run's preflight presents from base, the
// one after the last check's liveness probe in either layout: base+3N
// packed, base+0x9000+N otherwise.
func (r *Registry) preflightGNBID(base uint32, packed bool) uint32 {
	n := uint32(len(r.tests))
	if packed {
		return base + 3*n
	}
	return base + legacyLivenessOffset + n
}

// gnbIDsOf is the IDs check idx (its position in the full registry, so
// stable across category filters) presents from base. Packed, for a
// target that bounds its IDs, the suite's N checks take base..base+3N-1:
// own IDs base+i, handover targets base+N+i, probes base+2N+i. Otherwise
// the legacy layout: base+i, and +0x8000 and +0x9000 from it. The AMF does
// not cleanly re-key a reused gNB ID from a new association, so every
// role of every check has its own ID either way.
func (r *Registry) gnbIDsOf(base uint32, idx int, packed bool) (own, target, liveness uint32) {
	own = base + uint32(idx)
	if packed {
		n := uint32(len(r.tests))
		return own, own + n, own + 2*n
	}
	return own, own + legacyTargetOffset, own + legacyLivenessOffset
}

// PackedSpan is how many gNB IDs a packed suite's block needs: three per
// registered check, and the preflight's one.
func (r *Registry) PackedSpan() int { return 3*len(r.tests) + 1 }

// targetGNBID and livenessGNBID are the check's IDs for those roles: as
// RunStream assigned them, or (for a check run outside RunStream) the
// legacy layout's from its own ID.
func (e Env) targetGNBID() uint32 {
	if e.TargetGNBID != 0 {
		return e.TargetGNBID
	}
	return e.GNB.ID + legacyTargetOffset
}

func (e Env) livenessGNBID() uint32 {
	if e.LivenessGNBID != 0 {
		return e.LivenessGNBID
	}
	return e.GNB.ID + legacyLivenessOffset
}

// ObservedNeedsIdleCore is a check SKIPped because a population loads
// the core.
const ObservedNeedsIdleCore = "needs an idle core"

// timerWindowsReason is why the NAS timer checks need an idle core.
const timerWindowsReason = "measures timer windows; needs an idle core"

// Needer is a check that declares its needs. A check that does not needs
// no UE, and presents its own gNB ID and the liveness probe's.
type Needer interface {
	Needs() CheckNeeds
}

// defaultNeeds is a check's needs when it declares none: its own NG
// association, which it sets up before its stimulus, and the liveness
// probe.
var defaultNeeds = CheckNeeds{GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: []Procedure{ProcNGSetup}}

// rawNeeds is a check whose stimulus is the NG Setup itself: it needs no
// procedure to complete first.
var rawNeeds = CheckNeeds{GNBRoles: []GNBRole{RoleOwn, RoleLiveness}}

// The procedures a UE check needs first: its registration, and for a check
// that uses the UE's PDU session, the session too.
var (
	registrationFirst = []Procedure{ProcNGSetup, ProcRegistration}
	sessionFirst      = []Procedure{ProcNGSetup, ProcRegistration, ProcPDUSession}
)

// ueNeeds is a check that registers Env.UE (through a UEHarness) on its
// own gNB, probing liveness after. ueSessionNeeds is one that also needs
// the UE's PDU session.
var (
	ueNeeds        = CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: registrationFirst}
	ueSessionNeeds = CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: sessionFirst}
)

// mobilityNeeds is a check that registers a UE with a PDU session and
// presents a second gNB, the target of a handover, and also needs the
// procedures in more.
func mobilityNeeds(more ...Procedure) CheckNeeds {
	return CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleHandover, RoleLiveness},
		Procedures: append(append([]Procedure(nil), sessionFirst...), more...)}
}

// NeedsOf is a check's declared needs, or the default.
func NeedsOf(t Test) CheckNeeds {
	if n, ok := t.(Needer); ok {
		return n.Needs()
	}
	return defaultNeeds
}

// SuiteNeeds is what a run of the selected checks takes: UEs is the most
// any one check needs, since checks run one after another; GNBIDs is every
// ID a selected check presents and, when one of them signals on N2, the
// preflight's, ascending; SecondN2Source is set when any of them uses a
// second N2 source address.
type SuiteNeeds struct {
	UEs    int
	GNBIDs []uint32
	// SecondN2Source is whether a selected check uses a second N2 source
	// address.
	SecondN2Source bool
}

// Needs is the needs of the checks in cats (every check when empty) with
// gNB IDs from base in the layout packed picks, as RunStream assigns them.
func (r *Registry) Needs(base uint32, packed bool, cats ...Category) SuiteNeeds {
	var out SuiteNeeds
	seen := map[uint32]bool{}
	preflight := false
	for _, t := range r.Select(cats...) {
		n := NeedsOf(t)
		preflight = preflight || n.usesN2()
		out.UEs = max(out.UEs, n.UEs)
		out.SecondN2Source = out.SecondN2Source || n.SecondN2Source
		own, target, liveness := r.gnbIDsOf(base, r.indexOf(t), packed)
		for _, role := range n.GNBRoles {
			id := map[GNBRole]uint32{RoleOwn: own, RoleHandover: target, RoleLiveness: liveness}[role]
			if !seen[id] {
				seen[id] = true
				out.GNBIDs = append(out.GNBIDs, id)
			}
		}
	}
	if id := r.preflightGNBID(base, packed); preflight && !seen[id] {
		out.GNBIDs = append(out.GNBIDs, id)
	}
	slices.Sort(out.GNBIDs)
	return out
}

// The raw NG Setup checks: the NG Setup is their stimulus.
func (ngSetupMissingMandatoryIE) Needs() CheckNeeds { return rawNeeds }
func (ngSetupUnknownPLMN) Needs() CheckNeeds        { return rawNeeds }

// The GTP-U checks address the UPF's N3 endpoint and nothing on N2. The
// unknown-TEID check tells a UPF that discards from one that is down by
// GTP-U Echo, which it needs answered.
func (gtpuEchoResponse) Needs() CheckNeeds {
	return CheckNeeds{GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Inputs: []Input{InputUPFN3}}
}
func (gtpuUnknownTEID) Needs() CheckNeeds {
	return CheckNeeds{GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Inputs: []Input{InputUPFN3}, Procedures: []Procedure{ProcGTPUEcho}}
}

func (endMarkerOnPathSwitch) Needs() CheckNeeds { return mobilityNeeds(ProcXnPathSwitch) }

// Needs is a responder to send the downlink that pages the idle UE.
func (pagingOnDLData) Needs() CheckNeeds {
	n := ueSessionNeeds
	n.Procedures = append(append([]Procedure(nil), sessionFirst...), ProcANRelease)
	n.Inputs = []Input{InputDownlink}
	return n
}

// Needs is a responder for the downlink that shows the reactivated user
// plane.
func (serviceRequestReactivates) Needs() CheckNeeds {
	n := ueSessionNeeds
	n.Procedures = append(append([]Procedure(nil), sessionFirst...), ProcANRelease)
	n.Inputs = []Input{InputDownlink}
	return n
}
func (nasPDUUnknownDNN) Needs() CheckNeeds             { return ueNeeds }
func (nasRegRejectedNSSAI) Needs() CheckNeeds          { return ueNeeds }
func (nasDeregAcceptNotSwitchOff) Needs() CheckNeeds   { return ueNeeds }
func (ngapCtxReleaseAfterSwitchOff) Needs() CheckNeeds { return ueNeeds }
func (nasPDUReleaseUERequested) Needs() CheckNeeds     { return ueSessionNeeds }

// Needs is one subscriber: the check registers the same SUPI each time.
func (nasRegistrationAfterRelease) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, Subscribers: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: registrationFirst}
}

// Needs is one UE at a time and a subscriber for each of the check's three
// registrations.
func (nasSMCNoNIA0) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, Subscribers: 3, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: registrationFirst}
}

// Needs is one UE: the check registers it after the update.
func (ranConfigUpdateKeepsTA) Needs() CheckNeeds { return ueNeeds }

func (ngapPathSwitchAckTransfer) Needs() CheckNeeds   { return mobilityNeeds() }
func (ngapHandoverCommandTransfer) Needs() CheckNeeds { return mobilityNeeds() }

// Needs is one UE at a time and a subscriber for each of the check's three
// registrations.
func (nasInitialMsgContainerWins) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, Subscribers: 3, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: registrationFirst}
}
func (nasInitialMsgNoContainer) Needs() CheckNeeds          { return ueNeeds }
func (nasInitialMsgNonCleartextAccepted) Needs() CheckNeeds { return ueNeeds }
func (nasSMCIMEISVRequest) Needs() CheckNeeds               { return ueNeeds }
func (ngapPathSwitchSecCapMismatch) Needs() CheckNeeds      { return mobilityNeeds() }
func (ngapHandoverCancelAck) Needs() CheckNeeds             { return mobilityNeeds(ProcN2HandoverPreparation) }
func (ngapUECtxReleaseOnRequest) Needs() CheckNeeds         { return ueNeeds }

// Needs is an echo peer, whose echoes are the downlink the check samples.
func (n3DLQFIContainer) Needs() CheckNeeds {
	n := ueSessionNeeds
	n.Inputs = []Input{InputTraffic}
	return n
}
func (nasIntegrityFailDiscard) Needs() CheckNeeds { return uplinkNeeds }
func (nasReplayDiscard) Needs() CheckNeeds        { return uplinkNeeds }
func (nasCountNotAdvanced) Needs() CheckNeeds     { return ueNeeds }
func (nasRetransmitNewCount) Needs() CheckNeeds   { return ueNeeds }
func (nasBadMACRegistration) Needs() CheckNeeds   { return ueNeeds }

// uplinkNeeds is a check that drives uplink NAS by hand on a registered UE
// (newUplinkDriver): it needs the core to answer its PDU Session
// Establishment Requests, and NAS integrity protection.
var uplinkNeeds = CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: sessionFirst, Inputs: []Input{InputIntegrity}}

// The NAS timer checks measure retransmissions against tmax windows, which
// a loaded AMF's scheduling jitter would push out.
func (nasT3560AuthRetransmit) Needs() CheckNeeds   { return timerNeeds }
func (nasT3550AcceptRetransmit) Needs() CheckNeeds { return timerNeeds }

var timerNeeds = CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, RequiresIdleCore: true, IdleCoreReason: timerWindowsReason,
	Procedures: registrationFirst}

// The owners of the procedures other checks need (ADR-0011 rule 5).
func (gtpuEchoResponse) Owns() []Procedure            { return []Procedure{ProcGTPUEcho} }
func (ngapUECtxReleaseOnRequest) Owns() []Procedure   { return []Procedure{ProcANRelease} }
func (ngapPathSwitchAckTransfer) Owns() []Procedure   { return []Procedure{ProcXnPathSwitch} }
func (ngapHandoverCommandTransfer) Owns() []Procedure { return []Procedure{ProcN2HandoverPreparation} }
func (nasDeregAcceptNotSwitchOff) Owns() []Procedure  { return []Procedure{ProcDeregistration} }
func (serviceRequestReactivates) Owns() []Procedure   { return []Procedure{ProcServiceRequest} }
