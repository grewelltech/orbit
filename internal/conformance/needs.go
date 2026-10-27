package conformance

import "slices"

// CheckNeeds is what one check takes from the target while it runs, so a
// run can lease it before the suite starts (ADR-0008 §5, open question 6):
// how many UEs it registers at once, and the roles of the gNB IDs it
// presents (RunStream gives each role its ID; see gnbIDsOf).
type CheckNeeds struct {
	// UEs is the most subscribers the check registers at the same time.
	UEs int
	// GNBRoles are the gNBs it presents: its own, a handover target, the
	// liveness probe.
	GNBRoles []GNBRole
}

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
// registered check.
func (r *Registry) PackedSpan() int { return 3 * len(r.tests) }

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

// Needer is a check that declares its needs. A check that does not needs
// no UE, and presents its own gNB ID and the liveness probe's.
type Needer interface {
	Needs() CheckNeeds
}

// defaultNeeds is a check's needs when it declares none.
var defaultNeeds = CheckNeeds{GNBRoles: []GNBRole{RoleOwn, RoleLiveness}}

// ueNeeds is a check that registers Env.UE (through a UEHarness) on its
// own gNB, probing liveness after.
var ueNeeds = CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}}

// NeedsOf is a check's declared needs, or the default.
func NeedsOf(t Test) CheckNeeds {
	if n, ok := t.(Needer); ok {
		return n.Needs()
	}
	return defaultNeeds
}

// SuiteNeeds is what a run of the selected checks takes: UEs is the most
// any one check needs, since checks run one after another; GNBIDs is every
// ID a selected check presents, ascending.
type SuiteNeeds struct {
	UEs    int
	GNBIDs []uint32
}

// Needs is the needs of the checks in cats (every check when empty) with
// gNB IDs from base in the layout packed picks, as RunStream assigns them.
func (r *Registry) Needs(base uint32, packed bool, cats ...Category) SuiteNeeds {
	var out SuiteNeeds
	seen := map[uint32]bool{}
	for _, t := range r.selected(cats) {
		n := NeedsOf(t)
		out.UEs = max(out.UEs, n.UEs)
		own, target, liveness := r.gnbIDsOf(base, r.indexOf(t), packed)
		for _, role := range n.GNBRoles {
			id := map[GNBRole]uint32{RoleOwn: own, RoleHandover: target, RoleLiveness: liveness}[role]
			if !seen[id] {
				seen[id] = true
				out.GNBIDs = append(out.GNBIDs, id)
			}
		}
	}
	slices.Sort(out.GNBIDs)
	return out
}

func (endMarkerOnPathSwitch) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleHandover, RoleLiveness}}
}
func (nasSMCNoNIA0) Needs() CheckNeeds             { return ueNeeds }
func (nasT3560AuthRetransmit) Needs() CheckNeeds   { return ueNeeds }
func (nasT3550AcceptRetransmit) Needs() CheckNeeds { return ueNeeds }
