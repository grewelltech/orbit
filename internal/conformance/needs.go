package conformance

import "slices"

// CheckNeeds is what one check takes from the target while it runs, so a
// run can lease it before the suite starts (ADR-0008 §5, open question 6):
// how many UEs it registers at once, and the gNB IDs it presents, as
// offsets from its own ID (base + its registry index; see RunStream).
type CheckNeeds struct {
	// UEs is the most subscribers the check registers at the same time.
	UEs int
	// GNBOffsets are the IDs it presents beyond its own: 0 is its own,
	// targetGNBOffset a handover target, aliveGNBOffset the liveness probe.
	GNBOffsets []uint32
	// RequiresIdleCore marks a check whose verdict a loaded core would
	// distort: under a concurrent population it SKIPs, IdleCoreReason
	// saying why, rather than run and blame the core for the load.
	RequiresIdleCore bool
	IdleCoreReason   string
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

// defaultNeeds is a check's needs when it declares none.
var defaultNeeds = CheckNeeds{GNBOffsets: []uint32{0, aliveGNBOffset}}

// ueNeeds is a check that registers Env.UE (through a UEHarness) on its
// own gNB, probing liveness after.
var ueNeeds = CheckNeeds{UEs: 1, GNBOffsets: []uint32{0, aliveGNBOffset}}

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
// gNB IDs from base, as RunStream assigns them.
func (r *Registry) Needs(base uint32, cats ...Category) SuiteNeeds {
	var out SuiteNeeds
	seen := map[uint32]bool{}
	for _, t := range r.selected(cats) {
		n := NeedsOf(t)
		out.UEs = max(out.UEs, n.UEs)
		own := base + uint32(r.indexOf(t))
		for _, off := range n.GNBOffsets {
			if id := own + off; !seen[id] {
				seen[id] = true
				out.GNBIDs = append(out.GNBIDs, id)
			}
		}
	}
	slices.Sort(out.GNBIDs)
	return out
}

// GNBPattern is the gNB IDs a run of the checks in cats presents, as
// offsets from the base, ascending: what a base must leave free.
func (r *Registry) GNBPattern(cats ...Category) []uint32 {
	return r.Needs(0, cats...).GNBIDs
}

func (endMarkerOnPathSwitch) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, GNBOffsets: []uint32{0, targetGNBOffset, aliveGNBOffset}}
}
func (nasSMCNoNIA0) Needs() CheckNeeds { return ueNeeds }

// The NAS timer checks measure retransmissions against tmax windows, which
// a loaded AMF's scheduling jitter would push out.
func (nasT3560AuthRetransmit) Needs() CheckNeeds   { return timerNeeds }
func (nasT3550AcceptRetransmit) Needs() CheckNeeds { return timerNeeds }

var timerNeeds = CheckNeeds{UEs: 1, GNBOffsets: []uint32{0, aliveGNBOffset}, RequiresIdleCore: true, IdleCoreReason: timerWindowsReason}
