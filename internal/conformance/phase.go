package conformance

import "slices"

// Phase is when in a suite a check runs. A suite runs its phases in order,
// and the checks of one phase in registration order, so the run order is
// the same on every run. A check's phase changes only its place in the
// run: its gNB IDs and its subscribers follow its place in the registry
// (gnbIDsOf, subscriberBlock).
type Phase int

const (
	// PhaseMain is the phase of a check that declares none.
	PhaseMain Phase = iota
	// PhaseStranding is for a check that by design leaves a UE context in
	// the core that the core must clear by its own timers. Those contexts
	// then exist only after the main checks have run.
	PhaseStranding
	// PhaseDisruptive is for a check whose stimulus a core may not survive.
	// It runs last of all, so a core it takes down has already answered
	// every other check.
	PhaseDisruptive
)

// String names the phase as --list shows it.
func (p Phase) String() string {
	switch p {
	case PhaseMain:
		return "main"
	case PhaseStranding:
		return "stranding"
	case PhaseDisruptive:
		return "disruptive"
	}
	return "unknown"
}

// Phaser is a check that declares the phase it runs in. A check that does
// not runs in PhaseMain.
type Phaser interface {
	Phase() Phase
}

// PhaseOf is a check's declared phase, or PhaseMain.
func PhaseOf(t Test) Phase {
	if p, ok := t.(Phaser); ok {
		return p.Phase()
	}
	return PhaseMain
}

// runOrder sorts tests, which are in registration order, into the order a
// suite runs them: by phase, registration order kept within a phase.
func runOrder(tests []Test) {
	slices.SortStableFunc(tests, func(a, b Test) int { return int(PhaseOf(a)) - int(PhaseOf(b)) })
}

// The checks that withhold a reply, or break the UE's security context, and
// close the association with the UE not deregistered: the AMF keeps the UE
// context until its own timers clear it. They keep their order, so each NAS
// timer check still follows a check that waited out the core's
// retransmissions.

// Phase is PhaseStranding: REGISTRATION COMPLETE is never sent, and the AMF
// is left in 5GMM-REGISTERED with no N2 connection for the UE.
func (nasRetransmitNewCount) Phase() Phase { return PhaseStranding }

// Phase is PhaseStranding: REGISTRATION COMPLETE is never sent, and the AMF
// is left in 5GMM-REGISTERED with no N2 connection for the UE.
func (nasT3550AcceptRetransmit) Phase() Phase { return PhaseStranding }

// Phase is PhaseStranding: the authentication is never answered, and a core
// that does not release the UE at the fifth expiry keeps it.
func (nasT3560AuthRetransmit) Phase() Phase { return PhaseStranding }

// Phase is PhaseStranding: an AMF that advanced its uplink NAS COUNT can
// verify neither the session releases nor the deregistration of the
// cleanup, and keeps the UE and its sessions.
func (nasCountNotAdvanced) Phase() Phase { return PhaseStranding }

// Phase is PhaseDisruptive: the PATH SWITCH REQUEST with mismatching UE
// security capabilities is a message a core may not survive.
func (ngapPathSwitchSecCapMismatch) Phase() Phase { return PhaseDisruptive }
