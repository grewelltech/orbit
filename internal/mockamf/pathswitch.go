package mockamf

// PathSwitchAnswer selects how the mock answers a PATH SWITCH REQUEST that
// names a known UE and session (WithPathSwitchAnswer).
type PathSwitchAnswer int

const (
	// AckPathSwitch answers with a PATH SWITCH REQUEST ACKNOWLEDGE whose
	// Switched List carries a transfer per session: the default.
	AckPathSwitch PathSwitchAnswer = iota
	// AckWithoutTransfer acknowledges with an empty Path Switch Request
	// Acknowledge Transfer in every Switched List item.
	AckWithoutTransfer
	// AckWithNewUL moves each switched session's uplink tunnel at the UPF
	// and names the new one in the transfer's UL NG-U UP TNL Information,
	// as Config.ReallocateULOnPathSwitch does.
	AckWithNewUL
	// AckHidingULChange moves each switched session's uplink tunnel at the
	// UPF and acknowledges without UL NG-U UP TNL Information, so a gNB
	// that keeps the old uplink endpoint, as TS 38.413 8.4.4.2 has it do,
	// sends to a TEID the UPF no longer serves.
	AckHidingULChange
	// FailPathSwitch answers with a PATH SWITCH REQUEST FAILURE and leaves
	// the sessions where they were.
	FailPathSwitch
	// IgnorePathSwitch leaves the request unanswered.
	IgnorePathSwitch
	// CrashOnPathSwitch ends the association and every other one, and the
	// mock answers no later NG Setup.
	CrashOnPathSwitch
)

// WithPathSwitchAnswer makes the mock answer a PATH SWITCH REQUEST as ans
// says. The default is AckPathSwitch.
func WithPathSwitchAnswer(ans PathSwitchAnswer) Option {
	return func(a *AMF) { a.pathSwitchAnswer = ans }
}
